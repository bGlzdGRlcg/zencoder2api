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

func (s *OpenAIService) chatCompletionsViaAnthropic(ctx context.Context, body []byte) (*http.Response, error) {
	converted, err := convertChatToAnthropicBody(body)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Chat Completions request: %w", err)
	}

	resp, err := NewAnthropicService().Messages(ctx, converted, false)
	if err != nil {
		return nil, err
	}

	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return resp, nil
	}
	if request.Stream {
		return wrapAnthropicStreamAsChat(resp, request.Model), nil
	}
	return convertAnthropicResponseToChat(resp, request.Model)
}

// responsesViaAnthropic adapts OpenAI Responses requests for providers that
// are exposed by the gateway through Anthropic's /v1/messages API.
func (s *OpenAIService) responsesViaAnthropic(ctx context.Context, w http.ResponseWriter, body []byte) error {
	chatBody, modelID, stream, err := convertResponsesToChat(body)
	if err != nil {
		return fmt.Errorf("failed to convert Responses request: %w", err)
	}

	resp, err := s.chatCompletionsViaAnthropic(ctx, chatBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}
	if stream {
		return streamChatAsResponses(w, resp, modelID)
	}

	chatResponse, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	converted, err := convertChatJSONToResponses(chatResponse, modelID)
	if err != nil {
		return err
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") ||
			strings.EqualFold(key, "Content-Type") ||
			strings.EqualFold(key, "Content-Disposition") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(converted)
	return err
}

func convertChatToAnthropicBody(body []byte) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	if messages, ok := raw["messages"].([]interface{}); ok {
		converted, system := convertChatMessagesToAnthropic(messages)
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
		}
	}

	if value, ok := raw["max_completion_tokens"]; ok {
		raw["max_tokens"] = value
		delete(raw, "max_completion_tokens")
	}
	if _, ok := raw["max_tokens"]; !ok {
		raw["max_tokens"] = 4096
	}
	if value, ok := raw["stop"]; ok {
		raw["stop_sequences"] = value
		delete(raw, "stop")
	}

	for _, key := range []string{
		"reasoning_effort",
		"verbosity",
		"service_tier",
		"stream_options",
		"parallel_tool_calls",
		"response_format",
		"functions",
		"function_call",
	} {
		delete(raw, key)
	}

	return json.Marshal(raw)
}

func convertChatMessagesToAnthropic(messages []interface{}) ([]interface{}, []string) {
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
			converted = append(converted, map[string]interface{}{
				"role": "user",
				"content": []interface{}{map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": callID,
					"content":     responsesFunctionValue(message["content"]),
				}},
			})
			continue
		}

		content := anthropicContentBlocks(message["content"])
		if role == "assistant" {
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

		if role != "assistant" {
			role = "user"
		}
		converted = append(converted, map[string]interface{}{
			"role":    role,
			"content": content,
		})
	}

	return converted, system
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
	body, err := io.ReadAll(resp.Body)
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
			case "thinking":
				if value, ok := block["thinking"].(string); ok {
					reasoning.WriteString(value)
				}
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
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason, _ := raw["stop_reason"].(string)
	switch finishReason {
	case "end_turn", "stop_sequence", "":
		finishReason = "stop"
	case "max_tokens":
		finishReason = "length"
	case "tool_use":
		finishReason = "tool_calls"
	}

	response := map[string]interface{}{
		"id":      raw["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelID,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
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
	}
	return json.Marshal(response)
}

func numberAsInt(value interface{}) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}

func wrapAnthropicStreamAsChat(resp *http.Response, modelID string) *http.Response {
	source := resp.Body
	reader, writer := io.Pipe()
	resp.Body = reader
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "text/event-stream")

	go func() {
		defer source.Close()
		err := convertAnthropicStream(source, writer, modelID)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return resp
}

func convertAnthropicStream(source io.Reader, destination *io.PipeWriter, modelID string) error {
	reader := bufio.NewReader(source)
	eventName := ""
	dataLines := make([]string, 0)
	messageID := ""
	created := time.Now().Unix()
	toolIndex := -1
	toolCount := 0
	started := false

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

	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		currentEvent := eventName
		eventName = ""
		if data == "[DONE]" {
			_, err := io.WriteString(destination, "data: [DONE]\n\n")
			return err
		}

		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		if currentEvent == "" {
			currentEvent, _ = payload["type"].(string)
		}
		switch currentEvent {
		case "message_start":
			if message, ok := payload["message"].(map[string]interface{}); ok {
				messageID, _ = message["id"].(string)
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
			}
		case "message_delta":
			delta, _ := payload["delta"].(map[string]interface{})
			stopReason, _ := delta["stop_reason"].(string)
			if stopReason != "" {
				return writeChunk(map[string]interface{}{}, anthropicStopReasonToChat(stopReason))
			}
		case "message_stop":
			_, err := io.WriteString(destination, "data: [DONE]\n\n")
			return err
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
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
				return processEvent()
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
	default:
		return "stop"
	}
}
