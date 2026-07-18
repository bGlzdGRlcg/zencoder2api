package service

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const geminiThoughtSignatureBypass = "skip_thought_signature_validator"

// ChatCompletionsProxy adapts OpenAI Chat Completions requests to the native
// Gemini generateContent API. Zencoder exposes Gemini under /v1beta/models,
// not under an OpenAI-compatible /v1/chat/completions route.
func (s *GeminiService) ChatCompletionsProxy(ctx context.Context, w http.ResponseWriter, body []byte) error {
	modelID, stream, nativeBody, err := convertOpenAIChatToGemini(body)
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
		return streamGeminiAsChat(w, resp, modelID)
	}

	resp, err := s.GenerateContent(ctx, modelID, nativeBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	converted, err := convertGeminiResponseToChat(responseBody, modelID)
	if err != nil {
		return err
	}
	copyGeminiResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(converted)
	return err
}

func convertOpenAIChatToGemini(body []byte) (string, bool, []byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", false, nil, err
	}

	modelID, _ := raw["model"].(string)
	if modelID == "" {
		return "", false, nil, fmt.Errorf("model is required")
	}

	native := make(map[string]interface{})
	contents, systemInstruction, _ := convertChatMessagesToGemini(raw["messages"])
	if len(contents) == 0 {
		return "", false, nil, fmt.Errorf("messages is required")
	}
	native["contents"] = contents
	if len(systemInstruction) > 0 {
		native["systemInstruction"] = map[string]interface{}{
			"parts": systemInstruction,
		}
	}

	if tools, ok := raw["tools"].([]interface{}); ok {
		if converted := convertChatToolsToGemini(tools); len(converted) > 0 {
			native["tools"] = []interface{}{map[string]interface{}{
				"functionDeclarations": converted,
			}}
		}
	}
	if toolConfig := convertChatToolChoiceToGemini(raw["tool_choice"]); toolConfig != nil {
		native["toolConfig"] = toolConfig
	}

	generationConfig := make(map[string]interface{})
	for source, target := range map[string]string{
		"temperature": "temperature",
		"top_p":       "topP",
		"seed":        "seed",
	} {
		if value, ok := raw[source]; ok && value != nil {
			generationConfig[target] = value
		}
	}
	if value, ok := raw["max_completion_tokens"]; ok {
		generationConfig["maxOutputTokens"] = value
	} else if value, ok := raw["max_tokens"]; ok {
		generationConfig["maxOutputTokens"] = value
	}
	if stop, ok := raw["stop"]; ok {
		switch value := stop.(type) {
		case string:
			generationConfig["stopSequences"] = []string{value}
		case []interface{}:
			generationConfig["stopSequences"] = value
		}
	}
	if responseFormat, ok := raw["response_format"].(map[string]interface{}); ok {
		if formatType, _ := responseFormat["type"].(string); formatType == "json_object" {
			generationConfig["responseMimeType"] = "application/json"
		} else if formatType == "json_schema" {
			generationConfig["responseMimeType"] = "application/json"
			if schema, ok := responseFormat["json_schema"].(map[string]interface{}); ok {
				if value, ok := schema["schema"]; ok {
					generationConfig["responseSchema"] = sanitizeGeminiSchema(value)
				}
			}
		}
	}
	if len(generationConfig) > 0 {
		native["generationConfig"] = generationConfig
	}

	encoded, err := json.Marshal(native)
	return modelID, boolValue(raw["stream"]), encoded, err
}

func convertChatMessagesToGemini(messagesValue interface{}) ([]interface{}, []interface{}, map[string]string) {
	messages, _ := messagesValue.([]interface{})
	contents := make([]interface{}, 0, len(messages))
	systemInstruction := make([]interface{}, 0)
	toolNames := make(map[string]string)

	for _, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "system" {
			systemInstruction = append(systemInstruction, geminiTextParts(message["content"])...)
			continue
		}

		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}
		parts := geminiContentParts(message["content"])

		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]interface{}); ok {
				for _, item := range calls {
					call, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					function, _ := call["function"].(map[string]interface{})
					name, _ := function["name"].(string)
					if name == "" {
						continue
					}
					callID, _ := call["id"].(string)
					if callID != "" {
						toolNames[callID] = name
					}
					functionCall := map[string]interface{}{
						"name": name,
						"args": decodeGeminiArguments(function["arguments"]),
					}
					functionCallPart := map[string]interface{}{"functionCall": functionCall}
					thoughtSignature := openAIThoughtSignature(call)
					if thoughtSignature == "" {
						// OpenAI clients may not have a Gemini signature for a
						// historical tool call. Keep the request usable while the
						// gateway skips signature validation for this compatibility
						// path.
						thoughtSignature = geminiThoughtSignatureBypass
					}
					functionCallPart["thoughtSignature"] = thoughtSignature
					parts = append(parts, functionCallPart)
				}
			}
		}

		if role == "tool" || role == "function" {
			callID, _ := message["tool_call_id"].(string)
			name, _ := message["name"].(string)
			if name == "" {
				name = toolNames[callID]
			}
			if name == "" {
				name = callID
			}
			response := decodeGeminiFunctionResponse(message["content"])
			functionResponse := map[string]interface{}{
				"name":     name,
				"response": response,
			}
			parts = []interface{}{map[string]interface{}{"functionResponse": functionResponse}}
			geminiRole = "user"
		}

		if len(parts) > 0 {
			contents = append(contents, map[string]interface{}{
				"role":  geminiRole,
				"parts": parts,
			})
		}
	}

	return contents, systemInstruction, toolNames
}

func geminiContentParts(content interface{}) []interface{} {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"text": text}}
	}

	items, ok := content.([]interface{})
	if !ok {
		return nil
	}
	parts := make([]interface{}, 0, len(items))
	for _, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch partType, _ := part["type"].(string); partType {
		case "text", "input_text":
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, map[string]interface{}{"text": text})
			}
		case "image_url":
			if image := geminiInlineImage(part); image != nil {
				parts = append(parts, image)
			}
		case "image", "inline_data", "inlineData":
			if image := geminiInlineImage(part); image != nil {
				parts = append(parts, image)
			}
		}
	}
	return parts
}

func geminiTextParts(content interface{}) []interface{} {
	parts := geminiContentParts(content)
	textParts := make([]interface{}, 0, len(parts))
	for _, part := range parts {
		if partMap, ok := part.(map[string]interface{}); ok {
			if _, ok := partMap["text"]; ok {
				textParts = append(textParts, part)
			}
		}
	}
	return textParts
}

func geminiInlineImage(part map[string]interface{}) map[string]interface{} {
	var source string
	if imageURL, ok := part["image_url"].(string); ok {
		source = imageURL
	} else if imageURL, ok := part["image_url"].(map[string]interface{}); ok {
		source, _ = imageURL["url"].(string)
	} else if image, ok := part["source"].(map[string]interface{}); ok {
		data, _ := image["data"].(string)
		mediaType, _ := image["media_type"].(string)
		if data != "" {
			return map[string]interface{}{"inlineData": map[string]interface{}{
				"mimeType": mediaType,
				"data":     data,
			}}
		}
	}

	if !strings.HasPrefix(source, "data:") {
		return nil
	}
	parts := strings.SplitN(source, ",", 2)
	if len(parts) != 2 {
		return nil
	}
	header := parts[0]
	mediaType := "application/octet-stream"
	if semi := strings.Index(header, ";"); semi > len("data:") {
		mediaType = header[len("data:"):semi]
	}
	data := parts[1]
	if strings.Contains(header, ";base64") {
		if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
			data = base64.StdEncoding.EncodeToString(decoded)
		}
	}
	return map[string]interface{}{"inlineData": map[string]interface{}{
		"mimeType": mediaType,
		"data":     data,
	}}
}

func convertChatToolsToGemini(tools []interface{}) []interface{} {
	declarations := make([]interface{}, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		parameters := function["parameters"]
		if parameters == nil {
			parameters = map[string]interface{}{"type": "object"}
		}
		parameters = sanitizeGeminiSchema(parameters)
		declaration := map[string]interface{}{
			"name":       name,
			"parameters": parameters,
		}
		if description, ok := function["description"].(string); ok && description != "" {
			declaration["description"] = description
		}
		declarations = append(declarations, declaration)
	}
	return declarations
}

// sanitizeGeminiSchema converts a general JSON Schema into the smaller Schema
// shape accepted by Gemini function declarations and response schemas.
func sanitizeGeminiSchema(value interface{}) interface{} {
	switch schema := value.(type) {
	case []interface{}:
		items := make([]interface{}, 0, len(schema))
		for _, item := range schema {
			items = append(items, sanitizeGeminiSchema(item))
		}
		return items
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, item := range schema {
			switch key {
			case "type", "format", "title", "description", "nullable", "enum", "maxItems", "minItems", "required", "propertyOrdering":
				result[key] = item
			case "properties":
				properties, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				cleanProperties := make(map[string]interface{}, len(properties))
				for propertyName, propertySchema := range properties {
					cleanProperties[propertyName] = sanitizeGeminiSchema(propertySchema)
				}
				result[key] = cleanProperties
			case "items":
				result[key] = sanitizeGeminiSchema(item)
			}
		}
		if _, ok := result["type"]; !ok {
			result["type"] = "object"
		}
		return result
	default:
		return value
	}
}

func convertChatToolChoiceToGemini(value interface{}) map[string]interface{} {
	mode := "AUTO"
	var allowed []interface{}
	switch choice := value.(type) {
	case string:
		switch choice {
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		case "auto", "":
			mode = "AUTO"
		}
	case map[string]interface{}:
		if function, ok := choice["function"].(map[string]interface{}); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				mode = "ANY"
				allowed = []interface{}{name}
			}
		}
	}
	if value == nil {
		return nil
	}
	config := map[string]interface{}{"mode": mode}
	if len(allowed) > 0 {
		config["allowedFunctionNames"] = allowed
	}
	return map[string]interface{}{"functionCallingConfig": config}
}

func decodeGeminiArguments(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		var decoded interface{}
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
		return map[string]interface{}{"text": text}
	}
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func decodeGeminiFunctionResponse(value interface{}) interface{} {
	decoded := decodeGeminiArguments(value)
	if _, ok := decoded.(map[string]interface{}); ok {
		return decoded
	}
	return map[string]interface{}{"result": decoded}
}

func openAIThoughtSignature(call map[string]interface{}) string {
	extra, _ := call["extra_content"].(map[string]interface{})
	google, _ := extra["google"].(map[string]interface{})
	if signature, ok := google["thought_signature"].(string); ok {
		return signature
	}
	if signature, ok := google["thoughtSignature"].(string); ok {
		return signature
	}
	return ""
}

func geminiThoughtSignature(part map[string]interface{}) string {
	if signature, ok := part["thoughtSignature"].(string); ok {
		return signature
	}
	if signature, ok := part["thought_signature"].(string); ok {
		return signature
	}
	if functionCall, ok := part["functionCall"].(map[string]interface{}); ok {
		if signature, ok := functionCall["thoughtSignature"].(string); ok {
			return signature
		}
		if signature, ok := functionCall["thought_signature"].(string); ok {
			return signature
		}
	}
	return ""
}

func boolValue(value interface{}) bool {
	result, _ := value.(bool)
	return result
}

func convertGeminiResponseToChat(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	message := map[string]interface{}{"role": "assistant", "content": ""}
	var text strings.Builder
	toolCalls := make([]interface{}, 0)
	finishReason := "stop"
	if candidates, ok := raw["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			finishReason = geminiFinishReason(candidate["finishReason"])
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					for _, item := range parts {
						part, ok := item.(map[string]interface{})
						if !ok {
							continue
						}
						if value, ok := part["text"].(string); ok {
							if thought, _ := part["thought"].(bool); thought {
								if existing, _ := message["reasoning_content"].(string); existing != "" {
									message["reasoning_content"] = existing + value
								} else {
									message["reasoning_content"] = value
								}
							} else {
								text.WriteString(value)
							}
						}
						if call, ok := part["functionCall"].(map[string]interface{}); ok {
							name, _ := call["name"].(string)
							arguments, _ := json.Marshal(call["args"])
							id, _ := call["id"].(string)
							if id == "" {
								id = fmt.Sprintf("call_%d", len(toolCalls))
							}
							toolCall := map[string]interface{}{
								"id":   id,
								"type": "function",
								"function": map[string]interface{}{
									"name":      name,
									"arguments": string(arguments),
								},
							}
							if thoughtSignature := geminiThoughtSignature(part); thoughtSignature != "" {
								toolCall["extra_content"] = map[string]interface{}{
									"google": map[string]interface{}{
										"thought_signature": thoughtSignature,
									},
								}
							}
							toolCalls = append(toolCalls, toolCall)
						}
					}
				}
			}
		}
	}
	message["content"] = text.String()
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}

	usage := map[string]interface{}{}
	if metadata, ok := raw["usageMetadata"].(map[string]interface{}); ok {
		usage["prompt_tokens"] = metadata["promptTokenCount"]
		usage["completion_tokens"] = metadata["candidatesTokenCount"]
		usage["total_tokens"] = metadata["totalTokenCount"]
	}
	response := map[string]interface{}{
		"id":      firstNonEmptyString(raw["responseId"], fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelID,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	return json.Marshal(response)
}

func streamGeminiAsChat(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyGeminiResponseHeaders(w, resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	operationID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	started := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal([]byte(data), &raw) != nil {
			continue
		}
		choices := make([]interface{}, 0)
		if candidates, ok := raw["candidates"].([]interface{}); ok && len(candidates) > 0 {
			if candidate, ok := candidates[0].(map[string]interface{}); ok {
				delta := map[string]interface{}{}
				if content, ok := candidate["content"].(map[string]interface{}); ok {
					if parts, ok := content["parts"].([]interface{}); ok {
						for _, item := range parts {
							part, ok := item.(map[string]interface{})
							if !ok {
								continue
							}
							if text, ok := part["text"].(string); ok {
								if thought, _ := part["thought"].(bool); thought {
									delta["reasoning_content"] = text
								} else {
									delta["content"] = text
								}
							}
							if call, ok := part["functionCall"].(map[string]interface{}); ok {
								name, _ := call["name"].(string)
								arguments, _ := json.Marshal(call["args"])
								id, _ := call["id"].(string)
								if id == "" {
									id = fmt.Sprintf("call_%d", time.Now().UnixNano())
								}
								toolCall := map[string]interface{}{
									"index": 0,
									"id":    id,
									"type":  "function",
									"function": map[string]interface{}{
										"name":      name,
										"arguments": string(arguments),
									},
								}
								if thoughtSignature := geminiThoughtSignature(part); thoughtSignature != "" {
									toolCall["extra_content"] = map[string]interface{}{
										"google": map[string]interface{}{
											"thought_signature": thoughtSignature,
										},
									}
								}
								delta["tool_calls"] = []interface{}{toolCall}
							}
						}
					}
				}
				finishReason := interface{}(nil)
				if value, ok := candidate["finishReason"]; ok {
					finishReason = geminiFinishReason(value)
				}
				if len(delta) > 0 || finishReason != nil {
					if len(delta) > 0 && !started {
						delta["role"] = "assistant"
						started = true
					}
					choices = append(choices, map[string]interface{}{
						"index":         0,
						"delta":         delta,
						"finish_reason": finishReason,
					})
				}
			}
		}
		chunk := map[string]interface{}{
			"id":      operationID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelID,
			"choices": choices,
		}
		if metadata, ok := raw["usageMetadata"].(map[string]interface{}); ok {
			chunk["usage"] = map[string]interface{}{
				"prompt_tokens":     metadata["promptTokenCount"],
				"completion_tokens": metadata["candidatesTokenCount"],
				"total_tokens":      metadata["totalTokenCount"],
			}
		}
		if len(choices) > 0 || chunk["usage"] != nil {
			encoded, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
	return err
}

func geminiFinishReason(value interface{}) string {
	reason, _ := value.(string)
	switch reason {
	case "STOP", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL", "OTHER":
		return "tool_calls"
	default:
		return strings.ToLower(reason)
	}
}

func firstNonEmptyString(value interface{}, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}

func copyGeminiResponseHeaders(w http.ResponseWriter, headers http.Header) {
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}
