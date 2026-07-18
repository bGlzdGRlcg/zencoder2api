package service

import (
	"errors"
	"fmt"
	"net/http"
)

const maxAccountAttempts = 3

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
	if len(e.Body) == 0 {
		return fmt.Sprintf("upstream request failed with status %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream request failed with status %d: %s", e.StatusCode, e.Body)
}

func (e *UpstreamError) Status() int {
	if e == nil || e.StatusCode == 0 {
		return http.StatusBadGateway
	}
	return e.StatusCode
}
