# 部署与供应链基线

本服务是本地优先的 HTTP 应用。生产环境只把它暴露在反向代理之后，代理
负责 TLS、HSTS 和访问控制；不要把 Go 进程直接发布到公网。

## 生产必需项

1. 使用外部 secret manager 注入 `AUTH_TOKEN` 和 `ADMIN_PASSWORD`。不要把它们写入
   镜像、Compose 文件、Git 或日志。OAuth token 和 Zencoder API key 会以明文保存在
   SQLite 中，因此数据卷必须限制访问并纳入加密备份。
2. 管理会话是 15 分钟的 HttpOnly、SameSite=Strict cookie；浏览器只在内存中保留
   CSRF token。TLS 请求会自动设置 Secure 属性。
3. 积分在 UTC 09:09 重置；多实例通过 SQLite lease 只执行一次，但 SQLite
   备份/恢复必须纳入运维流程。
4. usage-based Credit 每 15 分钟自动刷新。每个账号和定时扫描都使用 SQLite lease，
   多实例不会并发写入同一快照；长批次扫描会续租。余额主查询是
   `GET /api/v1/quotas/me/tokens`，不需要 operation ID，因此新账号也能直接查询。
   旧的 operation credits 接口仅作为兼容回退；查询失败只让余额降级为未知或过期，
   不改变账号健康状态。管理页支持单账号和全部账号手动刷新。
5. 让代理只转发 `GET /`、`/static/*`、`/api/*` 以及需要的
   OAuth 回调；对外部请求移除客户端传入的 `Forwarded`、`X-Forwarded-For`、
   `X-Forwarded-Proto` 和 `X-Request-ID`，由代理重新生成。当前应用不信任
   forwarded header，代理必须是唯一入口并绑定内网地址。
6. TLS 最低使用 TLS 1.2，优先 TLS 1.3；启用
   `Strict-Transport-Security: max-age=31536000; includeSubDomains`
   （确认所有子域都支持 HTTPS 后再加 `preload`）。证书私钥只放在代理的
   secret store，配置自动续期和失效告警。

`DB_PATH` 只接受普通文件路径或 `:memory:`，不接受 `file:` SQLite URI；数据卷目录
应仅对服务用户可访问。

## Compose

Docker Compose 通过 `environment` 直接注入运行变量，不读取 `.env` 文件：

```text
AUTH_TOKEN=<token>
ADMIN_PASSWORD=<password>
```

然后执行 `docker compose config --quiet`，再由部署系统执行
`docker compose pull` 和 `docker compose up -d`。SQLite 数据只在 `/data`
named volume 中持久化，必须纳入加密备份和恢复演练。

## 发布验证

发布 GitHub Release 时，`.github/workflows/release.yml` 会构建并上传 Linux、
macOS 和 Windows 二进制文件，同时将 amd64/arm64 镜像推送到 GHCR。
镜像基础层和 Dockerfile frontend 均使用 digest pin；更新时必须在审查中记录
新 digest 和对应版本。

## 运行检查

```text
docker inspect --format '{{.Config.User}}' <container>
docker image inspect <image@sha256:digest>
```

真实上游 smoke test 应使用隔离账号，并确保请求、响应、日志和抓包已脱敏。
