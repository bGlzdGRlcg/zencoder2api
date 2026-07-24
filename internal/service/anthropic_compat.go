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

func isAnthropicCompatibleModel(modelID string) bool {
	providerID := model.ResolveOpenAIModel(modelID).ProviderID
	return providerID == "anthropic" || providerID == "glm" || providerID == "minimax"
}

func (s *OpenAIService) chatCompletionsViaAnthropic(ctx context.Context, body []byte, forceIncludeUsage bool) (*http.Response, error) {
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	converted, includeUsage, err := convertChatToAnthropicBody(body)
	if err != nil {
		return nil, openAICompatibilityError(err)
	}

	resp, err := NewAnthropicService().Messages(ctx, converted, request.Stream)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return resp, nil
	}
	if request.Stream {
		return wrapAnthropicStreamAsChat(resp, request.Model, includeUsage || forceIncludeUsage), nil
	}
	return convertAnthropicResponseToChat(resp, request.Model)
}

// responsesViaAnthropic adapts OpenAI Responses requests for providers that
// are exposed by the gateway through Anthropic's /v1/messages API.
func (s *OpenAIService) responsesViaAnthropic(ctx context.Context, w http.ResponseWriter, body []byte) error {
	var responsesRaw map[string]interface{}
	if err := json.Unmarshal(body, &responsesRaw); err != nil {
		return openAICompatibilityError(err)
	}
	if err := validateResponsesForAnthropic(responsesRaw); err != nil {
		return openAICompatibilityError(err)
	}
	chatBody, modelID, stream, err := convertResponsesToChat(body)
	if err != nil {
		return openAICompatibilityError(err)
	}

	resp, err := s.chatCompletionsViaAnthropic(ctx, chatBody, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := readUpstreamErrorBody(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}
	if stream {
		return streamChatAsResponses(w, resp, modelID)
	}

	chatResponse, err := readCompatibilityResponseBody(resp.Body)
	if err != nil {
		return err
	}

	converted, err := convertChatJSONToResponses(chatResponse, modelID)
	if err != nil {
		return openAICompatibilityError(err)
	}

	copyResponseHeaders(w.Header(), resp.Header, true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(converted)
	return err
}

func convertChatToAnthropicBody(body []byte) ([]byte, bool, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	removeUndefinedPlaceholders(raw)
	includeUsage, err := consumeChatStreamOptions(raw, "Anthropic Chat")
	if err != nil {
		return nil, false, err
	}
	consumeAnthropicReasoningEffort(raw)
	if err := validateChatForAnthropic(raw); err != nil {
		return nil, false, err
	}

	if messages, ok := raw["messages"].([]interface{}); ok {
		converted, system, err := convertChatMessagesToAnthropic(messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = converted
		if len(system) > 0 {
			raw["system"] = strings.Join(system, "\n\n")
		}
	}

	if tools, ok := raw["tools"].([]interface{}); ok {
		converted := make([]interface{}, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			function, ok := tool["function"].(map[string]interface{})
			if !ok {
				converted = append(converted, tool)
				continue
			}
			anthropicTool := map[string]interface{}{
				"name":         function["name"],
				"description":  function["description"],
				"input_schema": function["parameters"],
			}
			if anthropicTool["description"] == nil {
				delete(anthropicTool, "description")
			}
			if anthropicTool["input_schema"] == nil {
				anthropicTool["input_schema"] = map[string]interface{}{"type": "object"}
			}
			converted = append(converted, anthropicTool)
		}
		raw["tools"] = converted
	}

	if toolChoice, ok := raw["tool_choice"].(map[string]interface{}); ok {
		if function, ok := toolChoice["function"].(map[string]interface{}); ok {
			raw["tool_choice"] = map[string]interface{}{
				"type": "tool",
				"name": function["name"],
			}
		}
	} else if toolChoice, ok := raw["tool_choice"].(string); ok {
		switch toolChoice {
		case "auto":
			raw["tool_choice"] = map[string]interface{}{"type": "auto"}
		case "required":
			raw["tool_choice"] = map[string]interface{}{"type": "any"}
		case "none":
			delete(raw, "tool_choice")
			delete(raw, "tools")
		}
	}
	if parallel, ok := raw["parallel_tool_calls"].(bool); ok {
		if !parallel {
			choice, _ := raw["tool_choice"].(map[string]interface{})
			if choice == nil {
				choice = map[string]interface{}{"type": "auto"}
			}
			choice["disable_parallel_tool_use"] = true
			raw["tool_choice"] = choice
		}
		delete(raw, "parallel_tool_calls")
	}

	if value, ok := raw["max_completion_tokens"]; ok {
		raw["max_tokens"] = value
		delete(raw, "max_completion_tokens")
	}
	if _, ok := raw["max_tokens"]; !ok {
		raw["max_tokens"] = 4096
	}
	if value, ok := raw["stop"]; ok {
		if stop, ok := value.(string); ok {
			raw["stop_sequences"] = []string{stop}
		} else {
			raw["stop_sequences"] = value
		}
		delete(raw, "stop")
	}

	converted, err := json.Marshal(raw)
	return converted, includeUsage, err
}

func consumeAnthropicReasoningEffort(raw map[string]interface{}) {
	value, exists := raw["reasoning_effort"]
	delete(raw, "reasoning_effort")
	if !exists || raw["thinking"] != nil {
		return
	}

	effort := strings.ToLower(strings.TrimSpace(stringValue(value)))
	if effort == "none" {
		raw["thinking"] = map[string]interface{}{"type": "disabled"}
		return
	}

	budget := 4096
	switch effort {
	case "minimal":
		budget = 1024
	case "low":
		budget = 2048
	case "high":
		budget = 8192
	case "xhigh":
		budget = 16384
	}
	raw["thinking"] = map[string]interface{}{
		"type":          "enabled",
		"budget_tokens": budget,
	}
}

func convertChatMessagesToAnthropic(messages []interface{}) ([]interface{}, []string, error) {
	converted := make([]interface{}, 0, len(messages))
	system := make([]string, 0)

	for _, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := message["role"].(string)

		if role == "system" {
			if text := anthropicContentText(message["content"]); text != "" {
				system = append(system, text)
			}
			continue
		}

		if role == "tool" || role == "function" {
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				callID, _ = message["call_id"].(string)
			}
			if callID == "" {
				callID, _ = message["name"].(string)
			}
			toolResult := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     responsesFunctionValue(message["content"]),
			}
			if isError, ok := message["is_error"].(bool); ok {
				toolResult["is_error"] = isError
			}
			converted = appendAnthropicMessage(converted, map[string]interface{}{
				"role":    "user",
				"content": []interface{}{toolResult},
			})
			continue
		}

		content := anthropicContentBlocks(message["content"])
		if role == "assistant" {
			reasoning, err := anthropicReasoningBlocks(message)
			if err != nil {
				return nil, nil, err
			}
			if len(reasoning) > 0 {
				content = append(reasoning, content...)
			}
			if calls, ok := message["tool_calls"].([]interface{}); ok {
				content = appendAnthropicToolUseBlocks(content, calls)
			}
			if functionCall, ok := message["function_call"].(map[string]interface{}); ok {
				content = appendAnthropicToolUseBlocks(content, []interface{}{functionCall})
			}
		}
		if len(content) == 0 {
			continue
		}

		if role == "developer" || role == "system" {
			if text := anthropicContentText(message["content"]); text != "" {
				system = append(system, text)
			}
			continue
		}
		if role != "assistant" {
			role = "user"
		}
		converted = appendAnthropicMessage(converted, map[string]interface{}{
			"role":    role,
			"content": content,
		})
	}

	return converted, system, nil
}

func appendAnthropicMessage(messages []interface{}, message map[string]interface{}) []interface{} {
	if len(messages) == 0 {
		return append(messages, message)
	}
	previous, ok := messages[len(messages)-1].(map[string]interface{})
	if !ok || previous["role"] != message["role"] {
		return append(messages, message)
	}
	previousContent, _ := previous["content"].([]interface{})
	content, _ := message["content"].([]interface{})
	previous["content"] = append(previousContent, content...)
	return messages
}

func anthropicReasoningBlocks(message map[string]interface{}) ([]interface{}, error) {
	if details, ok := message["reasoning_details"].([]interface{}); ok {
		blocks := make([]interface{}, 0, len(details))
		for index, item := range details {
			block, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("reasoning_details[%d] must be an object", index)
			}
			switch stringValue(block["type"]) {
			case "thinking", "redacted_thinking":
				blocks = append(blocks, block)
			default:
				return nil, fmt.Errorf("reasoning_details[%d] type %q cannot be represented by Anthropic", index, stringValue(block["type"]))
			}
		}
		return blocks, nil
	}
	reasoning := stringValue(message["reasoning_content"])
	if reasoning == "" {
		return nil, nil
	}
	signature := stringValue(message["reasoning_signature"])
	if signature == "" {
		signature = geminiThoughtSignatureBypass
	}
	return []interface{}{map[string]interface{}{
		"type": "thinking", "thinking": reasoning, "signature": signature,
	}}, nil
}

func anthropicContentBlocks(content interface{}) []interface{} {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": text}}
	}

	parts, ok := content.([]interface{})
	if !ok {
		return nil
	}
	converted := make([]interface{}, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch partType, _ := part["type"].(string); partType {
		case "text", "input_text":
			converted = append(converted, map[string]interface{}{
				"type": "text",
				"text": part["text"],
			})
		case "image_url":
			converted = append(converted, anthropicImageBlock(part))
		case "image", "document":
			converted = append(converted, part)
		}
	}
	return converted
}

func anthropicImageBlock(part map[string]interface{}) map[string]interface{} {
	url := ""
	detail := interface{}(nil)
	switch imageURL := part["image_url"].(type) {
	case string:
		url = imageURL
	case map[string]interface{}:
		url, _ = imageURL["url"].(string)
		detail = imageURL["detail"]
	}
	_ = detail

	if strings.HasPrefix(url, "data:") {
		parts := strings.SplitN(url, ",", 2)
		if len(parts) == 2 {
			mediaType := "application/octet-stream"
			if semi := strings.Index(parts[0], ";"); semi > len("data:") {
				mediaType = parts[0][len("data:"):semi]
			}
			return map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type":       "base64",
					"media_type": mediaType,
					"data":       parts[1],
				},
			}
		}
	}
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type": "url",
			"url":  url,
		},
	}
}

func appendAnthropicToolUseBlocks(content []interface{}, calls []interface{}) []interface{} {
	for index, item := range calls {
		call, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]interface{})
		if !ok {
			function = call
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		id, _ := call["id"].(string)
		if id == "" {
			id, _ = call["call_id"].(string)
		}
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		content = append(content, map[string]interface{}{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": decodeJSONValue(function["arguments"]),
		})
	}
	return content
}

func decodeJSONValue(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		var decoded interface{}
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func anthropicContentText(content interface{}) string {
	if text, ok := content.(string); ok {
		return text
	}
	var texts []string
	for _, block := range anthropicContentBlocks(content) {
		if blockMap, ok := block.(map[string]interface{}); ok {
			if text, ok := blockMap["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func convertAnthropicResponseToChat(resp *http.Response, modelID string) (*http.Response, error) {
	body, err := readCompatibilityResponseBody(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	converted, err := convertAnthropicJSONToChat(body, modelID)
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(converted))
	resp.ContentLength = int64(len(converted))
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

func convertAnthropicJSONToChat(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	text := strings.Builder{}
	reasoning := strings.Builder{}
	reasoningDetails := make([]interface{}, 0)
	annotations := make([]interface{}, 0)
	toolCalls := make([]interface{}, 0)
	if content, ok := raw["content"].([]interface{}); ok {
		for _, item := range content {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if value, ok := block["text"].(string); ok {
					text.WriteString(value)
				}
				if citations, ok := block["citations"].([]interface{}); ok {
					annotations = append(annotations, citations...)
				}
			case "thinking":
				if value, ok := block["thinking"].(string); ok {
					reasoning.WriteString(value)
				}
				reasoningDetails = append(reasoningDetails, block)
			case "redacted_thinking":
				reasoningDetails = append(reasoningDetails, block)
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				arguments, _ := json.Marshal(block["input"])
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": string(arguments),
					},
				})
			default:
				return nil, fmt.Errorf("anthropic response content type %q cannot be represented by Chat Completions", stringValue(block["type"]))
			}
		}
	}

	message := map[string]interface{}{
		"role":    "assistant",
		"content": text.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(reasoningDetails) > 0 {
		message["reasoning_details"] = reasoningDetails
		for _, item := range reasoningDetails {
			if block, ok := item.(map[string]interface{}); ok && stringValue(block["signature"]) != "" {
				message["reasoning_signature"] = block["signature"]
				break
			}
		}
	}
	if len(annotations) > 0 {
		message["annotations"] = annotations
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason, _ := raw["stop_reason"].(string)
	finishReason = anthropicStopReasonToChat(finishReason)

	response := map[string]interface{}{
		"id":      raw["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelID,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
			"stop_sequence": raw["stop_sequence"],
		}},
	}
	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		inputTokens := numberAsInt(usage["input_tokens"])
		outputTokens := numberAsInt(usage["output_tokens"])
		response["usage"] = map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
		if cached := numberAsInt(usage["cache_read_input_tokens"]); cached > 0 {
			response["usage"].(map[string]interface{})["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		}
	}
	return json.Marshal(response)
}

func numberAsInt(value interface{}) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}

func wrapAnthropicStreamAsChat(resp *http.Response, modelID string, includeUsage bool) *http.Response {
	source := resp.Body
	reader, writer := io.Pipe()
	resp.Body = reader
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "text/event-stream")

	go func() {
		defer source.Close()
		err := convertAnthropicStream(source, writer, modelID, includeUsage)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return resp
}

func convertAnthropicStream(source io.Reader, destination *io.PipeWriter, modelID string, includeUsage bool) error {
	reader := bufio.NewReader(source)
	eventName := ""
	dataLines := make([]string, 0)
	messageID := ""
	created := time.Now().Unix()
	toolIndex := -1
	toolCount := 0
	started := false
	terminalSeen := false
	usage := map[string]interface{}{}
	usageSeen := false

	writeChunk := func(delta map[string]interface{}, finishReason interface{}) error {
		if !started {
			delta["role"] = "assistant"
			started = true
		}
		payload := map[string]interface{}{
			"id":      messageID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelID,
			"choices": []interface{}{map[string]interface{}{
				"index": 0,
				"delta": delta,
			}},
		}
		choice := payload["choices"].([]interface{})[0].(map[string]interface{})
		if finishReason != nil {
			choice["finish_reason"] = finishReason
		} else {
			choice["finish_reason"] = nil
		}
		encoded, _ := json.Marshal(payload)
		_, err := fmt.Fprintf(destination, "data: %s\n\n", encoded)
		return err
	}
	writeError := func(message string) error {
		encoded, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": message, "type": "upstream_protocol_error"}})
		if _, err := fmt.Fprintf(destination, "data: %s\n\ndata: [DONE]\n\n", encoded); err != nil {
			return err
		}
		return nil
	}
	writeUsage := func() error {
		if !usageSeen {
			return nil
		}
		promptTokens := numberAsInt(usage["input_tokens"])
		completionTokens := numberAsInt(usage["output_tokens"])
		chatUsage := map[string]interface{}{
			"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
			"total_tokens": promptTokens + completionTokens,
		}
		if cached := numberAsInt(usage["cache_read_input_tokens"]); cached > 0 {
			chatUsage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		}
		encoded, err := json.Marshal(map[string]interface{}{
			"id": messageID, "object": "chat.completion.chunk", "created": created,
			"model": modelID, "choices": []interface{}{}, "usage": chatUsage,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(destination, "data: %s\n\n", encoded)
		return err
	}

	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		currentEvent := eventName
		eventName = ""
		if data == "[DONE]" {
			return nil
		}

		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return fmt.Errorf("invalid upstream anthropic SSE event: %w", err)
		}
		if currentEvent == "" {
			currentEvent, _ = payload["type"].(string)
		}
		switch currentEvent {
		case "message_start":
			if message, ok := payload["message"].(map[string]interface{}); ok {
				messageID, _ = message["id"].(string)
				if initialUsage, ok := message["usage"].(map[string]interface{}); ok {
					for key, value := range initialUsage {
						usage[key] = value
					}
					usageSeen = true
				}
			}
			return writeChunk(map[string]interface{}{}, nil)
		case "content_block_start":
			block, _ := payload["content_block"].(map[string]interface{})
			blockType, _ := block["type"].(string)
			if blockType == "tool_use" {
				toolIndex = toolCount
				toolCount++
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				return writeChunk(map[string]interface{}{
					"tool_calls": []interface{}{map[string]interface{}{
						"index": toolIndex,
						"id":    id,
						"type":  "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": "",
						},
					}},
				}, nil)
			}
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]interface{})
			deltaType, _ := delta["type"].(string)
			switch deltaType {
			case "text_delta":
				return writeChunk(map[string]interface{}{"content": delta["text"]}, nil)
			case "input_json_delta":
				return writeChunk(map[string]interface{}{
					"tool_calls": []interface{}{map[string]interface{}{
						"index": toolIndex,
						"function": map[string]interface{}{
							"arguments": delta["partial_json"],
						},
					}},
				}, nil)
			case "thinking_delta":
				return writeChunk(map[string]interface{}{"reasoning_content": delta["thinking"]}, nil)
			case "signature_delta":
				return writeChunk(map[string]interface{}{"reasoning_signature": delta["signature"]}, nil)
			}
		case "message_delta":
			if deltaUsage, ok := payload["usage"].(map[string]interface{}); ok {
				for key, value := range deltaUsage {
					usage[key] = value
				}
				usageSeen = true
			}
			delta, _ := payload["delta"].(map[string]interface{})
			stopReason, _ := delta["stop_reason"].(string)
			if stopReason != "" {
				return writeChunk(map[string]interface{}{}, anthropicStopReasonToChat(stopReason))
			}
		case "message_stop":
			terminalSeen = true
			if includeUsage {
				if err := writeUsage(); err != nil {
					return err
				}
			}
			_, err := io.WriteString(destination, "data: [DONE]\n\n")
			return err
		}
		return nil
	}

	for {
		line, err := readSSELine(reader)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		} else if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		} else if trimmed == "" {
			if eventErr := processEvent(); eventErr != nil {
				return eventErr
			}
		}
		if err != nil {
			if err == io.EOF {
				if eventErr := processEvent(); eventErr != nil {
					return eventErr
				}
				if !terminalSeen {
					return writeError("upstream Anthropic stream ended without a terminal event")
				}
				return nil
			}
			return err
		}
	}
}

func anthropicStopReasonToChat(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	case "model_context_window_exceeded":
		return "length"
	default:
		return "stop"
	}
}
