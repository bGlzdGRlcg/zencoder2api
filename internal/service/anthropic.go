package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zencoder-2api/internal/model"
)

// logRequestDetails 记录请求详细信息
func logRequestDetails(ctx context.Context, prefix string, headers http.Header, body []byte) {
	logToContext(ctx, "request_details prefix=%q header_count=%d body_bytes=%d", prefix, len(headers), len(body))
}

const AnthropicBaseURL = "https://api.zencoder.ai/anthropic"

func anthropicCompatibleBaseURL(providerID string) string {
	baseURL := zencoderGatewayBaseURL()
	if providerID == "" || providerID == "anthropic" {
		return baseURL + "/anthropic"
	}
	return baseURL + "/" + strings.Trim(providerID, "/")
}

type AnthropicService struct{}

func NewAnthropicService() *AnthropicService {
	return &AnthropicService{}
}

// Messages 处理/v1/messages请求，直接透传到Anthropic API
func (s *AnthropicService) Messages(ctx context.Context, body []byte, isStream bool) (*http.Response, error) {
	ctx = ensureOperationID(ctx)
	var req struct {
		Model    string                 `json:"model"`
		Thinking map[string]interface{} `json:"thinking,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	// 记录请求的模型和thinking状态
	thinkingStatus := "disabled"
	if req.Thinking != nil {
		if enabled, ok := req.Thinking["enabled"].(bool); ok && enabled {
			thinkingStatus = "enabled"
		} else if thinkingType, ok := req.Thinking["type"].(string); ok && thinkingType == "enabled" {
			thinkingStatus = "enabled"
		}
		// 如果有thinking配置且有budget_tokens，也记录
		if budget, ok := req.Thinking["budget_tokens"].(float64); ok && budget > 0 {
			thinkingStatus = fmt.Sprintf("enabled(budget=%g)", budget)
		}
	}
	// 只在非限速测试时输出请求信息
	if IsDebugMode() && !strings.Contains(req.Model, "test") {
		logToContext(ctx, "anthropic_request model=%s thinking=%s", req.Model, thinkingStatus)
	}

	// 检查模型是否存在于模型字典中
	_, exists := model.GetZenModel(req.Model)
	if !exists {
		DebugLog(ctx, "[Anthropic] 模型不存在: %s", req.Model)
		return nil, unknownModelError(req.Model)
	}

	DebugLogRequest(ctx, "Anthropic", "/v1/messages", req.Model)

	var lastErr error
	tried := make(map[uint]struct{})
	refreshedAfter401 := make(map[uint]struct{})
	attemptLimit := accountAttemptLimit()
	for i := 0; i < attemptLimit; i++ {
		account, err := GetNextAccountContext(ctx, tried)
		if err != nil {
			DebugLogRequestEnd(ctx, "Anthropic", false, err)
			return nil, err
		}
		tried[account.ID] = struct{}{}
		DebugLogAccountSelected(ctx, "Anthropic", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, req.Model, body)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			MarkAccountFailure(account, 0, 0, err)
			DebugLogRetry(ctx, "Anthropic", i+1, account.ID, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		// 只在调试模式下且非限速测试时输出详细响应信息
		if IsDebugMode() && !strings.Contains(req.Model, "test") {
			DebugLogResponseReceived(ctx, "Anthropic", resp.StatusCode)

			// 只输出积分信息，不输出所有响应头
			if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
				resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
				resp.Header.Get("Zen-Request-Cost") != "" {
				DebugLog(ctx, "[Anthropic] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
					resp.Header.Get("Zen-Pricing-Period-Limit"),
					resp.Header.Get("Zen-Pricing-Period-Cost"),
					resp.Header.Get("Zen-Request-Cost"))
			}
		}

		if resp.StatusCode >= 300 {
			// 读取错误响应内容
			errBody, _ := readUpstreamErrorBody(resp.Body)
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_, alreadyRefreshed := refreshedAfter401[account.ID]
			if resp.StatusCode == http.StatusUnauthorized && account.CredentialType == model.CredentialOAuth && !alreadyRefreshed {
				if err := ForceOAuthRefresh(ctx, account); err == nil {
					refreshedAfter401[account.ID] = struct{}{}
					delete(tried, account.ID)
					i--
					continue
				}
			}
			if shouldRetryUpstreamStatus(resp.StatusCode) {
				MarkAccountFailure(account, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), &UpstreamError{StatusCode: resp.StatusCode})
			}

			// 检查是否是官方API直接抛出的错误（413、400、429）
			// 这些错误不是token池问题，应直接返回给客户端
			if resp.StatusCode == 413 || resp.StatusCode == 400 || resp.StatusCode == 429 {
				// 对于400错误，根据错误类型决定日志级别
				if resp.StatusCode == 400 {
					// 解析thinking状态用于日志
					thinkingStatus := "disabled"
					if req.Thinking != nil {
						if enabled, ok := req.Thinking["enabled"].(bool); ok && enabled {
							thinkingStatus = "enabled"
						} else if thinkingType, ok := req.Thinking["type"].(string); ok && thinkingType == "enabled" {
							thinkingStatus = "enabled"
						}
						// 如果有thinking配置且有budget_tokens，也记录
						if budget, ok := req.Thinking["budget_tokens"].(float64); ok && budget > 0 {
							thinkingStatus = fmt.Sprintf("enabled(budget=%g)", budget)
						}
					}

					// 尝试解析错误类型
					var errResp struct {
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}

					isKnownError := false
					isPromptTooLongError := false
					if err := json.Unmarshal(errBody, &errResp); err == nil && errResp.Error.Type != "" {
						// 检查是否是已知的错误类型
						knownErrors := []string{
							"prompt is too long",
							"max_tokens",
							"invalid_request_error",
							"authentication_error",
							"permission_error",
							"rate_limit_error",
						}

						errorMessage := strings.ToLower(errResp.Error.Message)
						for _, known := range knownErrors {
							if strings.Contains(errorMessage, known) || errResp.Error.Type == known {
								isKnownError = true
								if known == "prompt is too long" || strings.Contains(errorMessage, "prompt is too long") {
									isPromptTooLongError = true
								}
								break
							}
						}

						if isKnownError {
							// 已知错误，只输出简单日志，包含请求模型ID和thinking状态
							logToContext(ctx, "anthropic_error status=400 type=%s model=%s thinking=%s", errResp.Error.Type, req.Model, thinkingStatus)

							// 对于非"prompt is too long"错误，在DEBUG模式下输出详细信息
							if !isPromptTooLongError && IsDebugMode() {
								if originalHeaders, ok := originalHeadersFromContext(ctx); ok {
									logRequestDetails(ctx, "[Anthropic] 原始客户端", originalHeaders, body)
								}
							}
						} else {
							// 未知错误，输出详细日志用于调试，包含请求模型ID和thinking状态
							logToContext(ctx, "anthropic_error status=400 type=unknown model=%s thinking=%s body_bytes=%d", req.Model, thinkingStatus, len(errBody))
							if IsDebugMode() {
								// DEBUG模式下输出原始请求信息
								if originalHeaders, ok := originalHeadersFromContext(ctx); ok {
									logRequestDetails(ctx, "[Anthropic] 原始客户端", originalHeaders, body)
								}
							}
						}
					} else {
						// 解析失败，输出完整错误用于调试，包含请求模型ID和thinking状态
						logToContext(ctx, "anthropic_error status=400 type=undecodable model=%s thinking=%s body_bytes=%d", req.Model, thinkingStatus, len(errBody))
						if IsDebugMode() {
							// DEBUG模式下输出原始请求信息
							if originalHeaders, ok := originalHeadersFromContext(ctx); ok {
								logRequestDetails(ctx, "[Anthropic] 原始客户端", originalHeaders, body)
							}
						}
					}
				} else if resp.StatusCode == 429 {
					// 简化429错误日志输出
					s.classifyAndLog429Error(ctx, string(errBody), account.ID, account.OAuthEmail)

					// 检查是否是Claude官方的429错误
					isClaudeOfficialError := s.isClaudeOfficial429Error(string(errBody))

					// 只有Claude官方的429错误才返回原始响应，其他429错误返回通用错误
					if isClaudeOfficialError {
						// Claude官方429错误，返回原始响应
						return &http.Response{
							StatusCode: resp.StatusCode,
							Header:     resp.Header,
							Body:       io.NopCloser(bytes.NewReader(errBody)),
						}, nil
					} else {
						// 非Claude官方429错误，不返回原始响应，继续重试其他账号
						lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
						if IsDebugMode() {
							DebugLogRetry(ctx, "Anthropic", i+1, account.ID, lastErr)
						}
						continue
					}
				}
				// 对于其他官方 API 错误（400、413），不计算账号错误次数，
				// 直接返回原始响应。
				return &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader(errBody)),
				}, nil
			}

			// 503和529错误：上游API错误，不是token问题
			if resp.StatusCode == 503 || resp.StatusCode == 529 {
				lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
				continue
			}

			// 其余可重试状态（包括所有 5xx）统一换用下一健康凭据。
			// 409 等请求语义错误已由 shouldRetryUpstreamStatus 排除，避免重复副作用。
			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}

			// 只在调试模式下输出详细错误信息
			if IsDebugMode() {
				DebugLogErrorResponse(ctx, "Anthropic", resp.StatusCode, string(errBody))
				DebugLogRetry(ctx, "Anthropic", i+1, account.ID, lastErr)
			} else {
				// 非调试模式下只输出简单的重试信息
				logToContext(ctx, "anthropic_retry status=%d attempt=%d", resp.StatusCode, i+1)
			}
			continue
		}

		zenModel, exists := model.GetZenModel(req.Model)
		multiplier := 1.0
		if exists {
			multiplier = zenModel.Multiplier
		}
		if isStream {
			finalizeStreamingAccount(ctx, resp, account, multiplier, streamAnthropic)
		} else {
			UpdateAccountCreditsFromResponse(account, resp, multiplier)
			MarkAccountHealthy(account)
		}

		DebugLogRequestEnd(ctx, "Anthropic", true, nil)
		return resp, nil
	}

	// 只在调试模式下输出详细的请求结束日志
	if IsDebugMode() {
		DebugLogRequestEnd(ctx, "Anthropic", false, lastErr)
	} else {
		// 非调试模式下只输出简单的失败信息
		logToContext(ctx, "anthropic_retries_exhausted error_type=%T", lastErr)
	}

	// 检查是否是网络连接错误，如果是则返回统一的错误信息，避免暴露内部网络详情
	if lastErr != nil {
		errStr := lastErr.Error()
		// 检查常见的网络连接错误
		if strings.Contains(errStr, "dial tcp") ||
			strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "no such host") ||
			strings.Contains(errStr, "cannot assign requested address") ||
			strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "network is unreachable") {
			return nil, ErrNoAvailableAccount
		}
	}

	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func prepareAnthropicRequestBody(body []byte, actualModel string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["model"] = actualModel
	return json.Marshal(raw)
}

func (s *AnthropicService) doRequest(ctx context.Context, account *model.Account, modelID string, body []byte) (*http.Response, error) {
	zenModel, exists := model.GetZenModel(modelID)
	if !exists {
		return nil, unknownModelError(modelID)
	}

	// Send the unchanged gateway catalog IDs in both the request header and the
	// Anthropic body; the normalized public ID is only used by this API.
	modifiedBody, err := prepareAnthropicRequestBody(body, zenModel.Model)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare request body: %w", err)
	}

	// 对于需要 thinking 的模型，强制添加 thinking 配置
	modifiedBody, err = s.ensureThinkingConfig(modifiedBody, modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure thinking config: %w", err)
	}

	// 根据模型要求调整参数（温度、top_p等）
	modifiedBody, err = s.adjustParametersForModel(modifiedBody, modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to adjust parameters: %w", err)
	}

	// 注意：已移除模型重定向逻辑，直接使用用户请求的模型名
	DebugLogActualModel(ctx, "Anthropic", modelID, modelID)

	reqURL := anthropicCompatibleBaseURL(zenModel.ProviderID) + "/v1/messages"
	DebugLogRequestSent(ctx, "Anthropic", reqURL)

	resp, err := s.makeRequest(ctx, modifiedBody, account, zenModel)
	if err != nil {
		return nil, err
	}

	// 检查是否是400错误，需要特殊处理
	if resp.StatusCode == 400 {
		bodyBytes, readErr := readUpstreamErrorBody(resp.Body)
		resp.Body.Close()

		if readErr == nil {
			errorBody := string(bodyBytes)

			// 检查是否是thinking格式错误，但不再进行模型切换
			if s.isThinkingFormatError(errorBody) {
				logToContext(ctx, "anthropic_thinking_format_error body_bytes=%d", len(errorBody))
			}

			// 检查是否是thinking signature过期错误
			if s.isThinkingSignatureError(errorBody) {
				// 解析当前请求的模型和thinking状态
				var reqInfo struct {
					Model    string                 `json:"model"`
					Thinking map[string]interface{} `json:"thinking,omitempty"`
				}
				json.Unmarshal(modifiedBody, &reqInfo)

				thinkingStatus := "disabled"
				if reqInfo.Thinking != nil {
					if enabled, ok := reqInfo.Thinking["enabled"].(bool); ok && enabled {
						thinkingStatus = "enabled"
					} else if thinkingType, ok := reqInfo.Thinking["type"].(string); ok && thinkingType == "enabled" {
						thinkingStatus = "enabled"
					}
					if budget, ok := reqInfo.Thinking["budget_tokens"].(float64); ok && budget > 0 {
						thinkingStatus = fmt.Sprintf("enabled(budget=%g)", budget)
					}
				}

				if IsDebugMode() {
					logToContext(ctx, "anthropic_signature_retry mode=redacted")
				} else {
					logToContext(ctx, "anthropic_signature_retry model=%s thinking=%s", reqInfo.Model, thinkingStatus)
				}

				// 转换请求体：将assistant消息转换为user消息
				fixedBody, fixErr := s.convertAssistantMessagesToUser(modifiedBody)
				if fixErr == nil {
					return s.makeRequest(ctx, fixedBody, account, zenModel)
				} else {
					logToContext(ctx, "anthropic_message_conversion_failed error_type=%T", fixErr)
				}
			}

			// 检查是否是参数冲突错误（temperature 和 top_p 不能同时指定）
			if s.isParameterConflictError(errorBody) {
				DebugLogRequestSent(ctx, "Anthropic", "Retrying with only temperature parameter")

				// 移除 top_p 参数，只保留 temperature
				fixedBody, fixErr := s.removeTopP(modifiedBody)
				if fixErr == nil {
					return s.makeRequest(ctx, fixedBody, account, zenModel)
				}
			}

			// 检查是否是温度参数错误
			if s.isTemperatureError(errorBody) {
				DebugLogRequestSent(ctx, "Anthropic", "Retrying with temperature=1.0")

				// 强制设置温度为1.0并重试
				fixedBody, fixErr := s.forceTemperature(modifiedBody, 1.0)
				if fixErr == nil {
					return s.makeRequest(ctx, fixedBody, account, zenModel)
				}
			}

		}

		// 如果不是thinking相关的可修复错误，返回原始响应
		return &http.Response{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
		}, nil
	}

	return resp, nil
}

func (s *AnthropicService) makeRequest(ctx context.Context, body []byte, account *model.Account, zenModel model.ZenModel) (*http.Response, error) {
	baseURL := anthropicCompatibleBaseURL(zenModel.ProviderID)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 设置Zencoder自定义请求头
	if err := SetZencoderHeaders(httpReq, account, zenModel); err != nil {
		return nil, err
	}

	// Anthropic特有请求头
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	// 添加模型配置的额外请求头
	ApplyModelExtraHeaders(httpReq, zenModel)

	// 只在非限速测试且调试模式下记录请求头
	if IsDebugMode() {
		// 检查请求体中的模型以判断是否为限速测试
		var reqCheck struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &reqCheck) == nil && !strings.Contains(reqCheck.Model, "test") {
			DebugLogRequestHeaders(ctx, "Anthropic", httpReq.Header)
		}
	}

	httpClient := newDirectHTTPClient(10 * time.Minute)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	// 不输出响应头调试信息以减少日志量

	// 如果是400错误，记录详细的请求信息
	if resp.StatusCode == 400 {
		// 读取错误响应内容
		errBody, _ := readUpstreamErrorBody(resp.Body)
		resp.Body.Close()

		// 检查是否是"prompt is too long"错误
		isPromptTooLongError := false
		// 检查是否是thinking格式错误（将在doRequest中处理并重试）
		isThinkingFormatError := false
		// 检查是否是thinking signature过期错误（将在doRequest中处理并重试）
		isThinkingSignatureError := false
		var errResp struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}

		if err := json.Unmarshal(errBody, &errResp); err == nil {
			errorMessage := strings.ToLower(errResp.Error.Message)
			if strings.Contains(errorMessage, "prompt is too long") {
				isPromptTooLongError = true
				// 对于prompt过长错误，只输出简单的错误信息
				logToContext(ctx, "anthropic_error status=400 type=%s", errResp.Error.Type)
			}
			// 检查是否是thinking格式错误
			if strings.Contains(errResp.Error.Message, "When `thinking` is enabled") ||
				strings.Contains(errResp.Error.Message, "Expected `thinking` or `redacted_thinking`") {
				isThinkingFormatError = true
				// 输出详细的thinking格式错误信息
				logToContext(ctx, "anthropic_thinking_format_error body_bytes=%d", len(errBody))
			}
			// 检查是否是thinking signature过期错误
			if strings.Contains(errResp.Error.Message, "Invalid `signature` in `thinking` block") {
				isThinkingSignatureError = true
				// 对于thinking signature过期错误，只输出简单信息，详细处理留给doRequest
			}
		}

		// 只在非调试模式且非已知可重试错误时才输出详细debug信息
		// thinking相关错误会在doRequest中处理，如果重试成功就不需要输出debug日志
		shouldOutputDetails := !isPromptTooLongError && !isThinkingFormatError && !isThinkingSignatureError
		if shouldOutputDetails {
			logToContext(ctx, "anthropic_error status=400 body_bytes=%d", len(errBody))
			// 只在调试模式下输出详细的请求信息
			if IsDebugMode() {
				logRequestDetails(ctx, "[Anthropic] 实际API", httpReq.Header, body)
			}
		} else if isThinkingSignatureError && IsDebugMode() {
			// thinking signature错误只在调试模式下输出简单信息
			logToContext(ctx, "anthropic_error status=400 body_bytes=%d", len(errBody))
			logRequestDetails(ctx, "[Anthropic] 实际API", httpReq.Header, body)
		}

		// 重新构建响应，因为body已经被读取
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
	}

	return resp, nil
}

// isThinkingFormatError 检查是否是thinking格式相关的错误
func (s *AnthropicService) isThinkingFormatError(errorBody string) bool {
	return strings.Contains(errorBody, "When `thinking` is enabled, a final `assistant` message must start with a thinking block") ||
		strings.Contains(errorBody, "Expected `thinking` or `redacted_thinking`") ||
		strings.Contains(errorBody, "To avoid this requirement, disable `thinking`")
}

// isThinkingSignatureError 检查是否是thinking signature过期错误
func (s *AnthropicService) isThinkingSignatureError(errorBody string) bool {
	return strings.Contains(errorBody, "Invalid `signature` in `thinking` block") ||
		strings.Contains(errorBody, "invalid_request_error") && strings.Contains(errorBody, "signature")
}

// isTemperatureError 检查是否是温度参数相关的错误
func (s *AnthropicService) isTemperatureError(errorBody string) bool {
	return strings.Contains(errorBody, "requires temperature=1.0") ||
		strings.Contains(errorBody, "Parallel Thinking' requires temperature")
}

// isParameterConflictError 检查是否是参数冲突错误
func (s *AnthropicService) isParameterConflictError(errorBody string) bool {
	return strings.Contains(errorBody, "`temperature` and `top_p` cannot both be specified")
}

// isClaudeOfficial429Error 检查是否是Claude官方的429限流错误
func (s *AnthropicService) isClaudeOfficial429Error(errorBody string) bool {
	// 尝试解析错误响应
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}

	// 如果能解析成功且符合Claude官方格式
	if err := json.Unmarshal([]byte(errorBody), &errResp); err == nil {
		// Claude官方错误特征：
		// 1. type = "error"
		// 2. error.type = "rate_limit_error"
		// 3. 错误消息包含anthropic.com或claude.com域名
		if errResp.Type == "error" &&
			errResp.Error.Type == "rate_limit_error" &&
			(strings.Contains(errResp.Error.Message, "anthropic.com") ||
				strings.Contains(errResp.Error.Message, "claude.com") ||
				strings.Contains(errResp.Error.Message, "docs.claude.com")) {
			return true
		}
	}

	// 检查是否是非Claude官方的错误格式（如Google API格式）
	var nonClaudeErr struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(errorBody), &nonClaudeErr); err == nil {
		// 非Claude官方错误特征：有code和status字段
		if nonClaudeErr.Error.Code == 429 &&
			nonClaudeErr.Error.Status == "RESOURCE_EXHAUSTED" {
			return false
		}
	}

	// 默认情况下，如果无法确定，保守处理：不返回原始响应
	return false
}

// classifyAndLog429Error 分类并记录429错误的简化日志
func (s *AnthropicService) classifyAndLog429Error(ctx context.Context, errorBody string, accountID uint, email string) {
	// 尝试解析Claude官方错误
	var claudeErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(errorBody), &claudeErr); err == nil {
		if claudeErr.Type == "error" && claudeErr.Error.Type == "rate_limit_error" {
			// Claude官方限流错误
			logToContext(ctx, "anthropic_rate_limit source=claude account_id=%d", accountID)
			return
		}
	}

	// 尝试解析GCP错误
	var gcpErr struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(errorBody), &gcpErr); err == nil {
		if gcpErr.Error.Code == 429 && gcpErr.Error.Status == "RESOURCE_EXHAUSTED" {
			// GCP限流错误
			logToContext(ctx, "anthropic_rate_limit source=gcp account_id=%d", accountID)
			return
		}
	}

	// 其他未识别的429错误
	logToContext(ctx, "anthropic_rate_limit source=unknown account_id=%d", accountID)
}

// MessagesProxy 直接代理请求和响应
func (s *AnthropicService) MessagesProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	var req struct {
		Model    string                 `json:"model"`
		Stream   bool                   `json:"stream"`
		Thinking map[string]interface{} `json:"thinking"`
	}
	// 忽略错误，Messages方法会再次解析
	_ = json.Unmarshal(body, &req)
	if zenModel, ok := model.GetZenModel(req.Model); ok && zenModel.ProviderID == "xai" {
		return NewOpenAIService().MessagesProxy(ctx, w, body)
	}

	resp, err := s.Messages(ctx, body, req.Stream)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 判断是否需要过滤thinking内容
	// 规则：如果用户调用的是非thinking版本，但平台强制开启了thinking，则需要过滤
	needsFiltering := false

	// 获取模型配置
	zenModel, exists := model.GetZenModel(req.Model)

	// Catalog thinking is hidden only when the caller did not explicitly ask
	// for it. Public model IDs do not use a "-thinking" suffix.
	if exists && zenModel.Parameters != nil && zenModel.Parameters.Thinking != nil {
		thinkingType := stringValue(req.Thinking["type"])
		explicitThinking := thinkingType == "enabled" || thinkingType == "adaptive"
		if enabled, ok := req.Thinking["enabled"].(bool); ok && enabled {
			explicitThinking = true
		}
		if _, ok := req.Thinking["enabled"].(map[string]interface{}); ok {
			explicitThinking = true
		}
		needsFiltering = !explicitThinking
	}

	if needsFiltering {
		if req.Stream {
			return s.streamFilteredResponse(w, resp)
		}
		return s.handleNonStreamFilteredResponse(w, resp)
	}

	return StreamResponse(w, resp)
}

func (s *AnthropicService) handleNonStreamFilteredResponse(w http.ResponseWriter, resp *http.Response) error {
	// 读取全部响应体
	bodyBytes, err := readCompatibilityResponseBody(resp.Body)
	if err != nil {
		return err
	}

	copyResponseHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)

	// 尝试解析响应
	var raw map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		w.Write(bodyBytes)
		return nil
	}

	// 过滤 content 中的 thinking block
	if content, ok := raw["content"].([]interface{}); ok {
		var newContent []interface{}
		for _, block := range content {
			if b, ok := block.(map[string]interface{}); ok {
				if typeStr, ok := b["type"].(string); ok && (typeStr == "thinking" || typeStr == "thought" || typeStr == "redacted_thinking") {
					continue
				}
			}
			newContent = append(newContent, block)
		}
		raw["content"] = newContent
	}

	return json.NewEncoder(w).Encode(raw)
}

// adjustTemperatureForModel 根据模型要求调整温度参数
func (s *AnthropicService) adjustTemperatureForModel(body []byte, modelID string) ([]byte, error) {
	// 获取模型配置
	zenModel, exists := model.GetZenModel(modelID)

	// 检查模型配置中是否有特定的温度要求
	if exists && zenModel.Parameters != nil && zenModel.Parameters.Temperature != nil {
		var request map[string]interface{}
		if json.Unmarshal(body, &request) == nil {
			if thinking, ok := request["thinking"].(map[string]interface{}); ok && stringValue(thinking["type"]) == "disabled" {
				return body, nil
			}
		}
		return s.forceTemperature(body, *zenModel.Parameters.Temperature)
	}

	return body, nil
}

// forceTemperature 强制设置温度参数
func (s *AnthropicService) forceTemperature(body []byte, temperature float64) ([]byte, error) {
	// 解析请求体
	var reqMap map[string]interface{}
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return body, nil // 如果解析失败，返回原始body
	}

	// 强制设置 temperature
	reqMap["temperature"] = temperature

	// 如果同时存在 top_p，移除它（某些模型不允许同时指定）
	delete(reqMap, "top_p")

	// 重新序列化
	return json.Marshal(reqMap)
}

// removeTopP 移除 top_p 参数，避免与 temperature 冲突
func (s *AnthropicService) removeTopP(body []byte) ([]byte, error) {
	// 解析请求体
	var reqMap map[string]interface{}
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return body, nil // 如果解析失败，返回原始body
	}

	// 移除 top_p 参数
	delete(reqMap, "top_p")

	// 重新序列化
	return json.Marshal(reqMap)
}

// ensureThinkingConfig 确保需要 thinking 的模型有正确的配置
func (s *AnthropicService) ensureThinkingConfig(body []byte, modelID string) ([]byte, error) {
	const defaultThinkingBudgetTokens = 4096

	// 获取模型配置
	zenModel, exists := model.GetZenModel(modelID)

	// 检查模型配置中是否包含thinking参数
	needsThinking := false
	thinkingType := "enabled"
	var modelBudgetTokens int
	if exists && zenModel.Parameters != nil && zenModel.Parameters.Thinking != nil {
		needsThinking = true
		if zenModel.Parameters.Thinking.Type != "" {
			thinkingType = zenModel.Parameters.Thinking.Type
		}
		modelBudgetTokens = zenModel.Parameters.Thinking.BudgetTokens
		if thinkingType == "enabled" && modelBudgetTokens == 0 {
			modelBudgetTokens = 4096 // 默认值
		}
	}

	// 解析请求体
	var reqMap map[string]interface{}
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return body, nil
	}

	existingThinking, hasThinking := reqMap["thinking"].(map[string]interface{})
	if !needsThinking {
		// Models such as MiniMax do not force thinking by default, but clients
		// may still explicitly enable it. Normalize that request instead of
		// forwarding an incomplete `type: enabled` object.
		if !hasThinking {
			return body, nil
		}

		thinkingType, _ := existingThinking["type"].(string)
		if thinkingType == "" {
			if enabled, ok := existingThinking["enabled"].(bool); ok {
				if !enabled {
					existingThinking["type"] = "disabled"
				} else {
					thinkingType = "enabled"
				}
			} else if enabledConfig, ok := existingThinking["enabled"].(map[string]interface{}); ok {
				thinkingType = "enabled"
				if budget, ok := enabledConfig["budget_tokens"]; ok {
					existingThinking["budget_tokens"] = budget
				}
			}
		}

		if thinkingType == "enabled" {
			budgetTokens := numberAsInt(existingThinking["budget_tokens"])
			if budgetTokens <= 0 {
				budgetTokens = defaultThinkingBudgetTokens
			}
			existingThinking["type"] = "enabled"
			existingThinking["budget_tokens"] = budgetTokens
			delete(existingThinking, "enabled")
			reqMap["temperature"] = 1.0
			delete(reqMap, "top_p")

			if maxTokens := numberAsInt(reqMap["max_tokens"]); maxTokens > 0 && maxTokens <= budgetTokens {
				reqMap["max_tokens"] = maxTokens + budgetTokens
			}
		} else if thinkingType == "adaptive" || thinkingType == "disabled" {
			existingThinking["type"] = thinkingType
			delete(existingThinking, "enabled")
			if thinkingType != "enabled" {
				delete(existingThinking, "budget_tokens")
			}
		}

		reqMap["thinking"] = existingThinking
		return json.Marshal(reqMap)
	}

	// 检查用户是否明确不想要thinking模式
	userDisablesThinking := false
	if hasThinking {
		if thinkingType, ok := existingThinking["type"].(string); ok && thinkingType == "disabled" {
			userDisablesThinking = true
		}
		if enabled, ok := existingThinking["enabled"].(bool); ok && !enabled {
			userDisablesThinking = true
		}
	} else {
		// The model catalog already determines whether this model uses thinking.
		// Do not infer it from a model-name suffix: catalog IDs use `-think`,
		// while provider model IDs do not have that suffix.
	}

	// An explicit disabled setting is already valid Anthropic input. Rewriting
	// assistant history changes conversation and tool semantics.
	if userDisablesThinking {
		existingThinking["type"] = "disabled"
		delete(existingThinking, "enabled")
		delete(existingThinking, "budget_tokens")
		reqMap["thinking"] = existingThinking
		modifiedBody, err := json.Marshal(reqMap)
		if err != nil {
			return body, err
		}
		return modifiedBody, nil
	}

	// 注意：即使有tool_choice，某些模型仍然需要thinking配置
	// 因此不再因为tool_choice的存在而跳过thinking配置

	// 检查请求体中是否已有thinking配置
	if existingThinking, ok := reqMap["thinking"].(map[string]interface{}); ok {
		// Match the gateway catalog: some Anthropic models use adaptive
		// thinking, while others use enabled + budget_tokens.
		// Some clients send the alternate {"enabled": {...}} shape. The
		// gateway may validate that branch before looking at `type`, so remove
		// it after applying the catalog's canonical Anthropic shape.
		delete(existingThinking, "enabled")
		existingThinking["type"] = thinkingType
		if thinkingType == "enabled" {
			existingThinking["budget_tokens"] = modelBudgetTokens
		} else {
			delete(existingThinking, "budget_tokens")
		}
		reqMap["thinking"] = existingThinking
	} else {
		thinkingConfig := map[string]interface{}{"type": thinkingType}
		if thinkingType == "enabled" {
			thinkingConfig["budget_tokens"] = modelBudgetTokens
		}
		reqMap["thinking"] = thinkingConfig
	}
	if thinkingType == "enabled" {
		if maxTokens := numberAsInt(reqMap["max_tokens"]); maxTokens > 0 && maxTokens <= modelBudgetTokens {
			reqMap["max_tokens"] = maxTokens + modelBudgetTokens
		}
	}

	// 当启用 thinking 时，必须设置 temperature = 1.0
	reqMap["temperature"] = 1.0
	// 移除 top_p 以避免冲突
	delete(reqMap, "top_p")

	// 注意：不再尝试为assistant消息添加thinking块，因为signature信息无法正确生成
	// 如果模型要求thinking模式但用户消息不符合格式，让API返回错误由上层处理

	// 重新序列化
	modifiedBody, err := json.Marshal(reqMap)
	if err != nil {
		return body, err
	}

	return modifiedBody, nil
}

// 已移除fixAssistantMessageForThinking函数，因为signature信息无法正确生成

// convertAssistantToUserMessage 将assistant消息转换为user消息，避免thinking格式要求
// 使用range循环逐个处理块，保留缓存信息，不合并消息
func (s *AnthropicService) convertAssistantToUserMessage(msgMap map[string]interface{}) error {
	content, ok := msgMap["content"]
	if !ok {
		return nil
	}

	// 将角色从assistant改为user
	msgMap["role"] = "user"

	switch c := content.(type) {
	case string:
		// 如果是字符串content，保持不变，只改角色
	case []interface{}:
		// 使用range循环逐个处理每个块，保留结构和缓存信息
		for i, block := range c {
			if blockMap, ok := block.(map[string]interface{}); ok {
				blockType, _ := blockMap["type"].(string)

				// 保留原有的缓存控制信息
				var cacheControl interface{}
				if cache, hasCacheControl := blockMap["cache_control"]; hasCacheControl {
					cacheControl = cache
				}

				switch blockType {
				case "thinking", "redacted_thinking":
					// 将thinking块转换为text块，保留缓存信息
					if thinkingText, ok := blockMap["thinking"].(string); ok {
						newBlock := map[string]interface{}{
							"type": "text",
							"text": "[thinking] " + thinkingText,
						}
						if cacheControl != nil {
							newBlock["cache_control"] = cacheControl
						}
						c[i] = newBlock
					}
				case "tool_use":
					// 将tool_use块转换为text描述，保留缓存信息
					toolName, _ := blockMap["name"].(string)
					toolId, _ := blockMap["id"].(string)
					newBlock := map[string]interface{}{
						"type": "text",
						"text": fmt.Sprintf("[tool_use] %s (ID: %s)", toolName, toolId),
					}
					if cacheControl != nil {
						newBlock["cache_control"] = cacheControl
					}
					c[i] = newBlock
				case "tool_result":
					// 将tool_result块转换为text描述，保留缓存信息
					toolUseId, _ := blockMap["tool_use_id"].(string)
					isError, _ := blockMap["is_error"].(bool)
					var resultText string
					if isError {
						resultText = fmt.Sprintf("[tool_error] (ID: %s)", toolUseId)
					} else {
						resultText = fmt.Sprintf("[tool_result] (ID: %s)", toolUseId)
					}
					newBlock := map[string]interface{}{
						"type": "text",
						"text": resultText,
					}
					if cacheControl != nil {
						newBlock["cache_control"] = cacheControl
					}
					c[i] = newBlock
				default:
					// text块和其他类型的块保持不变，包括缓存信息
					// 不需要修改，保持原样
				}
			}
			// 非map类型的块也保持不变
		}

		msgMap["content"] = c
	}

	return nil
}

// convertAssistantMessagesToUser 将请求体中的所有assistant消息转换为user消息
func (s *AnthropicService) convertAssistantMessagesToUser(body []byte) ([]byte, error) {
	// 解析请求体
	var reqMap map[string]interface{}
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return body, err
	}

	// 处理messages数组，同时处理工具调用关系
	if messages, ok := reqMap["messages"].([]interface{}); ok {
		for i, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				// 无论是assistant还是user消息，都要检查并转换工具相关块
				if role, ok := msgMap["role"].(string); ok {
					if role == "assistant" {
						// 转换assistant消息为user消息
						if err := s.convertAssistantToUserMessage(msgMap); err != nil {
							continue
						}
					} else if role == "user" {
						// 对于user消息，也要确保tool_result被正确处理
						if err := s.convertToolBlocksToText(msgMap); err != nil {
							continue
						}
					}
					messages[i] = msgMap
				}
			}
		}
		reqMap["messages"] = messages
	}

	// 重新序列化
	modifiedBody, err := json.Marshal(reqMap)
	if err != nil {
		return body, err
	}

	return modifiedBody, nil
}

// convertToolBlocksToText 将消息中的所有工具相关块转换为文本
func (s *AnthropicService) convertToolBlocksToText(msgMap map[string]interface{}) error {
	content, ok := msgMap["content"]
	if !ok {
		return nil
	}

	switch c := content.(type) {
	case []interface{}:
		// 使用range循环逐个处理每个块，将工具相关块转换为文本
		for i, block := range c {
			if blockMap, ok := block.(map[string]interface{}); ok {
				blockType, _ := blockMap["type"].(string)

				// 保留原有的缓存控制信息
				var cacheControl interface{}
				if cache, hasCacheControl := blockMap["cache_control"]; hasCacheControl {
					cacheControl = cache
				}

				switch blockType {
				case "tool_use":
					// 将tool_use块转换为text块
					toolName, _ := blockMap["name"].(string)
					toolId, _ := blockMap["id"].(string)
					newBlock := map[string]interface{}{
						"type": "text",
						"text": fmt.Sprintf("[tool_use] %s (ID: %s)", toolName, toolId),
					}
					if cacheControl != nil {
						newBlock["cache_control"] = cacheControl
					}
					c[i] = newBlock
				case "tool_result":
					// 将tool_result块转换为text块
					toolUseId, _ := blockMap["tool_use_id"].(string)
					isError, _ := blockMap["is_error"].(bool)
					var resultText string
					if isError {
						resultText = fmt.Sprintf("[tool_error] (ID: %s)", toolUseId)
					} else {
						resultText = fmt.Sprintf("[tool_result] (ID: %s)", toolUseId)
					}
					newBlock := map[string]interface{}{
						"type": "text",
						"text": resultText,
					}
					if cacheControl != nil {
						newBlock["cache_control"] = cacheControl
					}
					c[i] = newBlock
				default:
					// text块和其他类型的块保持不变
					// 不需要修改，保持原样
				}
			}
		}

		msgMap["content"] = c
	}

	return nil
}

// adjustParametersForModel 根据模型要求调整参数，避免冲突
func (s *AnthropicService) adjustParametersForModel(body []byte, modelID string) ([]byte, error) {
	// 对于 claude-opus-4-5-20251101 等模型，不能同时有 temperature 和 top_p
	modelsNoTopP := []string{
		"claude-opus-4-5-20251101",
		"claude-opus-4-1-20250805",
	}

	for _, model := range modelsNoTopP {
		if modelID == model {
			body, _ = s.removeTopP(body)
			break
		}
	}

	// 继续处理温度参数
	return s.adjustTemperatureForModel(body, modelID)
}

func (s *AnthropicService) streamFilteredResponse(w http.ResponseWriter, resp *http.Response) error {
	copyResponseHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, resp.Body)
		return err
	}

	reader := bufio.NewReader(resp.Body)
	isThinking := false // 标记当前是否处于 thinking block 中

	for {
		line, err := readSSELine(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" {
			fmt.Fprintf(w, "\n")
			flusher.Flush()
			continue
		}

		if strings.HasPrefix(trimmedLine, "event:") {
			// 读取下一行 data
			dataLine, err := readSSELine(reader)
			if err != nil {
				return err
			}

			// 解析 event 类型
			event := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "event:"))
			data := strings.TrimSpace(strings.TrimPrefix(dataLine, "data:"))

			var shouldFilter bool

			if event == "content_block_start" {
				var payload struct {
					ContentBlock struct {
						Type string `json:"type"`
					} `json:"content_block"`
				}
				if json.Unmarshal([]byte(data), &payload) == nil {
					if payload.ContentBlock.Type == "thinking" || payload.ContentBlock.Type == "thought" || payload.ContentBlock.Type == "redacted_thinking" {
						isThinking = true
						shouldFilter = true
					}

				}
			} else if event == "content_block_delta" {
				if isThinking {
					shouldFilter = true
				}
			} else if event == "content_block_stop" {
				if isThinking {
					shouldFilter = true
					isThinking = false
				}
			}

			if !shouldFilter {
				fmt.Fprint(w, line)     // event: ...
				fmt.Fprint(w, dataLine) // data: ...
				flusher.Flush()
			}
		} else {
			// 其他格式（如 ping），直接透传
			fmt.Fprint(w, line)
			flusher.Flush()
		}
	}
}
