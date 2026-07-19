package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"zencoder-2api/internal/model"
)

const GeminiBaseURL = "https://api.zencoder.ai/gemini"

type GeminiService struct{}

func NewGeminiService() *GeminiService {
	return &GeminiService{}
}

// GenerateContent 处理generateContent请求
func (s *GeminiService) GenerateContent(ctx context.Context, modelName string, body []byte) (*http.Response, error) {
	ctx = ensureOperationID(ctx)
	// 检查模型是否存在于模型字典中
	_, exists := model.GetZenModel(modelName)
	if !exists {
		DebugLog(ctx, "[Gemini] 模型不存在: %s", modelName)
		return nil, unknownModelError(modelName)
	}

	DebugLogRequest(ctx, "Gemini", "generateContent", modelName)

	var lastErr error
	tried := make(map[uint]struct{})
	refreshedAfter401 := make(map[uint]struct{})
	attemptLimit := accountAttemptLimit()
	for i := 0; i < attemptLimit; i++ {
		account, err := GetNextAccountContext(ctx, tried)
		if err != nil {
			DebugLogRequestEnd(ctx, "Gemini", false, err)
			return nil, err
		}
		tried[account.ID] = struct{}{}
		DebugLogAccountSelected(ctx, "Gemini", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, modelName, body, false)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			MarkAccountFailure(account, 0, 0, err)
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		DebugLogResponseReceived(ctx, "Gemini", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Gemini", resp.Header)

		if resp.StatusCode >= 300 {
			// 读取错误响应内容用于日志
			errBody, _ := readUpstreamErrorBody(resp.Body)
			resp.Body.Close()
			DebugLogErrorResponse(ctx, "Gemini", resp.StatusCode, string(errBody))
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

			// 400和500错误直接返回，不进行账号错误计数
			if !shouldRetryUpstreamStatus(resp.StatusCode) {
				DebugLogRequestEnd(ctx, "Gemini", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			MarkAccountFailure(account, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), lastErr)
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, lastErr)
			continue
		}

		zenModel, exists := model.GetZenModel(modelName)
		if !exists {
			// 模型不存在，使用默认倍率
			UpdateAccountCreditsFromResponse(account, resp, 1.0)
		} else {
			// 使用统一的积分更新函数，自动处理响应头中的积分信息
			UpdateAccountCreditsFromResponse(account, resp, zenModel.Multiplier)
		}

		DebugLogRequestEnd(ctx, "Gemini", true, nil)
		MarkAccountHealthy(account)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Gemini", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// StreamGenerateContent 处理streamGenerateContent请求
func (s *GeminiService) StreamGenerateContent(ctx context.Context, modelName string, body []byte) (*http.Response, error) {
	ctx = ensureOperationID(ctx)
	// 检查模型是否存在于模型字典中
	_, exists := model.GetZenModel(modelName)
	if !exists {
		DebugLog(ctx, "[Gemini] 模型不存在: %s", modelName)
		return nil, unknownModelError(modelName)
	}

	DebugLogRequest(ctx, "Gemini", "streamGenerateContent", modelName)

	var lastErr error
	tried := make(map[uint]struct{})
	refreshedAfter401 := make(map[uint]struct{})
	attemptLimit := accountAttemptLimit()
	for i := 0; i < attemptLimit; i++ {
		account, err := GetNextAccountContext(ctx, tried)
		if err != nil {
			DebugLogRequestEnd(ctx, "Gemini", false, err)
			return nil, err
		}
		tried[account.ID] = struct{}{}
		DebugLogAccountSelected(ctx, "Gemini", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, modelName, body, true)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			MarkAccountFailure(account, 0, 0, err)
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		DebugLogResponseReceived(ctx, "Gemini", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Gemini", resp.Header)

		if resp.StatusCode >= 300 {
			// 读取错误响应内容用于日志
			errBody, _ := readUpstreamErrorBody(resp.Body)
			resp.Body.Close()
			DebugLogErrorResponse(ctx, "Gemini", resp.StatusCode, string(errBody))
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

			// 400和500错误直接返回，不进行账号错误计数
			if !shouldRetryUpstreamStatus(resp.StatusCode) {
				DebugLogRequestEnd(ctx, "Gemini", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			MarkAccountFailure(account, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), lastErr)
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, lastErr)
			continue
		}

		zenModel, exists := model.GetZenModel(modelName)
		multiplier := 1.0
		if exists {
			multiplier = zenModel.Multiplier
		}
		finalizeStreamingAccount(ctx, resp, account, multiplier, streamGemini)

		DebugLogRequestEnd(ctx, "Gemini", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Gemini", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (s *GeminiService) doRequest(ctx context.Context, account *model.Account, modelName string, body []byte, stream bool) (*http.Response, error) {
	zenModel, exists := model.GetZenModel(modelName)
	if !exists {
		return nil, unknownModelError(modelName)
	}
	httpClient := newDirectHTTPClient(10 * time.Minute)

	action := "generateContent"
	queryParam := ""
	if stream {
		action = "streamGenerateContent"
		queryParam = "?alt=sse"
	}
	// The local route uses the public model ID, while the gateway path uses the
	// actual model name from its catalog (the same split as the VSCode CLI).
	reqURL := fmt.Sprintf("%s/gemini/v1beta/models/%s:%s%s", zencoderGatewayBaseURL(), zenModel.Model, action, queryParam)
	DebugLogRequestSent(ctx, "Gemini", reqURL)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 设置Zencoder自定义请求头
	if err := SetZencoderHeaders(httpReq, account, zenModel); err != nil {
		return nil, err
	}

	// 流式请求禁用压缩，确保可以逐行读取
	if stream {
		httpReq.Header.Set("Accept-Encoding", "identity")
	}

	// 添加模型配置的额外请求头
	ApplyModelExtraHeaders(httpReq, zenModel)

	// 记录请求头用于调试
	DebugLogRequestHeaders(ctx, "Gemini", httpReq.Header)

	return httpClient.Do(httpReq)
}

// GenerateContentProxy 代理generateContent请求
func (s *GeminiService) GenerateContentProxy(ctx context.Context, w http.ResponseWriter, modelName string, body []byte) error {
	resp, err := s.GenerateContent(ctx, modelName, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return CopyResponse(w, resp)
}

// StreamGenerateContentProxy 代理streamGenerateContent请求
func (s *GeminiService) StreamGenerateContentProxy(ctx context.Context, w http.ResponseWriter, modelName string, body []byte) error {
	resp, err := s.StreamGenerateContent(ctx, modelName, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return StreamResponse(w, resp)
}
