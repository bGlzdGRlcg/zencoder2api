package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const defaultMaxRequestBodyBytes int64 = 4 << 20

func maxRequestBodyBytes() int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("MAX_REQUEST_BODY_BYTES")), 10, 64)
	if err != nil || value <= 0 || value > 128<<20 {
		return defaultMaxRequestBodyBytes
	}
	return value
}

// readRequestBody applies a second boundary at the handler level. The global
// middleware catches Content-Length early, while this also covers chunked input.
func readRequestBody(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes())
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &requestBodyTooLargeError{limit: maxErr.Limit}
		}
		return nil, err
	}
	return body, nil
}

type requestBodyTooLargeError struct{ limit int64 }

func (e *requestBodyTooLargeError) Error() string {
	return "request body exceeds the configured limit"
}

func bodyErrorStatus(err error) int {
	var tooLarge *requestBodyTooLargeError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func requestTraceID(c *gin.Context) string {
	if id := c.Writer.Header().Get("X-Request-ID"); id != "" {
		return id
	}
	return "unavailable"
}

func decodeStrictJSON(c *gin.Context, dst interface{}, limit int64) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain exactly one JSON value")
		}
		return err
	}
	return nil
}
