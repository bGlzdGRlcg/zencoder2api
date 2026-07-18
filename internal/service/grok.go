package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	DebugLogRequest(ctx, "Grok", "/v1/chat/completions", req.Model)

	var lastErr error
	for i := 0; i < maxAccountAttempts; i++ {
		account, err := GetNextAccount()
		if err != nil {
			DebugLogRequestEnd(ctx, "Grok", false, err)
			return nil, err
		}
		DebugLogAccountSelected(ctx, "Grok", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, req.Model, body)
		if err != nil {
			lastErr = err
			DebugLogRetry(ctx, "Grok", i+1, account.ID, err)
			continue
		}

		DebugLogResponseReceived(ctx, "Grok", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Grok", resp.Header)

		// 总是输出重要的响应头信息
		if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
			resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
			resp.Header.Get("Zen-Request-Cost") != "" {
			log.Printf("[Grok] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
				resp.Header.Get("Zen-Pricing-Period-Limit"),
				resp.Header.Get("Zen-Pricing-Period-Cost"),
				resp.Header.Get("Zen-Request-Cost"))
		}

		if resp.StatusCode >= 400 {
			// 读取错误响应内容
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// 429 错误特殊处理 - 直接返回，不重试
			if resp.StatusCode == 429 {
				DebugLogErrorResponse(ctx, "Grok", resp.StatusCode, string(errBody))
				DebugLogRequestEnd(ctx, "Grok", false, ErrNoAvailableAccount)
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			DebugLogErrorResponse(ctx, "Grok", resp.StatusCode, string(errBody))

			// 400和500错误直接返回，不进行账号错误计数
			if resp.StatusCode == 400 || resp.StatusCode == 500 {
				DebugLogRequestEnd(ctx, "Grok", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			DebugLogRetry(ctx, "Grok", i+1, account.ID, lastErr)
			continue
		}

		zenModel := model.ResolveOpenAIModel(req.Model)
		UpdateAccountCreditsFromResponse(account, resp, zenModel.Multiplier)

		DebugLogRequestEnd(ctx, "Grok", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Grok", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (s *GrokService) doRequest(ctx context.Context, account *model.Account, modelID string, body []byte) (*http.Response, error) {
	zenModel := model.ResolveOpenAIModel(modelID)
	httpClient := newDirectHTTPClient(10 * time.Minute)

	// The public model ID is carried by zen-model-id. The gateway request body
	// must contain the provider's actual catalog model, just like the VSCode
	// CLI request.
	modifiedBody, err := prepareGatewayRequestBody(body, modelID, "/v1/chat/completions", zenModel)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	// 处理请求体，Grok Code 模型要求 temperature=0
	if strings.Contains(modelID, "grok-code") {
		modifiedBody, err = s.setTemperatureZero(modifiedBody)
		if err != nil {
			return nil, fmt.Errorf("invalid request body: %w", err)
		}
	}

	reqURL := GrokBaseURL + "/v1/chat/completions"
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
	if zenModel.Parameters != nil && zenModel.Parameters.ExtraHeaders != nil {
		for k, v := range zenModel.Parameters.ExtraHeaders {
			httpReq.Header.Set(k, v)
		}
	}

	// 记录请求头用于调试
	DebugLogRequestHeaders(ctx, "Grok", httpReq.Header)

	return httpClient.Do(httpReq)
}

// setTemperatureZero 设置 temperature=0
func (s *GrokService) setTemperatureZero(body []byte) ([]byte, error) {
	var reqMap map[string]interface{}
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return body, err
	}
	reqMap["temperature"] = 0
	return json.Marshal(reqMap)
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
