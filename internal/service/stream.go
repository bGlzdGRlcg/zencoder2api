package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"zencoder-2api/internal/model"
)

const maxSSELineBytes = 8 << 20

type streamProtocol uint8

const (
	streamOpenAIChat streamProtocol = iota
	streamOpenAIResponses
	streamAnthropic
	streamGemini
)

var errUpstreamStreamTruncated = errors.New("upstream stream ended without a terminal event")

type accountFinalizingBody struct {
	io.ReadCloser
	ctx        context.Context
	resp       *http.Response
	account    *model.Account
	multiplier float64
	protocol   streamProtocol
	tail       []byte
	finalized  atomic.Bool
	closed     atomic.Bool
}

func finalizeStreamingAccount(ctx context.Context, resp *http.Response, account *model.Account, multiplier float64, protocol streamProtocol) {
	resp.Body = &accountFinalizingBody{
		ReadCloser: resp.Body,
		ctx:        ctx,
		resp:       resp,
		account:    account,
		multiplier: multiplier,
		protocol:   protocol,
	}
}

func (body *accountFinalizingBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if n > 0 && !body.finalized.Load() {
		combined := append(body.tail, data[:n]...)
		if streamTerminalSeen(body.protocol, combined) && body.finalized.CompareAndSwap(false, true) {
			UpdateAccountCreditsFromResponse(body.account, body.resp, body.multiplier)
			MarkAccountHealthy(body.account)
		}
		const terminalMarkerWindow = 64
		if len(combined) > terminalMarkerWindow {
			combined = combined[len(combined)-terminalMarkerWindow:]
		}
		body.tail = append(body.tail[:0], combined...)
	}
	if err != nil && !body.closed.Load() && body.ctx.Err() == nil && body.finalized.CompareAndSwap(false, true) {
		MarkAccountFailure(body.account, 0, 0, errUpstreamStreamTruncated)
	}
	return n, err
}

func (body *accountFinalizingBody) Close() error {
	body.closed.Store(true)
	return body.ReadCloser.Close()
}

func streamTerminalSeen(protocol streamProtocol, data []byte) bool {
	switch protocol {
	case streamOpenAIChat:
		return containsSSELinePrefix(data, "data: [DONE]") || containsSSELinePrefix(data, "data:[DONE]")
	case streamOpenAIResponses:
		return containsSSEEvent(data, "response.completed") ||
			containsSSEEvent(data, "response.failed") ||
			containsSSEEvent(data, "response.incomplete")
	case streamAnthropic:
		return containsSSEEvent(data, "message_stop")
	case streamGemini:
		return bytes.Contains(data, []byte("\"finishReason\"")) ||
			bytes.Contains(data, []byte("\"blockReason\""))
	default:
		return false
	}
}

func containsSSEEvent(data []byte, event string) bool {
	return containsSSELinePrefix(data, "event: "+event) || containsSSELinePrefix(data, "event:"+event)
}

func containsSSELinePrefix(data []byte, prefix string) bool {
	marker := []byte(prefix)
	for offset := 0; offset < len(data); {
		index := bytes.Index(data[offset:], marker)
		if index < 0 {
			return false
		}
		index += offset
		if index == 0 || data[index-1] == '\n' {
			return true
		}
		offset = index + 1
	}
	return false
}

func readSSELine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxSSELineBytes {
			return "", fmt.Errorf("SSE line exceeds %d bytes", maxSSELineBytes)
		}
		line = append(line, part...)
		if err != bufio.ErrBufferFull {
			return string(line), err
		}
	}
}

var hopByHopResponseHeaders = map[string]struct{}{
	"connection": {}, "keep-alive": {}, "proxy-authenticate": {},
	"proxy-authorization": {}, "te": {}, "trailer": {},
	"transfer-encoding": {}, "upgrade": {},
}

var sensitiveUpstreamResponseHeaders = map[string]struct{}{
	"alt-svc":                             {},
	"authorization":                       {},
	"clear-site-data":                     {},
	"content-security-policy":             {},
	"content-security-policy-report-only": {},
	"cross-origin-embedder-policy":        {},
	"cross-origin-opener-policy":          {},
	"cross-origin-resource-policy":        {},
	"forwarded":                           {},
	"nel":                                 {},
	"permissions-policy":                  {},
	"proxy-authorization":                 {},
	"proxy-status":                        {},
	"referrer-policy":                     {},
	"report-to":                           {},
	"reporting-endpoints":                 {},
	"server":                              {},
	"set-cookie":                          {},
	"strict-transport-security":           {},
	"via":                                 {},
	"x-api-key":                           {},
	"x-content-type-options":              {},
	"x-frame-options":                     {},
	"x-goog-api-key":                      {},
	"x-request-id":                        {},
	"x-xss-protection":                    {},
	"zencoder-api-key":                    {},
}

var blockedUpstreamResponseHeaderPrefixes = []string{"access-control-", "x-forwarded-"}

func copyResponseHeaders(dst, src http.Header, bodyModified bool) {
	blocked := make(map[string]struct{}, len(hopByHopResponseHeaders)+len(sensitiveUpstreamResponseHeaders))
	for key := range hopByHopResponseHeaders {
		blocked[key] = struct{}{}
	}
	for key := range sensitiveUpstreamResponseHeaders {
		blocked[key] = struct{}{}
	}
	for _, value := range src.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
		}
	}
	if bodyModified {
		for _, key := range []string{"content-length", "content-encoding", "content-md5", "digest", "etag"} {
			blocked[key] = struct{}{}
		}
	}
	for key, values := range src {
		normalizedKey := strings.ToLower(key)
		_, skip := blocked[normalizedKey]
		for _, prefix := range blockedUpstreamResponseHeaderPrefixes {
			skip = skip || strings.HasPrefix(normalizedKey, prefix)
		}
		if skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type flushingWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (w flushingWriter) Write(data []byte) (int, error) {
	n, err := w.w.Write(data)
	if n > 0 {
		w.f.Flush()
	}
	return n, err
}

// StreamResponse 流式传输响应到客户端
func StreamResponse(w http.ResponseWriter, resp *http.Response) error {
	// 复制响应头
	copyResponseHeaders(w.Header(), resp.Header, false)
	w.WriteHeader(resp.StatusCode)

	// 获取Flusher接口
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 如果不支持Flusher，直接复制
		_, err := io.Copy(w, resp.Body)
		return err
	}

	_, err := io.CopyBuffer(flushingWriter{w: w, f: flusher}, resp.Body, make([]byte, 32<<10))
	return err
}

// CopyResponse 普通响应复制
func CopyResponse(w http.ResponseWriter, resp *http.Response) error {
	copyResponseHeaders(w.Header(), resp.Header, false)
	w.WriteHeader(resp.StatusCode)
	_, err := io.Copy(w, resp.Body)
	return err
}
