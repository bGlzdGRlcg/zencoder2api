package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zencoder-2api/internal/model"
)

func sseFixture(value string) string { return strings.ReplaceAll(value, `\n`, "\n") }

func TestAnthropicThinkingNormalizationPreservesHistoryAndFinalBudget(t *testing.T) {
	svc := NewAnthropicService()
	disabled, err := svc.ensureThinkingConfig([]byte(`{"model":"claude-haiku-4-5","max_tokens":2048,"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"f","input":{}}]}]}`), "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	var disabledBody map[string]interface{}
	if err := json.Unmarshal(disabled, &disabledBody); err != nil {
		t.Fatal(err)
	}
	messages := disabledBody["messages"].([]interface{})
	message := messages[0].(map[string]interface{})
	block := message["content"].([]interface{})[0].(map[string]interface{})
	if message["role"] != "assistant" || block["type"] != "tool_use" {
		t.Fatalf("disabled thinking rewrote history: %s", disabled)
	}

	enabled, err := svc.ensureThinkingConfig([]byte(`{"model":"claude-haiku-4-5","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`), "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	var enabledBody map[string]interface{}
	if err := json.Unmarshal(enabled, &enabledBody); err != nil {
		t.Fatal(err)
	}
	thinking := enabledBody["thinking"].(map[string]interface{})
	if intValue(enabledBody["max_tokens"]) <= intValue(thinking["budget_tokens"]) {
		t.Fatalf("max_tokens does not exceed final catalog budget: %s", enabled)
	}
}

func TestAnthropicDisabledThinkingKeepsTemperature(t *testing.T) {
	svc := NewAnthropicService()
	body, err := svc.adjustParametersForModel([]byte(`{"model":"claude-haiku-4-5","temperature":0.2,"thinking":{"type":"disabled"}}`), "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"temperature":0.2`) {
		t.Fatalf("disabled thinking was forced to catalog temperature: %s", body)
	}
}

func TestChatToResponsesPreservesLegacyFunctionSemantics(t *testing.T) {
	body, err := NewOpenAIService().convertChatToResponsesBody([]byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"assistant","content":"","function_call":{"name":"lookup","arguments":"{}"}},
			{"role":"function","name":"lookup","content":"ok"}
		],
		"functions":[{"name":"lookup","parameters":{"type":"object"}}],
		"function_call":{"name":"lookup"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]interface{}
	if err := json.Unmarshal(body, &converted); err != nil {
		t.Fatal(err)
	}
	choice := converted["tool_choice"].(map[string]interface{})
	input := converted["input"].([]interface{})
	call := input[0].(map[string]interface{})
	result := input[1].(map[string]interface{})
	if choice["name"] != "lookup" || call["type"] != "function_call" || result["type"] != "function_call_output" || call["call_id"] != result["call_id"] {
		t.Fatalf("legacy function semantics were lost: %s", body)
	}
}

func TestResponsesGeminiFunctionChoiceAndSchemaConstraints(t *testing.T) {
	_, _, native, err := convertResponsesToGemini([]byte(`{
		"model":"gemini-3-flash-preview",
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"name":{"type":"string","minLength":2,"maxLength":8},"count":{"type":"number","minimum":1,"maximum":5}}}}],
		"tool_choice":{"type":"function","name":"lookup"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(native)
	for _, want := range []string{`"allowedFunctionNames":["lookup"]`, `"minLength":2`, `"maxLength":8`, `"minimum":1`, `"maximum":5`} {
		if !strings.Contains(text, want) {
			t.Fatalf("Gemini conversion dropped %s: %s", want, text)
		}
	}
}

func TestChatJSONToResponsesPreservesRefusalOnlyMessage(t *testing.T) {
	body, err := convertChatJSONToResponses([]byte(`{"id":"chat-1","created":1,"choices":[{"message":{"role":"assistant","content":"","refusal":"blocked"},"finish_reason":"content_filter"}]}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]interface{})
	if len(output) != 1 || !strings.Contains(string(body), `"type":"refusal"`) || !strings.Contains(string(body), `"refusal":"blocked"`) {
		t.Fatalf("refusal-only output was lost: %s", body)
	}
}

func TestChatStreamAsResponsesUsesAppendOnlyToolDeltas(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1,\"b\":2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	recorder := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsResponses(recorder, resp, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	var arguments strings.Builder
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil || event["type"] != "response.function_call_arguments.delta" {
			continue
		}
		arguments.WriteString(stringValue(event["delta"]))
	}
	if arguments.String() != `{"a":1,"b":2}` {
		t.Fatalf("tool argument deltas are not append-only: %q\n%s", arguments.String(), recorder.Body.String())
	}
}

func TestChatStreamAsResponsesPreservesRefusal(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`data: {"choices":[{"delta":{"refusal":"blocked"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	recorder := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsResponses(recorder, resp, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `response.refusal.delta`) || !strings.Contains(body, `"refusal":"blocked"`) || !strings.Contains(body, `event: response.incomplete`) || strings.Contains(body, `event: response.completed`) {
		t.Fatalf("refusal stream was not preserved: %s", body)
	}
}

func TestChatStreamAsAnthropicNormalizesCumulativeToolArguments(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	recorder := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsAnthropic(recorder, resp, "claude-haiku-4-5"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	var arguments strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		delta, _ := event["delta"].(map[string]interface{})
		if stringValue(delta["type"]) == "input_json_delta" {
			arguments.WriteString(stringValue(delta["partial_json"]))
		}
	}
	if arguments.String() != `{"a":1}` {
		t.Fatalf("cumulative tool arguments were not normalized: %s", body)
	}
}

func TestAnthropicStreamPreservesSignatureDelta(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`event: message_start\ndata: {"type":"message_start","message":{"id":"msg-1"}}`,
		`event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"why"}}`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
		`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`event: message_stop\ndata: {"type":"message_stop"}`,
		"",
	}, "\n\n"))
	resp := wrapAnthropicStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "claude-haiku-4-5")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_signature":"sig-1"`) {
		t.Fatalf("signature delta was lost: %s", body)
	}
}

func TestCrossProtocolStreamsFailClosedOnTruncatedEOF(t *testing.T) {
	t.Run("chat_to_responses", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sseFixture(`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}\n\n`)))}
		if err := streamChatAsResponses(recorder, resp, "grok-4.5"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(recorder.Body.String(), "response.failed") || strings.Contains(recorder.Body.String(), "response.completed") {
			t.Fatalf("truncated Chat stream was reported successful: %s", recorder.Body.String())
		}
	})

	t.Run("gemini_to_chat", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sseFixture(`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}\n\n`)))}
		if err := streamGeminiAsChat(recorder, resp, "gemini-3-flash-preview"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(recorder.Body.String(), "upstream_protocol_error") {
			t.Fatalf("truncated Gemini stream was reported successful: %s", recorder.Body.String())
		}
	})

	t.Run("responses_to_chat", func(t *testing.T) {
		stream := sseFixture(`event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"partial"}\n\n`)
		resp := wrapResponsesStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "gpt-5.4")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "upstream_error") || !strings.Contains(string(body), "[DONE]") {
			t.Fatalf("truncated Responses stream was not failed: %s", body)
		}
	})
}

func TestFilteredAnthropicResponseUsesSafeHeaders(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Set-Cookie":       {"secret=1"},
			"Connection":       {"keep-alive"},
			"Content-Encoding": {"gzip"},
			"Zen-Request-Cost": {"1"},
		},
		Body: io.NopCloser(strings.NewReader(`{"content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"ok"}]}`)),
	}
	recorder := httptest.NewRecorder()
	if err := NewAnthropicService().handleNonStreamFilteredResponse(recorder, resp); err != nil {
		t.Fatal(err)
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("Connection") != "" || recorder.Header().Get("Content-Encoding") != "" || recorder.Header().Get("Zen-Request-Cost") != "1" {
		t.Fatalf("unsafe or stale upstream headers leaked: %#v", recorder.Header())
	}
}

func TestDoneSentinelWithoutFinishDoesNotComplete(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := sseFixture(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamGeminiAsResponses(recorder, resp, "gemini-3-flash-preview"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "response.failed") || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("done sentinel was treated as success: %s", recorder.Body.String())
	}
}

func TestChatFinishWithoutDoneSentinelDoesNotComplete(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := sseFixture(`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"stop"}]}

`)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsResponses(recorder, resp, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "response.failed") || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("finish_reason without done sentinel was treated as success: %s", recorder.Body.String())
	}
}

func TestAnthropicMessageDeltaWithoutStopFailsClosed(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`event: message_start\ndata: {"type":"message_start","message":{"id":"msg-1"}}`,
		`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"",
	}, "\n\n"))
	resp := wrapAnthropicStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "gpt-5.4")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "upstream_protocol_error") || strings.Contains(string(body), "finish_reason\\\":\\\"stop") {
		t.Fatalf("Anthropic message_delta without message_stop was accepted: %s", body)
	}
}

func TestGeminiResponsesReusesUnindexedFunctionCall(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"a":1}}}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"a":1,"b":2}}}]},"finishReason":"STOP"}]}`,
		"",
	}, "\n\n"))
	recorder := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamGeminiAsResponses(recorder, resp, "gemini-3-flash-preview"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"arguments":"{\"a\":1,\"b\":2}"`) || strings.Count(body, `event: response.output_item.added`) != 1 || strings.Count(body, `event: response.output_item.done`) != 1 {
		t.Fatalf("unindexed Gemini function call was split or lost: %s", body)
	}
}

func TestGeminiResponsesStreamFailsClosedOnReasoning(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{
			name:   "gemini_thinking",
			stream: `data: {"candidates":[{"content":{"parts":[{"text":"why","thought":true}]},"finishReason":"STOP"}]}\n\n`,
		},
		{
			name:   "gemini_signature",
			stream: `data: {"candidates":[{"content":{"parts":[{"thoughtSignature":"sig-1"}]},"finishReason":"STOP"}]}\n\n`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(sseFixture(test.stream))),
			}
			if err := streamGeminiAsResponses(recorder, resp, "gemini-3-flash-preview"); err != nil {
				t.Fatal(err)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `event: response.failed`) || strings.Contains(body, `event: response.completed`) {
				t.Fatalf("reasoning/signature was silently accepted: %s", body)
			}
		})
	}
}

func TestResponsesFunctionCallFallsBackToItemID(t *testing.T) {
	body, err := convertResponsesJSONToChat([]byte(`{
		"id":"resp-1","status":"completed",
		"output":[{"type":"function_call","id":"fc-1","name":"lookup","arguments":"{}"}]
	}`), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]interface{}
	if err := json.Unmarshal(body, &converted); err != nil {
		t.Fatal(err)
	}
	choice := converted["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	call := message["tool_calls"].([]interface{})[0].(map[string]interface{})
	if call["id"] != "fc-1" {
		t.Fatalf("Responses item ID was not used as the tool call ID: %s", body)
	}
}

func TestGeminiFinishReasonsNeverInventToolCalls(t *testing.T) {
	for _, reason := range []string{"MALFORMED_FUNCTION_CALL", "OTHER", "UNKNOWN_FUTURE_REASON"} {
		if got := geminiFinishReason(reason); got == "tool_calls" || (got != "stop" && got != "content_filter") {
			t.Errorf("Gemini finish reason %q mapped to invalid Chat reason %q", reason, got)
		}
	}
	for _, reason := range []string{"SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "RECITATION"} {
		if got := geminiFinishReason(reason); got != "content_filter" {
			t.Errorf("Gemini safety reason %q mapped to %q", reason, got)
		}
	}
}

func TestResponsesStreamFunctionCallFallsBackToItemID(t *testing.T) {
	stream := sseFixture(strings.Join([]string{
		`event: response.output_item.added\ndata: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc-stream-1","name":"lookup"}}`,
		`event: response.completed\ndata: {"type":"response.completed","response":{"status":"completed"}}`,
		"",
	}, "\n\n"))
	resp := wrapResponsesStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "gpt-5.4")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"id":"fc-stream-1"`) {
		t.Fatalf("Responses stream item ID was not used as the tool call ID: %s", body)
	}
}

func TestResponsesCompatibilityFailsClosedOnStreamedReasoning(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := sseFixture(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"secret-thought","reasoning_signature":"sig"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsResponses(recorder, resp, "gpt-5.4"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "response.failed") || strings.Contains(body, "response.completed") || strings.Contains(body, "secret-thought") {
		t.Fatalf("streamed reasoning was silently lost or exposed as success: %s", body)
	}
}

func TestCrossProtocolStreamUsageIsPreserved(t *testing.T) {
	t.Run("responses_to_chat", func(t *testing.T) {
		stream := sseFixture(`event: response.completed\ndata: {"type":"response.completed","response":{"id":"resp-usage","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}\n\n`)
		resp := wrapResponsesStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "gpt-5.4")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"choices":[]`, `"prompt_tokens":3`, `"completion_tokens":5`, `"cached_tokens":2`, `"reasoning_tokens":1`} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("Responses usage lost %s: %s", want, body)
			}
		}
	})

	t.Run("anthropic_to_chat", func(t *testing.T) {
		stream := sseFixture(strings.Join([]string{
			`event: message_start\ndata: {"type":"message_start","message":{"id":"msg-usage","usage":{"input_tokens":4,"cache_read_input_tokens":2}}}`,
			`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`,
			`event: message_stop\ndata: {"type":"message_stop"}`,
			"",
		}, "\n\n"))
		resp := wrapAnthropicStreamAsChat(&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, "claude-haiku-4-5")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"choices":[]`, `"prompt_tokens":4`, `"completion_tokens":6`, `"total_tokens":10`, `"cached_tokens":2`} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("Anthropic usage lost %s: %s", want, body)
			}
		}
	})
}

func TestGeminiPromptBlockMapsToRefusal(t *testing.T) {
	body, err := convertGeminiResponseToChat([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`), "gemini-3-flash-preview")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"refusal":"SAFETY"`) || !strings.Contains(string(body), `"finish_reason":"content_filter"`) {
		t.Fatalf("Gemini prompt block was not mapped to refusal: %s", body)
	}
	recorder := httptest.NewRecorder()
	stream := sseFixture("data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\\n\\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamGeminiAsChat(recorder, resp, "gemini-3-flash-preview"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), `"refusal":"SAFETY"`) || !strings.Contains(recorder.Body.String(), `"finish_reason":"content_filter"`) {
		t.Fatalf("Gemini prompt block stream was not mapped to refusal: %s", recorder.Body.String())
	}
}

func TestGeminiChatStreamPreservesThoughtSignature(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := sseFixture(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"why","thought":true,"thoughtSignature":"sig-1"}]},"finishReason":"STOP"}]}`,
		"",
	}, "\n\n"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamGeminiAsChat(recorder, resp, "gemini-3-flash-preview"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"reasoning_content":"why"`) || !strings.Contains(body, `"reasoning_signature":"sig-1"`) {
		t.Fatalf("Gemini thought signature was lost from Chat stream: %s", body)
	}
}

func TestChatJSONToResponsesRejectsNonStreamedSignatures(t *testing.T) {
	tests := map[string]string{
		"reasoning": `{"choices":[{"message":{"reasoning_content":"why","reasoning_signature":"sig-1"}}]}`,
		"tool":      `{"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig-1"}}}]}}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := convertChatJSONToResponses([]byte(body), "gemini-3-flash-preview"); err == nil || !strings.Contains(err.Error(), "signature") {
				t.Fatalf("expected explicit signature rejection, got %v", err)
			}
		})
	}
}

func TestStrictToolsArePreservedOrRejectedExplicitly(t *testing.T) {
	request := []byte(`{"model":"grok-4.5","input":"x","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}]}`)
	chatBody, _, _, err := convertResponsesToChat(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatBody), `"strict":true`) {
		t.Fatalf("Responses strict tool was not preserved for Chat: %s", chatBody)
	}

	if _, _, _, err := convertResponsesToGemini([]byte(strings.ReplaceAll(string(request), "grok-4.5", "gemini-3-flash-preview"))); err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("expected Gemini strict rejection, got %v", err)
	}
	if _, err := convertChatToAnthropicBody([]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"},"strict":true}}]}`)); err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("expected Anthropic strict rejection, got %v", err)
	}
}

func TestResponsesFileIDIsRejectedInsteadOfDropped(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file-1"}]}]}`)
	if _, _, _, err := convertResponsesToChat(body); err == nil || !strings.Contains(err.Error(), "file_id") {
		t.Fatalf("expected Chat file_id rejection, got %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(body), "grok-4.5", "claude-haiku-4-5")), &raw); err != nil {
		t.Fatal(err)
	}
	if err := validateResponsesForAnthropic(raw); err == nil || !strings.Contains(err.Error(), "file_id") {
		t.Fatalf("expected Anthropic file_id rejection, got %v", err)
	}
}

func TestAnthropicThinkingToGeminiPreservesSignatureAndRejectsRedacted(t *testing.T) {
	chatBody, _, _, err := convertAnthropicMessagesToChat([]byte(`{"model":"gemini-3-flash-preview","max_tokens":256,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"why","signature":"sig-a"},{"type":"text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, native, err := convertOpenAIChatToGemini(chatBody)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(native), `"thoughtSignature":"sig-a"`) {
		t.Fatalf("Anthropic thinking signature was not mapped to Gemini: %s", native)
	}

	redactedChat, _, _, err := convertAnthropicMessagesToChat([]byte(`{"model":"gemini-3-flash-preview","max_tokens":256,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := convertOpenAIChatToGemini(redactedChat); err == nil || !strings.Contains(err.Error(), "redacted_thinking") {
		t.Fatalf("expected redacted thinking rejection, got %v", err)
	}
}

func TestGeminiFlatToolChoiceForcesNamedFunction(t *testing.T) {
	_, _, native, err := convertOpenAIChatToGemini([]byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","name":"lookup"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]interface{}
	if err := json.Unmarshal(native, &converted); err != nil {
		t.Fatal(err)
	}
	toolConfig := converted["toolConfig"].(map[string]interface{})
	calling := toolConfig["functionCallingConfig"].(map[string]interface{})
	allowed := calling["allowedFunctionNames"].([]interface{})
	if calling["mode"] != "ANY" || len(allowed) != 1 || allowed[0] != "lookup" {
		t.Fatalf("flat tool choice did not force lookup: %s", native)
	}
}

func TestGrokCodeTemperatureIsForcedAcrossCompatibilityPaths(t *testing.T) {
	zenModel, ok := model.GetZenModel("grok-code-fast")
	if !ok {
		t.Fatal("grok-code-fast model missing")
	}
	body, err := prepareGatewayRequestBody([]byte(`{"model":"grok-code-fast","messages":[{"role":"user","content":"x"}],"temperature":0.8}`), "grok-code-fast", "/v1/chat/completions", zenModel)
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]interface{}
	if err := json.Unmarshal(body, &converted); err != nil {
		t.Fatal(err)
	}
	if intValue(converted["temperature"]) != 0 {
		t.Fatalf("grok-code temperature was not forced to zero: %s", body)
	}
}

func TestAnthropicDisableParallelToolUseMapsOrFailsClosed(t *testing.T) {
	chatBody, _, _, err := convertAnthropicMessagesToChat([]byte(`{"model":"gpt-5.4","max_tokens":256,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatBody), `"parallel_tool_calls":false`) {
		t.Fatalf("disable_parallel_tool_use was not mapped: %s", chatBody)
	}
	geminiBody := []byte(strings.ReplaceAll(string(chatBody), "gpt-5.4", "gemini-3-flash-preview"))
	if _, _, _, err := convertOpenAIChatToGemini(geminiBody); err == nil || !strings.Contains(err.Error(), "parallel_tool_calls") {
		t.Fatalf("expected Gemini parallel tool rejection, got %v", err)
	}
}

func TestGeminiUsageDetailsArePreserved(t *testing.T) {
	native := `{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8,"cachedContentTokenCount":2,"thoughtsTokenCount":1}}`
	body, err := convertGeminiResponseToChat([]byte(native), "gemini-3-flash-preview")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cached_tokens":2`, `"reasoning_tokens":1`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("non-stream Gemini usage lost %s: %s", want, body)
		}
	}

	recorder := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: " + native + "\n\n"))}
	if err := streamGeminiAsChat(recorder, resp, "gemini-3-flash-preview"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cached_tokens":2`, `"reasoning_tokens":1`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("stream Gemini usage lost %s: %s", want, recorder.Body.String())
		}
	}
}

func TestResponsesVerbosityIsRejectedForChatCompatibility(t *testing.T) {
	_, _, _, err := convertResponsesToChat([]byte(`{"model":"grok-4.5","input":"x","text":{"verbosity":"low"}}`))
	if err == nil || !strings.Contains(err.Error(), "verbosity") {
		t.Fatalf("expected verbosity rejection, got %v", err)
	}
}

func TestChatStreamAsResponsesPreservesMixedTextAndRefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := sseFixture(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"refusal":"blocked"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := streamChatAsResponses(recorder, resp, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	var terminal map[string]interface{}
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "response.incomplete" {
			terminal = event
		}
	}
	if terminal == nil {
		t.Fatalf("missing terminal Responses event: %s", recorder.Body.String())
	}
	response := terminal["response"].(map[string]interface{})
	output := response["output"].([]interface{})
	message := output[0].(map[string]interface{})
	content := message["content"].([]interface{})
	if len(content) != 2 || content[0].(map[string]interface{})["type"] != "output_text" || content[1].(map[string]interface{})["type"] != "refusal" {
		t.Fatalf("mixed text/refusal parts were not preserved: %s", recorder.Body.String())
	}
}
