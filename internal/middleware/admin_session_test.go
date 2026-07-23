package middleware

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/database"
)

func TestAdminSessionCookieAndCSRF(t *testing.T) {
	const password = "correct-horse-battery-staple"
	t.Setenv("ADMIN_PASSWORD", password)
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "false")
	t.Setenv("PUBLIC_BASE_URL", "https://proxy.example.test")
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "admin-session.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/admin/session", CreateAdminSession())
	engine.GET("/api/accounts", AdminAuthMiddleware(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.POST("/api/accounts", AdminAuthMiddleware(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.DELETE("/api/admin/session", DestroyAdminSession())

	loginRequest := httptest.NewRequest(http.MethodPost, "/api/admin/session", nil)
	loginRequest.Header.Set("Authorization", "Bearer "+password)
	loginResponse := httptest.NewRecorder()
	engine.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d", loginResponse.Code, http.StatusOK)
	}
	var loginBody struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginBody); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginBody.CSRFToken == "" {
		t.Fatal("csrfToken is empty")
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/api" {
		t.Fatalf("unsafe cookie attributes: %#v", cookie)
	}
	if !strings.HasPrefix(cookie.Value, "enc:v1:") || strings.Contains(cookie.Value, loginBody.CSRFToken) {
		t.Fatal("admin session cookie is not opaque encrypted data")
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	getRequest.AddCookie(cookie)
	getResponse := httptest.NewRecorder()
	engine.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusNoContent {
		t.Fatalf("session GET status = %d, want %d", getResponse.Code, http.StatusNoContent)
	}

	missingCSRFRequest := httptest.NewRequest(http.MethodPost, "/api/accounts", nil)
	missingCSRFRequest.AddCookie(cookie)
	missingCSRFResponse := httptest.NewRecorder()
	engine.ServeHTTP(missingCSRFResponse, missingCSRFRequest)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", missingCSRFResponse.Code, http.StatusForbidden)
	}

	validRequest := httptest.NewRequest(http.MethodPost, "/api/accounts", nil)
	validRequest.AddCookie(cookie)
	validRequest.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	validResponse := httptest.NewRecorder()
	engine.ServeHTTP(validResponse, validRequest)
	if validResponse.Code != http.StatusNoContent {
		t.Fatalf("valid CSRF status = %d, want %d", validResponse.Code, http.StatusNoContent)
	}

	logoutRequest := httptest.NewRequest(http.MethodDelete, "/api/admin/session", nil)
	logoutRequest.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	engine.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResponse.Code, http.StatusNoContent)
	}
	revokedRequest := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	revokedRequest.AddCookie(cookie)
	revokedResponse := httptest.NewRecorder()
	engine.ServeHTTP(revokedResponse, revokedRequest)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d, want %d", revokedResponse.Code, http.StatusUnauthorized)
	}
}

func TestAdminSessionRejectsTampering(t *testing.T) {
	const password = "password-one"
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("e", 32))))
	payload := adminSessionPayload{
		IssuedAt:            1,
		ExpiresAt:           2,
		CSRFHash:            hashCSRFToken("csrf"),
		Nonce:               "nonce",
		PasswordFingerprint: adminPasswordFingerprint(password),
	}
	token, err := encryptAdminSession(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(token, payload.Nonce) || strings.Contains(token, payload.CSRFHash) {
		t.Fatal("encrypted token contains plaintext session fields")
	}
	if _, err := verifyAdminSession(token, password, time.Unix(1, 0)); err != nil {
		t.Fatalf("verify valid session: %v", err)
	}
	if _, err := verifyAdminSession(token, "password-two", time.Unix(1, 0)); !errors.Is(err, errInvalidAdminSession) {
		t.Fatalf("verify error = %v, want invalid session", err)
	}
	tamperAt := len("enc:v1:") + 4
	replacement := byte('A')
	if token[tamperAt] == replacement {
		replacement = 'B'
	}
	tampered := token[:tamperAt] + string(replacement) + token[tamperAt+1:]
	if _, err := verifyAdminSession(tampered, password, time.Unix(1, 0)); !errors.Is(err, errInvalidAdminSession) {
		t.Fatalf("tampered verify error = %v, want invalid session", err)
	}
	if _, err := verifyAdminSession("legacy-payload.legacy-signature", password, time.Unix(1, 0)); !errors.Is(err, errInvalidAdminSession) {
		t.Fatalf("legacy verify error = %v, want invalid session", err)
	}
}

func TestGeminiQueryAuthenticationIsScopedAndRedacted(t *testing.T) {
	const token = "0123456789abcdef"
	t.Setenv("AUTH_TOKEN", token)
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "false")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1beta/models/*path", AuthMiddleware(), func(c *gin.Context) {
		if c.Query("key") != "" || strings.Contains(c.Request.RequestURI, "key=") {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	engine.POST("/v1/chat/completions", AuthMiddleware(), func(c *gin.Context) { c.Status(http.StatusNoContent) })

	geminiRequest := httptest.NewRequest(http.MethodPost, "/v1beta/models/test:generateContent?key="+token, nil)
	geminiResponse := httptest.NewRecorder()
	engine.ServeHTTP(geminiResponse, geminiRequest)
	if geminiResponse.Code != http.StatusNoContent {
		t.Fatalf("Gemini query auth status = %d, want %d", geminiResponse.Code, http.StatusNoContent)
	}

	openAIRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key="+token, nil)
	openAIResponse := httptest.NewRecorder()
	engine.ServeHTTP(openAIResponse, openAIRequest)
	if openAIResponse.Code != http.StatusUnauthorized {
		t.Fatalf("non-Gemini query auth status = %d, want %d", openAIResponse.Code, http.StatusUnauthorized)
	}
}
