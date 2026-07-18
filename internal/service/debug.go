package service

import (
	"context"
	"os"
	"strings"
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

// logToContext keeps the context parameter in the debug helpers' shared API.
func logToContext(ctx context.Context, format string, args ...interface{}) {
	if IsDebugMode() {
		logging.Debugf(format, args...)
	}
}

// DebugLog 调试日志输出
func DebugLog(ctx context.Context, format string, args ...interface{}) {
	logToContext(ctx, format, args...)
}

// DebugLogRequest 请求开始日志
func DebugLogRequest(ctx context.Context, provider, endpoint, model string) {
	logToContext(ctx, "[%s] >>> 请求开始: endpoint=%s, model=%s", provider, endpoint, model)
}

// DebugLogRetry 重试日志
func DebugLogRetry(ctx context.Context, provider string, attempt int, accountID uint, err error) {
	logToContext(ctx, "[%s] ↻ 重试 #%d: accountID=%d, error=%v", provider, attempt, accountID, err)
}

// DebugLogAccountSelected 账号选择日志
func DebugLogAccountSelected(ctx context.Context, provider string, accountID uint, email string) {
	logToContext(ctx, "[%s] ✓ 选择账号: id=%d, email=%s", provider, accountID, email)
}

// DebugLogRequestSent 请求发送日志
func DebugLogRequestSent(ctx context.Context, provider, url string) {
	logToContext(ctx, "[%s] → 发送请求: %s", provider, url)
}

// DebugLogResponseReceived 响应接收日志
func DebugLogResponseReceived(ctx context.Context, provider string, statusCode int) {
	logToContext(ctx, "[%s] ← 收到响应: status=%d", provider, statusCode)
}

// DebugLogRequestEnd 请求结束日志
func DebugLogRequestEnd(ctx context.Context, provider string, success bool, err error) {
	if !success || err != nil {
		logToContext(ctx, "[%s] <<< 请求完成: success=false, error=%v", provider, err)
	} else {
		logToContext(ctx, "[%s] <<< 请求完成: success=true", provider)
	}
}

// DebugLogRequestHeaders 请求头日志
func DebugLogRequestHeaders(ctx context.Context, provider string, headers map[string][]string) {
	logToContext(ctx, "[%s] 请求头:", provider)
	for k, v := range headers {
		// 隐藏敏感信息
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "zencoder-api-key", "x-goog-api-key":
			logToContext(ctx, "[%s]   %s: ***", provider, k)
		default:
			logToContext(ctx, "[%s]   %s: %v", provider, k, v)
		}
	}
}

// DebugLogResponseHeaders 响应头日志
func DebugLogResponseHeaders(ctx context.Context, provider string, headers map[string][]string) {
	logToContext(ctx, "[%s] 响应头:", provider)
	for k, v := range headers {
		// 隐藏敏感信息
		switch strings.ToLower(k) {
		case "x-api-key", "authorization", "x-goog-api-key":
			logToContext(ctx, "[%s]   %s: ***", provider, k)
		default:
			logToContext(ctx, "[%s]   %s: %v", provider, k, v)
		}
	}
}

// DebugLogActualModel 实际调用模型日志
func DebugLogActualModel(ctx context.Context, provider, requestModel, actualModel string) {
	logToContext(ctx, "[%s] 模型映射: %s → %s", provider, requestModel, actualModel)
}

// DebugLogErrorResponse 错误响应内容日志
func DebugLogErrorResponse(ctx context.Context, provider string, statusCode int, body string) {
	logToContext(ctx, "[%s] ✗ 错误响应 [%d]: %s", provider, statusCode, body)
}
