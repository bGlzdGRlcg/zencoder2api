package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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
	// 检查模型是否存在于模型字典中
	_, exists := model.GetZenModel(modelName)
	if !exists {
		DebugLog(ctx, "[Gemini] 模型不存在: %s", modelName)
		return nil, ErrNoAvailableAccount
	}

	DebugLogRequest(ctx, "Gemini", "generateContent", modelName)

	var lastErr error
	for i := 0; i < maxAccountAttempts; i++ {
		account, err := GetNextAccount()
		if err != nil {
			DebugLogRequestEnd(ctx, "Gemini", false, err)
			return nil, err
		}
		DebugLogAccountSelected(ctx, "Gemini", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, modelName, body, false)
		if err != nil {
			lastErr = err
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, err)
			continue
		}

		DebugLogResponseReceived(ctx, "Gemini", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Gemini", resp.Header)

		// 总是输出重要的响应头信息
		if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
			resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
			resp.Header.Get("Zen-Request-Cost") != "" {
			log.Printf("[Gemini] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
				resp.Header.Get("Zen-Pricing-Period-Limit"),
				resp.Header.Get("Zen-Pricing-Period-Cost"),
				resp.Header.Get("Zen-Request-Cost"))
		}

		if resp.StatusCode >= 400 {
			// 读取错误响应内容用于日志
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			DebugLogErrorResponse(ctx, "Gemini", resp.StatusCode, string(errBody))

			// 400和500错误直接返回，不进行账号错误计数
			if resp.StatusCode == 400 || resp.StatusCode == 500 {
				DebugLogRequestEnd(ctx, "Gemini", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = fmt.Errorf("API error: %d", resp.StatusCode)
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
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Gemini", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// StreamGenerateContent 处理streamGenerateContent请求
func (s *GeminiService) StreamGenerateContent(ctx context.Context, modelName string, body []byte) (*http.Response, error) {
	// 检查模型是否存在于模型字典中
	_, exists := model.GetZenModel(modelName)
	if !exists {
		DebugLog(ctx, "[Gemini] 模型不存在: %s", modelName)
		return nil, ErrNoAvailableAccount
	}

	DebugLogRequest(ctx, "Gemini", "streamGenerateContent", modelName)

	var lastErr error
	for i := 0; i < maxAccountAttempts; i++ {
		account, err := GetNextAccount()
		if err != nil {
			DebugLogRequestEnd(ctx, "Gemini", false, err)
			return nil, err
		}
		DebugLogAccountSelected(ctx, "Gemini", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, modelName, body, true)
		if err != nil {
			lastErr = err
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, err)
			continue
		}

		DebugLogResponseReceived(ctx, "Gemini", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "Gemini", resp.Header)

		// 总是输出重要的响应头信息
		if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
			resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
			resp.Header.Get("Zen-Request-Cost") != "" {
			log.Printf("[Gemini] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
				resp.Header.Get("Zen-Pricing-Period-Limit"),
				resp.Header.Get("Zen-Pricing-Period-Cost"),
				resp.Header.Get("Zen-Request-Cost"))
		}

		if resp.StatusCode >= 400 {
			// 读取错误响应内容用于日志
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			DebugLogErrorResponse(ctx, "Gemini", resp.StatusCode, string(errBody))

			// 400和500错误直接返回，不进行账号错误计数
			if resp.StatusCode == 400 || resp.StatusCode == 500 {
				DebugLogRequestEnd(ctx, "Gemini", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = fmt.Errorf("API error: %d", resp.StatusCode)
			DebugLogRetry(ctx, "Gemini", i+1, account.ID, lastErr)
			continue
		}

		zenModel, exists := model.GetZenModel(modelName)
		if !exists {
			// 模型不存在，使用默认倍率
			UseCredit(account, 1.0)
		} else {
			// 流式响应，暂时使用模型倍率（因为没有完整响应头）
			UseCredit(account, zenModel.Multiplier)
		}

		DebugLogRequestEnd(ctx, "Gemini", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "Gemini", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (s *GeminiService) doRequest(ctx context.Context, account *model.Account, modelName string, body []byte, stream bool) (*http.Response, error) {
	zenModel, exists := model.GetZenModel(modelName)
	if !exists {
		return nil, ErrNoAvailableAccount
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
	reqURL := fmt.Sprintf("%s/v1beta/models/%s:%s%s", GeminiBaseURL, zenModel.Model, action, queryParam)
	DebugLogRequestSent(ctx, "Gemini", reqURL)
	httpReq, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
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
	if zenModel.Parameters != nil && zenModel.Parameters.ExtraHeaders != nil {
		for k, v := range zenModel.Parameters.ExtraHeaders {
			httpReq.Header.Set(k, v)
		}
	}

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

	return StreamResponse(w, resp)
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
