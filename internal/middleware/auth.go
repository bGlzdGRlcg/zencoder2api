package middleware

import (
	"crypto/subtle"
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
		// 如果没有配置全局 Token，则跳过鉴权
		if token == "" {
			c.Next()
			return
		}

		// 1. 检查 OpenAI 格式: Authorization: Bearer <token>
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && parts[0] == "Bearer" && constantTimeEqual(parts[1], token) {
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
		if constantTimeEqual(c.Query("key"), token) {
			c.Next()
			return
		}

		// 鉴权失败
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
		// 如果没有配置管理密码，则跳过鉴权
		if adminPassword == "" {
			c.Next()
			return
		}

		// 检查请求头中的管理密码
		// 支持多种格式：
		// 1. Authorization: Bearer <password>
		// 2. X-Admin-Password: <password>
		// 3. Admin-Password: <password>

		var providedPassword string

		// 检查 Authorization: Bearer <password>
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && parts[0] == "Bearer" {
				providedPassword = parts[1]
			}
		}

		// 检查 X-Admin-Password
		if providedPassword == "" {
			providedPassword = c.GetHeader("X-Admin-Password")
		}

		// 检查 Admin-Password
		if providedPassword == "" {
			providedPassword = c.GetHeader("Admin-Password")
		}

		// 验证密码
		if constantTimeEqual(providedPassword, adminPassword) {
			c.Next()
			return
		}

		// 鉴权失败
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Invalid admin password",
				"type":    "authentication_error",
			},
		})
	}
}
