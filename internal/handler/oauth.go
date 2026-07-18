package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/service"
)

type OAuthHandler struct {
	svc *service.OAuthService
}

func NewOAuthHandler() *OAuthHandler {
	return &OAuthHandler{svc: service.NewOAuthService()}
}

func (h *OAuthHandler) StartZencoder(c *gin.Context) {
	origin, err := publicOrigin(c.Request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := h.svc.StartZencoderLogin(origin)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法启动 Zencoder 登录"})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *OAuthHandler) ZencoderCallback(c *gin.Context) {
	state := c.Param("state")
	code := c.Query("code")
	provider := c.Query("provider")
	result, err := h.svc.CompleteZencoderLogin(c.Request.Context(), state, code, provider)
	if err != nil {
		logging.Warnf("Zencoder OAuth callback failed: %v", err)
		h.renderCallback(c, result.Origin, false, "Zencoder 登录失败，请关闭窗口后重试")
		return
	}
	h.renderCallback(c, result.Origin, true, "Zencoder 账号已自动添加")
}

func (h *OAuthHandler) renderCallback(c *gin.Context, origin string, success bool, message string) {
	if origin == "" {
		origin, _ = publicOrigin(c.Request)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "zencoder-oauth",
		"success": success,
		"message": message,
	})
	target := origin
	if parsed, err := url.Parse(origin); err == nil {
		target = parsed.Scheme + "://" + parsed.Host
	}
	targetOrigin, _ := json.Marshal(target)
	status := "登录成功"
	if !success {
		status = "登录失败"
	}
	fallback := strings.TrimRight(origin, "/") + "/?oauth="
	if success {
		fallback += "success"
	} else {
		fallback += "error"
	}
	fallbackJSON, _ := json.Marshal(fallback)
	page := fmt.Sprintf(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title><style>body{margin:0;font-family:system-ui,sans-serif;background:#f8fafc;color:#0f172a;display:grid;place-items:center;min-height:100vh}.card{max-width:28rem;margin:2rem;padding:2rem;border:1px solid #e2e8f0;border-radius:1rem;background:white;box-shadow:0 12px 32px rgba(15,23,42,.08);text-align:center}h1{font-size:1.25rem;margin:0 0 .75rem}p{color:#64748b;margin:0}</style></head>
<body><main class="card"><h1>%s</h1><p>%s</p></main><script>
(function(){const payload=%s;const origin=%s;const fallback=%s;if(window.opener&&!window.opener.closed){window.opener.postMessage(payload,origin);setTimeout(()=>window.close(),300);}else{window.location.replace(fallback);}})();
</script></body></html>`, status, status, message, payload, targetOrigin, fallbackJSON)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
}

func publicOrigin(req *http.Request) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); configured != "" {
		return validatePublicOrigin(configured)
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	if forwarded := firstForwardedValue(req.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := req.Host
	if forwarded := firstForwardedValue(req.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	return validatePublicOrigin(scheme + "://" + host)
}

func validatePublicOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("PUBLIC_BASE_URL 必须是有效的 HTTP(S) 地址")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("PUBLIC_BASE_URL 不能包含凭据、查询参数或片段")
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

func firstForwardedValue(value string) string {
	value = strings.SplitN(value, ",", 2)[0]
	return strings.TrimSpace(value)
}
