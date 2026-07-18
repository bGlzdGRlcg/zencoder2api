package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MessagesProxy accepts Anthropic Messages requests for OpenAI models. Cherry
// Studio can select the Anthropic SDK independently of the model family, so
// this adapter prevents Anthropic-only fields such as thinking from reaching
// the OpenAI gateway unchanged.
func (s *OpenAIService) MessagesProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	chatBody, modelID, stream, err := convertAnthropicMessagesToChat(body)
	if err != nil {
		return fmt.Errorf("failed to convert Anthropic Messages request: %w", err)
	}

	resp, err := s.ChatCompletions(ctx, chatBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}
	if stream {
		return streamChatAsAnthropic(w, resp, modelID)
	}

	chatResponse, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	converted, err := convertChatJSONToAnthropic(chatResponse, modelID)
	if err != nil {
		return err
	}
	copyCompatHeaders(w, resp.Header, "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(converted)
	return err
}

func convertAnthropicMessagesToChat(body []byte) ([]byte, string, bool, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false, err
	}
	removeUndefinedPlaceholders(raw)

	modelID, _ := raw["model"].(string)
	if modelID == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	messages := make([]interface{}, 0)
	if system := anthropicSystemText(raw["system"]); system != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": system})
	}
	if source, ok := raw["messages"].([]interface{}); ok {
		for _, item := range source {
			message, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := message["role"].(string)
			blocks := anthropicMessageBlocks(message["content"])
			textParts := make([]interface{}, 0)
			toolCalls := make([]interface{}, 0)
			toolResults := make([]interface{}, 0)
			for _, block := range blocks {
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					textParts = append(textParts, map[string]interface{}{"type": "text", "text": block["text"]})
				case "image":
					if imagePart, ok := anthropicImageToChatPart(block); ok {
						textParts = append(textParts, imagePart)
					}
				case "tool_use":
					arguments, _ := json.Marshal(block["input"])
					callID := stringValue(block["id"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   callID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      block["name"],
							"arguments": string(arguments),
						},
					})
				case "tool_result":
					toolResults = append(toolResults, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": stringValue(block["tool_use_id"]),
						"content":      anthropicSystemText(block["content"]),
					})
				}
			}

			if role == "assistant" {
				chatMessage := map[string]interface{}{"role": "assistant", "content": textParts}
				if len(toolCalls) > 0 {
					chatMessage["tool_calls"] = toolCalls
				}
				messages = append(messages, chatMessage)
			} else if len(textParts) > 0 {
				messages = append(messages, map[string]interface{}{"role": "user", "content": textParts})
			}
			messages = append(messages, toolResults...)
		}
	}
	if len(messages) == 0 {
		return nil, "", false, fmt.Errorf("messages is required")
	}

	chat := map[string]interface{}{
		"model":    modelID,
		"messages": messages,
		"stream":   boolValue(raw["stream"]),
	}
	if value, ok := raw["max_tokens"]; ok {
		chat["max_completion_tokens"] = value
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := raw[key]; ok {
			chat[key] = value
		}
	}
	if value, ok := raw["stop_sequences"]; ok {
		chat["stop"] = value
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		converted := make([]interface{}, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok || stringValue(tool["name"]) == "" {
				continue
			}
			converted = append(converted, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        tool["name"],
					"description": tool["description"],
					"parameters":  tool["input_schema"],
				},
			})
		}
		if len(converted) > 0 {
			chat["tools"] = converted
		}
	}
	if choice, ok := raw["tool_choice"].(map[string]interface{}); ok {
		switch stringValue(choice["type"]) {
		case "auto":
			chat["tool_choice"] = "auto"
		case "any":
			chat["tool_choice"] = "required"
		case "tool":
			chat["tool_choice"] = map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": choice["name"]},
			}
		}
	}

	encoded, err := json.Marshal(chat)
	return encoded, modelID, boolValue(raw["stream"]), err
}

func anthropicImageToChatPart(block map[string]interface{}) (map[string]interface{}, bool) {
	source, _ := block["source"].(map[string]interface{})
	if len(source) == 0 {
		return nil, false
	}

	var url string
	switch stringValue(source["type"]) {
	case "base64":
		data := stringValue(source["data"])
		if data == "" {
			return nil, false
		}
		mediaType := stringValue(source["media_type"])
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		url = "data:" + mediaType + ";base64," + data
	case "url":
		url = stringValue(source["url"])
	default:
		return nil, false
	}
	if url == "" {
		return nil, false
	}
	return map[string]interface{}{
		"type":      "image_url",
		"image_url": map[string]interface{}{"url": url},
	}, true
}

func anthropicMessageBlocks(value interface{}) []map[string]interface{} {
	if text, ok := value.(string); ok {
		return []map[string]interface{}{{"type": "text", "text": text}}
	}
	items, _ := value.([]interface{})
	blocks := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if block, ok := item.(map[string]interface{}); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func anthropicSystemText(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []string
	for _, block := range anthropicMessageBlocks(value) {
		if text := stringValue(block["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func convertChatJSONToAnthropic(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	choices, _ := raw["choices"].([]interface{})
	if len(choices) == 0 {
		return nil, fmt.Errorf("chat completions response has no choices")
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	content := make([]interface{}, 0)
	if text := stringValue(message["content"]); text != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": text})
	}
	if calls, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range calls {
			call, _ := item.(map[string]interface{})
			function, _ := call["function"].(map[string]interface{})
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    call["id"],
				"name":  function["name"],
				"input": decodeJSONValue(function["arguments"]),
			})
		}
	}
	stopReason := "end_turn"
	if stringValue(choice["finish_reason"]) == "tool_calls" {
		stopReason = "tool_use"
	} else if stringValue(choice["finish_reason"]) == "length" {
		stopReason = "max_tokens"
	}
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	if source, ok := raw["usage"].(map[string]interface{}); ok {
		usage["input_tokens"] = intValue(source["prompt_tokens"])
		usage["output_tokens"] = intValue(source["completion_tokens"])
	}
	return json.Marshal(map[string]interface{}{
		"id":            stringValue(raw["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         modelID,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	})
}

type anthropicStreamTool struct {
	id         string
	name       string
	blockIndex int
	started    bool
}

func streamChatAsAnthropic(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyCompatHeaders(w, resp.Header, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	textStarted := false
	text := strings.Builder{}
	tools := make(map[int]*anthropicStreamTool)
	toolOrder := make([]int, 0)
	finishReason := "end_turn"
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0}

	writeEvent := func(eventType string, value interface{}) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": messageID, "type": "message", "role": "assistant", "model": modelID,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": usage,
		},
	}); err != nil {
		return err
	}
	startText := func() error {
		if textStarted {
			return nil
		}
		textStarted = true
		return writeEvent("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if source, ok := chunk["usage"].(map[string]interface{}); ok {
			usage["input_tokens"] = intValue(source["prompt_tokens"])
			usage["output_tokens"] = intValue(source["completion_tokens"])
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			switch reason {
			case "tool_calls":
				finishReason = "tool_use"
			case "length":
				finishReason = "max_tokens"
			default:
				finishReason = "end_turn"
			}
		}
		delta, _ := choice["delta"].(map[string]interface{})
		if value := stringValue(delta["content"]); value != "" {
			if err := startText(); err != nil {
				return err
			}
			text.WriteString(value)
			if err := writeEvent("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]interface{}{"type": "text_delta", "text": value},
			}); err != nil {
				return err
			}
		}
		calls, _ := delta["tool_calls"].([]interface{})
		for _, item := range calls {
			call, _ := item.(map[string]interface{})
			index := intValue(call["index"])
			state := tools[index]
			if state == nil {
				blockIndex := len(toolOrder)
				if textStarted {
					blockIndex++
				}
				state = &anthropicStreamTool{id: fmt.Sprintf("toolu_%d", index), blockIndex: blockIndex}
				tools[index] = state
				toolOrder = append(toolOrder, index)
			}
			function, _ := call["function"].(map[string]interface{})
			if name := stringValue(function["name"]); name != "" {
				state.name = name
			}
			if call["id"] != nil && state.id == fmt.Sprintf("toolu_%d", index) {
				state.id = stringValue(call["id"])
			}
			if state.name != "" && !state.started {
				if err := writeEvent("content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": state.blockIndex,
					"content_block": map[string]interface{}{"type": "tool_use", "id": state.id, "name": state.name, "input": map[string]interface{}{}},
				}); err != nil {
					return err
				}
				state.started = true
			}
			if arguments := stringValue(function["arguments"]); arguments != "" {
				if err := writeEvent("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": state.blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": arguments},
				}); err != nil {
					return err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if textStarted {
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0}); err != nil {
			return err
		}
	}
	for _, index := range toolOrder {
		state := tools[index]
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": state.blockIndex}); err != nil {
			return err
		}
	}
	if err := writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": finishReason, "stop_sequence": nil}, "usage": usage,
	}); err != nil {
		return err
	}
	return writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
}

func copyCompatHeaders(w http.ResponseWriter, headers http.Header, contentType string) {
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Length") ||
			strings.EqualFold(key, "Content-Type") ||
			strings.EqualFold(key, "Content-Disposition") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Del("Content-Length")
}
