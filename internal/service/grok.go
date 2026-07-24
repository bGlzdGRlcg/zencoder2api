package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"zencoder-2api/internal/model"
)

const GrokBaseURL = "https://api.zencoder.ai/xai"

type GrokService struct{}

func NewGrokService() *GrokService {
	return &GrokService{}
}

// ChatCompletions 处理/v1/chat/completions请求
func (s *GrokService) ChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	ctx = ensureOperationID(ctx)
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	zenModel, known := model.GetZenModel(req.Model)
	if !known || zenModel.ProviderID != "xai" {
		return nil, unknownModelError(req.Model)
	}
	// The XAI Chat endpoint currently fails its internal Responses adapter for
	// reasoning requests with function tools. Its native Responses endpoint
	// supports the same request, so adapt at our boundary and restore the Chat
	// response shape for callers.
	if requiresResponsesForFunctionTools(req.Model, body) {
		return NewOpenAIService().chatCompletionsViaResponses(ctx, req.Model, body)
	}

	DebugLogRequest(ctx, "Grok", "/v1/chat/completions", req.Model)

	var lastErr error
	tried := make(map[uint]struct{})
	refreshedAfter401 := make(map[uint]struct{})
	attemptLimit := accountAttemptLimit()
	for i := 0; i < attemptLimit; i++ {
		account, err := GetNextAccountContext(ctx, tried)
		if err != nil {
			DebugLogRequestEnd(ctx, "Grok", false, err)
			return nil, err
		}
		tried[account.ID] = struct{}{}
		DebugLogAccountSelected(ctx, "Grok", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, req.Model, body)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			MarkAccountFailure(account, 0, 0, err)
			DebugLogRetry(ctx, "Grok", i+1, account.ID, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		DebugLogResponseReceived(ctx, "Grok", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Grok", resp.Header)

		if resp.StatusCode >= 300 {
			// 读取错误响应内容
			errBody, _ := readUpstreamErrorBody(resp.Body)
			resp.Body.Close()

			DebugLogErrorResponse(ctx, "Grok", resp.StatusCode, string(errBody))
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
				DebugLogRequestEnd(ctx, "Grok", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			MarkAccountFailure(account, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), lastErr)
			DebugLogRetry(ctx, "Grok", i+1, account.ID, lastErr)
			continue
		}

		zenModel := model.ResolveOpenAIModel(req.Model)
		if req.Stream {
			finalizeStreamingAccount(ctx, resp, account, zenModel.Multiplier, streamOpenAIChat)
		} else {
			UpdateAccountCreditsFromResponse(account, resp, zenModel.Multiplier)
			MarkAccountHealthy(account)
		}

		DebugLogRequestEnd(ctx, "Grok", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Grok", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (s *GrokService) doRequest(ctx context.Context, account *model.Account, modelID string, body []byte) (*http.Response, error) {
	zenModel, known := model.GetZenModel(modelID)
	if !known || zenModel.ProviderID != "xai" {
		return nil, unknownModelError(modelID)
	}
	httpClient := newDirectHTTPClient(10 * time.Minute)

	// The public model ID is carried by zen-model-id. The gateway request body
	// must contain the provider's actual catalog model, just like the VSCode
	// CLI request.
	modifiedBody, err := prepareGatewayRequestBody(body, modelID, "/v1/chat/completions", zenModel)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	reqURL := zencoderGatewayBaseURL() + "/xai/v1/chat/completions"
	DebugLogRequestSent(ctx, "Grok", reqURL)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(modifiedBody))
	if err != nil {
		return nil, err
	}

	// 设置Zencoder自定义请求头
	if err := SetZencoderHeaders(httpReq, account, zenModel); err != nil {
		return nil, err
	}

	// 添加模型配置的额外请求头
	ApplyModelExtraHeaders(httpReq, zenModel)

	// 记录请求头用于调试
	DebugLogRequestHeaders(ctx, "Grok", httpReq.Header)

	return httpClient.Do(httpReq)
}

// ChatCompletionsProxy 代理chat completions请求
func (s *GrokService) ChatCompletionsProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	resp, err := s.ChatCompletions(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if req.Stream {
		return StreamResponse(w, resp)
	}
	return CopyResponse(w, resp)
}
