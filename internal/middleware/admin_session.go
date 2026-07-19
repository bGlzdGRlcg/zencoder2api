package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

const (
	adminSessionCookieName = "zencoder_admin_session"
	adminSessionLifetime   = 15 * time.Minute
)

var (
	errInvalidAdminSession = errors.New("invalid admin session")
	errInvalidCSRFToken    = errors.New("invalid CSRF token")
)

type adminSessionPayload struct {
	IssuedAt            int64  `json:"iat"`
	ExpiresAt           int64  `json:"exp"`
	CSRFHash            string `json:"csrf"`
	Nonce               string `json:"nonce"`
	PasswordFingerprint string `json:"password_fingerprint"`
}

// CreateAdminSession exchanges the administrator password for a short-lived,
// HttpOnly session. The CSRF token is returned once and must remain in memory.
func CreateAdminSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		password := os.Getenv("ADMIN_PASSWORD")
		if password == "" {
			if allowInsecureLocalRequest(c) {
				c.JSON(http.StatusOK, gin.H{"csrfToken": "", "expiresAt": time.Now().Add(adminSessionLifetime).Unix()})
				return
			}
			unauthenticatedConfiguration(c)
			return
		}

		provided, ok := bearerCredential(c.GetHeader("Authorization"))
		if !ok || !constantTimeEqual(provided, password) {
			writeAdminUnauthorized(c)
			return
		}

		csrfToken, err := randomAdminToken(32)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": "unable to create admin session",
				"type":    "internal_error",
			}})
			return
		}
		nonce, err := randomAdminToken(16)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": "unable to create admin session",
				"type":    "internal_error",
			}})
			return
		}

		now := time.Now()
		payload := adminSessionPayload{
			IssuedAt:            now.Unix(),
			ExpiresAt:           now.Add(adminSessionLifetime).Unix(),
			CSRFHash:            hashCSRFToken(csrfToken),
			Nonce:               nonce,
			PasswordFingerprint: adminPasswordFingerprint(password),
		}
		sessionToken, err := encryptAdminSession(payload)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": "unable to create admin session",
				"type":    "internal_error",
			}})
			return
		}
		if err := persistAdminSession(c.Request.Context(), payload.Nonce, now, now.Add(adminSessionLifetime)); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": "unable to persist admin session",
				"type":    "internal_error",
			}})
			return
		}

		http.SetCookie(c.Writer, &http.Cookie{
			Name:     adminSessionCookieName,
			Value:    sessionToken,
			Path:     "/api",
			MaxAge:   int(adminSessionLifetime.Seconds()),
			Expires:  now.Add(adminSessionLifetime),
			HttpOnly: true,
			Secure:   adminCookieSecure(),
			SameSite: http.SameSiteStrictMode,
		})
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"csrfToken": csrfToken,
			"expiresAt": payload.ExpiresAt,
		})
	}
}

// DestroyAdminSession always clears the browser cookie. Session tokens are
// short-lived and password rotation invalidates every outstanding token.
func DestroyAdminSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if password := os.Getenv("ADMIN_PASSWORD"); password != "" {
			if cookie, err := c.Request.Cookie(adminSessionCookieName); err == nil {
				if payload, err := verifyAdminSession(cookie.Value, password, time.Now()); err == nil {
					if err := revokeAdminSession(c.Request.Context(), payload.Nonce); err != nil {
						logging.Warnf("Revoke admin session: %v", err)
					}
				}
			}
		}
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     adminSessionCookieName,
			Value:    "",
			Path:     "/api",
			MaxAge:   -1,
			Expires:  time.Unix(1, 0),
			HttpOnly: true,
			Secure:   adminCookieSecure(),
			SameSite: http.SameSiteStrictMode,
		})
		c.Status(http.StatusNoContent)
	}
}

func authenticateAdminSession(c *gin.Context, password string) error {
	cookie, err := c.Request.Cookie(adminSessionCookieName)
	if err != nil {
		return errInvalidAdminSession
	}
	payload, err := verifyAdminSession(cookie.Value, password, time.Now())
	if err != nil {
		return err
	}
	if err := requireActiveAdminSession(c.Request.Context(), payload.Nonce, time.Now()); err != nil {
		return errInvalidAdminSession
	}
	if isSafeMethod(c.Request.Method) {
		return nil
	}
	provided := strings.TrimSpace(c.GetHeader("X-CSRF-Token"))
	if provided == "" {
		return errInvalidCSRFToken
	}
	expected, err := hex.DecodeString(payload.CSRFHash)
	if err != nil {
		return errInvalidAdminSession
	}
	actualSum := sha256.Sum256([]byte(provided))
	if len(expected) != len(actualSum) || subtle.ConstantTimeCompare(expected, actualSum[:]) != 1 {
		return errInvalidCSRFToken
	}
	return nil
}

func encryptAdminSession(payload adminSessionPayload) (string, error) {
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return secret.Encrypt(string(encodedPayload))
}

func verifyAdminSession(token, password string, now time.Time) (adminSessionPayload, error) {
	if !secret.IsEncrypted(token) {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	encodedPayload, err := secret.Decrypt(token)
	if err != nil {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	var payload adminSessionPayload
	if err := json.Unmarshal([]byte(encodedPayload), &payload); err != nil {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	if payload.Nonce == "" || payload.CSRFHash == "" || payload.PasswordFingerprint == "" || payload.IssuedAt <= 0 || payload.ExpiresAt <= payload.IssuedAt {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	if !constantTimeEqual(payload.PasswordFingerprint, adminPasswordFingerprint(password)) {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	if payload.ExpiresAt-payload.IssuedAt > int64(adminSessionLifetime.Seconds()) || now.Unix() >= payload.ExpiresAt || payload.IssuedAt > now.Add(time.Minute).Unix() {
		return adminSessionPayload{}, errInvalidAdminSession
	}
	return payload, nil
}

func adminPasswordFingerprint(password string) string {
	sum := sha256.Sum256([]byte("zencoder2api/admin-session/password/v1\x00" + password))
	return hex.EncodeToString(sum[:])
}

func hashCSRFToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashAdminSessionNonce(nonce string) string {
	sum := sha256.Sum256([]byte("zencoder2api/admin-session/nonce/v1\x00" + nonce))
	return hex.EncodeToString(sum[:])
}

func persistAdminSession(ctx context.Context, nonce string, now, expiresAt time.Time) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&model.AdminSession{}).Error; err != nil {
			return err
		}
		return tx.Create(&model.AdminSession{NonceHash: hashAdminSessionNonce(nonce), ExpiresAt: expiresAt}).Error
	})
}

func requireActiveAdminSession(ctx context.Context, nonce string, now time.Time) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	var session model.AdminSession
	if err := db.WithContext(ctx).Where("nonce_hash = ? AND expires_at > ?", hashAdminSessionNonce(nonce), now).First(&session).Error; err != nil {
		return err
	}
	return nil
}

func revokeAdminSession(ctx context.Context, nonce string) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	return db.WithContext(ctx).Where("nonce_hash = ?", hashAdminSessionNonce(nonce)).Delete(&model.AdminSession{}).Error
}

func randomAdminToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func bearerCredential(header string) (string, bool) {
	parts := strings.Fields(header)
	returnValue := ""
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		returnValue = parts[1]
	}
	return returnValue, returnValue != ""
}

func adminCookieSecure() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ADMIN_COOKIE_SECURE")), "true") {
		return true
	}
	publicBaseURL, err := url.Parse(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")))
	return err == nil && strings.EqualFold(publicBaseURL.Scheme, "https")
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func writeAdminUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="zencoder2api-admin"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"message": "Invalid admin credentials",
		"type":    "authentication_error",
	}})
}
