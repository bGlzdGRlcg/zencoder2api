package service

import (
	"context"
	"net"
	"net/http"
	"time"
)

type originalHeadersContextKey struct{}

func WithOriginalHeaders(ctx context.Context, headers http.Header) context.Context {
	return context.WithValue(ctx, originalHeadersContextKey{}, headers)
}

func originalHeadersFromContext(ctx context.Context) (http.Header, bool) {
	headers, ok := ctx.Value(originalHeadersContextKey{}).(http.Header)
	return headers, ok
}

// newDirectHTTPClient creates a direct client. Proxy configuration is
// intentionally disabled, including HTTP_PROXY inherited from the process.
func newDirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		Timeout: timeout,
	}
}
