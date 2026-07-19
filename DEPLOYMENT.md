# 部署与供应链基线

本服务是本地优先的 HTTP 应用。生产环境只把它暴露在反向代理之后，代理
负责 TLS、HSTS 和访问控制；不要把 Go 进程直接发布到公网。

## 生产必需项

1. 使用外部 secret manager/KMS 注入 `AUTH_TOKEN`、`ADMIN_PASSWORD` 和
   `TOKEN_ENCRYPTION_KEY`。不要把它们写入镜像、Compose 文件、Git 或日志。
   当前应用用该密钥加密 OAuth token 和 Zencoder API key；KMS envelope encryption、版本轮换和
   自动重加密仍属于部署系统责任。
2. 设置 `PUBLIC_BASE_URL=https://console.example.com` 和
   `ADMIN_COOKIE_SECURE=true`。管理会话是 15 分钟的 HttpOnly、Secure、
   SameSite=Strict cookie；浏览器只在内存中保留 CSRF token。
3. 设置积分重置时区和时间，例如 `CREDIT_RESET_TIMEZONE=Asia/Shanghai`、
   `CREDIT_RESET_AT=09:09`。时间使用 IANA 时区和 24 小时 `HH:MM`；多实例通过
   SQLite lease 只执行一次，但 SQLite 备份/恢复必须纳入运维流程。
4. 让代理只转发 `GET /livez`、`GET /readyz`、`GET /`、`/static/*`、`/api/*` 以及需要的
   OAuth 回调；对外部请求移除客户端传入的 `Forwarded`、`X-Forwarded-For`、
   `X-Forwarded-Proto` 和 `X-Request-ID`，由代理重新生成。当前应用不信任
   任意 forwarded header；`TRUSTED_PROXIES` 必须只列出实际代理的 IP/CIDR，
   代理必须是唯一入口并绑定内网地址。留空表示完全不信任 forwarded header。
5. TLS 最低使用 TLS 1.2，优先 TLS 1.3；启用
   `Strict-Transport-Security: max-age=31536000; includeSubDomains`
   （确认所有子域都支持 HTTPS 后再加 `preload`）。证书私钥只放在代理的
   secret store，配置自动续期和失效告警。

默认推理请求体上限为 4 MiB、并发上限为 32；在 512 MiB 容器内，这把仅原始 body 的
理论并发占用限制在约 128 MiB。提高 `MAX_REQUEST_BODY_BYTES` 或
`MAX_CONCURRENT_REQUESTS` 前必须同步提高内存限制并完成并发压测。`DB_PATH` 只接受普通
文件路径或 `:memory:`，不接受 `file:` SQLite URI；数据卷目录应仅对服务用户可访问。

## Compose

`.env` 只用于本机注入变量。Compose 强制要求不可变镜像引用：

```text
ZENCODER2API_IMAGE_REPOSITORY=ghcr.io/bglzdgrlcg/zencoder2api
ZENCODER2API_IMAGE_DIGEST=sha256:<release-digest>
PUBLIC_BASE_URL=https://console.example.com
ADMIN_COOKIE_SECURE=true
```

然后执行 `docker compose config --quiet`，再由部署系统执行
`docker compose pull` 和 `docker compose up -d`。服务只绑定宿主机 loopback，
容器启用只读根文件系统、非 root、丢弃 Linux capabilities、禁止提权、PID/
CPU/内存/日志上限以及 `/tmp` 的 `noexec,nosuid,nodev`。SQLite 数据只在
`/data` named volume 中持久化，必须纳入加密备份和恢复演练。

## 发布验证

每个 PR/push 必须通过 `.github/workflows/ci.yml` 的 fmt、test、race、vet、
staticcheck、govulncheck、secret scan 和容器构建。release workflow 会重新
验证 tag、生成校验和及 SPDX SBOM，并为归档和镜像生成 provenance/attestation。
若仓库属于 GitHub Organization，需在 Actions secret 中配置 Gitleaks license；
个人仓库不需要该 license。
镜像基础层和 Dockerfile frontend 均使用 digest pin；更新时必须在审查中记录
新 digest 和对应版本。

## 运行检查

```text
curl --fail https://console.example.com/livez
curl --fail https://console.example.com/readyz
docker inspect --format '{{.Config.User}}' <container>
docker image inspect <image@sha256:digest>
```

`/livez` 只检查进程，`/readyz` 检查 SQLite 以及是否存在至少一个
不在冷却期且不需重新授权的凭据；两者都不代表上游
Zencoder 可用。真实上游
smoke test 应使用隔离账号，并确保请求、响应、日志和抓包已脱敏。
