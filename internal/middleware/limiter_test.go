package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestLimiterRateLimit(t *testing.T) {
	limiter := NewRequestLimiter(2, 2)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(limiter.Middleware())
	engine.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for attempt, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer caller")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, response.Code, want)
		}
	}
}

func TestRemoteRequestLimiterDoesNotBucketByGuessedPassword(t *testing.T) {
	limiter := NewRemoteRequestLimiter(2, 2)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(limiter.Middleware())
	engine.POST("/", func(c *gin.Context) { c.Status(http.StatusUnauthorized) })

	for attempt, want := range []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		request.RemoteAddr = "192.0.2.10:1234"
		request.Header.Set("Authorization", "Bearer guess-"+strconv.Itoa(attempt))
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, response.Code, want)
		}
	}
}

func TestRemoteRequestLimiterNormalizesIPv6Prefix(t *testing.T) {
	limiter := NewRemoteRequestLimiter(2, 2)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(limiter.Middleware())
	engine.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for attempt, remoteAddress := range []string{
		"[2001:db8:1:2::1]:1234",
		"[2001:db8:1:2::2]:1234",
		"[2001:db8:1:2::3]:1234",
		"[2001:db8:1:3::1]:1234",
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = remoteAddress
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		want := http.StatusNoContent
		if attempt == 2 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, response.Code, want)
		}
	}
}

func TestRequestLimiterConcurrencyLimit(t *testing.T) {
	limiter := NewRequestLimiter(1, 100)
	limiter.acquireTimeout = 20 * time.Millisecond
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(limiter.Middleware())
	started := make(chan struct{})
	release := make(chan struct{})
	engine.GET("/", func(c *gin.Context) {
		close(started)
		<-release
		c.Status(http.StatusNoContent)
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", "Bearer first")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		firstDone <- response
	}()
	<-started

	secondRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	secondRequest.Header.Set("Authorization", "Bearer second")
	secondResponse := httptest.NewRecorder()
	engine.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want %d", secondResponse.Code, http.StatusServiceUnavailable)
	}
	close(release)
	if firstResponse := <-firstDone; firstResponse.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstResponse.Code, http.StatusNoContent)
	}
}

func TestRequestLimiterRejectsNewBucketAtCapacity(t *testing.T) {
	limiter := NewRequestLimiter(1, 100)
	limiter.maxBuckets = 2
	if allowed, _ := limiter.allow("first"); !allowed {
		t.Fatal("first bucket was rejected")
	}
	if allowed, _ := limiter.allow("second"); !allowed {
		t.Fatal("second bucket was rejected")
	}
	if allowed, _ := limiter.allow("third"); allowed {
		t.Fatal("new bucket was accepted after reaching the hard limit")
	}
	if len(limiter.buckets) != limiter.maxBuckets {
		t.Fatalf("bucket count = %d, want %d", len(limiter.buckets), limiter.maxBuckets)
	}
}
