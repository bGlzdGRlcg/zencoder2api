package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/logging"
)

func TestAuthMiddlewareFailsClosedWithoutToken(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "")
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "false")

	response := runMiddlewareRequest(t, http.MethodGet, AuthMiddleware(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestAuthMiddlewareAllowsExplicitLoopbackDevelopment(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "")
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "true")

	response := runMiddlewareRequest(t, http.MethodGet, AuthMiddleware(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestAuthMiddlewareRejectsNonLoopbackInDevelopmentMode(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "")
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "true")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(AuthMiddleware())
	engine.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestAuthMiddlewareAcceptsCaseInsensitiveBearerScheme(t *testing.T) {
	const token = "0123456789abcdef"
	t.Setenv("AUTH_TOKEN", token)
	t.Setenv("ALLOW_INSECURE_LOCALHOST", "false")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(AuthMiddleware())
	engine.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "bearer "+token)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestBodyLimitRejectsKnownAndChunkedBodies(t *testing.T) {
	for _, test := range []struct {
		name          string
		contentLength int64
	}{
		{name: "known length", contentLength: 5},
		{name: "chunked", contentLength: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(BodyLimit(4))
			engine.POST("/", func(c *gin.Context) {
				if _, err := io.ReadAll(c.Request.Body); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
			request.ContentLength = test.contentLength
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
			}
		})
	}
}

func TestRequestIDAndSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestID(), SecurityHeaders())
	var contextRequestID string
	engine.GET("/api/accounts", func(c *gin.Context) {
		contextRequestID = logging.RequestIDFromContext(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID is empty")
	}
	if contextRequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("context request ID = %q, header = %q", contextRequestID, response.Header().Get("X-Request-ID"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", response.Header().Get("X-Content-Type-Options"))
	}
	csp := response.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "cdn.tailwindcss.com") {
		t.Fatalf("CSP contains external/inline script allowance: %q", csp)
	}
}

func TestWriteIdleTimeoutRenewsAndClearsDeadline(t *testing.T) {
	giveUpAfter := 2 * time.Second
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	gine := gin.New()
	gine.Use(WriteIdleTimeout(giveUpAfter))
	gine.GET("/", func(c *gin.Context) {
		_, _ = c.Writer.WriteString("chunk")
		c.Writer.Flush()
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	gine.ServeHTTP(response, request)
	deadlines := response.recordedDeadlines()
	if len(deadlines) < 3 {
		t.Fatalf("deadline count = %d, want at least 3", len(deadlines))
	}
	for _, deadline := range deadlines[:len(deadlines)-1] {
		if deadline.IsZero() {
			t.Fatal("active write deadline was cleared before middleware exit")
		}
	}
	if !deadlines[len(deadlines)-1].IsZero() {
		t.Fatalf("final deadline = %v, want zero", deadlines[len(deadlines)-1])
	}
}

func TestRecoveryDoesNotExposePanicDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Recovery(), RequestID())
	engine.GET("/panic", func(*gin.Context) {
		panic("authorization-secret-must-not-leak")
	})

	request := httptest.NewRequest(http.MethodGet, "/panic?key=query-secret", nil)
	request.Header.Set("Authorization", "Bearer header-secret")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	for _, secret := range []string{"authorization-secret-must-not-leak", "query-secret", "header-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("response exposed %q", secret)
		}
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID is empty")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func (r *deadlineRecorder) recordedDeadlines() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

func runMiddlewareRequest(t *testing.T, method string, middleware gin.HandlerFunc, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware)
	engine.Handle(method, "/", handler)
	request := httptest.NewRequest(method, "/", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}
