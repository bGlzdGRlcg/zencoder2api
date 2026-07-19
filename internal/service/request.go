package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type originalHeadersContextKey struct{}

func WithOriginalHeaders(ctx context.Context, headers http.Header) context.Context {
	safe := make(http.Header)
	for _, key := range []string{"Content-Type", "User-Agent", "Anthropic-Version"} {
		if values := headers.Values(key); len(values) > 0 {
			safe[key] = append([]string(nil), values...)
		}
	}
	return context.WithValue(ctx, originalHeadersContextKey{}, safe)
}

func originalHeadersFromContext(ctx context.Context) (http.Header, bool) {
	headers, ok := ctx.Value(originalHeadersContextKey{}).(http.Header)
	return headers, ok
}

var gatewayTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	ExpectContinueTimeout: time.Second,
}

func zencoderGatewayBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("ZENCODER_GATEWAY_BASE_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return "https://api.zencoder.ai"
}

// ValidateUpstreamEndpointConfig prevents a configuration typo from sending
// bearer tokens or API keys over plaintext or to a malformed URL. HTTP is
// permitted only for loopback integration tests and local development.
func ValidateUpstreamEndpointConfig() error {
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "ZENCODER_GATEWAY_BASE_URL", value: os.Getenv("ZENCODER_GATEWAY_BASE_URL")},
		{name: "ZENCODER_AUTH_BASE_URL", value: os.Getenv("ZENCODER_AUTH_BASE_URL")},
	} {
		if strings.TrimSpace(item.value) == "" {
			continue
		}
		if err := validateUpstreamBaseURL(item.value); err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
	}
	return nil
}

func validateUpstreamBaseURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be a valid HTTP(S) base URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must not contain credentials, query parameters, or fragments")
	}
	if parsed.Scheme != "https" && !loopbackEndpointHost(parsed.Hostname()) {
		return fmt.Errorf("must use HTTPS unless its host is loopback")
	}
	return nil
}

func loopbackEndpointHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newDirectHTTPClient retains its historical name for callers, but shares one
// connection pool and honors the standard proxy environment variables.
func newDirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: gatewayTransport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Gateway requests carry credentials. Never forward them to a redirect
			// target or let net/http rewrite a POST into a GET.
			return http.ErrUseLastResponse
		},
	}
}
