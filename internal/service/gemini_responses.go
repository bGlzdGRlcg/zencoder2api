package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ResponsesProxy adapts OpenAI Responses requests to the native Gemini
// compatibility path. Gemini is exposed by Zencoder through generateContent,
// not through the OpenAI Responses endpoint.
func (s *GeminiService) ResponsesProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	modelID, stream, nativeBody, err := convertResponsesToGemini(body)
	if err != nil {
		return &UpstreamError{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, err.Error())),
		}
	}

	if stream {
		resp, err := s.StreamGenerateContent(ctx, modelID, nativeBody)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return streamGeminiAsResponses(w, resp, modelID)
	}

	resp, err := s.GenerateContent(ctx, modelID, nativeBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}
	nativeResponse, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	chatResponse, err := convertGeminiResponseToChat(nativeResponse, modelID)
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
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(converted)
	return err
}

func convertResponsesToGemini(body []byte) (string, bool, []byte, error) {
	chatBody, _, _, err := convertResponsesToChat(body)
	if err != nil {
		return "", false, nil, err
	}
	return convertOpenAIChatToGemini(chatBody)
}

type geminiResponsesTool struct {
	id          string
	name        string
	outputIndex int
	arguments   strings.Builder
}

func streamGeminiAsResponses(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyGeminiResponseHeaders(w, resp.Header)
	w.Header().Del("Content-Disposition")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()
	responseModel := modelID
	text := strings.Builder{}
	tools := make(map[int]*geminiResponsesTool)
	toolOrder := make([]int, 0)
	messageID := fmt.Sprintf("msg_%s", responseID)
	messageStarted := false
	usage := map[string]interface{}{}
	finishReason := "stop"
	sequenceNumber := 0

	writeEvent := func(eventType string, value interface{}) error {
		sequenceNumber++
		if object, ok := value.(map[string]interface{}); ok {
			object["sequence_number"] = sequenceNumber
		}
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
	startTextMessage := func() error {
		if messageStarted {
			return nil
		}
		messageStarted = true
		if err := writeEvent("response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"response_id":  responseID,
			"output_index": 0,
			"item": map[string]interface{}{
				"type":    "message",
				"id":      messageID,
				"status":  "in_progress",
				"role":    "assistant",
				"content": []interface{}{},
			},
		}); err != nil {
			return err
		}
		return writeEvent("response.content_part.added", map[string]interface{}{
			"type":          "response.content_part.added",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        "",
				"annotations": []interface{}{},
			},
		})
	}

	if err := writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      responseModel,
			"output":     []interface{}{},
		},
	}); err != nil {
		return err
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

		var raw map[string]interface{}
		if json.Unmarshal([]byte(payload), &raw) != nil {
			continue
		}
		if value := stringValue(raw["model"]); value != "" {
			responseModel = value
		}
		if metadata, ok := raw["usageMetadata"].(map[string]interface{}); ok {
			usage["input_tokens"] = metadata["promptTokenCount"]
			usage["output_tokens"] = metadata["candidatesTokenCount"]
			usage["total_tokens"] = metadata["totalTokenCount"]
		}

		candidates, _ := raw["candidates"].([]interface{})
		if len(candidates) == 0 {
			continue
		}
		candidate, _ := candidates[0].(map[string]interface{})
		if reason := stringValue(candidate["finishReason"]); reason != "" {
			finishReason = geminiFinishReason(reason)
		}
		content, _ := candidate["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		for _, item := range parts {
			part, _ := item.(map[string]interface{})
			if value, ok := part["text"].(string); ok && value != "" {
				if thought, _ := part["thought"].(bool); !thought {
					if err := startTextMessage(); err != nil {
						return err
					}
					text.WriteString(value)
					if err := writeEvent("response.output_text.delta", map[string]interface{}{
						"type":          "response.output_text.delta",
						"response_id":   responseID,
						"output_index":  0,
						"content_index": 0,
						"item_id":       messageID,
						"delta":         value,
					}); err != nil {
						return err
					}
				}
			}

			functionCall, ok := part["functionCall"].(map[string]interface{})
			if !ok {
				continue
			}
			index := len(toolOrder)
			if existing, ok := functionCall["index"].(float64); ok {
				index = int(existing)
			}
			state := tools[index]
			if state == nil {
				state = &geminiResponsesTool{
					id:          fmt.Sprintf("call_%d", index),
					outputIndex: len(toolOrder) + 1,
				}
				tools[index] = state
				toolOrder = append(toolOrder, index)
				if err := writeEvent("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"response_id":  responseID,
					"output_index": state.outputIndex,
					"item": map[string]interface{}{
						"type":      "function_call",
						"id":        state.id,
						"call_id":   state.id,
						"status":    "in_progress",
						"name":      stringValue(functionCall["name"]),
						"arguments": "",
					},
				}); err != nil {
					return err
				}
			}
			if name := stringValue(functionCall["name"]); name != "" {
				state.name = name
			}
			arguments := functionCall["args"]
			if arguments != nil {
				encoded, err := json.Marshal(arguments)
				if err != nil {
					return err
				}
				argumentText := string(encoded)
				state.arguments.WriteString(argumentText)
				if err := writeEvent("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"response_id":  responseID,
					"item_id":      state.id,
					"output_index": state.outputIndex,
					"delta":        argumentText,
				}); err != nil {
					return err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	output := make([]interface{}, 0, 1+len(toolOrder))
	if text.Len() > 0 {
		if err := startTextMessage(); err != nil {
			return err
		}
		if err := writeEvent("response.output_text.done", map[string]interface{}{
			"type":          "response.output_text.done",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"text":          text.String(),
		}); err != nil {
			return err
		}
		output = append(output, map[string]interface{}{
			"type":   "message",
			"id":     messageID,
			"role":   "assistant",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"text":        text.String(),
				"annotations": []interface{}{},
			}},
		})
	}
	for _, index := range toolOrder {
		state := tools[index]
		toolOutput := map[string]interface{}{
			"type":      "function_call",
			"id":        state.id,
			"call_id":   state.id,
			"status":    "completed",
			"name":      state.name,
			"arguments": state.arguments.String(),
		}
		output = append(output, toolOutput)

		// Cherry Studio turns a streamed function call into an executable tool
		// invocation only after this terminal item event. Without it the UI keeps
		// the call in progress until the stream closes.
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": state.outputIndex,
			"item":         toolOutput,
		}); err != nil {
			return err
		}
	}
	if messageStarted {
		if err := writeEvent("response.content_part.done", map[string]interface{}{
			"type":          "response.content_part.done",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        text.String(),
				"annotations": []interface{}{},
			},
		}); err != nil {
			return err
		}
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": 0,
			"item":         output[0],
		}); err != nil {
			return err
		}
	}

	status := "completed"
	completedEvent := "response.completed"
	if finishReason == "length" {
		status = "incomplete"
		completedEvent = "response.incomplete"
	}
	response := map[string]interface{}{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt,
		"status":      status,
		"model":       responseModel,
		"output":      output,
		"output_text": text.String(),
	}
	// Cherry Studio's Responses parser requires usage to be present on the
	// terminal event, even when the upstream stream did not include usage
	// metadata. Keep the shape compatible and use zeroes when unavailable.
	response["usage"] = normalizedResponsesUsage(usage)
	return writeEvent(completedEvent, map[string]interface{}{
		"type":     completedEvent,
		"response": response,
	})
}

func normalizedResponsesUsage(usage map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":  intValue(usage["input_tokens"]),
		"output_tokens": intValue(usage["output_tokens"]),
		"total_tokens":  intValue(usage["total_tokens"]),
		"input_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]interface{}{
			"reasoning_tokens": 0,
		},
	}
}

// streamChatAsResponses converts an OpenAI Chat Completions SSE stream to a
// Responses SSE stream without buffering the upstream response. It is used by
// the Anthropic compatibility path after its native stream has been adapted to
// Chat Completions chunks.
func streamChatAsResponses(w http.ResponseWriter, resp *http.Response, modelID string) error {
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()
	responseModel := modelID
	messageID := fmt.Sprintf("msg_%s", responseID)
	messageStarted := false
	text := strings.Builder{}
	tools := make(map[int]*geminiResponsesTool)
	toolOrder := make([]int, 0)
	usage := map[string]interface{}{}
	finishReason := "stop"
	sequenceNumber := 0

	writeEvent := func(eventType string, value interface{}) error {
		sequenceNumber++
		if object, ok := value.(map[string]interface{}); ok {
			object["sequence_number"] = sequenceNumber
		}
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
	startTextMessage := func() error {
		if messageStarted {
			return nil
		}
		messageStarted = true
		if err := writeEvent("response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"response_id":  responseID,
			"output_index": 0,
			"item": map[string]interface{}{
				"type":    "message",
				"id":      messageID,
				"status":  "in_progress",
				"role":    "assistant",
				"content": []interface{}{},
			},
		}); err != nil {
			return err
		}
		return writeEvent("response.content_part.added", map[string]interface{}{
			"type":          "response.content_part.added",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        "",
				"annotations": []interface{}{},
			},
		})
	}

	if err := writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      responseModel,
			"output":     []interface{}{},
		},
	}); err != nil {
		return err
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
		if value := stringValue(chunk["model"]); value != "" {
			responseModel = value
		}
		if chunkUsage, ok := chunk["usage"].(map[string]interface{}); ok {
			usage["input_tokens"] = chunkUsage["prompt_tokens"]
			usage["output_tokens"] = chunkUsage["completion_tokens"]
			usage["total_tokens"] = chunkUsage["total_tokens"]
		}

		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			finishReason = reason
		}
		delta, _ := choice["delta"].(map[string]interface{})
		if value := stringValue(delta["content"]); value != "" {
			if err := startTextMessage(); err != nil {
				return err
			}
			text.WriteString(value)
			if err := writeEvent("response.output_text.delta", map[string]interface{}{
				"type":          "response.output_text.delta",
				"response_id":   responseID,
				"output_index":  0,
				"content_index": 0,
				"item_id":       messageID,
				"delta":         value,
			}); err != nil {
				return err
			}
		}

		toolCalls, _ := delta["tool_calls"].([]interface{})
		for _, item := range toolCalls {
			call, _ := item.(map[string]interface{})
			index := intValue(call["index"])
			state := tools[index]
			newTool := false
			if state == nil {
				state = &geminiResponsesTool{
					id:          fmt.Sprintf("call_%d", index),
					outputIndex: len(toolOrder) + 1,
				}
				tools[index] = state
				toolOrder = append(toolOrder, index)
				newTool = true
			}
			function, _ := call["function"].(map[string]interface{})
			if name := stringValue(function["name"]); name != "" {
				state.name = name
			}
			if newTool {
				if err := writeEvent("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"response_id":  responseID,
					"output_index": state.outputIndex,
					"item": map[string]interface{}{
						"type":      "function_call",
						"id":        state.id,
						"call_id":   state.id,
						"status":    "in_progress",
						"name":      state.name,
						"arguments": "",
					},
				}); err != nil {
					return err
				}
			}

			arguments := stringValue(function["arguments"])
			if arguments == "" {
				continue
			}
			state.arguments.WriteString(arguments)
			if err := writeEvent("response.function_call_arguments.delta", map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"response_id":  responseID,
				"item_id":      state.id,
				"output_index": state.outputIndex,
				"delta":        arguments,
			}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	output := make([]interface{}, 0, 1+len(toolOrder))
	if messageStarted {
		messageOutput := map[string]interface{}{
			"type":   "message",
			"id":     messageID,
			"role":   "assistant",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"text":        text.String(),
				"annotations": []interface{}{},
			}},
		}
		output = append(output, messageOutput)
		if err := writeEvent("response.output_text.done", map[string]interface{}{
			"type":          "response.output_text.done",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"text":          text.String(),
		}); err != nil {
			return err
		}
		if err := writeEvent("response.content_part.done", map[string]interface{}{
			"type":          "response.content_part.done",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"item_id":       messageID,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        text.String(),
				"annotations": []interface{}{},
			},
		}); err != nil {
			return err
		}
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": 0,
			"item":         messageOutput,
		}); err != nil {
			return err
		}
	}

	for _, index := range toolOrder {
		state := tools[index]
		toolOutput := map[string]interface{}{
			"type":      "function_call",
			"id":        state.id,
			"call_id":   state.id,
			"status":    "completed",
			"name":      state.name,
			"arguments": state.arguments.String(),
		}
		output = append(output, toolOutput)
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": state.outputIndex,
			"item":         toolOutput,
		}); err != nil {
			return err
		}
	}

	status := "completed"
	completedEvent := "response.completed"
	if finishReason == "length" {
		status = "incomplete"
		completedEvent = "response.incomplete"
	}
	response := map[string]interface{}{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt,
		"status":      status,
		"model":       responseModel,
		"output":      output,
		"output_text": text.String(),
		"usage":       normalizedResponsesUsage(usage),
	}
	return writeEvent(completedEvent, map[string]interface{}{
		"type":     completedEvent,
		"response": response,
	})
}

func convertResponsesToChat(body []byte) ([]byte, string, bool, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false, err
	}
	removeUndefinedPlaceholders(raw)

	modelID, _ := raw["model"].(string)
	if modelID == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	messages := responsesInputToChatMessages(raw["input"])
	if len(messages) == 0 {
		return nil, "", false, fmt.Errorf("input is required")
	}

	chat := map[string]interface{}{
		"model":    modelID,
		"messages": messages,
		"stream":   boolValue(raw["stream"]),
	}

	for _, key := range []string{"temperature", "top_p", "stop", "seed", "response_format"} {
		if value, ok := raw[key]; ok {
			chat[key] = value
		}
	}
	if value, ok := raw["max_output_tokens"]; ok {
		chat["max_completion_tokens"] = value
	}
	if value, ok := raw["tool_choice"]; ok {
		chat["tool_choice"] = responsesToolChoiceToChat(value)
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		if converted := responsesToolsToChat(tools); len(converted) > 0 {
			chat["tools"] = converted
		}
	}

	encoded, err := json.Marshal(chat)
	return encoded, modelID, boolValue(raw["stream"]), err
}

func responsesInputToChatMessages(value interface{}) []interface{} {
	if text, ok := value.(string); ok && text != "" {
		return []interface{}{map[string]interface{}{"role": "user", "content": text}}
	}

	items, _ := value.([]interface{})
	messages := make([]interface{}, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		typeName, _ := entry["type"].(string)
		switch typeName {
		case "function_call":
			name, _ := entry["name"].(string)
			if name == "" {
				continue
			}
			callID, _ := entry["call_id"].(string)
			if callID == "" {
				callID, _ = entry["id"].(string)
			}
			messages = append(messages, map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{map[string]interface{}{
					"id":   callID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": stringValue(entry["arguments"]),
					},
				}},
			})
		case "function_call_output":
			callID, _ := entry["call_id"].(string)
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      stringValue(entry["output"]),
			})
		case "reasoning":
			continue
		default:
			role, _ := entry["role"].(string)
			if role == "" {
				if typeName == "message" {
					role = "assistant"
				} else {
					role = "user"
				}
			}
			// Responses uses developer for high-priority instructions. Both
			// Gemini and Anthropic adapters represent that role as a system
			// instruction instead of a user turn.
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, map[string]interface{}{
				"role":    role,
				"content": responsesContentToChat(entry["content"]),
			})
		}
	}
	return messages
}

func responsesContentToChat(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		return text
	}
	items, ok := value.([]interface{})
	if !ok {
		return ""
	}
	content := make([]interface{}, 0, len(items))
	for _, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "input_text", "output_text", "text":
			if text, ok := part["text"].(string); ok {
				content = append(content, map[string]interface{}{"type": "text", "text": text})
			}
		case "input_image", "image_url", "image":
			if imageURL, ok := part["image_url"]; ok {
				content = append(content, map[string]interface{}{"type": "image_url", "image_url": imageURL})
			}
		}
	}
	return content
}

func responsesToolsToChat(tools []interface{}) []interface{} {
	converted := make([]interface{}, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType != "function" {
			continue
		}
		function := map[string]interface{}{}
		for _, key := range []string{"name", "description", "parameters"} {
			if value, ok := tool[key]; ok {
				function[key] = value
			}
		}
		if _, ok := function["name"]; !ok {
			continue
		}
		converted = append(converted, map[string]interface{}{
			"type":     "function",
			"function": function,
		})
	}
	return converted
}

func responsesToolChoiceToChat(value interface{}) interface{} {
	choice, ok := value.(map[string]interface{})
	if !ok {
		return value
	}
	if _, hasFunction := choice["function"]; hasFunction {
		return value
	}
	if name, ok := choice["name"].(string); ok && name != "" {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name,
			},
		}
	}
	return value
}

func convertChatJSONToResponses(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	responseID := stringValue(raw["id"])
	if responseID == "" {
		responseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	output := make([]interface{}, 0)
	outputText := strings.Builder{}
	if choices, ok := raw["choices"].([]interface{}); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		message, _ := choice["message"].(map[string]interface{})
		text := stringValue(message["content"])
		messageOutput := map[string]interface{}{
			"type":   "message",
			"id":     fmt.Sprintf("msg_%s", responseID),
			"role":   "assistant",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"text":        text,
				"annotations": []interface{}{},
			}},
		}
		if text != "" {
			output = append(output, messageOutput)
			outputText.WriteString(text)
		}

		if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
			for _, item := range toolCalls {
				call, _ := item.(map[string]interface{})
				function, _ := call["function"].(map[string]interface{})
				callID := stringValue(call["id"])
				output = append(output, map[string]interface{}{
					"type":      "function_call",
					"id":        callID,
					"call_id":   callID,
					"status":    "completed",
					"name":      stringValue(function["name"]),
					"arguments": stringValue(function["arguments"]),
				})
			}
		}
	}

	response := map[string]interface{}{
		"id":          responseID,
		"object":      "response",
		"created_at":  int64Value(raw["created"]),
		"status":      "completed",
		"model":       modelID,
		"output":      output,
		"output_text": outputText.String(),
	}
	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		response["usage"] = map[string]interface{}{
			"input_tokens":  usage["prompt_tokens"],
			"output_tokens": usage["completion_tokens"],
			"total_tokens":  usage["total_tokens"],
		}
	}
	return json.Marshal(response)
}

func stringValue(value interface{}) string {
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

func int64Value(value interface{}) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return time.Now().Unix()
	}
}

func intValue(value interface{}) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case string:
		parsed, _ := strconv.Atoi(number)
		return parsed
	default:
		return 0
	}
}
