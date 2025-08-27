package main

import (
    "context"
    "crypto/ecdsa"
    "crypto/elliptic"
    "crypto/rand"
    "crypto/rsa"
    "crypto/sha256"
    "crypto/x509"
    "encoding/hex"
    "encoding/pem"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "time"

    "github.com/docker/docker/api/types"
    "github.com/docker/docker/api/types/filters"
    swm "github.com/docker/docker/api/types/swarm"
    docker "github.com/docker/docker/client"
    "github.com/go-acme/lego/v4/certificate"
    dnsprov "github.com/go-acme/lego/v4/providers/dns"
    "github.com/go-acme/lego/v4/lego"
    "github.com/go-acme/lego/v4/registration"
)

func env(key, def string) string {
    if v := os.Getenv(key); v != "" { return v }
    return def
}

func mustEnv(key string) string {
    v := os.Getenv(key)
    if v == "" { log.Fatalf("missing env %s", key) }
    return v
}

func sha256Hex(b []byte) string {
    h := sha256.Sum256(b)
    return hex.EncodeToString(h[:])
}

func main() {
    // Config
    // DOCKER_HOST 可选：若未设置且容器挂载了 /var/run/docker.sock，则默认走本地 unix socket
    labels := env("EDGE_SERVICE_LABELS", env("EDGE_SERVICE_LABEL", "edge.traefik.service=true"))
    interval := env("LOOP_INTERVAL", "12h")
    domainsCSV := mustEnv("DOMAINS")
    email := mustEnv("LEGO_EMAIL")
    provider := mustEnv("DNS_PROVIDER")
    renewDays := env("RENEW_DAYS", "30")
    keySet := env("KEY_SET", "ec256")
    acmeServer := env("ACME_SERVER", "")
    eabKid := env("EAB_KID", "")
    eabHmac := env("EAB_HMAC", "")
    certMode := strings.ToLower(env("CERT_MODE", "san")) // san | split
    retainN := parseInt(env("RETAIN_GENERATIONS", "2"), 2)
    persistDir := env("PERSIST_DIR", "/data/.lego")
    tlsCfgEnable := strings.EqualFold(env("TLS_CONFIG_ENABLE", "false"), "true")
    tlsCfgPrefix := env("TLS_CONFIG_NAME_PREFIX", "prod-edge-traefik-certs")
    tlsCfgTarget := env("TLS_CONFIG_TARGET", "/etc/traefik/dynamic/certs.yml")
    // Optional: provider env from files, e.g. "CF_API_TOKEN=/run/secrets/CF_API_TOKEN,ALICLOUD_ACCESS_KEY=/run/secrets/ALI_KEY,..."
    loadEnvFiles(env("PROVIDER_ENV_FILES", ""))

    d, err := time.ParseDuration(interval)
    if err != nil { d = 12 * time.Hour }

    // Prefer env; fallback to default socket; enable API version negotiation
    opts := []docker.Opt{docker.FromEnv, docker.WithAPIVersionNegotiation()}
    if h := os.Getenv("DOCKER_HOST"); h != "" {
        opts = append(opts, docker.WithHost(h))
    }
    cli, err := docker.NewClientWithOpts(opts...)
    if err != nil { log.Fatal(err) }
    defer cli.Close()

    for {
        ctx := context.Background()
        svcs, err := cli.ServiceList(ctx, swm.ServiceListOptions{Filters: buildLabelFilter(labels)})
        if err != nil { log.Println("service list:", err) } else {
            if len(svcs) == 0 { log.Println("no traefik service matched labels:", labels) }
            for _, s := range svcs {
                domains := parseCSV(domainsCSV)
                if len(domains) == 0 { log.Println("no domains provided"); continue }
                ts := time.Now().Format("200601021504")
                var secretsToAdd []*swm.SecretReference
                var cfgName, cfgID string

                if certMode == "split" {
                    if tlsCfgEnable {
                        cfgName = fmt.Sprintf("%s_%s.yml", tlsCfgPrefix, ts)
                    }
                    // Build certs.yml dynamically
                    var b strings.Builder
                    if tlsCfgEnable { b.WriteString("tls:\n  certificates:\n") }
                    for _, dmn := range domains {
                        due, _ := isDueForRenew(filepath.Join(persistDir, dmn+".crt"), renewDays)
                        var certPEM, keyPEM []byte
                        if due {
                            certPEM, keyPEM, err = obtainOrRenew(email, provider, keySet, acmeServer, eabKid, eabHmac, []string{dmn})
                            if err != nil { log.Println("acme obtain:", dmn, err); continue }
                        } else {
                            certPEM, _ = os.ReadFile(filepath.Join(persistDir, dmn+".crt"))
                            keyPEM, _ = os.ReadFile(filepath.Join(persistDir, dmn+".key"))
                            if len(certPEM) == 0 || len(keyPEM) == 0 {
                                certPEM, keyPEM, err = obtainOrRenew(email, provider, keySet, acmeServer, eabKid, eabHmac, []string{dmn})
                                if err != nil { log.Println("acme obtain:", dmn, err); continue }
                            }
                        }
                        _ = os.MkdirAll(persistDir, 0o755)
                        _ = os.WriteFile(filepath.Join(persistDir, dmn+".crt"), certPEM, 0o600)
                        _ = os.WriteFile(filepath.Join(persistDir, dmn+".key"), keyPEM, 0o600)

                        safe := strings.NewReplacer("*", "star", ".", "-").Replace(dmn)
                        crtName := "edge_tls_crt_" + safe + "_" + ts
                        keyName := "edge_tls_key_" + safe + "_" + ts
                        crtID, err := createOrReplaceSecret(ctx, cli, crtName, certPEM)
                        if err != nil { log.Println("secret crt:", err); continue }
                        keyID, err := createOrReplaceSecret(ctx, cli, keyName, keyPEM)
                        if err != nil { log.Println("secret key:", err); continue }
                        secretsToAdd = append(secretsToAdd,
                            &swm.SecretReference{SecretID: crtID, SecretName: crtName, File: &swm.SecretReferenceFileTarget{Name: crtName, Mode: 0o400}},
                            &swm.SecretReference{SecretID: keyID, SecretName: keyName, File: &swm.SecretReferenceFileTarget{Name: keyName, Mode: 0o400}},
                        )
                        if tlsCfgEnable {
                            b.WriteString("    - certFile: /run/secrets/")
                            b.WriteString(crtName)
                            b.WriteString("\n      keyFile: /run/secrets/")
                            b.WriteString(keyName)
                            b.WriteString("\n")
                        }
                    }
                    if tlsCfgEnable {
                        data := []byte(b.String())
                        if id, err := createOrReplaceConfig(ctx, cli, cfgName, data); err != nil { log.Println("config:", err) } else { cfgID = id }
                    }
                } else { // san
                    mainDomain := domains[0]
                    // Decide renew window
                    due, _ := isDueForRenew(filepath.Join(persistDir, mainDomain+".crt"), renewDays)
                    var certPEM, keyPEM []byte
                    if due {
                        certPEM, keyPEM, err = obtainOrRenew(email, provider, keySet, acmeServer, eabKid, eabHmac, domains)
                        if err != nil { log.Println("acme obtain:", err); continue }
                    } else {
                        certPEM, _ = os.ReadFile(filepath.Join(persistDir, mainDomain+".crt"))
                        keyPEM, _ = os.ReadFile(filepath.Join(persistDir, mainDomain+".key"))
                        if len(certPEM) == 0 || len(keyPEM) == 0 {
                            certPEM, keyPEM, err = obtainOrRenew(email, provider, keySet, acmeServer, eabKid, eabHmac, domains)
                            if err != nil { log.Println("acme obtain:", err); continue }
                        }
                    }
                    _ = os.MkdirAll(persistDir, 0o755)
                    _ = os.WriteFile(filepath.Join(persistDir, mainDomain+".crt"), certPEM, 0o600)
                    _ = os.WriteFile(filepath.Join(persistDir, mainDomain+".key"), keyPEM, 0o600)
                    crtName := "edge_tls_crt_" + ts
                    keyName := "edge_tls_key_" + ts
                    crtID, err := createOrReplaceSecret(ctx, cli, crtName, certPEM)
                    if err != nil { log.Println("secret crt:", err); continue }
                    keyID, err := createOrReplaceSecret(ctx, cli, keyName, keyPEM)
                    if err != nil { log.Println("secret key:", err); continue }
                    secretsToAdd = append(secretsToAdd,
                        &swm.SecretReference{SecretID: crtID, SecretName: crtName, File: &swm.SecretReferenceFileTarget{Name: "edge_tls_crt", Mode: 0o400}},
                        &swm.SecretReference{SecretID: keyID, SecretName: keyName, File: &swm.SecretReferenceFileTarget{Name: "edge_tls_key", Mode: 0o400}},
                    )
                    if tlsCfgEnable {
                        cfgName = fmt.Sprintf("%s_%s.yml", tlsCfgPrefix, ts)
                        data := []byte("tls:\n  certificates:\n    - certFile: /run/secrets/edge_tls_crt\n      keyFile: /run/secrets/edge_tls_key\n")
                        if id, err := createOrReplaceConfig(ctx, cli, cfgName, data); err != nil { log.Println("config:", err) } else { cfgID = id }
                    }
                }

                // Update service: replace any previous refs and apply new ones
                if err := updateServiceSecretsAndConfigs(ctx, cli, s, secretsToAdd, tlsCfgEnable, cfgID, cfgName, tlsCfgPrefix, tlsCfgTarget); err != nil {
                    log.Println("service update:", err)
                } else {
                    log.Println("updated service:", s.Spec.Name)
                    // GC old secrets/configs beyond retention
                    if err := gcOldSecrets(ctx, cli, s, retainN); err != nil { log.Println("gc secrets:", err) }
                    if tlsCfgEnable { if err := gcOldConfigs(ctx, cli, s, tlsCfgPrefix, retainN); err != nil { log.Println("gc configs:", err) } }
                }
            }
        }
        time.Sleep(d)
    }
}

func buildLabelFilter(csv string) filters.Args {
    f := filters.NewArgs()
    for _, kv := range strings.Split(csv, ",") {
        kv = strings.TrimSpace(kv)
        if kv == "" { continue }
        parts := strings.SplitN(kv, "=", 2)
        if len(parts) != 2 { continue }
        f.Add("label", fmt.Sprintf("%s=%s", parts[0], parts[1]))
    }
    return f
}

// ACME with lego (DNS-01)
func obtainOrRenew(email, provider, keySet, acmeServer, eabKid, eabHmac string, domains []string) ([]byte, []byte, error) {
    // minimal user with in-memory key (new each run is fine when reuse-key true)
    u := &legoUser{email: email}
    u.generateKey(keySet)
    cfg := lego.NewConfig(u)
    if acmeServer != "" { cfg.CADirURL = acmeServer }
    client, err := lego.NewClient(cfg)
    if err != nil { return nil, nil, err }
    // DNS provider via env
    prov, err := dnsprov.NewDNSChallengeProviderByName(provider)
    if err != nil { return nil, nil, err }
    if err := client.Challenge.SetDNS01Provider(prov); err != nil { return nil, nil, err }
    if eabKid != "" && eabHmac != "" {
        if _, err = client.Registration.RegisterWithExternalAccountBinding(
            registration.RegisterEABOptions{Kid: eabKid, HmacEncoded: eabHmac, TermsOfServiceAgreed: true},
        ); err != nil { return nil, nil, err }
    } else {
        if _, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil { return nil, nil, err }
    }
    // Obtain
    req := certificate.ObtainRequest{Domains: domains, Bundle: true}
    res, err := client.Certificate.Obtain(req)
    if err != nil { return nil, nil, err }
    return res.Certificate, res.PrivateKey, nil
}

type legoUser struct { email string; key interface{} }
func (u *legoUser) GetEmail() string                        { return u.email }
func (u *legoUser) GetRegistration() *registration.Resource { return nil }
func (u *legoUser) GetPrivateKey() interface{}              { return u.key }
func (u *legoUser) generateKey(keySet string) {
    switch keySet {
    case "rsa2048": k, _ := rsa.GenerateKey(rand.Reader, 2048); u.key = k
    case "rsa4096": k, _ := rsa.GenerateKey(rand.Reader, 4096); u.key = k
    case "ec384": u.key, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
    default: u.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
    }
}

func isDueForRenew(certPath, renewDaysStr string) (bool, error) {
    b, err := os.ReadFile(certPath)
    if err != nil { return true, nil } // no cert -> need obtain
    block, _ := pem.Decode(b)
    if block == nil { return true, nil }
    cert, err := x509.ParseCertificate(block.Bytes)
    if err != nil { return true, nil }
    days := 30
    if v, err := time.ParseDuration(renewDaysStr+"24h"); err == nil { days = int(v.Hours()/24) }
    renewAt := cert.NotAfter.AddDate(0, 0, -days)
    return time.Now().After(renewAt), nil
}

func createOrReplaceSecret(ctx context.Context, cli *docker.Client, name string, data []byte) (string, error) {
    // delete if exists
    lst, err := cli.SecretList(ctx, types.SecretListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
    if err == nil && len(lst) > 0 {
        _ = cli.SecretRemove(ctx, lst[0].ID)
    }
    id, err := cli.SecretCreate(ctx, swm.SecretSpec{Name: name, Data: data})
    if err != nil { return "", err }
    return id.ID, nil
}

func createOrReplaceConfig(ctx context.Context, cli *docker.Client, name string, data []byte) (string, error) {
    lst, err := cli.ConfigList(ctx, types.ConfigListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
    if err == nil && len(lst) > 0 { _ = cli.ConfigRemove(ctx, lst[0].ID) }
    id, err := cli.ConfigCreate(ctx, swm.ConfigSpec{Name: name, Data: data})
    if err != nil { return "", err }
    return id.ID, nil
}

func updateServiceSecretsAndConfigs(ctx context.Context, cli *docker.Client, svc swm.Service, newSecrets []*swm.SecretReference, withCfg bool, cfgID, cfgName, cfgPrefix, cfgTarget string) error {
    inspect, _, err := cli.ServiceInspectWithRaw(ctx, svc.ID, types.ServiceInspectOptions{})
    if err != nil { return err }
    spec := inspect.Spec
    cs := spec.TaskTemplate.ContainerSpec
    // 清理旧前缀的动态 secrets（split 模式会大量增加，先移除同前缀再追加新集合）
    kept := make([]*swm.SecretReference, 0, len(cs.Secrets))
    for _, r := range cs.Secrets {
        if strings.HasPrefix(r.SecretName, "edge_tls_crt_") || strings.HasPrefix(r.SecretName, "edge_tls_key_") {
            // 跳过旧的版本化 secret
            continue
        }
        // 占位名让位于新集合，保留非证书相关 secret
        if r.SecretName == "edge_tls_crt_00000000" || r.SecretName == "edge_tls_key_00000000" { continue }
        kept = append(kept, r)
    }
    cs.Secrets = append(kept, newSecrets...)

    // Configs update (optional)
    if withCfg {
        cfgs := make([]*swm.ConfigReference, 0, len(cs.Configs)+1)
        for _, c := range cs.Configs {
            if strings.HasPrefix(c.ConfigName, cfgPrefix) { continue }
            cfgs = append(cfgs, c)
        }
        cfgs = append(cfgs, &swm.ConfigReference{ConfigID: cfgID, ConfigName: cfgName, File: &swm.ConfigReferenceFileTarget{Name: filepath.Base(cfgTarget), Mode: 0o444}})
        cs.Configs = cfgs
    }

    // Ensure start-first
    if spec.UpdateConfig == nil { spec.UpdateConfig = &swm.UpdateConfig{} }
    order := swm.UpdateOrderStartFirst
    spec.UpdateConfig.Order = &order
    spec.TaskTemplate.ContainerSpec = cs
    // bump force update
    spec.TaskTemplate.ForceUpdate++
    _, err = cli.ServiceUpdate(ctx, svc.ID, inspect.Version, spec, types.ServiceUpdateOptions{})
    return err
}

func parseCSV(csv string) []string {
    out := []string{}
    for _, s := range strings.Split(csv, ",") {
        s = strings.TrimSpace(s)
        if s != "" { out = append(out, s) }
    }
    return out
}

func parseInt(s string, def int) int {
    if s == "" { return def }
    var n int
    if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 { return n }
    return def
}

type secRef struct{ ID, Name, TS, Kind, Group string }

func splitSecretName(name string) (kind, group, ts string, ok bool) {
    // edge_tls_crt_YYYYMMDDHHMM          -> kind=crt, group=san, ts=...
    // edge_tls_crt_<safe>_YYYYMMDDHHMM   -> kind=crt, group=<safe>, ts=...
    if strings.HasPrefix(name, "edge_tls_crt_") { kind = "crt" } else if strings.HasPrefix(name, "edge_tls_key_") { kind = "key" } else { return "", "", "", false }
    rest := strings.TrimPrefix(strings.TrimPrefix(name, "edge_tls_crt_"), "edge_tls_key_")
    parts := strings.Split(rest, "_")
    if len(parts) == 1 { group = "san"; ts = parts[0] } else { group = strings.Join(parts[:len(parts)-1], "_"); ts = parts[len(parts)-1] }
    if len(ts) < 8 { return "", "", "", false }
    return kind, group, ts, true
}

func gcOldSecrets(ctx context.Context, cli *docker.Client, svc swm.Service, retain int) error {
    // collect current referenced secret names to protect
    cur := map[string]struct{}{}
    for _, r := range svc.Spec.TaskTemplate.ContainerSpec.Secrets {
        cur[r.SecretName] = struct{}{}
    }
    lst, err := cli.SecretList(ctx, types.SecretListOptions{})
    if err != nil { return err }
    groups := map[string][]secRef{}
    for _, s := range lst {
        if !(strings.HasPrefix(s.Spec.Name, "edge_tls_crt_") || strings.HasPrefix(s.Spec.Name, "edge_tls_key_")) { continue }
        if _, protected := cur[s.Spec.Name]; protected { continue }
        kind, group, ts, ok := splitSecretName(s.Spec.Name)
        if !ok { continue }
        key := kind+"/"+group
        groups[key] = append(groups[key], secRef{ID: s.ID, Name: s.Spec.Name, TS: ts, Kind: kind, Group: group})
    }
    for _, arr := range groups {
        sort.Slice(arr, func(i, j int) bool { return arr[i].TS < arr[j].TS })
        for i := 0; i < len(arr)-retain; i++ {
            _ = cli.SecretRemove(ctx, arr[i].ID)
        }
    }
    return nil
}

func gcOldConfigs(ctx context.Context, cli *docker.Client, svc swm.Service, prefix string, retain int) error {
    // current attached configs to protect
    cur := map[string]struct{}{}
    for _, c := range svc.Spec.TaskTemplate.ContainerSpec.Configs { cur[c.ConfigName] = struct{}{} }
    lst, err := cli.ConfigList(ctx, types.ConfigListOptions{})
    if err != nil { return err }
    type cfgRef struct{ ID, Name, TS string }
    arr := []cfgRef{}
    for _, c := range lst {
        if !strings.HasPrefix(c.Spec.Name, prefix+"_") { continue }
        if _, ok := cur[c.Spec.Name]; ok { continue }
        ts := c.Spec.Name[len(prefix)+1:]
        arr = append(arr, cfgRef{ID: c.ID, Name: c.Spec.Name, TS: ts})
    }
    sort.Slice(arr, func(i, j int) bool { return arr[i].TS < arr[j].TS })
    for i := 0; i < len(arr)-retain; i++ { _ = cli.ConfigRemove(ctx, arr[i].ID) }
    return nil
}

func loadEnvFiles(csv string) {
    for _, kv := range strings.Split(csv, ",") {
        kv = strings.TrimSpace(kv)
        if kv == "" { continue }
        parts := strings.SplitN(kv, "=", 2)
        if len(parts) != 2 { continue }
        path := parts[1]
        b, err := os.ReadFile(path)
        if err != nil { log.Println("env file read:", path, err); continue }
        os.Setenv(parts[0], strings.TrimSpace(string(b)))
    }
}


