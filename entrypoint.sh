set -euo pipefail

: ${DOCKER_HOST:?need DOCKER_HOST}
: ${LEGO_EMAIL:?}
: ${DNS_PROVIDER:?}
: ${DOMAINS:?}
: ${EDGE_SERVICE_LABEL:=edge.traefik.service=true}
: ${EDGE_SERVICE_LABELS:=${EDGE_SERVICE_LABEL}}
: ${RENEW_DAYS:=30}
: ${KEY_SET:=ec256}
: ${CHALLENGE:=dns-01}
: ${ACME_SERVER:=}
: ${TLS_CONFIG_ENABLE:=false}
: ${TLS_CONFIG_NAME_PREFIX:=prod-edge-traefik-certs}
: ${TLS_CONFIG_TARGET:=/etc/traefik/dynamic/certs.yml}

discover_service() {
  docker service ls --format '{{.Name}}' | while read -r n; do
    labels_json=$(docker service inspect "$n" --format '{{json .Spec.Labels}}')
    ok=1
    IFS=, read -ra filters <<<"$EDGE_SERVICE_LABELS"
    for f in "${filters[@]}"; do
      k="${f%%=*}"; v="${f#*=}"
      echo "$labels_json" | grep -q '"'"$k"'":"'"$v"'"' || { ok=0; break; }
    done
    [ $ok -eq 1 ] && { echo "$n"; break; }
  done
}

svc=$(discover_service || true)
[ -n "${svc:-}" ] || { echo "No traefik service found matching labels: $EDGE_SERVICE_LABELS"; exit 0; }

labels=$(docker service inspect "$svc" --format '{{json .Spec.Labels}}')
DOMAINS_FROM_LABEL=$(echo "$labels" | jq -r '."edge.traefik.domains" // empty')
[ -n "$DOMAINS_FROM_LABEL" ] && DOMAINS="$DOMAINS_FROM_LABEL"
KEY_SET_FROM_LABEL=$(echo "$labels" | jq -r '."edge.traefik.key_set" // empty')
[ -n "$KEY_SET_FROM_LABEL" ] && KEY_SET="$KEY_SET_FROM_LABEL"
RENEW_DAYS_FROM_LABEL=$(echo "$labels" | jq -r '."edge.traefik.renew_days" // empty')
[ -n "$RENEW_DAYS_FROM_LABEL" ] && RENEW_DAYS="$RENEW_DAYS_FROM_LABEL"
ACME_SERVER_FROM_LABEL=$(echo "$labels" | jq -r '."edge.traefik.acme_server" // empty')
[ -n "$ACME_SERVER_FROM_LABEL" ] && ACME_SERVER="$ACME_SERVER_FROM_LABEL"
CHALLENGE_FROM_LABEL=$(echo "$labels" | jq -r '."edge.traefik.challenge" // empty')
[ -n "$CHALLENGE_FROM_LABEL" ] && CHALLENGE="$CHALLENGE_FROM_LABEL"

export LEGO_EMAIL DNS_PROVIDER

args=()
[ -n "$ACME_SERVER" ] && args+=(--server "$ACME_SERVER")
IFS=, read -ra arr <<<"$DOMAINS"
for d in "${arr[@]}"; do args+=(-d "$d"); done
case "$KEY_SET" in
  ec256) args+=(--key-type ec256);;
  ec384) args+=(--key-type ec384);;
  rsa2048) args+=(--key-type rsa2048);;
  rsa4096) args+=(--key-type rsa4096);;
  both)   : ;; # 先用 ec256，再补 rsa2048
  *) echo "Unknown KEY_SET: $KEY_SET"; exit 1;;
esac
[ "$CHALLENGE" = "dns-01" ] && args+=(--dns "$DNS_PROVIDER")

main="${arr[0]}"
crt="/root/.lego/certificates/${main}.crt"
key="/root/.lego/certificates/${main}.key"
# 续签策略：若存在则 renew --days；否则首次 run
pre_hash=""
[ -f "$crt" ] && pre_hash=$(sha256sum "$crt" | awk '{print $1}')

if [ -f "$crt" ] && [ -f "$key" ]; then
  lego --accept-tos renew --days "$RENEW_DAYS" --reuse-key "${args[@]}" || true
else
  lego --accept-tos run "${args[@]}" || true
fi

# 若需要同时维护 RSA 证书，可在 KEY_SET=both 时追加一次 renew/run（不影响当前部署的 EC Secrets）
if [ "$KEY_SET" = "both" ]; then
  args_no_key=("${args[@]}")
  args_rsa=()
  for a in "${args_no_key[@]}"; do [ "$a" = "--key-type" ] && skip=1 && continue; [ "${skip:-}" = 1 ] && { unset skip; continue; }; args_rsa+=("$a"); done
  args_rsa+=(--key-type rsa2048)
  # 仅尝试续签，不影响下方部署的 Secrets（仍使用主算法证书）
  lego --accept-tos renew --days "$RENEW_DAYS" --reuse-key "${args_rsa[@]}" || lego --accept-tos run "${args_rsa[@]}" || true
fi

post_hash=""
[ -f "$crt" ] && post_hash=$(sha256sum "$crt" | awk '{print $1}')

if [ -n "$pre_hash" ] && [ "$pre_hash" = "$post_hash" ]; then
  echo "certificate unchanged; skip secrets update"
  exit 0
fi

now=$(date +%Y%m%d%H%M)
crt_name="edge_tls_crt_${now}"
key_name="edge_tls_key_${now}"

docker secret rm "$crt_name" >/dev/null 2>&1 || true
docker secret rm "$key_name" >/dev/null 2>&1 || true
docker secret create "$crt_name" "$crt" >/dev/null
docker secret create "$key_name" "$key" >/dev/null

# 可选：生成/更新 tls.certificates 动态配置（单证书条目；多证书时可扩展此列表）
CONFIG_ARGS=()
if [ "$TLS_CONFIG_ENABLE" = "true" ]; then
  cfg_file=$(mktemp)
  cat >"$cfg_file" <<'YAML'
tls:
  certificates:
    - certFile: /run/secrets/edge_tls_crt
      keyFile: /run/secrets/edge_tls_key
YAML
  cfg_name="${TLS_CONFIG_NAME_PREFIX}_$now.yml"
  # 删除同前缀的旧 config（仅从该服务解绑）
  old_cfgs=$(docker service inspect "$svc" --format '{{json .Spec.TaskTemplate.ContainerSpec.Configs}}' | jq -r '.[]?.ConfigName' | grep "^${TLS_CONFIG_NAME_PREFIX}" || true)
  for n in $old_cfgs; do CONFIG_ARGS+=(--config-rm "$n"); done
  # 创建新 config
  docker config rm "$cfg_name" >/dev/null 2>&1 || true
  docker config create "$cfg_name" "$cfg_file" >/dev/null
  CONFIG_ARGS+=(--config-add "source=$cfg_name,target=$TLS_CONFIG_TARGET,mode=0444")
  rm -f "$cfg_file"
fi

# 组合更新参数并滚动 Traefik
docker service update \
  --secret-rm edge_tls_crt_00000000 \
  --secret-rm edge_tls_key_00000000 \
  --secret-add source="$crt_name",target=/run/secrets/edge_tls_crt,mode=0400 \
  --secret-add source="$key_name",target=/run/secrets/edge_tls_key,mode=0400 \
  "${CONFIG_ARGS[@]}" \
  --update-order start-first \
  "$svc"

echo "rolled $svc to $crt_name/$key_name"

