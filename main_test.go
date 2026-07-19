package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/service"
)

func setValidServerEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_TOKEN", "api-token-1234567890")
	t.Setenv("ADMIN_PASSWORD", "admin-password-1234567890")
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("BIND_ADDRESS", "127.0.0.1")
	t.Setenv("PORT", "8080")
	t.Setenv("PUBLIC_BASE_URL", "")
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "false")
	t.Setenv("MAX_REQUEST_BODY_BYTES", "4194304")
	t.Setenv("MAX_CONCURRENT_REQUESTS", "32")
	t.Setenv("TRUSTED_PROXIES", "")
}

func TestLoadServerConfigRequiresCredentials(t *testing.T) {
	setValidServerEnvironment(t)
	t.Setenv("AUTH_TOKEN", "")
	if _, err := loadServerConfig(); err == nil {
		t.Fatal("expected missing credential error")
	}
}

func TestLoadServerConfigInsecureModeRequiresLoopback(t *testing.T) {
	setValidServerEnvironment(t)
	t.Setenv("AUTH_TOKEN", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "true")
	t.Setenv("BIND_ADDRESS", "0.0.0.0")
	if _, err := loadServerConfig(); err == nil {
		t.Fatal("expected non-loopback insecure mode error")
	}
}

func TestLoadServerConfigRejectsInsecureProxyCombinations(t *testing.T) {
	for _, test := range []struct {
		name          string
		publicBaseURL string
		trustedProxy  string
	}{
		{name: "public base URL", publicBaseURL: "https://proxy.example.test"},
		{name: "trusted proxy", trustedProxy: "127.0.0.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setValidServerEnvironment(t)
			t.Setenv("AUTH_TOKEN", "")
			t.Setenv("ADMIN_PASSWORD", "")
			t.Setenv("ALLOW_INSECURE_LOCALHOST", "true")
			t.Setenv("PUBLIC_BASE_URL", test.publicBaseURL)
			t.Setenv("TRUSTED_PROXIES", test.trustedProxy)
			if _, err := loadServerConfig(); err == nil {
				t.Fatal("expected insecure proxy configuration error")
			}
		})
	}
}

func TestLoadServerConfigRequiresEncryptionKey(t *testing.T) {
	setValidServerEnvironment(t)
	t.Setenv("TOKEN_ENCRYPTION_KEY", "")
	if _, err := loadServerConfig(); err == nil {
		t.Fatal("expected encryption key error")
	}
}

func TestLoadServerConfigAcceptsSecureConfiguration(t *testing.T) {
	setValidServerEnvironment(t)
	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != "127.0.0.1:8080" || cfg.maxRequestBytes != 4<<20 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadServerConfigRejectsExcessiveRequestMemoryBudget(t *testing.T) {
	setValidServerEnvironment(t)
	t.Setenv("MAX_REQUEST_BODY_BYTES", "134217728")
	t.Setenv("MAX_CONCURRENT_REQUESTS", "100000")
	if _, err := loadServerConfig(); err == nil {
		t.Fatal("expected request memory budget error")
	}
}

func TestCreditResetConfigurationFailsClosed(t *testing.T) {
	t.Setenv("CREDIT_RESET_TIMEZONE", "not/a-timezone")
	if err := service.ValidateCreditResetConfig(); err == nil {
		t.Fatal("expected invalid credit reset timezone error")
	}
}

func TestLoadServerConfigRejectsCredentialWhitespace(t *testing.T) {
	setValidServerEnvironment(t)
	t.Setenv("AUTH_TOKEN", "token with spaces-123456")
	if _, err := loadServerConfig(); err == nil {
		t.Fatal("expected whitespace credential error")
	}
}

func TestParseTrustedProxies(t *testing.T) {
	proxies, err := parseTrustedProxies("127.0.0.1, 10.0.0.0/8")
	if err != nil || len(proxies) != 2 {
		t.Fatalf("proxies=%v err=%v", proxies, err)
	}
	if _, err := parseTrustedProxies("not-an-ip"); err == nil {
		t.Fatal("expected invalid trusted proxy error")
	}
	for _, value := range []string{"0.0.0.0/0", "::/0"} {
		if _, err := parseTrustedProxies(value); err == nil {
			t.Fatalf("expected unrestricted trusted proxy %q to be rejected", value)
		}
	}
}

func TestValidatePublicBaseURLRequiresTLSForPublicHosts(t *testing.T) {
	for _, valid := range []string{"https://proxy.example.test", "http://localhost:8080", "http://127.0.0.1:8080"} {
		if err := validatePublicBaseURL(valid); err != nil {
			t.Fatalf("validate %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"http://proxy.example.test", "https://proxy.example.test/base", "https://user:pass@proxy.example.test"} {
		if err := validatePublicBaseURL(invalid); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
}

func TestSetupRoutesUsesEmbeddedWebAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	cfg := serverConfig{maxConcurrent: 2, requestsPerMinute: 10, adminLoginPerMinute: 2}
	if err := setupRoutes(engine, cfg); err != nil {
		t.Fatal(err)
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	pageResponse := httptest.NewRecorder()
	engine.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || !strings.Contains(pageResponse.Body.String(), "Zencoder Console") {
		t.Fatalf("embedded page status=%d body=%q", pageResponse.Code, pageResponse.Body.String())
	}
	assetRequest := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	assetResponse := httptest.NewRecorder()
	engine.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK || !strings.Contains(assetResponse.Body.String(), "--color") {
		t.Fatalf("embedded asset status=%d", assetResponse.Code)
	}
}

func TestReadinessHandlerCachesProbeResult(t *testing.T) {
	var probes atomic.Int32
	engine := gin.New()
	engine.GET("/readyz", newReadinessHandler(func(context.Context) bool {
		probes.Add(1)
		return true
	}))

	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, response.Code, http.StatusOK)
		}
	}
	if probes.Load() != 1 {
		t.Fatalf("probe count = %d, want 1", probes.Load())
	}
}

func TestReadinessHandlerRejectsConcurrentProbe(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	engine := gin.New()
	engine.GET("/readyz", newReadinessHandler(func(context.Context) bool {
		close(started)
		<-release
		return true
	}))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		firstDone <- response
	}()
	<-started

	secondRequest := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	secondResponse := httptest.NewRecorder()
	engine.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent status = %d, want %d", secondResponse.Code, http.StatusServiceUnavailable)
	}
	if secondResponse.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", secondResponse.Header().Get("Retry-After"))
	}

	close(release)
	if firstResponse := <-firstDone; firstResponse.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", firstResponse.Code, http.StatusOK)
	}
}
