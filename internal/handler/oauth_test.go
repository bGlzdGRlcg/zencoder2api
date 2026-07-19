package handler

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOAuthCallbackUsesNonceCSP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/oauth/zencoder/callback/state", nil)

	NewOAuthHandler().renderCallback(context, "https://console.example.test", true, "ok")

	csp := response.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("callback CSP permits unsafe-inline: %s", csp)
	}
	match := regexp.MustCompile(`script-src 'nonce-([^']+)'`).FindStringSubmatch(csp)
	if len(match) != 2 {
		t.Fatalf("callback CSP has no script nonce: %s", csp)
	}
	for _, tag := range []string{`<script nonce="` + match[1] + `">`, `<style nonce="` + match[1] + `">`} {
		if !strings.Contains(response.Body.String(), tag) {
			t.Fatalf("callback body is missing %s", tag)
		}
	}
}

func TestPublicOriginIgnoresForwardedHeaders(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/oauth/zencoder/start", nil)
	request.Host = "localhost:8080"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "attacker.example")

	origin, err := publicOrigin(request)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "http://localhost:8080" {
		t.Fatalf("origin = %q, want loopback request origin", origin)
	}
}

func TestPublicOriginRequiresConfigurationForNonLoopbackHost(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "")
	request := httptest.NewRequest(http.MethodPost, "http://attacker.example/api/oauth/zencoder/start", nil)
	request.Host = "attacker.example"
	if _, err := publicOrigin(request); err == nil {
		t.Fatal("expected non-loopback Host to be rejected")
	}
}
