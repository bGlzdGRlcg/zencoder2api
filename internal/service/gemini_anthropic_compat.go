package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MessagesProxy accepts Anthropic Messages requests for Gemini models. Some
// clients choose their wire protocol separately from the selected model, while
// Gemini itself only accepts its native generateContent protocol.
func (s *GeminiService) MessagesProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	chatBody, modelID, stream, err := convertAnthropicMessagesToChat(body)
	if err != nil {
		return &UpstreamError{StatusCode: http.StatusBadRequest, Body: []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, err.Error()))}
	}

	geminiModelID, _, nativeBody, err := convertOpenAIChatToGemini(chatBody)
	if err != nil {
		return &UpstreamError{StatusCode: http.StatusBadRequest, Body: []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, err.Error()))}
	}
	if stream {
		resp, err := s.StreamGenerateContent(ctx, geminiModelID, nativeBody)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= http.StatusBadRequest {
			errBody, _ := readUpstreamErrorBody(resp.Body)
			return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
		}
		return streamGeminiAsAnthropic(w, resp, modelID)
	}

	resp, err := s.GenerateContent(ctx, geminiModelID, nativeBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := readUpstreamErrorBody(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}
	nativeResponse, err := readCompatibilityResponseBody(resp.Body)
	if err != nil {
		return err
	}
	chatResponse, err := convertGeminiResponseToChat(nativeResponse, modelID)
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

type geminiAnthropicTool struct {
	id         string
	name       string
	blockIndex int
	started    bool
	arguments  string
}

func streamGeminiAsAnthropic(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyCompatHeaders(w, resp.Header, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	textStarted := false
	thinkingStarted := false
	thinkingSignatureSeen := false
	textBlockIndex := -1
	thinkingBlockIndex := -1
	nextBlockIndex := 0
	text := strings.Builder{}
	tools := make(map[int]*geminiAnthropicTool)
	toolOrder := make([]int, 0)
	finishReason := "end_turn"
	terminalSeen := false

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
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage,
		},
	}); err != nil {
		return err
	}
	startText := func() error {
		if textStarted {
			return nil
		}
		textStarted = true
		textBlockIndex = nextBlockIndex
		nextBlockIndex++
		return writeEvent("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": textBlockIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
	}
	startThinking := func() error {
		if thinkingStarted {
			return nil
		}
		thinkingStarted = true
		thinkingBlockIndex = nextBlockIndex
		nextBlockIndex++
		return writeEvent("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": thinkingBlockIndex,
			"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			_ = writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "invalid_request_error", "message": "invalid upstream Gemini SSE event"}})
			return nil
		}
		if metadata, ok := raw["usageMetadata"].(map[string]interface{}); ok {
			usage["input_tokens"] = intValue(metadata["promptTokenCount"])
			usage["output_tokens"] = intValue(metadata["candidatesTokenCount"])
		}
		candidates, _ := raw["candidates"].([]interface{})
		if len(candidates) == 0 {
			if reason := geminiPromptBlockReason(raw); reason != "" {
				finishReason = "refusal"
				terminalSeen = true
			}
			continue
		}
		if len(candidates) > 1 {
			_ = writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "invalid_request_error", "message": "Gemini returned multiple candidates; Anthropic Messages has no lossless multi-candidate representation"}})
			return nil
		}
		candidate, _ := candidates[0].(map[string]interface{})
		if reason := stringValue(candidate["finishReason"]); reason != "" {
			terminalSeen = true
			switch reason {
			case "MAX_TOKENS":
				finishReason = "max_tokens"
			case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "RECITATION", "MALFORMED_FUNCTION_CALL":
				finishReason = "refusal"
			case "STOP":
				finishReason = "end_turn"
			default:
				finishReason = "refusal"
			}
		}
		content, _ := candidate["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		for _, item := range parts {
			part, _ := item.(map[string]interface{})
			if value := stringValue(part["text"]); value != "" {
				if thought, _ := part["thought"].(bool); thought {
					if err := startThinking(); err != nil {
						return err
					}
					if err := writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingBlockIndex, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": value}}); err != nil {
						return err
					}
					if signature := geminiThoughtSignature(part); signature != "" && !thinkingSignatureSeen {
						thinkingSignatureSeen = true
						if err := writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingBlockIndex, "delta": map[string]interface{}{"type": "signature_delta", "signature": signature}}); err != nil {
							return err
						}
					}
				} else {
					if err := startText(); err != nil {
						return err
					}
					text.WriteString(value)
					if err := writeEvent("content_block_delta", map[string]interface{}{
						"type": "content_block_delta", "index": textBlockIndex,
						"delta": map[string]interface{}{"type": "text_delta", "text": value},
					}); err != nil {
						return err
					}
				}
			}
			functionCall, ok := part["functionCall"].(map[string]interface{})
			if !ok {
				continue
			}
			index := intValue(functionCall["index"])
			if _, exists := tools[index]; !exists && functionCall["index"] == nil {
				index = len(toolOrder)
			}
			state := tools[index]
			if state == nil {
				blockIndex := nextBlockIndex
				nextBlockIndex++
				state = &geminiAnthropicTool{id: fmt.Sprintf("toolu_%d", index), blockIndex: blockIndex}
				tools[index] = state
				toolOrder = append(toolOrder, index)
			}
			if name := stringValue(functionCall["name"]); name != "" {
				state.name = name
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
			if args := functionCall["args"]; args != nil {
				encoded, err := json.Marshal(args)
				if err != nil {
					return err
				}
				state.arguments = string(encoded)
			}
			finishReason = "tool_use"
		}
	}
	if err := scanner.Err(); err != nil {
		return writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": err.Error()}})
	}
	if !terminalSeen {
		return writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": "upstream Gemini stream ended without a terminal event"}})
	}
	if thinkingStarted {
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": thinkingBlockIndex}); err != nil {
			return err
		}
	}
	if textStarted {
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textBlockIndex}); err != nil {
			return err
		}
	}
	for _, index := range toolOrder {
		state := tools[index]
		if state.started {
			if state.arguments != "" {
				if err := writeEvent("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": state.blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": state.arguments},
				}); err != nil {
					return err
				}
			}
			if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": state.blockIndex}); err != nil {
				return err
			}
		}
	}
	if err := writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": finishReason, "stop_sequence": nil}, "usage": usage,
	}); err != nil {
		return err
	}
	return writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
}
