package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxAccountAttempts = 3
const maxUpstreamErrorBodyBytes = 1 << 20
const maxCompatibilityResponseBodyBytes = 64 << 20

var (
	ErrNoAvailableAccount = errors.New("没有可用token")
)

// UpstreamError preserves the status code and response body returned by
// Zencoder so callers can expose a useful, OpenAI-compatible error instead of
// turning every upstream failure into HTTP 500.
type UpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream request failed with status %d", e.StatusCode)
}

func (e *UpstreamError) Status() int {
	if e == nil || e.StatusCode == 0 {
		return http.StatusBadGateway
	}
	return e.StatusCode
}

func readUpstreamErrorBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxUpstreamErrorBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUpstreamErrorBodyBytes {
		return []byte(`{"error":{"message":"upstream error response exceeded 1 MiB","type":"upstream_error"}}`), nil
	}
	return data, nil
}

// readCompatibilityResponseBody bounds the full-body reads used by protocol
// adapters. Native passthrough streams are already copied incrementally.
func readCompatibilityResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxCompatibilityResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCompatibilityResponseBodyBytes {
		return nil, fmt.Errorf("compatibility response exceeds %d bytes", maxCompatibilityResponseBodyBytes)
	}
	return data, nil
}

func shouldRetryUpstreamStatus(status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly || status == http.StatusTooManyRequests || status == 529 {
		return true
	}
	return status >= 500 && status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported
}

func unknownModelError(modelID string) error {
	body := []byte(fmt.Sprintf(`{"error":{"message":"unknown model %q","type":"invalid_request_error","code":"model_not_found"}}`, modelID))
	return &UpstreamError{StatusCode: http.StatusBadRequest, Body: body}
}
