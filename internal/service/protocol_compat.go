package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

func openAICompatibilityError(err error) error {
	body, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": err.Error(), "type": "invalid_request_error"}})
	return &UpstreamError{StatusCode: http.StatusBadRequest, Body: body}
}

func rejectUnsupportedFields(raw map[string]interface{}, protocol string, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	unsupported := make([]string, 0)
	for key, value := range raw {
		if value == nil {
			continue
		}
		if _, ok := allow[key]; !ok {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return fmt.Errorf("%s compatibility path cannot preserve field(s): %s", protocol, strings.Join(unsupported, ", "))
}

func rejectPresent(raw map[string]interface{}, protocol string, fields ...string) error {
	for _, field := range fields {
		if value, ok := raw[field]; ok && value != nil {
			return fmt.Errorf("%s compatibility path cannot preserve field %q", protocol, field)
		}
	}
	return nil
}

func requireString(raw map[string]interface{}, field string) (string, error) {
	value, ok := raw[field].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func requireJSONArray(raw map[string]interface{}, field string) ([]interface{}, error) {
	value, ok := raw[field].([]interface{})
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func validateStringList(value interface{}, field string) error {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return fmt.Errorf("%s must not be empty", field)
		}
		return nil
	case []interface{}:
		for index, item := range typed {
			if text, ok := item.(string); !ok || text == "" {
				return fmt.Errorf("%s[%d] must be a non-empty string", field, index)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s must be a string or array of strings", field)
	}
}

func validateGeminiSchema(value interface{}, path string) error {
	switch schema := value.(type) {
	case map[string]interface{}:
		allowed := map[string]struct{}{
			"type": {}, "format": {}, "title": {}, "description": {},
			"nullable": {}, "enum": {}, "maxItems": {}, "minItems": {},
			"maxLength": {}, "minLength": {}, "maximum": {}, "minimum": {},
			"required": {}, "propertyOrdering": {}, "properties": {}, "items": {},
		}
		for key, item := range schema {
			if _, ok := allowed[key]; !ok {
				return fmt.Errorf("%s contains unsupported JSON Schema keyword %q", path, key)
			}
			switch key {
			case "properties":
				properties, ok := item.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%s.properties must be an object", path)
				}
				for name, property := range properties {
					if err := validateGeminiSchema(property, path+".properties."+name); err != nil {
						return err
					}
				}
			case "items":
				if err := validateGeminiSchema(item, path+".items"); err != nil {
					return err
				}
			}
		}
		return nil
	case bool:
		return fmt.Errorf("%s boolean schemas are not supported by Gemini", path)
	default:
		return fmt.Errorf("%s must be a JSON Schema object", path)
	}
}

func geminiSchema(value interface{}) (interface{}, error) {
	if err := validateGeminiSchema(value, "schema"); err != nil {
		return nil, err
	}
	return sanitizeGeminiSchema(value), nil
}

func jsonText(value interface{}) (string, error) {
	if text, ok := value.(string); ok {
		if !json.Valid([]byte(text)) {
			return "", fmt.Errorf("tool arguments must be valid JSON")
		}
		return text, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func cumulativeJSONDelta(previous, current string) (string, error) {
	if current == previous {
		return "", nil
	}
	if strings.HasPrefix(current, previous) {
		return strings.TrimPrefix(current, previous), nil
	}
	if previous == "" {
		return current, nil
	}
	return "", fmt.Errorf("upstream replaced streamed tool arguments; append-only target protocol cannot preserve the change")
}

func validateGeminiChatMessages(value interface{}) error {
	messages, ok := value.([]interface{})
	if !ok || len(messages) == 0 {
		return fmt.Errorf("messages is required")
	}
	for messageIndex, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d] must be an object", messageIndex)
		}
		if err := rejectUnsupportedFields(message, fmt.Sprintf("Gemini messages[%d]", messageIndex), "role", "content", "name", "tool_call_id", "call_id", "tool_calls", "function_call", "reasoning_content", "reasoning_signature", "reasoning_details", "is_error"); err != nil {
			return err
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return fmt.Errorf("messages[%d].role %q cannot be represented by Gemini", messageIndex, role)
		}
		if err := validateGeminiContent(message["content"], fmt.Sprintf("messages[%d].content", messageIndex)); err != nil {
			return err
		}
		if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
			if signature := stringValue(message["reasoning_signature"]); signature == "" {
				return fmt.Errorf("messages[%d].reasoning_content requires reasoning_signature for Gemini", messageIndex)
			}
		}
		if details, ok := message["reasoning_details"].([]interface{}); ok {
			for detailIndex, item := range details {
				detail, ok := item.(map[string]interface{})
				if !ok {
					return fmt.Errorf("messages[%d].reasoning_details[%d] must be an object", messageIndex, detailIndex)
				}
				switch stringValue(detail["type"]) {
				case "thinking":
					if stringValue(detail["thinking"]) == "" || stringValue(detail["signature"]) == "" {
						return fmt.Errorf("messages[%d].reasoning_details[%d] thinking requires thinking and signature", messageIndex, detailIndex)
					}
				case "redacted_thinking":
					return fmt.Errorf("messages[%d].reasoning_details[%d] redacted_thinking cannot be represented by Gemini", messageIndex, detailIndex)
				default:
					return fmt.Errorf("messages[%d].reasoning_details[%d] type %q cannot be represented by Gemini", messageIndex, detailIndex, stringValue(detail["type"]))
				}
			}
		}
		if calls, ok := message["tool_calls"].([]interface{}); ok {
			for callIndex, item := range calls {
				call, ok := item.(map[string]interface{})
				if !ok {
					return fmt.Errorf("messages[%d].tool_calls[%d] must be an object", messageIndex, callIndex)
				}
				function, ok := call["function"].(map[string]interface{})
				if !ok || stringValue(function["name"]) == "" {
					return fmt.Errorf("messages[%d].tool_calls[%d].function.name is required", messageIndex, callIndex)
				}
				if _, err := jsonText(function["arguments"]); err != nil {
					return fmt.Errorf("messages[%d].tool_calls[%d]: %w", messageIndex, callIndex, err)
				}
			}
		}
	}
	return nil
}

func validateGeminiContent(value interface{}, path string) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or content array", path)
	}
	for index, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		partType := stringValue(part["type"])
		switch partType {
		case "text", "input_text":
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("%s[%d].text must be a string", path, index)
			}
		case "image_url":
			var source string
			switch imageURL := part["image_url"].(type) {
			case string:
				source = imageURL
			case map[string]interface{}:
				source = stringValue(imageURL["url"])
			}
			if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
				return fmt.Errorf("%s[%d] remote images cannot be transferred losslessly to Gemini; use a data URL", path, index)
			}
			if !strings.HasPrefix(source, "data:") {
				return fmt.Errorf("%s[%d] image_url must be a data URL", path, index)
			}
		case "image", "inline_data", "inlineData":
			if geminiInlineImage(part) == nil {
				return fmt.Errorf("%s[%d] contains an invalid inline image", path, index)
			}
		default:
			return fmt.Errorf("%s[%d] content type %q cannot be represented by Gemini", path, index, partType)
		}
	}
	return nil
}

func validateGeminiTools(tools []interface{}) error {
	for index, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok || stringValue(tool["type"]) != "function" {
			return fmt.Errorf("tools[%d] must be a function tool", index)
		}
		function, ok := tool["function"].(map[string]interface{})
		if !ok || stringValue(function["name"]) == "" {
			return fmt.Errorf("tools[%d].function.name is required", index)
		}
		if _, exists := function["strict"]; exists && function["strict"] != nil {
			return fmt.Errorf("tools[%d].function.strict cannot be represented by Gemini", index)
		}
		if schema, ok := function["parameters"]; ok {
			if err := validateGeminiSchema(schema, fmt.Sprintf("tools[%d].function.parameters", index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateGeminiToolChoice(value interface{}) error {
	if value == nil {
		return nil
	}
	switch choice := value.(type) {
	case string:
		if choice != "none" && choice != "auto" && choice != "required" {
			return fmt.Errorf("tool_choice %q cannot be represented by Gemini", choice)
		}
	case map[string]interface{}:
		if function, ok := choice["function"].(map[string]interface{}); ok && stringValue(function["name"]) != "" {
			return nil
		}
		if stringValue(choice["type"]) != "function" || stringValue(choice["name"]) == "" {
			return fmt.Errorf("tool_choice function name is required")
		}
	default:
		return fmt.Errorf("tool_choice must be a string or function object")
	}
	return nil
}

func validateChatForAnthropic(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Anthropic Chat", "model", "messages", "stream", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "tools", "tool_choice", "parallel_tool_calls"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	messages, err := requireJSONArray(raw, "messages")
	if err != nil {
		return err
	}
	for messageIndex, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d] must be an object", messageIndex)
		}
		if err := rejectUnsupportedFields(message, fmt.Sprintf("Anthropic messages[%d]", messageIndex), "role", "content", "name", "tool_call_id", "call_id", "tool_calls", "function_call", "is_error", "reasoning_content", "reasoning_signature", "reasoning_details"); err != nil {
			return err
		}
		role := stringValue(message["role"])
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return fmt.Errorf("messages[%d].role %q cannot be represented by Anthropic", messageIndex, role)
		}
		if err := validateAnthropicContent(message["content"], fmt.Sprintf("messages[%d].content", messageIndex)); err != nil {
			return err
		}
		if (role == "tool" || role == "function") && stringValue(message["tool_call_id"]) == "" && stringValue(message["call_id"]) == "" && stringValue(message["name"]) == "" {
			return fmt.Errorf("messages[%d] tool_call_id is required", messageIndex)
		}
		if reasoning := stringValue(message["reasoning_content"]); reasoning != "" && stringValue(message["reasoning_signature"]) == "" {
			if _, ok := message["reasoning_details"].([]interface{}); !ok {
				return fmt.Errorf("messages[%d] reasoning_content requires reasoning_signature or reasoning_details", messageIndex)
			}
		}
	}
	if stop, ok := raw["stop"]; ok {
		if err := validateStringList(stop, "stop"); err != nil {
			return err
		}
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		for index, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok || stringValue(tool["type"]) != "function" {
				return fmt.Errorf("tools[%d] must be a function tool", index)
			}
			function, ok := tool["function"].(map[string]interface{})
			if !ok || stringValue(function["name"]) == "" {
				return fmt.Errorf("tools[%d].function.name is required", index)
			}
			if _, exists := function["strict"]; exists && function["strict"] != nil {
				return fmt.Errorf("tools[%d].function.strict cannot be represented by Anthropic", index)
			}
		}
	}
	return nil
}

func validateAnthropicContent(value interface{}, path string) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or content array", path)
	}
	for index, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(part["type"]) {
		case "text", "input_text":
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("%s[%d].text must be a string", path, index)
			}
		case "image_url", "image", "document":
		default:
			return fmt.Errorf("%s[%d] content type %q cannot be represented by Anthropic", path, index, stringValue(part["type"]))
		}
	}
	return nil
}

func validateAnthropicRequestForChat(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Anthropic compatibility", "model", "messages", "system", "max_tokens", "stream", "temperature", "top_p", "stop_sequences", "tools", "tool_choice", "top_k", "thinking"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	messages, err := requireJSONArray(raw, "messages")
	if err != nil {
		return err
	}
	for index, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d] must be an object", index)
		}
		if err := rejectUnsupportedFields(message, fmt.Sprintf("Anthropic messages[%d]", index), "role", "content"); err != nil {
			return err
		}
		if err := validateAnthropicBlocks(message["content"], fmt.Sprintf("messages[%d].content", index)); err != nil {
			return err
		}
	}
	if thinking, ok := raw["thinking"].(map[string]interface{}); ok {
		typ := stringValue(thinking["type"])
		if typ != "enabled" && typ != "disabled" {
			return fmt.Errorf("anthropic thinking type %q cannot be represented by OpenAI/Gemini", typ)
		}
		if typ == "enabled" {
			budget, ok := thinking["budget_tokens"]
			if !ok || intValue(budget) < 1 {
				return fmt.Errorf("thinking.budget_tokens is required when thinking is enabled")
			}
		}
	}
	if err := validateAnthropicToolChoice(raw["tool_choice"]); err != nil {
		return err
	}
	return nil
}

func validateAnthropicToolChoice(value interface{}) error {
	if value == nil {
		return nil
	}
	choice, ok := value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("anthropic tool_choice must be an object")
	}
	typeName := stringValue(choice["type"])
	if typeName != "auto" && typeName != "any" && typeName != "tool" {
		return fmt.Errorf("anthropic tool_choice type %q cannot be represented", typeName)
	}
	if disabled, exists := choice["disable_parallel_tool_use"]; exists && disabled != nil {
		if _, ok := disabled.(bool); !ok {
			return fmt.Errorf("anthropic tool_choice.disable_parallel_tool_use must be boolean")
		}
	}
	if typeName == "tool" && stringValue(choice["name"]) == "" {
		return fmt.Errorf("anthropic tool_choice.name is required for type tool")
	}
	return nil
}

func validateAnthropicBlocks(value interface{}, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or block array", path)
	}
	for index, item := range items {
		block, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(block["type"]) {
		case "text", "input_text", "output_text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking":
		case "image_url", "input_image":
			if fileID, exists := block["file_id"]; exists && fileID != nil {
				return fmt.Errorf("%s[%d].file_id cannot be represented by the compatibility path", path, index)
			}
			if block["image_url"] == nil && block["url"] == nil {
				return fmt.Errorf("%s[%d] %s requires image_url or url", path, index, stringValue(block["type"]))
			}
		default:
			return fmt.Errorf("%s[%d] block type %q cannot be represented", path, index, stringValue(block["type"]))
		}
	}
	return nil
}

func validateAnthropicForOpenAI(raw map[string]interface{}) error {
	if err := rejectPresent(raw, "OpenAI Anthropic", "top_k", "thinking"); err != nil {
		return err
	}
	if err := validateAnthropicToolChoice(raw["tool_choice"]); err != nil {
		return err
	}
	messages, _ := raw["messages"].([]interface{})
	for messageIndex, item := range messages {
		message, _ := item.(map[string]interface{})
		for blockIndex, block := range anthropicMessageBlocks(message["content"]) {
			switch stringValue(block["type"]) {
			case "thinking", "redacted_thinking":
				return fmt.Errorf("OpenAI Anthropic compatibility path cannot preserve messages[%d].content[%d] %s block", messageIndex, blockIndex, stringValue(block["type"]))
			case "tool_result":
				if isError, _ := block["is_error"].(bool); isError {
					return fmt.Errorf("OpenAI Anthropic compatibility path cannot preserve messages[%d].content[%d].is_error", messageIndex, blockIndex)
				}
			}
		}
	}
	return nil
}

func validateResponsesForGemini(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Gemini Responses", "model", "input", "instructions", "max_output_tokens", "tools", "tool_choice", "temperature", "top_p", "stream", "text", "reasoning"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	if _, ok := raw["input"]; !ok {
		return fmt.Errorf("input is required")
	}
	if reasoning, ok := raw["reasoning"].(map[string]interface{}); ok {
		if summary := reasoning["summary"]; summary != nil {
			return fmt.Errorf("Responses reasoning.summary cannot be represented by Gemini")
		}
		effort := stringValue(reasoning["effort"])
		if effort != "" && effort != "minimal" && effort != "low" && effort != "medium" && effort != "high" {
			return fmt.Errorf("Responses reasoning.effort %q cannot be represented by Gemini", effort)
		}
	}
	if textValue, exists := raw["text"]; exists && textValue != nil {
		text, ok := textValue.(map[string]interface{})
		if !ok {
			return fmt.Errorf("Gemini Responses text must be an object")
		}
		if err := rejectUnsupportedFields(text, "Gemini Responses text", "format"); err != nil {
			return err
		}
		if formatValue, exists := text["format"]; exists && formatValue != nil {
			format, ok := formatValue.(map[string]interface{})
			if !ok {
				return fmt.Errorf("Gemini Responses text.format must be an object")
			}
			typ := stringValue(format["type"])
			if typ != "text" && typ != "json_object" && typ != "json_schema" {
				return fmt.Errorf("Responses text.format.type %q cannot be represented by Gemini", typ)
			}
			if schema, ok := format["schema"]; ok {
				if err := validateGeminiSchema(schema, "text.format.schema"); err != nil {
					return err
				}
			}
		}
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		if err := validateResponsesToolsForGemini(tools); err != nil {
			return err
		}
	}
	if err := validateGeminiToolChoice(raw["tool_choice"]); err != nil {
		return err
	}
	return validateResponsesInputForGemini(raw["input"])
}

func validateResponsesToolsForGemini(tools []interface{}) error {
	for index, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok || stringValue(tool["type"]) != "function" {
			return fmt.Errorf("Responses tools[%d] built-in tool cannot be represented by Gemini", index)
		}
		if _, exists := tool["strict"]; exists && tool["strict"] != nil {
			return fmt.Errorf("Responses tools[%d].strict cannot be represented by Gemini", index)
		}
		if stringValue(tool["name"]) == "" {
			return fmt.Errorf("Responses tools[%d].name is required", index)
		}
		if schema, ok := tool["parameters"]; ok {
			if err := validateGeminiSchema(schema, fmt.Sprintf("tools[%d].parameters", index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateResponsesInputForGemini(value interface{}) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("input must be a string or item array")
	}
	for index, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("input[%d] must be an object", index)
		}
		typ := stringValue(entry["type"])
		switch typ {
		case "message":
			if err := validateResponsesContentForGemini(entry["content"], fmt.Sprintf("input[%d].content", index)); err != nil {
				return err
			}
		case "function_call":
			if stringValue(entry["name"]) == "" {
				return fmt.Errorf("input[%d].name is required", index)
			}
		case "function_call_output":
			if stringValue(entry["call_id"]) == "" {
				return fmt.Errorf("input[%d].call_id is required", index)
			}
		case "reasoning":
			return fmt.Errorf("input[%d] reasoning item cannot be represented by Gemini", index)
		default:
			return fmt.Errorf("input[%d] type %q cannot be represented by Gemini", index, typ)
		}
	}
	return nil
}

func validateResponsesContentForGemini(value interface{}, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or content array", path)
	}
	for index, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(part["type"]) {
		case "input_text", "output_text", "text":
		case "input_image", "image_url", "image":
			url := stringValue(part["image_url"])
			if url == "" {
				url = stringValue(part["url"])
			}
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				return fmt.Errorf("%s[%d] remote images cannot be transferred losslessly to Gemini", path, index)
			}
			if !strings.HasPrefix(url, "data:") {
				return fmt.Errorf("%s[%d] image must be a data URL", path, index)
			}
		default:
			return fmt.Errorf("%s[%d] type %q cannot be represented by Gemini", path, index, stringValue(part["type"]))
		}
	}
	return nil
}

func validateResponsesForAnthropic(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Anthropic Responses", "model", "input", "instructions", "max_output_tokens", "tools", "tool_choice", "temperature", "top_p", "stream"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	if _, ok := raw["input"]; !ok {
		return fmt.Errorf("input is required")
	}
	if err := validateResponsesInputForAnthropic(raw["input"]); err != nil {
		return err
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		for index, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok || stringValue(tool["type"]) != "function" {
				return fmt.Errorf("anthropic Responses tools[%d] built-in tool cannot be represented", index)
			}
			if _, exists := tool["strict"]; exists && tool["strict"] != nil {
				return fmt.Errorf("anthropic Responses tools[%d].strict cannot be represented", index)
			}
		}
	}
	if value := raw["reasoning"]; value != nil {
		return fmt.Errorf("responses reasoning cannot be represented by Anthropic without losing thinking budget/signature semantics")
	}
	return nil
}

func validateResponsesForChat(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Chat Responses", "model", "input", "instructions", "max_output_tokens", "tools", "tool_choice", "temperature", "top_p", "stream", "text", "reasoning", "parallel_tool_calls"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	if _, ok := raw["input"]; !ok {
		return fmt.Errorf("input is required")
	}
	if err := validateResponsesInputForChat(raw["input"]); err != nil {
		return err
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		for index, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok || stringValue(tool["type"]) != "function" || stringValue(tool["name"]) == "" {
				return fmt.Errorf("chat Responses tools[%d] built-in or malformed tool cannot be represented", index)
			}
		}
	}
	if reasoning, ok := raw["reasoning"].(map[string]interface{}); ok && reasoning["summary"] != nil {
		return fmt.Errorf("chat Responses reasoning.summary cannot be represented")
	}
	if textValue, exists := raw["text"]; exists && textValue != nil {
		text, ok := textValue.(map[string]interface{})
		if !ok {
			return fmt.Errorf("Chat Responses text must be an object")
		}
		if err := rejectUnsupportedFields(text, "Chat Responses text", "format"); err != nil {
			return err
		}
		if formatValue, exists := text["format"]; exists && formatValue != nil {
			if _, ok := formatValue.(map[string]interface{}); !ok {
				return fmt.Errorf("Chat Responses text.format must be an object")
			}
		}
	}
	return nil
}

func validateResponsesInputForChat(value interface{}) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("input must be a string or item array")
	}
	for index, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("input[%d] must be an object", index)
		}
		switch typ := stringValue(entry["type"]); typ {
		case "message":
			if err := validateResponsesChatContent(entry["content"], fmt.Sprintf("input[%d].content", index)); err != nil {
				return err
			}
		case "function_call":
			if stringValue(entry["name"]) == "" {
				return fmt.Errorf("input[%d].name is required", index)
			}
		case "function_call_output":
			if stringValue(entry["call_id"]) == "" {
				return fmt.Errorf("input[%d].call_id is required", index)
			}
		case "reasoning":
			return fmt.Errorf("input[%d] reasoning item cannot be represented by Chat Completions", index)
		default:
			return fmt.Errorf("input[%d] type %q cannot be represented by Chat Completions", index, typ)
		}
	}
	return nil
}

func validateResponsesChatContent(value interface{}, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or content array", path)
	}
	for index, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(part["type"]) {
		case "input_text", "output_text", "text", "input_image", "image_url", "image":
			if partType := stringValue(part["type"]); partType == "input_image" {
				if fileID, exists := part["file_id"]; exists && fileID != nil {
					return fmt.Errorf("%s[%d].file_id cannot be represented by Chat Completions", path, index)
				}
				if part["image_url"] == nil && part["url"] == nil {
					return fmt.Errorf("%s[%d] input_image requires image_url or url", path, index)
				}
			}
		default:
			return fmt.Errorf("%s[%d] type %q cannot be represented by Chat Completions", path, index, stringValue(part["type"]))
		}
	}
	return nil
}

func validateResponsesInputForAnthropic(value interface{}) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("input must be a string or item array")
	}
	for index, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("input[%d] must be an object", index)
		}
		switch typ := stringValue(entry["type"]); typ {
		case "message":
			if err := validateAnthropicBlocks(entry["content"], fmt.Sprintf("input[%d].content", index)); err != nil {
				return err
			}
		case "function_call", "function_call_output":
		default:
			return fmt.Errorf("input[%d] type %q cannot be represented by Anthropic", index, typ)
		}
	}
	return nil
}
