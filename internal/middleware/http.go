package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"zencoder-2api/internal/logging"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "request_id"

// RequestID gives every request a server-owned correlation ID. Client supplied
// IDs are deliberately not trusted because they can be used to forge logs.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := newRequestID()
		c.Set(requestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// Entropy failure is exceptionally rare; keep the response usable without
	// pretending this fallback is cryptographically unique.
	return fmt.Sprintf("request-%d", fallbackRequestID.Add(1))
}

var fallbackRequestID atomic.Uint64

// Recovery reports only the request correlation ID and panic type. Gin's
// default recovery logger may dump request metadata on broken connections,
// which can expose credentials and bypass the configured application logger.
func Recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		requestID := logging.RequestIDFromContext(c.Request.Context())
		logging.Errorf("Recovered panic request_id=%s type=%T", requestID, recovered)
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

// SecurityHeaders adds conservative browser protections. API responses are
// never cacheable because several routes use credentials in request headers.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.Writer.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		header.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'")
		if publicBaseURLUsesHTTPS() {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if isPrivateResponse(c.Request.URL.Path) {
			header.Set("Cache-Control", "no-store")
			header.Set("Pragma", "no-cache")
		}
		c.Next()
	}
}

func publicBaseURLUsesHTTPS() bool {
	value := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL"))
	return strings.HasPrefix(strings.ToLower(value), "https://")
}

func isPrivateResponse(path string) bool {
	return path == "/healthz" ||
		path == "/livez" ||
		path == "/readyz" ||
		path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/") ||
		path == "/v1/chat/completions" ||
		path == "/v1/responses" ||
		path == "/v1/messages" ||
		strings.HasPrefix(path, "/v1beta/models") ||
		strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/oauth/")
}

// WriteIdleTimeout renews the connection write deadline whenever a response
// writes or flushes data. This bounds stalled SSE clients without imposing a
// fixed maximum duration on healthy streams.
func WriteIdleTimeout(timeout time.Duration) gin.HandlerFunc {
	if timeout <= 0 {
		panic("write idle timeout must be positive")
	}
	return func(c *gin.Context) {
		originalWriter := c.Writer
		writer := &writeIdleTimeoutWriter{ResponseWriter: originalWriter, timeout: timeout}
		c.Writer = writer
		defer func() {
			_ = writer.setDeadline(time.Time{})
			c.Writer = originalWriter
		}()
		c.Next()
	}
}

type writeIdleTimeoutWriter struct {
	gin.ResponseWriter
	timeout time.Duration
}

func (w *writeIdleTimeoutWriter) Write(data []byte) (int, error) {
	w.refreshDeadline()
	return w.ResponseWriter.Write(data)
}

func (w *writeIdleTimeoutWriter) WriteString(data string) (int, error) {
	w.refreshDeadline()
	return w.ResponseWriter.WriteString(data)
}

func (w *writeIdleTimeoutWriter) Flush() {
	w.refreshDeadline()
	w.ResponseWriter.Flush()
}

func (w *writeIdleTimeoutWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *writeIdleTimeoutWriter) refreshDeadline() {
	_ = w.setDeadline(time.Now().Add(w.timeout))
}

func (w *writeIdleTimeoutWriter) setDeadline(deadline time.Time) error {
	return http.NewResponseController(w).SetWriteDeadline(deadline)
}

// BodyLimit bounds request memory before protocol handlers read the body.
// MaxBytesReader also protects chunked requests where Content-Length is absent.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	if maxBytes <= 0 {
		panic("request body limit must be positive")
	}
	return func(c *gin.Context) {
		if c.Request.Body == nil {
			c.Next()
			return
		}
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": gin.H{
					"message": "request body too large",
					"type":    "invalid_request_error",
				},
			})
			return
		}

		tooLarge := false
		originalWriter := c.Writer
		c.Writer = &bodyLimitWriter{ResponseWriter: originalWriter, tooLarge: &tooLarge}
		c.Request.Body = &bodyLimitReader{
			ReadCloser: http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes),
			tooLarge:   &tooLarge,
		}
		defer func() { c.Writer = originalWriter }()
		c.Next()
		if tooLarge && !c.Writer.Written() {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": gin.H{
					"message": "request body too large",
					"type":    "invalid_request_error",
				},
			})
		}
	}
}

type bodyLimitReader struct {
	ReadCloser io.ReadCloser
	tooLarge   *bool
}

func (r *bodyLimitReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		*r.tooLarge = true
	}
	return n, err
}

func (r *bodyLimitReader) Close() error { return r.ReadCloser.Close() }

type bodyLimitWriter struct {
	gin.ResponseWriter
	tooLarge *bool
}

func (w *bodyLimitWriter) WriteHeader(code int) {
	if *w.tooLarge && code != http.StatusRequestEntityTooLarge {
		code = http.StatusRequestEntityTooLarge
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodyLimitWriter) WriteHeaderNow() { w.WriteHeader(http.StatusOK) }
