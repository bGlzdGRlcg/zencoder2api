package service

import (
	"bufio"
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

const OpenAIBaseURL = "https://api.zencoder.ai/openai"

func openAICompatibleBaseURL(providerID string) string {
	if providerID == "" || providerID == "openai" {
		return OpenAIBaseURL
	}
	return "https://api.zencoder.ai/" + providerID
}

type OpenAIService struct{}

func NewOpenAIService() *OpenAIService {
	return &OpenAIService{}
}

// ChatCompletions 处理/v1/chat/completions请求
func (s *OpenAIService) ChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// The gateway catalog exposes Anthropic-compatible providers such as GLM
	// and MiniMax through /v1/messages. Accept Chat Completions for them too,
	// adapting both request and response formats at the proxy boundary.
	if isAnthropicCompatibleModel(req.Model) {
		return s.chatCompletionsViaAnthropic(ctx, body)
	}

	// The gateway can reject function tools on Chat Completions when reasoning
	// is enabled. The Responses API supports this combination, so transparently
	// adapt requests for reasoning-capable catalog models and restore the Chat
	// Completions response shape for third-party clients.
	if requiresResponsesForFunctionTools(req.Model, body) {
		return s.chatCompletionsViaResponses(ctx, req.Model, body)
	}

	DebugLogRequest(ctx, "OpenAI", "/v1/chat/completions", req.Model)

	var lastErr error
	for i := 0; i < maxAccountAttempts; i++ {
		account, err := GetNextAccount()
		if err != nil {
			DebugLogRequestEnd(ctx, "OpenAI", false, err)
			return nil, err
		}
		DebugLogAccountSelected(ctx, "OpenAI", account.ID, account.OAuthEmail)

		// Chat Completions and Responses are different APIs. Keep the incoming
		// Chat Completions body and call the matching gateway route.
		resp, err := s.doRequest(ctx, account, req.Model, "/v1/chat/completions", body)
		if err != nil {
			lastErr = err
			DebugLogRetry(ctx, "OpenAI", i+1, account.ID, err)
			continue
		}

		DebugLogResponseReceived(ctx, "OpenAI", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "OpenAI", resp.Header)

		// 总是输出重要的响应头信息
		if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
			resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
			resp.Header.Get("Zen-Request-Cost") != "" {
			log.Printf("[OpenAI] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
				resp.Header.Get("Zen-Pricing-Period-Limit"),
				resp.Header.Get("Zen-Pricing-Period-Cost"),
				resp.Header.Get("Zen-Request-Cost"))
		}

		if resp.StatusCode >= 400 {
			// 读取错误响应内容
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			DebugLogErrorResponse(ctx, "OpenAI", resp.StatusCode, string(errBody))

			// 400和500错误直接返回，不进行账号错误计数
			if resp.StatusCode == 400 || resp.StatusCode == 500 {
				DebugLogRequestEnd(ctx, "OpenAI", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			DebugLogRetry(ctx, "OpenAI", i+1, account.ID, lastErr)
			continue
		}

		zenModel := model.ResolveOpenAIModel(req.Model)
		UpdateAccountCreditsFromResponse(account, resp, zenModel.Multiplier)

		DebugLogRequestEnd(ctx, "OpenAI", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "OpenAI", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func requiresResponsesForFunctionTools(modelID string, body []byte) bool {
	zenModel := model.ResolveOpenAIModel(modelID)
	// Any catalog model with reasoning parameters can hit the gateway rule
	// that rejects function tools on Chat Completions. Use the Responses route
	// generically instead of maintaining a model-name denylist.
	if zenModel.Parameters == nil || zenModel.Parameters.Reasoning == nil {
		return false
	}

	var raw map[string]interface{}
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	return hasFunctionTools(raw)
}

func (s *OpenAIService) chatCompletionsViaResponses(ctx context.Context, modelID string, body []byte) (*http.Response, error) {
	responsesBody, err := s.convertChatToResponsesBody(body)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Chat Completions request: %w", err)
	}

	resp, err := s.Responses(ctx, responsesBody)
	if err != nil {
		return nil, err
	}

	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	if request.Stream {
		return wrapResponsesStreamAsChat(resp, modelID), nil
	}

	converted, err := convertResponsesResponseToChat(resp, modelID)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return converted, nil
}

func convertResponsesResponseToChat(resp *http.Response, modelID string) (*http.Response, error) {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}

	converted, err := convertResponsesJSONToChat(body, modelID)
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(converted))
	resp.ContentLength = int64(len(converted))
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

func convertResponsesJSONToChat(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	removeUndefinedPlaceholders(raw)

	content := ""
	toolCalls := make([]interface{}, 0)
	if output, ok := raw["output"].([]interface{}); ok {
		for _, item := range output {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch itemMap["type"] {
			case "message":
				if parts, ok := itemMap["content"].([]interface{}); ok {
					for _, part := range parts {
						partMap, ok := part.(map[string]interface{})
						if !ok {
							continue
						}
						if text, ok := partMap["text"].(string); ok {
							content += text
						}
					}
				}
			case "function_call":
				function := map[string]interface{}{
					"name":      itemMap["name"],
					"arguments": itemMap["arguments"],
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":       itemMap["call_id"],
					"type":     "function",
					"function": function,
				})
			}
		}
	}
	if content == "" {
		if outputText, ok := raw["output_text"].(string); ok {
			content = outputText
		}
	}

	message := map[string]interface{}{
		"role":    "assistant",
		"content": content,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if status, _ := raw["status"].(string); status == "incomplete" {
		finishReason = "length"
	}

	usage := map[string]interface{}{}
	if source, ok := raw["usage"].(map[string]interface{}); ok {
		usage["prompt_tokens"] = source["input_tokens"]
		usage["completion_tokens"] = source["output_tokens"]
		usage["total_tokens"] = source["total_tokens"]
	}

	id, _ := raw["id"].(string)
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	created := time.Now().Unix()
	if value, ok := raw["created_at"].(float64); ok {
		created = int64(value)
	}

	return json.Marshal(map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelID,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	})
}

func wrapResponsesStreamAsChat(resp *http.Response, modelID string) *http.Response {
	source := resp.Body
	reader, writer := io.Pipe()
	resp.Body = reader
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Del("Content-Length")
	go func() {
		defer source.Close()
		err := convertResponsesStreamToChat(source, writer, modelID)
		_ = writer.CloseWithError(err)
	}()
	return resp
}

func convertResponsesStreamToChat(source io.Reader, destination *io.PipeWriter, modelID string) error {
	reader := bufio.NewReader(source)
	operationID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	toolCallSeen := false
	toolIndexes := make(map[string]int)
	nextToolIndex := 0
	eventName := ""
	dataLines := make([]string, 0, 1)

	writeChunk := func(delta map[string]interface{}, finishReason interface{}) error {
		chunk := map[string]interface{}{
			"id":      operationID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelID,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(destination, "data: %s\n\n", encoded)
		return err
	}

	writeDone := func() error {
		_, err := io.WriteString(destination, "data: [DONE]\n\n")
		return err
	}

	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			return writeDone()
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil
		}
		if eventName == "" {
			eventName, _ = event["type"].(string)
		}

		if response, ok := event["response"].(map[string]interface{}); ok {
			if id, ok := response["id"].(string); ok && id != "" {
				operationID = id
			}
			if value, ok := response["created_at"].(float64); ok {
				created = int64(value)
			}
		}

		switch eventName {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			if delta != "" {
				return writeChunk(map[string]interface{}{"content": delta}, nil)
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]interface{})
			if itemType, _ := item["type"].(string); itemType == "function_call" {
				toolCallSeen = true
				itemID, _ := item["id"].(string)
				index := nextToolIndex
				nextToolIndex++
				if itemID != "" {
					toolIndexes[itemID] = index
				}
				return writeChunk(map[string]interface{}{
					"role": "assistant",
					"tool_calls": []interface{}{map[string]interface{}{
						"index": index,
						"id":    item["call_id"],
						"type":  "function",
						"function": map[string]interface{}{
							"name":      item["name"],
							"arguments": "",
						},
					}},
				}, nil)
			}
		case "response.function_call_arguments.delta":
			toolCallSeen = true
			itemID, _ := event["item_id"].(string)
			index, ok := toolIndexes[itemID]
			if !ok {
				index = 0
			}
			delta, _ := event["delta"].(string)
			return writeChunk(map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": index,
					"function": map[string]interface{}{
						"arguments": delta,
					},
				}},
			}, nil)
		case "response.completed", "response.failed", "response.incomplete":
			finishReason := "stop"
			if toolCallSeen {
				finishReason = "tool_calls"
			}
			if eventName != "response.completed" {
				finishReason = "length"
			}
			if err := writeChunk(map[string]interface{}{}, finishReason); err != nil {
				return err
			}
			return writeDone()
		}
		eventName = ""
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case trimmed == "":
			if err := processEvent(); err != nil {
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Responses 处理/v1/responses请求
func (s *OpenAIService) Responses(ctx context.Context, body []byte) (*http.Response, error) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	DebugLogRequest(ctx, "OpenAI", "/v1/responses", req.Model)

	var lastErr error
	for i := 0; i < maxAccountAttempts; i++ {
		account, err := GetNextAccount()
		if err != nil {
			DebugLogRequestEnd(ctx, "OpenAI", false, err)
			return nil, err
		}
		DebugLogAccountSelected(ctx, "OpenAI", account.ID, account.OAuthEmail)

		resp, err := s.doRequest(ctx, account, req.Model, "/v1/responses", body)
		if err != nil {
			lastErr = err
			DebugLogRetry(ctx, "OpenAI", i+1, account.ID, err)
			continue
		}

		DebugLogResponseReceived(ctx, "OpenAI", resp.StatusCode)
		DebugLogResponseHeaders(ctx, "OpenAI", resp.Header)

		// 总是输出重要的响应头信息
		if resp.Header.Get("Zen-Pricing-Period-Limit") != "" ||
			resp.Header.Get("Zen-Pricing-Period-Cost") != "" ||
			resp.Header.Get("Zen-Request-Cost") != "" {
			log.Printf("[OpenAI] 积分信息 - 周期限额: %s, 周期消耗: %s, 本次消耗: %s",
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
				DebugLogErrorResponse(ctx, "OpenAI", resp.StatusCode, string(errBody))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			DebugLogErrorResponse(ctx, "OpenAI", resp.StatusCode, string(errBody))

			// 400和500错误直接返回，不进行账号错误计数
			if resp.StatusCode == 400 || resp.StatusCode == 500 {
				DebugLogRequestEnd(ctx, "OpenAI", false, fmt.Errorf("API error: %d", resp.StatusCode))
				return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			}

			lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			DebugLogRetry(ctx, "OpenAI", i+1, account.ID, lastErr)
			continue
		}

		zenModel := model.ResolveOpenAIModel(req.Model)
		UpdateAccountCreditsFromResponse(account, resp, zenModel.Multiplier)

		DebugLogRequestEnd(ctx, "OpenAI", true, nil)
		return resp, nil
	}

	DebugLogRequestEnd(ctx, "OpenAI", false, lastErr)
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// convertChatToResponsesBody 将 Chat Completion 的请求体转换为 Responses API 的请求体
func (s *OpenAIService) convertChatToResponsesBody(body []byte) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	// Convert Chat Completions function tools to Responses function tools.
	// Responses expects name/description/parameters at the tool's top level.
	if tools, ok := raw["tools"].([]interface{}); ok {
		convertedTools := make([]interface{}, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			function, isFunction := tool["function"].(map[string]interface{})
			if toolType, _ := tool["type"].(string); isFunction && toolType == "function" {
				converted := map[string]interface{}{"type": "function"}
				for _, key := range []string{"name", "description", "parameters", "strict"} {
					if value, exists := function[key]; exists {
						converted[key] = value
					}
				}
				convertedTools = append(convertedTools, converted)
				continue
			}
			convertedTools = append(convertedTools, tool)
		}
		raw["tools"] = convertedTools
	}
	if functions, ok := raw["functions"].([]interface{}); ok {
		convertedTools, _ := raw["tools"].([]interface{})
		for _, item := range functions {
			function, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			converted := map[string]interface{}{"type": "function"}
			for _, key := range []string{"name", "description", "parameters"} {
				if value, exists := function[key]; exists {
					converted[key] = value
				}
			}
			convertedTools = append(convertedTools, converted)
		}
		if len(convertedTools) > 0 {
			raw["tools"] = convertedTools
		}
	}

	// 移除 /v1/responses API 不支持的参数
	delete(raw, "stream_options") // 不支持 stream_options.include_usage 等
	delete(raw, "function_call")  // 旧版函数调用参数
	delete(raw, "functions")      // 旧版函数定义参数
	if toolChoice, ok := raw["tool_choice"].(map[string]interface{}); ok {
		if function, ok := toolChoice["function"].(map[string]interface{}); ok {
			converted := map[string]interface{}{"type": "function"}
			if name, exists := function["name"]; exists {
				converted["name"] = name
			}
			raw["tool_choice"] = converted
		}
	}

	// 转换 token 限制参数
	// max_completion_tokens (新) / max_tokens (旧) -> max_output_tokens (Responses API)
	if val, ok := raw["max_completion_tokens"]; ok {
		raw["max_output_tokens"] = val
		delete(raw, "max_completion_tokens")
	} else if val, ok := raw["max_tokens"]; ok {
		raw["max_output_tokens"] = val
		delete(raw, "max_tokens")
	}

	// 检查是否有 messages 字段
	if messages, ok := raw["messages"].([]interface{}); ok {
		// Chat assistant tool_calls/tool messages are represented as
		// function_call/function_call_output items by Responses. Use the same
		// conversion for every reasoning model so model aliases do not change
		// the wire format.
		raw["input"] = convertChatMessagesToResponsesInput(messages)
		delete(raw, "messages")
	}

	return json.Marshal(raw)
}

func convertChatMessagesToResponsesInput(messages []interface{}) []interface{} {
	input := make([]interface{}, 0, len(messages))

	for messageIndex, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := message["role"].(string)
		switch role {
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				callID, _ = message["call_id"].(string)
			}
			if callID == "" {
				continue
			}
			input = append(input, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  responsesFunctionValue(message["content"]),
			})
		case "assistant":
			if hasChatMessageContent(message["content"]) {
				input = append(input, copyChatMessageWithoutToolCalls(message))
			}

			if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
				input = appendResponsesFunctionCalls(input, toolCalls, messageIndex)
			}
			if functionCall, ok := message["function_call"].(map[string]interface{}); ok {
				input = appendResponsesFunctionCalls(input, []interface{}{functionCall}, messageIndex)
			}
		default:
			input = append(input, copyChatMessageWithoutToolCalls(message))
		}
	}

	return input
}

func copyChatMessageWithoutToolCalls(message map[string]interface{}) map[string]interface{} {
	copy := make(map[string]interface{}, len(message))
	role, _ := message["role"].(string)
	for key, value := range message {
		if key == "tool_calls" || key == "function_call" || key == "tool_call_id" {
			continue
		}
		if key == "content" {
			value = convertChatMessageContent(value, role)
		}
		copy[key] = value
	}
	return copy
}

func convertChatMessageContent(content interface{}, role string) interface{} {
	parts, ok := content.([]interface{})
	if !ok {
		return content
	}

	converted := make([]interface{}, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		partType, _ := part["type"].(string)
		switch partType {
		case "text":
			textPart := map[string]interface{}{
				"type": responseTextPartType(role),
			}
			if text, exists := part["text"]; exists {
				textPart["text"] = text
			}
			converted = append(converted, textPart)
		case "image_url":
			imagePart := map[string]interface{}{"type": "input_image"}
			switch imageURL := part["image_url"].(type) {
			case string:
				imagePart["image_url"] = imageURL
			case map[string]interface{}:
				if url, exists := imageURL["url"]; exists {
					imagePart["image_url"] = url
				}
				if detail, exists := imageURL["detail"]; exists {
					imagePart["detail"] = detail
				}
			}
			converted = append(converted, imagePart)
		default:
			// Keep already Responses-compatible parts and unknown parts intact;
			// this avoids rewriting newer content types that the gateway supports.
			converted = append(converted, part)
		}
	}

	return converted
}

func responseTextPartType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

func hasChatMessageContent(content interface{}) bool {
	if content == nil {
		return false
	}
	if text, ok := content.(string); ok {
		return text != ""
	}
	if parts, ok := content.([]interface{}); ok {
		return len(parts) > 0
	}
	return true
}

func appendResponsesFunctionCalls(input []interface{}, toolCalls []interface{}, messageIndex int) []interface{} {
	for callIndex, item := range toolCalls {
		toolCall, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		function, ok := toolCall["function"].(map[string]interface{})
		if !ok {
			function = toolCall
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}

		callID, _ := toolCall["id"].(string)
		if callID == "" {
			callID, _ = toolCall["call_id"].(string)
		}
		if callID == "" {
			callID = fmt.Sprintf("call_%d_%d", messageIndex, callIndex)
		}

		input = append(input, map[string]interface{}{
			"type":      "function_call",
			"call_id":   callID,
			"name":      name,
			"arguments": responsesFunctionValue(function["arguments"]),
		})
	}
	return input
}

func responsesFunctionValue(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// prepareGatewayRequestBody mirrors the VSCode CLI's gateway request format.
// The public model ID is sent in zen-model-id, while the request body uses the
// provider's actual model ID from the gateway catalog.
func hasFunctionTools(raw map[string]interface{}) bool {
	if tools, ok := raw["tools"].([]interface{}); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if toolType, _ := tool["type"].(string); toolType == "function" {
				return true
			}
			if _, ok := tool["function"]; ok {
				return true
			}
		}
	}
	if functions, ok := raw["functions"].([]interface{}); ok {
		if len(functions) > 0 {
			return true
		}
	}
	if messages, ok := raw["messages"].([]interface{}); ok {
		for _, item := range messages {
			message, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, ok := message["tool_calls"]; ok {
				return true
			}
			if _, ok := message["function_call"]; ok {
				return true
			}
			if role, _ := message["role"].(string); role == "tool" || role == "function" {
				return true
			}
		}
	}
	return false
}

func prepareGatewayRequestBody(body []byte, modelID, path string, zenModel model.ZenModel) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	removeUndefinedPlaceholders(raw)
	// Cherry Studio's provider options use camelCase, while the upstream
	// OpenAI-compatible API only accepts service_tier. Undefined values can
	// also be serialized by clients, so never forward the internal alias.
	delete(raw, "serviceTier")
	_, isKnownModel := model.GetZenModel(modelID)
	isOpenAIProvider := zenModel.ProviderID == "" || zenModel.ProviderID == "openai"

	raw["model"] = zenModel.Model
	if path == "/v1/chat/completions" {
		if isOpenAIProvider {
			if _, ok := raw["max_completion_tokens"]; !ok {
				if value, ok := raw["max_tokens"]; ok {
					raw["max_completion_tokens"] = value
					delete(raw, "max_tokens")
				}
			}
		} else {
			// Fireworks and other OpenAI-compatible gateways use the legacy
			// max_tokens field. Keep their provider-native request shape.
			if _, ok := raw["max_tokens"]; !ok {
				if value, ok := raw["max_completion_tokens"]; ok {
					raw["max_tokens"] = value
				}
			}
			delete(raw, "max_completion_tokens")
		}

		if zenModel.Parameters != nil && zenModel.Parameters.Reasoning != nil {
			// The gateway's accepted effort values are model-specific. The
			// catalog value is the same value selected by the VSCode CLI, so
			// use it instead of forwarding an arbitrary third-party value such
			// as "none" or an effort unsupported by this model.
			reasoningEffort := zenModel.Parameters.Reasoning.Effort
			raw["reasoning_effort"] = reasoningEffort
		} else if isKnownModel {
			// Do not send reasoning controls to catalog models that do not
			// support reasoning; some of them reject the field outright.
			delete(raw, "reasoning_effort")
		}
		if zenModel.Parameters != nil && zenModel.Parameters.Temperature != nil {
			if _, exists := raw["temperature"]; !exists {
				raw["temperature"] = *zenModel.Parameters.Temperature
			}
		}
		// Chat Completions uses reasoning_effort; Responses uses reasoning.
		delete(raw, "reasoning")
		if isOpenAIProvider {
			if _, ok := raw["service_tier"]; !ok {
				raw["service_tier"] = "auto"
			}
		}
	} else if path == "/v1/responses" {
		if _, ok := raw["max_output_tokens"]; !ok {
			if value, ok := raw["max_completion_tokens"]; ok {
				raw["max_output_tokens"] = value
				delete(raw, "max_completion_tokens")
			} else if value, ok := raw["max_tokens"]; ok {
				raw["max_output_tokens"] = value
				delete(raw, "max_tokens")
			}
		}

		// reasoning_effort belongs to Chat Completions, not Responses.
		delete(raw, "reasoning_effort")
		if zenModel.Parameters != nil && zenModel.Parameters.Reasoning != nil {
			raw["reasoning"] = map[string]interface{}{
				"effort": zenModel.Parameters.Reasoning.Effort,
			}
		} else if isKnownModel {
			delete(raw, "reasoning")
		}
		// Some third-party clients still send the legacy top-level verbosity.
		// Responses requires it under text.verbosity.
		textConfig, _ := raw["text"].(map[string]interface{})
		if textConfig == nil {
			textConfig = make(map[string]interface{})
		}
		if value, ok := raw["verbosity"]; ok {
			if _, exists := textConfig["verbosity"]; !exists {
				textConfig["verbosity"] = value
			}
			delete(raw, "verbosity")
		}
		if zenModel.Parameters != nil && zenModel.Parameters.Text != nil {
			if _, ok := textConfig["verbosity"]; !ok {
				textConfig["verbosity"] = zenModel.Parameters.Text.Verbosity
			}
		}
		if len(textConfig) > 0 {
			raw["text"] = textConfig
		}
		if _, ok := raw["service_tier"]; !ok {
			raw["service_tier"] = "auto"
		}
	}

	_ = modelID // retained for call-site clarity and future model-specific rules
	return json.Marshal(raw)
}

func removeUndefinedPlaceholders(value interface{}) {
	switch current := value.(type) {
	case map[string]interface{}:
		for key, child := range current {
			if text, ok := child.(string); ok && text == "[undefined]" {
				delete(current, key)
				continue
			}
			removeUndefinedPlaceholders(child)
		}
	case []interface{}:
		for _, child := range current {
			removeUndefinedPlaceholders(child)
		}
	}
}

func (s *OpenAIService) doRequest(ctx context.Context, account *model.Account, modelID, path string, body []byte) (*http.Response, error) {
	zenModel := model.ResolveOpenAIModel(modelID)
	httpClient := newDirectHTTPClient(10 * time.Minute)

	modifiedBody, err := prepareGatewayRequestBody(body, modelID, path, zenModel)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}

	// 注意：已移除模型重定向逻辑，直接使用用户请求的模型名
	DebugLogActualModel(ctx, "OpenAI", modelID, modelID)

	reqURL := openAICompatibleBaseURL(zenModel.ProviderID) + path
	DebugLogRequestSent(ctx, "OpenAI", reqURL)

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
	DebugLogRequestHeaders(ctx, "OpenAI", httpReq.Header)

	return httpClient.Do(httpReq)
}

// ChatCompletionsProxy 代理chat completions请求
func (s *OpenAIService) ChatCompletionsProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	// 解析 model 和 stream 参数
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	// 忽略错误，因为ChatCompletions会再次解析并处理错误
	_ = json.Unmarshal(body, &req)

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

// ResponsesProxy 代理responses请求
func (s *OpenAIService) ResponsesProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if isAnthropicCompatibleModel(req.Model) {
		return s.responsesViaAnthropic(ctx, w, body)
	}

	resp, err := s.Responses(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if req.Stream {
		return StreamResponse(w, resp)
	}
	return CopyResponse(w, resp)
}
