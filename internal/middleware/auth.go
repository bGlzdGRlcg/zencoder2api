package middleware

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func AuthMiddleware() gin.HandlerFunc {
	// 从环境变量获取全局 Token
	token := os.Getenv("AUTH_TOKEN")

	return func(c *gin.Context) {
		// Empty credentials are only allowed for an explicitly enabled loopback
		// development server. Production misconfiguration fails closed.
		if token == "" {
			if allowInsecureLocalRequest(c) {
				c.Next()
				return
			}
			unauthenticatedConfiguration(c)
			return
		}

		// 1. 检查 OpenAI 格式: Authorization: Bearer <token>
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && constantTimeEqual(parts[1], token) {
				c.Next()
				return
			}
		}

		// 2. 检查 Anthropic 格式: x-api-key: <token>
		if constantTimeEqual(c.GetHeader("x-api-key"), token) {
			c.Next()
			return
		}

		// 3. 检查 Gemini 格式: x-goog-api-key: <token> 或 query param key=<token>
		if constantTimeEqual(c.GetHeader("x-goog-api-key"), token) {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/models") && constantTimeEqual(c.Request.URL.Query().Get("key"), token) {
			// Gemini SDKs commonly put their key in the query. Remove it immediately
			// after authentication so downstream logs and handlers never retain it.
			query := c.Request.URL.Query()
			query.Del("key")
			c.Request.URL.RawQuery = query.Encode()
			c.Request.RequestURI = c.Request.URL.RequestURI()
			c.Next()
			return
		}

		// 鉴权失败
		c.Header("WWW-Authenticate", `Bearer realm="zencoder2api"`)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Invalid authentication token",
				"type":    "authentication_error",
			},
		})
	}
}

// AdminAuthMiddleware 后台管理密码验证中间件
func AdminAuthMiddleware() gin.HandlerFunc {
	// 从环境变量获取后台管理密码
	adminPassword := os.Getenv("ADMIN_PASSWORD")

	return func(c *gin.Context) {
		// Empty credentials are only allowed for an explicitly enabled loopback
		// development server. Production misconfiguration fails closed.
		if adminPassword == "" {
			if allowInsecureLocalRequest(c) {
				c.Next()
				return
			}
			unauthenticatedConfiguration(c)
			return
		}

		// Bearer remains available for non-browser automation. Browser clients
		// exchange it once for a short-lived HttpOnly session.
		if providedPassword, ok := bearerCredential(c.GetHeader("Authorization")); ok && constantTimeEqual(providedPassword, adminPassword) {
			c.Next()
			return
		}
		if err := authenticateAdminSession(c, adminPassword); err == nil {
			c.Next()
			return
		} else if errors.Is(err, errInvalidCSRFToken) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{
				"message": "Invalid CSRF token",
				"type":    "permission_error",
			}})
			return
		}

		writeAdminUnauthorized(c)
	}
}

func unauthenticatedConfiguration(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"message": "server authentication is not configured",
			"type":    "configuration_error",
		},
	})
}

func allowInsecureLocalRequest(c *gin.Context) bool {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_INSECURE_LOCALHOST")), "true") {
		return false
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		host = c.Request.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
