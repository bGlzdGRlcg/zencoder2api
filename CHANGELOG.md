# Changelog

## Unreleased - 2026-07-19

- 对照 VSIX 3.4.1、JetBrains 3.31.0 和 Zenflow 2.3.5 完成 Provider Gateway
  端点、认证 header、OAuth 和协议转换审计。
- 修复 Zencoder OAuth：加密持久化 PKCE verifier，按 provider 分流 WorkOS/Frontegg
  exchange/refresh 端点，并增加不含凭据的诊断日志。
- 增加 AES-256-GCM OAuth/API Key 加密、启动密钥校验、旧明文迁移和 API Key
  创建/轮换接口；凭据 revision 防止跨实例旧缓存继续使用已撤销密钥。
- 增加账号健康状态、Retry-After/指数退避、401 单次强制刷新、refresh lease 和
  多账号去重重试；`/readyz` 同时检查数据库与可用账号池。
- 修正账号失败后动态缩短重试上限、跨实例凭据 revision 失效、OAuth 刷新并发回滚、
  API Key 轮换重复身份和 readiness context 未贯穿问题。
- 修正 GORM `OAuth*` 字段的实际 `o_auth_*` 列名，确保 OAuth/API Key 已有账号可更新；
  将 `golang.org/x/net` 升至 v0.53.0 以修复可达 HTTP/2 漏洞。
- 增加 fail-closed 认证、请求体上限、request ID、安全响应头、HTTP 超时和优雅关闭。
- 统一共享 HTTP Transport、代理/重定向策略、Gateway metadata、operation ID、错误体
  上限、响应头过滤和 SSE 边界。
- 修正 xAI、Gemini、Anthropic、OpenAI Responses 的 provider 路由和模型清单隐藏项。
- 限制管理 API 分页/批量删除、修正删除全部语义，避免内部数据库错误直接暴露。
- 管理页改用 HttpOnly 短期 session、内存 CSRF 和自托管静态资源，移除 CDN、inline
  handler 与浏览器凭据 storage；OAuth callback 使用 nonce CSP。
- SQLite 启用 WAL/busy timeout/foreign key；OAuth claim、refresh 与每日积分重置使用
  数据库 CAS/lease，账号池和 scheduler 支持优雅停止。
- 跨供应商转换对不可保真字段、远程图片和 JSON Schema 显式拒绝，保留多 candidate、
  thinking/signature 与稳定工具 ID，并将坏 SSE/failed/incomplete 转为协议错误事件。
- 修复 Chat→Responses/Anthropic 流式 refusal 丢失、累计工具参数重复、Gemini 无 index
  工具调用拆分，以及仅收到 `[DONE]`、缺少语义终止事件时被误报为成功的问题。
- 新增 OAuth、Gateway endpoint、加密、模型、流边界、认证和管理 API 回归测试。
- Docker 构建上下文改为源码 allowlist，Compose 默认 loopback、secret 必填并启用容器
  最小权限与健康检查。
- 追加 Provider Gateway 契约收敛：保护模型/操作 metadata、排除 409 重试、限制兼容转换
  响应体、拒绝全网可信代理和 `file:` SQLite DSN，并降低默认请求内存预算。
- 追加跨协议回归：Responses 工具 ID fallback、流式 usage、Gemini safety/refusal 与
  合法 finish reason；无法无损表示的 reasoning/signature 改为显式协议失败。
- OAuth callback claim 增加 CAS heartbeat 和取消清理；账号健康增加独立 revision，避免
  多实例旧成功、旧失败或 refresh 覆盖新 cooldown/reauth。
- 流式健康与 usage 延迟到标准终止事件；无终止 EOF 标记上游截断，客户端主动关闭不
  误伤账号；service debug 日志统一携带中间件生成的 request ID。
- 补齐 Gemini thought signature、cached/reasoning usage、named tool choice、Anthropic
  串行工具控制、strict/file_id/verbosity 拒绝和 Chat→Responses 混合 refusal part；
  Grok Code 参数约束收敛到共享 Gateway 请求准备函数。
- OAuth refresh lease 增加 heartbeat/失权取消/有界释放；API key 与 OAuth 完成流程
  使用事务/CAS；quota 绝对快照按 credential revision 和 pricing period 单调写回；
  SQLite 迁移增加 schema version 与跨进程写事务互斥。
- release 全链路固定已验证 commit SHA；health/ready 增加缓存和单探针 gate；SSE 增加
  idle write deadline；IPv6 限流按 `/64` 聚合；管理员 session 改为独立密钥加密；
  recovery、开发豁免、Dependabot 和 body×并发预算同步收紧。
- Windows 数据库文件使用受保护 NTFS DACL，Unix 保持 `0700/0600`；数据库初始化
  任一步骤失败都会关闭已打开的 SQLite 句柄。
