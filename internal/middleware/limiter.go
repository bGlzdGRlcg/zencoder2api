package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const limiterWindow = time.Minute
const defaultMaxLimiterBuckets = 10000

type requestBucket struct {
	windowStart time.Time
	count       int
}

// RequestLimiter bounds both long-running concurrent streams and per-caller
// request rate. Caller keys are one-way hashes; credentials are never retained.
type RequestLimiter struct {
	slots          chan struct{}
	requestsPerMin int
	acquireTimeout time.Duration
	now            func() time.Time
	identity       func(*gin.Context) string

	mu          sync.Mutex
	buckets     map[string]requestBucket
	lastCleanup time.Time
	maxBuckets  int
}

func NewRequestLimiter(maxConcurrent, requestsPerMinute int) *RequestLimiter {
	if maxConcurrent <= 0 {
		panic("max concurrent requests must be positive")
	}
	if requestsPerMinute <= 0 {
		panic("requests per minute must be positive")
	}
	now := time.Now()
	return &RequestLimiter{
		slots:          make(chan struct{}, maxConcurrent),
		requestsPerMin: requestsPerMinute,
		acquireTimeout: 2 * time.Second,
		now:            time.Now,
		identity:       requestLimitIdentity,
		buckets:        make(map[string]requestBucket),
		lastCleanup:    now,
		maxBuckets:     defaultMaxLimiterBuckets,
	}
}

func NewRemoteRequestLimiter(maxConcurrent, requestsPerMinute int) *RequestLimiter {
	limiter := NewRequestLimiter(maxConcurrent, requestsPerMinute)
	limiter.identity = remoteLimitIdentity
	return limiter
}

func (l *RequestLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed, retryAfter := l.allow(l.identity(c))
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(maxInt(1, int(retryAfter.Round(time.Second)/time.Second))))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
				"message": "request rate limit exceeded",
				"type":    "rate_limit_error",
			}})
			return
		}

		timer := time.NewTimer(l.acquireTimeout)
		defer timer.Stop()
		select {
		case l.slots <- struct{}{}:
			defer func() { <-l.slots }()
			c.Next()
		case <-timer.C:
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"message": "server concurrency limit reached",
				"type":    "overloaded_error",
			}})
		case <-c.Request.Context().Done():
			c.Abort()
		}
	}
}

func (l *RequestLimiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastCleanup) >= limiterWindow {
		for bucketKey, bucket := range l.buckets {
			if now.Sub(bucket.windowStart) >= 2*limiterWindow {
				delete(l.buckets, bucketKey)
			}
		}
		l.lastCleanup = now
	}

	bucket, exists := l.buckets[key]
	if !exists && len(l.buckets) >= l.maxBuckets {
		return false, limiterWindow
	}
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= limiterWindow {
		bucket = requestBucket{windowStart: now}
	}
	if bucket.count >= l.requestsPerMin {
		return false, limiterWindow - now.Sub(bucket.windowStart)
	}
	bucket.count++
	l.buckets[key] = bucket
	return true, 0
}

func requestLimitIdentity(c *gin.Context) string {
	request := c.Request
	credential := strings.TrimSpace(request.Header.Get("Authorization"))
	if credential == "" {
		credential = strings.TrimSpace(request.Header.Get("x-api-key"))
	}
	if credential == "" {
		credential = strings.TrimSpace(request.Header.Get("x-goog-api-key"))
	}
	if credential == "" {
		credential = strings.TrimSpace(request.URL.Query().Get("key"))
	}
	if credential != "" {
		sum := sha256.Sum256([]byte(credential))
		return "credential:" + hex.EncodeToString(sum[:])
	}
	return remoteLimitIdentity(c)
}

func remoteLimitIdentity(c *gin.Context) string {
	clientIP := c.ClientIP()
	address, err := netip.ParseAddr(clientIP)
	if err != nil {
		return "remote:" + clientIP
	}
	address = address.Unmap().WithZone("")
	if address.Is6() {
		prefix, err := address.Prefix(64)
		if err == nil {
			return "remote:" + prefix.Masked().String()
		}
	}
	return "remote:" + address.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
