package service

import (
	"context"
	"net/url"
	"os"
	"sync"

	"zencoder-2api/internal/logging"
)

var (
	debugMode     bool
	debugModeOnce sync.Once
)

// IsDebugMode 检查是否启用调试模式
func IsDebugMode() bool {
	debugModeOnce.Do(func() {
		debugMode = os.Getenv("DEBUG") == "true" || os.Getenv("DEBUG") == "1"
	})
	return debugMode
}

func logToContext(ctx context.Context, format string, args ...interface{}) {
	if IsDebugMode() {
		if requestID := logging.RequestIDFromContext(ctx); requestID != "" {
			args = append([]interface{}{requestID}, args...)
			format = "request_id=%s " + format
		}
		logging.Debugf(format, args...)
	}
}

// DebugLog intentionally discards arbitrary formatted values. Historical
// callers passed prompts, tool payloads and signatures here; the structured
// helpers below are the only permitted debug logging surface.
func DebugLog(context.Context, string, ...interface{}) {
}

// DebugLogRequest 请求开始日志
func DebugLogRequest(ctx context.Context, provider, endpoint, model string) {
	logToContext(ctx, "[%s] >>> 请求开始: endpoint=%s, model=%s", provider, endpoint, model)
}

// DebugLogRetry 重试日志
func DebugLogRetry(ctx context.Context, provider string, attempt int, accountID uint, err error) {
	logToContext(ctx, "provider_retry provider=%s attempt=%d account_id=%d error_type=%T", provider, attempt, accountID, err)
}

// DebugLogAccountSelected 账号选择日志
func DebugLogAccountSelected(ctx context.Context, provider string, accountID uint, email string) {
	logToContext(ctx, "account_selected provider=%s account_id=%d", provider, accountID)
}

// DebugLogRequestSent 请求发送日志
func DebugLogRequestSent(ctx context.Context, provider, endpointURL string) {
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		logToContext(ctx, "upstream_request provider=%s target=redacted", provider)
		return
	}
	logToContext(ctx, "upstream_request provider=%s host=%s path=%s", provider, parsed.Host, parsed.EscapedPath())
}

// DebugLogResponseReceived 响应接收日志
func DebugLogResponseReceived(ctx context.Context, provider string, statusCode int) {
	logToContext(ctx, "[%s] ← 收到响应: status=%d", provider, statusCode)
}

// DebugLogRequestEnd 请求结束日志
func DebugLogRequestEnd(ctx context.Context, provider string, success bool, err error) {
	if !success || err != nil {
		logToContext(ctx, "request_complete provider=%s success=false error_type=%T", provider, err)
	} else {
		logToContext(ctx, "[%s] <<< 请求完成: success=true", provider)
	}
}

// DebugLogRequestHeaders 请求头日志
func DebugLogRequestHeaders(ctx context.Context, provider string, headers map[string][]string) {
	logToContext(ctx, "request_headers provider=%s header_count=%d", provider, len(headers))
}

// DebugLogResponseHeaders 响应头日志
func DebugLogResponseHeaders(ctx context.Context, provider string, headers map[string][]string) {
	logToContext(ctx, "response_headers provider=%s header_count=%d", provider, len(headers))
}

// DebugLogActualModel 实际调用模型日志
func DebugLogActualModel(ctx context.Context, provider, requestModel, actualModel string) {
	logToContext(ctx, "[%s] 模型映射: %s → %s", provider, requestModel, actualModel)
}

// DebugLogErrorResponse 错误响应内容日志
func DebugLogErrorResponse(ctx context.Context, provider string, statusCode int, body string) {
	// Upstream error messages can echo prompts, tool arguments, or provider
	// metadata. Keep only the bounded size in debug logs.
	logToContext(ctx, "[%s] upstream error status=%d body_bytes=%d", provider, statusCode, len(body))
}
