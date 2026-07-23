package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
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

func (h *OAuthHandler) CompleteZencoder(c *gin.Context) {
	var request struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := decodeStrictJSON(c, &request, 64<<10); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效，请提交 callback_url"})
		return
	}

	state, code, provider, err := parseZencoderCallbackURL(request.CallbackURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, err = h.svc.CompleteZencoderLogin(c.Request.Context(), state, code, provider)
	if errors.Is(err, service.ErrOAuthSessionInvalidOrExpired) {
		c.JSON(http.StatusGone, gin.H{
			"error":      "该授权链接已过期或已使用，请重新复制授权链接",
			"reset_flow": true,
		})
		return
	}
	if err != nil {
		logging.Warnf("Manual Zencoder OAuth completion failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{
			"error":      "授权回调处理失败，请重新复制授权链接后重试",
			"reset_flow": true,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Zencoder 账号连接成功",
	})
}

func parseZencoderCallbackURL(raw string) (state string, code string, provider string, err error) {
	callbackURL, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || callbackURL.Scheme == "" || callbackURL.Host == "" || callbackURL.Opaque != "" {
		return "", "", "", errors.New("请输入浏览器地址栏中的完整 localhost 回调地址")
	}
	if callbackURL.Scheme != "http" && callbackURL.Scheme != "https" {
		return "", "", "", errors.New("回调地址必须以 http:// 或 https:// 开头")
	}
	if callbackURL.User != nil {
		return "", "", "", errors.New("回调地址不能包含用户名或密码")
	}
	if !strings.EqualFold(callbackURL.Hostname(), "localhost") {
		return "", "", "", errors.New("回调地址的主机名必须是 localhost")
	}
	if callbackURL.Fragment != "" {
		return "", "", "", errors.New("回调地址不能包含片段标识")
	}
	if callbackURL.RawPath != "" {
		return "", "", "", errors.New("回调地址路径格式无效")
	}

	const callbackPrefix = "/oauth/zencoder/callback/"
	if !strings.HasPrefix(callbackURL.Path, callbackPrefix) {
		return "", "", "", errors.New("这不是有效的 Zencoder 回调地址")
	}
	state = strings.TrimPrefix(callbackURL.Path, callbackPrefix)
	if state == "" || strings.Contains(state, "/") {
		return "", "", "", errors.New("回调地址缺少有效的授权状态")
	}

	query, queryErr := url.ParseQuery(callbackURL.RawQuery)
	if queryErr != nil {
		return "", "", "", errors.New("回调地址的查询参数格式无效")
	}
	codes := query["code"]
	if len(codes) != 1 || strings.TrimSpace(codes[0]) == "" {
		return "", "", "", errors.New("回调地址中缺少有效的授权码 code")
	}
	code = codes[0]
	provider = query.Get("provider")
	return state, code, provider, nil
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
	var nonceBytes [24]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to render OAuth callback"})
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes[:])
	c.Header("Cache-Control", "no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", fmt.Sprintf("default-src 'none'; style-src 'nonce-%s'; script-src 'nonce-%s'; base-uri 'none'; frame-ancestors 'none'", nonce, nonce))
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
<title>%s</title><style nonce="%s">body{margin:0;font-family:system-ui,sans-serif;background:#f8fafc;color:#0f172a;display:grid;place-items:center;min-height:100vh}.card{max-width:28rem;margin:2rem;padding:2rem;border:1px solid #e2e8f0;border-radius:1rem;background:white;box-shadow:0 12px 32px rgba(15,23,42,.08);text-align:center}h1{font-size:1.25rem;margin:0 0 .75rem}p{color:#64748b;margin:0}</style></head>
<body><main class="card"><h1>%s</h1><p>%s</p></main><script nonce="%s">
(function(){const payload=%s;const origin=%s;const fallback=%s;if(window.opener&&!window.opener.closed){window.opener.postMessage(payload,origin);setTimeout(()=>window.close(),300);}else{window.location.replace(fallback);}})();
</script></body></html>`, html.EscapeString(status), nonce, html.EscapeString(status), html.EscapeString(message), nonce, payload, targetOrigin, fallbackJSON)
	responseStatus := http.StatusOK
	if !success {
		responseStatus = http.StatusBadGateway
	}
	c.Data(responseStatus, "text/html; charset=utf-8", []byte(page))
}

func publicOrigin(req *http.Request) (string, error) {
	if requestOrigin := strings.TrimSpace(req.Header.Get("Origin")); requestOrigin != "" {
		parsed, err := url.Parse(requestOrigin)
		if err != nil || !strings.EqualFold(parsed.Host, req.Host) {
			return "", errors.New("request Origin does not match Host")
		}
		return validateRequestOrigin(requestOrigin)
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return validateRequestOrigin(scheme + "://" + req.Host)
}

func validateRequestOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("请求来源必须是有效的 HTTP(S) 地址")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("请求来源不能包含凭据、查询参数或片段")
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}
