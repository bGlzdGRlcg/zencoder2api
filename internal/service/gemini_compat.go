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
	modelID, stream, includeUsage, nativeBody, err := convertOpenAIChatToGemini(body)
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
		return streamGeminiAsChat(w, resp, modelID, includeUsage)
	}

	resp, err := s.GenerateContent(ctx, modelID, nativeBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBody, err := readCompatibilityResponseBody(resp.Body)
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

func convertOpenAIChatToGemini(body []byte) (string, bool, bool, []byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", false, false, nil, err
	}
	removeUndefinedPlaceholders(raw)
	includeUsage, err := consumeChatStreamOptions(raw, "Gemini Chat")
	if err != nil {
		return "", false, false, nil, err
	}
	if err := consumeGeminiToolResultMetadata(raw["messages"]); err != nil {
		return "", false, false, nil, err
	}
	if err := rejectUnsupportedFields(raw, "Gemini Chat", "model", "messages", "stream", "temperature", "top_p", "top_k", "seed", "max_completion_tokens", "max_tokens", "stop", "tools", "tool_choice", "response_format", "n", "reasoning_effort", "frequency_penalty", "presence_penalty", "thinking"); err != nil {
		return "", false, false, nil, err
	}

	modelID, _ := raw["model"].(string)
	if modelID == "" {
		return "", false, false, nil, fmt.Errorf("model is required")
	}

	if err := validateGeminiChatMessages(raw["messages"]); err != nil {
		return "", false, false, nil, err
	}
	if err := validateGeminiToolChoice(raw["tool_choice"]); err != nil {
		return "", false, false, nil, err
	}
	native := make(map[string]interface{})
	contents, systemInstruction, _ := convertChatMessagesToGemini(raw["messages"])
	if len(contents) == 0 {
		return "", false, false, nil, fmt.Errorf("messages is required")
	}
	native["contents"] = contents
	if len(systemInstruction) > 0 {
		native["systemInstruction"] = map[string]interface{}{
			"parts": systemInstruction,
		}
	}

	if tools, ok := raw["tools"].([]interface{}); ok {
		if err := validateGeminiTools(tools); err != nil {
			return "", false, false, nil, err
		}
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
	if value, ok := raw["n"]; ok {
		count := intValue(value)
		if count < 1 || count > 8 {
			return "", false, false, nil, fmt.Errorf("n must be between 1 and 8 for Gemini")
		}
		generationConfig["candidateCount"] = count
	}
	if value, ok := raw["frequency_penalty"]; ok {
		generationConfig["frequencyPenalty"] = value
	}
	if value, ok := raw["presence_penalty"]; ok {
		generationConfig["presencePenalty"] = value
	}
	if value, ok := raw["reasoning_effort"]; ok {
		effort, ok := value.(string)
		if !ok {
			return "", false, false, nil, fmt.Errorf("reasoning_effort must be a string")
		}
		level := map[string]string{"minimal": "MINIMAL", "low": "LOW", "medium": "MEDIUM", "high": "HIGH"}[effort]
		if level == "" {
			return "", false, false, nil, fmt.Errorf("reasoning_effort %q cannot be represented by Gemini", effort)
		}
		generationConfig["thinkingConfig"] = map[string]interface{}{"thinkingLevel": level, "includeThoughts": true}
	}
	if thinking, ok := raw["thinking"].(map[string]interface{}); ok {
		typ := stringValue(thinking["type"])
		if typ == "enabled" {
			budget := intValue(thinking["budget_tokens"])
			if budget < 1 {
				return "", false, false, nil, fmt.Errorf("thinking.budget_tokens must be positive")
			}
			generationConfig["thinkingConfig"] = map[string]interface{}{"thinkingBudget": budget, "includeThoughts": true}
		} else if typ != "disabled" {
			return "", false, false, nil, fmt.Errorf("thinking type %q cannot be represented by Gemini", typ)
		}
	}
	if responseFormat, ok := raw["response_format"].(map[string]interface{}); ok {
		if formatType, _ := responseFormat["type"].(string); formatType == "json_object" {
			generationConfig["responseMimeType"] = "application/json"
		} else if formatType == "json_schema" {
			generationConfig["responseMimeType"] = "application/json"
			if schema, ok := responseFormat["json_schema"].(map[string]interface{}); ok {
				if value, ok := schema["schema"]; ok {
					clean, err := geminiSchema(value)
					if err != nil {
						return "", false, false, nil, err
					}
					generationConfig["responseJsonSchema"] = clean
				}
			}
		} else {
			return "", false, false, nil, fmt.Errorf("response_format type %q cannot be represented by Gemini", formatType)
		}
	}
	if len(generationConfig) > 0 {
		native["generationConfig"] = generationConfig
	}

	encoded, err := json.Marshal(native)
	return modelID, boolValue(raw["stream"]), includeUsage, encoded, err
}

// Cherry Studio propagates a tool call's OpenAI-compatible provider metadata
// to the matching tool result. Gemini needs the thought signature on the
// functionCall part, but functionResponse has no corresponding field, so the
// repeated metadata is redundant once the assistant message has preserved it.
func consumeGeminiToolResultMetadata(value interface{}) error {
	messages, _ := value.([]interface{})
	for index, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		role := stringValue(message["role"])
		if role != "tool" && role != "function" {
			continue
		}
		extraValue, exists := message["extra_content"]
		if !exists || extraValue == nil {
			delete(message, "extra_content")
			continue
		}
		extra, ok := extraValue.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d].extra_content must be an object", index)
		}
		if err := rejectUnsupportedFields(extra, fmt.Sprintf("Gemini messages[%d].extra_content", index), "google"); err != nil {
			return err
		}
		if googleValue, exists := extra["google"]; exists && googleValue != nil {
			google, ok := googleValue.(map[string]interface{})
			if !ok {
				return fmt.Errorf("messages[%d].extra_content.google must be an object", index)
			}
			if err := rejectUnsupportedFields(google, fmt.Sprintf("Gemini messages[%d].extra_content.google", index), "thought_signature"); err != nil {
				return err
			}
			if signature, exists := google["thought_signature"]; exists && signature != nil {
				if _, ok := signature.(string); !ok {
					return fmt.Errorf("messages[%d].extra_content.google.thought_signature must be a string", index)
				}
			}
		}
		delete(message, "extra_content")
	}
	return nil
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
		if role == "system" || role == "developer" {
			systemInstruction = append(systemInstruction, geminiTextParts(message["content"])...)
			continue
		}

		geminiRole := "user"
		parts := geminiContentParts(message["content"])
		if role == "assistant" {
			parts = append(geminiReasoningParts(message), parts...)
			geminiRole = "model"
		}

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
			if isError, _ := message["is_error"].(bool); isError {
				response = map[string]interface{}{"error": response}
			}
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

func geminiReasoningParts(message map[string]interface{}) []interface{} {
	parts := make([]interface{}, 0)
	if details, ok := message["reasoning_details"].([]interface{}); ok {
		for _, item := range details {
			detail, ok := item.(map[string]interface{})
			if !ok || stringValue(detail["type"]) != "thinking" {
				continue
			}
			part := map[string]interface{}{"text": detail["thinking"], "thought": true}
			if signature := stringValue(detail["signature"]); signature != "" {
				part["thoughtSignature"] = signature
			}
			parts = append(parts, part)
		}
		if len(parts) > 0 {
			return parts
		}
	}
	if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
		part := map[string]interface{}{"text": reasoning, "thought": true}
		signature := openAIMessageReasoningSignature(message)
		if signature == "" {
			signature = geminiThoughtSignatureBypass
		}
		part["thoughtSignature"] = signature
		parts = append(parts, part)
	}
	return parts
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
			"name":                 name,
			"parametersJsonSchema": parameters,
		}
		if description, ok := function["description"].(string); ok && description != "" {
			declaration["description"] = description
		}
		declarations = append(declarations, declaration)
	}
	return declarations
}

// sanitizeGeminiSchema keeps the JSON Schema keywords accepted by Gemini's
// parametersJsonSchema/responseJsonSchema fields and drops metadata such as
// the draft-identifying $schema keyword.
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
			case "$id", "$ref", "$anchor", "type", "format", "title", "description", "nullable", "enum", "maxItems", "minItems", "maxLength", "minLength", "maximum", "minimum", "required", "propertyOrdering":
				result[key] = item
			case "properties", "$defs":
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
			case "prefixItems", "anyOf", "oneOf":
				result[key] = sanitizeGeminiSchema(item)
			case "additionalProperties":
				if _, ok := item.(bool); ok {
					result[key] = item
				} else {
					result[key] = sanitizeGeminiSchema(item)
				}
			}
		}
		if _, hasType := result["type"]; !hasType && result["$ref"] == nil && result["anyOf"] == nil && result["oneOf"] == nil {
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
		} else if stringValue(choice["type"]) == "function" {
			if name := stringValue(choice["name"]); name != "" {
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

func openAIMessageReasoningSignature(message map[string]interface{}) string {
	if signature := stringValue(message["reasoning_signature"]); signature != "" {
		return signature
	}
	if calls, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range calls {
			call, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if signature := openAIThoughtSignature(call); signature != "" {
				return signature
			}
		}
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
	candidates, ok := raw["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		if reason := geminiPromptBlockReason(raw); reason != "" {
			return json.Marshal(map[string]interface{}{
				"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion",
				"created": time.Now().Unix(), "model": modelID,
				"choices": []interface{}{map[string]interface{}{
					"index": 0, "message": map[string]interface{}{
						"role": "assistant", "content": "", "refusal": reason,
					}, "finish_reason": "content_filter",
				}},
			})
		}
		return nil, fmt.Errorf("gemini response has no candidates")
	}
	choices := make([]interface{}, 0, len(candidates))
	for candidateIndex, item := range candidates {
		candidate, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("gemini candidate %d is not an object", candidateIndex)
		}
		message := map[string]interface{}{"role": "assistant", "content": ""}
		var text strings.Builder
		reasoning := strings.Builder{}
		reasoningSignature := ""
		toolCalls := make([]interface{}, 0)
		if content, ok := candidate["content"].(map[string]interface{}); ok {
			if parts, ok := content["parts"].([]interface{}); ok {
				for partIndex, item := range parts {
					part, ok := item.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("gemini candidate %d part %d is not an object", candidateIndex, partIndex)
					}
					if value, ok := part["text"].(string); ok {
						if thought, _ := part["thought"].(bool); thought {
							reasoning.WriteString(value)
							if signature := geminiThoughtSignature(part); signature != "" {
								reasoningSignature = signature
							}
						} else {
							text.WriteString(value)
						}
					}
					if call, ok := part["functionCall"].(map[string]interface{}); ok {
						name, _ := call["name"].(string)
						if name == "" {
							return nil, fmt.Errorf("gemini candidate %d function call has no name", candidateIndex)
						}
						arguments, err := json.Marshal(call["args"])
						if err != nil {
							return nil, err
						}
						id, _ := call["id"].(string)
						if id == "" {
							id = fmt.Sprintf("call_%d_%d", candidateIndex, len(toolCalls))
						}
						toolCall := map[string]interface{}{
							"id": id, "type": "function",
							"function": map[string]interface{}{"name": name, "arguments": string(arguments)},
						}
						if signature := geminiThoughtSignature(part); signature != "" {
							toolCall["extra_content"] = map[string]interface{}{"google": map[string]interface{}{"thought_signature": signature}}
						}
						toolCalls = append(toolCalls, toolCall)
					}
				}
			}
		}
		message["content"] = text.String()
		if reasoning.Len() > 0 {
			message["reasoning_content"] = reasoning.String()
			if reasoningSignature != "" {
				message["reasoning_signature"] = reasoningSignature
			}
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if refusal := stringValue(candidate["finishMessage"]); refusal != "" && text.Len() == 0 && len(toolCalls) == 0 {
			message["refusal"] = refusal
		}
		finishReason := geminiFinishReason(candidate["finishReason"])
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
		choices = append(choices, map[string]interface{}{"index": candidateIndex, "message": message, "finish_reason": finishReason})
	}

	usage := map[string]interface{}{}
	if metadata, ok := raw["usageMetadata"].(map[string]interface{}); ok {
		usage["prompt_tokens"] = metadata["promptTokenCount"]
		usage["completion_tokens"] = metadata["candidatesTokenCount"]
		usage["total_tokens"] = metadata["totalTokenCount"]
		if cached := metadata["cachedContentTokenCount"]; cached != nil {
			usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		}
		if reasoning := metadata["thoughtsTokenCount"]; reasoning != nil {
			usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": reasoning}
		}
	}
	response := map[string]interface{}{
		"id":      firstNonEmptyString(raw["responseId"], fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelID,
		"choices": choices,
	}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	return json.Marshal(response)
}

func geminiPromptBlockReason(raw map[string]interface{}) string {
	feedback, _ := raw["promptFeedback"].(map[string]interface{})
	return stringValue(feedback["blockReason"])
}

func streamGeminiAsChat(w http.ResponseWriter, resp *http.Response, modelID string, includeUsage bool) error {
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := readUpstreamErrorBody(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}
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
	started := make(map[int]bool)
	seenCandidates := make(map[int]bool)
	finishedCandidates := make(map[int]bool)
	usage := map[string]interface{}{}
	usageSeen := false
	type toolState struct {
		id, name, arguments string
		candidate, index    int
		started             bool
		flushed             bool
	}
	tools := make(map[string]*toolState)
	toolOrder := make([]string, 0)
	nextToolIndex := make(map[int]int)
	flushTools := func(candidate int, deltas []interface{}) []interface{} {
		for _, key := range toolOrder {
			state := tools[key]
			if state.candidate != candidate || state.flushed {
				continue
			}
			arguments := state.arguments
			if arguments == "" {
				arguments = "{}"
			}
			merged := false
			for _, item := range deltas {
				delta, _ := item.(map[string]interface{})
				if intValue(delta["index"]) != state.index {
					continue
				}
				function, _ := delta["function"].(map[string]interface{})
				function["arguments"] = arguments
				merged = true
				break
			}
			if !merged {
				deltas = append(deltas, map[string]interface{}{
					"index": state.index,
					"function": map[string]interface{}{
						"arguments": arguments,
					},
				})
			}
			state.flushed = true
		}
		return deltas
	}
	writeError := func(err error) error {
		payload, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": err.Error(), "type": "upstream_protocol_error"}})
		if _, writeErr := fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload); writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			return writeError(fmt.Errorf("invalid upstream gemini SSE event: %w", err))
		}
		choices := make([]interface{}, 0)
		candidates, _ := raw["candidates"].([]interface{})
		if len(candidates) == 0 {
			if reason := geminiPromptBlockReason(raw); reason != "" {
				choices = append(choices, map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{"role": "assistant", "content": "", "refusal": reason},
					"finish_reason": "content_filter",
				})
				seenCandidates[0] = true
				finishedCandidates[0] = true
			}
		}
		for fallbackIndex, item := range candidates {
			candidate, ok := item.(map[string]interface{})
			if !ok {
				return writeError(fmt.Errorf("gemini candidate %d is not an object", fallbackIndex))
			}
			candidateIndex := fallbackIndex
			if candidate["index"] != nil {
				candidateIndex = intValue(candidate["index"])
			}
			seenCandidates[candidateIndex] = true
			delta := map[string]interface{}{}
			var contentText, reasoningText strings.Builder
			reasoningSignature := ""
			toolDeltas := make([]interface{}, 0)
			content, _ := candidate["content"].(map[string]interface{})
			parts, _ := content["parts"].([]interface{})
			callOrdinal := 0
			for partIndex, item := range parts {
				part, ok := item.(map[string]interface{})
				if !ok {
					return writeError(fmt.Errorf("gemini candidate %d part %d is not an object", candidateIndex, partIndex))
				}
				if text, ok := part["text"].(string); ok {
					if thought, _ := part["thought"].(bool); thought {
						reasoningText.WriteString(text)
					} else {
						contentText.WriteString(text)
					}
				}
				if thought, _ := part["thought"].(bool); thought {
					if signature := geminiThoughtSignature(part); signature != "" {
						reasoningSignature = signature
					}
				}
				call, ok := part["functionCall"].(map[string]interface{})
				if !ok {
					continue
				}
				name := stringValue(call["name"])
				if name == "" {
					return writeError(fmt.Errorf("gemini candidate %d function call has no name", candidateIndex))
				}
				id := stringValue(call["id"])
				key := fmt.Sprintf("%d:%s:%d", candidateIndex, name, callOrdinal)
				if id != "" {
					key = fmt.Sprintf("%d:id:%s", candidateIndex, id)
				}
				state := tools[key]
				if state == nil {
					if id == "" {
						id = fmt.Sprintf("call_%d_%d", candidateIndex, nextToolIndex[candidateIndex])
					}
					state = &toolState{id: id, name: name, candidate: candidateIndex, index: nextToolIndex[candidateIndex]}
					nextToolIndex[candidateIndex]++
					tools[key] = state
					toolOrder = append(toolOrder, key)
				}
				if args := call["args"]; args != nil {
					arguments, err := json.Marshal(args)
					if err != nil {
						return writeError(err)
					}
					state.arguments = string(arguments)
				}
				if !state.started {
					toolCall := map[string]interface{}{"index": state.index, "function": map[string]interface{}{"arguments": ""}}
					toolCall["id"] = state.id
					toolCall["type"] = "function"
					toolCall["function"].(map[string]interface{})["name"] = state.name
					if signature := geminiThoughtSignature(part); signature != "" {
						toolCall["extra_content"] = map[string]interface{}{"google": map[string]interface{}{"thought_signature": signature}}
					}
					state.started = true
					toolDeltas = append(toolDeltas, toolCall)
				}
				callOrdinal++
			}
			if contentText.Len() > 0 {
				delta["content"] = contentText.String()
			}
			if reasoningText.Len() > 0 {
				delta["reasoning_content"] = reasoningText.String()
			}
			if reasoningSignature != "" {
				delta["reasoning_signature"] = reasoningSignature
			}
			if len(toolDeltas) > 0 {
				delta["tool_calls"] = toolDeltas
			}
			finishReason := interface{}(nil)
			if value := candidate["finishReason"]; stringValue(value) != "" {
				finishReason = geminiFinishReason(value)
				finishedCandidates[candidateIndex] = true
				toolDeltas = flushTools(candidateIndex, toolDeltas)
				if len(toolDeltas) > 0 {
					delta["tool_calls"] = toolDeltas
				}
			}
			if len(delta) > 0 || finishReason != nil {
				if !started[candidateIndex] {
					delta["role"] = "assistant"
					started[candidateIndex] = true
				}
				choices = append(choices, map[string]interface{}{"index": candidateIndex, "delta": delta, "finish_reason": finishReason})
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
			usage = map[string]interface{}{
				"prompt_tokens":     metadata["promptTokenCount"],
				"completion_tokens": metadata["candidatesTokenCount"],
				"total_tokens":      metadata["totalTokenCount"],
			}
			if cached := metadata["cachedContentTokenCount"]; cached != nil {
				usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
			}
			if reasoning := metadata["thoughtsTokenCount"]; reasoning != nil {
				usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": reasoning}
			}
			usageSeen = true
		}
		if len(choices) > 0 {
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
		return writeError(err)
	}
	if len(seenCandidates) == 0 {
		return writeError(fmt.Errorf("upstream Gemini stream ended without a terminal event"))
	}
	for candidate := range seenCandidates {
		if !finishedCandidates[candidate] {
			return writeError(fmt.Errorf("upstream Gemini candidate %d ended without a terminal event", candidate))
		}
	}
	flushedCandidates := make(map[int]bool)
	flushChoices := make([]interface{}, 0)
	for _, key := range toolOrder {
		state := tools[key]
		if state.flushed || flushedCandidates[state.candidate] {
			continue
		}
		flushedCandidates[state.candidate] = true
		deltas := flushTools(state.candidate, nil)
		if len(deltas) > 0 {
			flushChoices = append(flushChoices, map[string]interface{}{
				"index": state.candidate, "delta": map[string]interface{}{"tool_calls": deltas}, "finish_reason": nil,
			})
		}
	}
	if len(flushChoices) > 0 {
		encoded, err := json.Marshal(map[string]interface{}{
			"id": operationID, "object": "chat.completion.chunk", "created": created,
			"model": modelID, "choices": flushChoices,
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		flusher.Flush()
	}
	if includeUsage && usageSeen {
		encoded, err := json.Marshal(map[string]interface{}{
			"id": operationID, "object": "chat.completion.chunk", "created": created,
			"model": modelID, "choices": []interface{}{}, "usage": usage,
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		flusher.Flush()
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
	return err
}

func geminiFinishReason(value interface{}) string {
	reason, _ := value.(string)
	switch reason {
	case "STOP", "FINISH_REASON_UNSPECIFIED", "OTHER", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "RECITATION",
		"MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
		"IMAGE_RECITATION", "IMAGE_OTHER", "NO_IMAGE":
		return "content_filter"
	default:
		return "stop"
	}
}

func firstNonEmptyString(value interface{}, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}

func copyGeminiResponseHeaders(w http.ResponseWriter, headers http.Header) {
	copyResponseHeaders(w.Header(), headers, true)
}
