cert-secrets-controller（Go 版）
================================

Languages: 中文 | English (see English Summary below)

简介
----
纯 Go、distroless 运行的 ACME 证书控制器：在 Swarm 中为带标签的 Traefik 服务自动签发/续签证书，创建版本化 Secrets/Configs，并以 start-first 滚动更新。

特性
----
- 发现带 `edge.traefik.service=true` 标签的 Traefik 服务
- 从标签读取域名与参数：
  - `edge.traefik.domains=example.com,*.example.com`
  - `edge.traefik.key_set=ec256|ec384|rsa2048|rsa4096|both`
  - `edge.traefik.renew_days=30`
  - `edge.traefik.acme_server=...`
  - `edge.traefik.challenge=dns-01|http-01`
- 生成版本化 Secrets：`edge_tls_crt_YYYYMMDD` / `edge_tls_key_YYYYMMDD`
- 以 start-first 滚动替换占位 Secret（要求 Traefik 服务预先引用占位名 `edge_tls_*_00000000`）
- 多架构镜像（amd64/arm64），自动构建（按 lego tag + CA 指纹判重）
- 证书模式：支持 SAN（单证书多域）与 split（每域一证）
- 支持 EAB（ZeroSSL 等 ACME 提供商）

快速开始（Swarm）
-----------------
1) 让 Traefik 服务挂占位 Secrets，并打上标签：
```
deploy.labels:
  - edge.traefik.service=true
  - edge.traefik.domains=example.com,*.example.com
```
2) 运行控制器（示例）：
```
services:
  cert-secrets-controller:
    image: ghcr.io/swarmnative/swarm-traefik-acme-controller:latest
    environment:
      - DOCKER_HOST=tcp://docker-socket-proxy:2375
      - LEGO_EMAIL=admin@example.com
      - DNS_PROVIDER=cloudflare
      - DOMAINS=example.com,*.example.com
      - LOOP_INTERVAL=12h
      - PERSIST_DIR=/data/.lego
      - PROVIDER_ENV_FILES=CF_API_TOKEN=/run/secrets/DNS_API_TOKEN
```

环境变量
--------
- `DOCKER_HOST`：指向 socket-proxy，如 `tcp://docker-socket-proxy:2375`
- `LEGO_EMAIL`：ACME 账户邮箱
- `DNS_PROVIDER`：DNS-01 提供商（如 cloudflare、alidns、route53）
- `DOMAINS`：逗号分隔域名列表；可被 Traefik 服务标签覆盖
- `EDGE_SERVICE_LABEL`：用于定位 Traefik 服务（默认 `edge.traefik.service=true`）
- `EDGE_SERVICE_LABELS`：逗号分隔的多个 key=value 过滤（覆盖 `EDGE_SERVICE_LABEL`），用于更精确地选择 Traefik 服务，例如：
  - `edge.traefik.service=true,traefik.pool=a`
- `KEY_SET`：证书类型（默认 ec256；可选 both 以同时签发 EC 与 RSA）
- `CHALLENGE`：`dns-01` 或 `http-01`（默认 dns-01）
- `ACME_SERVER`：可选，自定义 ACME 端点
- `RENEW_DAYS`：续签阈值（默认 30）
- `PERSIST_DIR`：本地持久化目录（默认 `/data/.lego`）
- `LOOP_INTERVAL`：巡检间隔（默认 `12h`）
- `PROVIDER_ENV_FILES`：从文件注入 Provider 变量映射，如 `CF_API_TOKEN=/run/secrets/DNS_API_TOKEN`
- `TLS_CONFIG_ENABLE`：是否由控制器生成/更新 Traefik 证书动态配置文件（Docker config）（默认 false）
- `TLS_CONFIG_NAME_PREFIX`：证书动态配置的 config 名称前缀（默认 `prod-edge-traefik-certs`）
- `TLS_CONFIG_TARGET`：在 Traefik 容器内挂载的目标路径（默认 `/etc/traefik/dynamic/certs.yml`）
- `CERT_MODE`：`san|split`（默认 `san`）。`san` 为单张证书覆盖全部 DOMAINS；`split` 为每个域名单独签发并写入多条 `tls.certificates`。
- `EAB_KID`：可选，ACME 外部账户绑定 KID（ZeroSSL 必需）
- `EAB_HMAC`：可选，ACME 外部账户绑定 HMAC（ZeroSSL 必需，Base64 编码）
- 提示：EAB_KID/EAB_HMAC 建议通过 `PROVIDER_ENV_FILES` 从 Secret 文件注入，例如 `EAB_KID=/run/secrets/zerossl_kid,EAB_HMAC=/run/secrets/zerossl_hmac`。
- `RETAIN_GENERATIONS`：保留多少代历史（默认 2，即“最新版+上一版”），老的 Secrets/Configs 将被清理。

构建与发布
----------
GitHub Actions 自动构建并推送到 GHCR：
- 推送到 main/master 分支，或每日定时触发
- 多架构：linux/amd64, linux/arm64

CA 证书与 Traefik 配置
----------------------
- 镜像在构建期内置系统 CA（/etc/ssl/certs/ca-certificates.crt），用于 HTTPS 与 DNS Provider API 的 TLS 校验，无需额外挂载。
- Traefik 最小配置：
  - 启用 file provider 并 watch：
    ```
    --providers.file.directory=/etc/traefik/dynamic
    --providers.file.watch=true
    ```
  - 证书 Secrets 占位（后续由控制器替换为版本化 Secret）：
    - edge_tls_crt_00000000 → /run/secrets/edge_tls_crt
    - edge_tls_key_00000000 → /run/secrets/edge_tls_key
  - 若启用 `TLS_CONFIG_ENABLE=true`：控制器会生成 certs.yml 并以 Docker config 注入到 `/etc/traefik/dynamic/certs.yml`，Traefik 自动生效。

socket-proxy 权限建议
--------------------
- 最小权限只需 Swarm 读取与 Service 更新：
  - 允许：`/services`（list/inspect/update）、`/secrets`（list/create/remove）、`/configs`（list/create/remove）
  - 可选：`/nodes`（只读）、`/events`（只读）
- 禁止：`/containers`、`/images` 等与本控制器无关的写操作。

示例 Swarm Stack（通用）
----------------------
```yaml
networks:
  app-net:
    external: true

secrets:
  dns_api_token:
    external: true

volumes:
  certlego: {}

services:
  cert-secrets-controller:
    image: ghcr.io/swarmnative/swarm-traefik-acme-controller:latest
    environment:
      - DOCKER_HOST=tcp://docker-socket-proxy:2375
      - LOOP_INTERVAL=12h
      - PERSIST_DIR=/data/.lego
      - PROVIDER_ENV_FILES=CF_API_TOKEN=/run/secrets/DNS_API_TOKEN
      - LEGO_EMAIL=admin@example.com
      - DNS_PROVIDER=cloudflare
      - DOMAINS=example.com,*.example.com
      - EDGE_SERVICE_LABELS=edge.traefik.service=true
      - RENEW_DAYS=30
      - TLS_CONFIG_ENABLE=false
    secrets:
      - source: dns_api_token
        target: /run/secrets/DNS_API_TOKEN
        mode: 0400
    networks:
      - app-net
    volumes:
      - certlego:/data/.lego
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.role == manager
```
  
注意：
- 将 `docker-socket-proxy` 改为你的 socket-proxy 服务名；或直接挂载 `/var/run/docker.sock`（不推荐）。
- `dns_api_token` 请在 Swarm 预先创建，并在容器内由 DNS Provider 官方变量读取（如 Cloudflare 为 `CF_API_TOKEN`，通过 `PROVIDER_ENV_FILES` 注入）。
- `OWNER` 替换为你的 GitHub 用户/组织名。
- 若无需 socket-proxy，可不设 `DOCKER_HOST`，并在服务中挂载本地 `/var/run/docker.sock:/var/run/docker.sock:ro`。

多域名证书与 Traefik 配置
------------------------
- 单证书-SAN（一个证书覆盖多个域名）
  - 不需要更改 Traefik 配置。保持 file provider 中 `tls.certificates` 指向固定路径（例如：
    `certFile: /run/secrets/edge_tls_crt`、`keyFile: /run/secrets/edge_tls_key`）。
  - 控制器只替换 Docker Secret 的来源，目标路径不变，Traefik 滚动后自动生效。
- 多证书（每域一证）或双算法（EC+RSA）
  - 需要在动态配置中声明多条 `tls.certificates`（每套证书一条）。
  - 若设置 `TLS_CONFIG_ENABLE=true`，控制器会自动生成/更新该动态配置（作为 Docker config 注入 Traefik），并随证书 Secrets 一起滚动更新。

socket-proxy 权限建议
--------------------
- 最小权限只需 Swarm 读取与 Service 更新：
  - 允许：`/services`（list/inspect/update）、`/secrets`（list/create/remove）、`/configs`（list/create/remove）
  - 可选：`/nodes`（只读）、`/events`（只读）
- 禁止：`/containers`、`/images` 等与本控制器无关的写操作。

附录：最小 Traefik Stack 片段（file provider）
-------------------------------------------
```yaml
networks:
  app-net:
    external: true

services:
  traefik:
    image: traefik:latest
    command:
      - --providers.docker=true
      - --providers.docker.swarmMode=true
      - --providers.docker.network=app-net
      - --providers.file.directory=/etc/traefik/dynamic
      - --providers.file.watch=true
      - --entrypoints.web.address=:80
      - --entrypoints.websecure.address=:443
      - --api.dashboard=false
    ports:
      - target: 80
        published: 80
        protocol: tcp
        mode: host
      - target: 443
        published: 443
        protocol: tcp
        mode: host
    networks:
      - app-net
    secrets:
      - source: edge_tls_crt_00000000
        target: /run/secrets/edge_tls_crt
        mode: 0400
      - source: edge_tls_key_00000000
        target: /run/secrets/edge_tls_key
        mode: 0400
    configs:
      - source: traefik_dynamic_base
        target: /etc/traefik/dynamic/base.yml
        mode: 0444
    deploy:
      replicas: 1
      labels:
        - edge.traefik.service=true
      update_config:
        order: start-first
        parallelism: 1
      placement:
        constraints:
          - node.labels.edge.traefik == true

secrets:
  edge_tls_crt_00000000:
    external: true
  edge_tls_key_00000000:
    external: true

configs:
  traefik_dynamic_base:
    external: true
```
说明：
- `edge_tls_*_00000000` 为占位 secret，控制器会替换为版本化证书 secrets。
- `traefik_dynamic_base` 可为空的基础动态配置（如中间件链），证书条目由控制器按需生成 `certs.yml` 注入。

证书模式说明
------------
- SAN（默认）：
  - 一张证书覆盖 DOMAINS 列表；配额少、管理简单；注意 SAN 会在证书中并列可见域名。
- split：
  - 每个域名单独证书，隔离更好；增加签发/续签次数，需关注 ACME 速率限制。

ZeroSSL（EAB）示例（split + 通配符，Secret 注入）
----------------------------------------------
```yaml
services:
  cert-secrets-controller:
    image: ghcr.io/swarmnative/swarm-traefik-acme-controller:latest
    environment:
      - DOCKER_HOST=tcp://docker-socket-proxy:2375
      - EDGE_SERVICE_LABELS=edge.traefik.service=true
      - DNS_PROVIDER=cloudflare
      - LEGO_EMAIL=admin@example.com
      - ACME_SERVER=https://acme.zerossl.com/v2/DV90
      - PROVIDER_ENV_FILES=EAB_KID=/run/secrets/zerossl_kid,EAB_HMAC=/run/secrets/zerossl_hmac,CF_API_TOKEN=/run/secrets/DNS_API_TOKEN
      - CERT_MODE=split
      - DOMAINS=*.a.example.com,*.b.example.com
      - TLS_CONFIG_ENABLE=true
    secrets:
      - source: zerossl_kid
        target: /run/secrets/zerossl_kid
        mode: 0400
      - source: zerossl_hmac
        target: /run/secrets/zerossl_hmac
        mode: 0400
      - source: dns_api_token
        target: /run/secrets/DNS_API_TOKEN
        mode: 0400
secrets:
  zerossl_kid:
    external: true
  zerossl_hmac:
    external: true
  dns_api_token:
    external: true
```

许可证
----
MIT

免责声明
------
本项目为社区维护，与 Docker, Inc.、Mirantis、Traefik 或任何相关公司无关、无隶属或背书关系。

English Summary
---------------
Go-based ACME controller for Docker Swarm and Traefik. It issues/renews certificates (Let’s Encrypt / ZeroSSL EAB), injects versioned Secrets/Configs, and rolls out with start-first updates.

- Modes: SAN (one cert for multiple domains) and split (one cert per domain)
- ACME: DNS-01; supports ZeroSSL EAB (EAB_KID/EAB_HMAC + ACME_SERVER)
- TLS config: optional certs.yml (enable via TLS_CONFIG_ENABLE=true)
- Retention: RETAIN_GENERATIONS for GC of old Secrets/Configs (default 2)
- Docker access: DOCKER_HOST (socket-proxy) or fallback to /var/run/docker.sock

Quick usage
```
services:
  cert-secrets-controller:
    image: ghcr.io/swarmnative/swarm-traefik-acme-controller:latest
    environment:
      - DOCKER_HOST=tcp://docker-socket-proxy:2375
      - EDGE_SERVICE_LABELS=edge.traefik.service=true
      - LEGO_EMAIL=admin@example.com
      - DNS_PROVIDER=cloudflare
      - DOMAINS=example.com,*.example.com
      - TLS_CONFIG_ENABLE=false
```
See the Chinese sections above for full environment variables, Traefik snippet, ZeroSSL secret injection, and socket-proxy permissions.

