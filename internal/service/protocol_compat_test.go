package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatToAnthropicRejectsLossyFieldsAndMergesTools(t *testing.T) {
	if _, err := convertChatToAnthropicBody([]byte(`{"model":"claude","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`)); err == nil {
		t.Fatal("expected response_format rejection")
	}
	body, err := convertChatToAnthropicBody([]byte(`{"model":"claude","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","content":"nope","is_error":true},{"role":"user","content":"again"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"parallel_tool_calls":false,"stop":"END"}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`"stop_sequences":["END"]`, `"is_error":true`, `"disable_parallel_tool_use":true`} {
		if !strings.Contains(text, want) {
			t.Fatalf("converted body missing %s: %s", want, text)
		}
	}
}

func TestGeminiSchemaAndRemoteImageAreRejected(t *testing.T) {
	_, _, _, err := convertOpenAIChatToGemini([]byte(`{"model":"gemini","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","$ref":"#/defs/x"}}}]}`))
	if err == nil || !strings.Contains(err.Error(), "$ref") {
		t.Fatalf("expected schema rejection, got %v", err)
	}
	_, _, _, err = convertOpenAIChatToGemini([]byte(`{"model":"gemini","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/x.png"}}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "remote images") {
		t.Fatalf("expected remote image rejection, got %v", err)
	}
}

func TestGeminiResponsePreservesCandidates(t *testing.T) {
	body, err := convertGeminiResponseToChat([]byte(`{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"},{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}]}`), "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"index":1`) || !strings.Contains(string(body), "b") {
		t.Fatalf("second candidate was lost: %s", body)
	}
}

func TestGeminiStreamStableToolIDAndArgumentDelta(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1}}}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1,\"b\":2}}}]},\"finishReason\":\"STOP\"}]}\n\n")),
	}
	recorder := httptest.NewRecorder()
	if err := streamGeminiAsChat(recorder, resp, "gemini"); err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	var argumentStream strings.Builder
	toolIDCount := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatal(err)
		}
		choices, _ := event["choices"].([]interface{})
		for _, choiceValue := range choices {
			choice, _ := choiceValue.(map[string]interface{})
			delta, _ := choice["delta"].(map[string]interface{})
			toolCalls, _ := delta["tool_calls"].([]interface{})
			for _, toolValue := range toolCalls {
				toolCall, _ := toolValue.(map[string]interface{})
				if stringValue(toolCall["id"]) == "call_0_0" {
					toolIDCount++
				}
				function, _ := toolCall["function"].(map[string]interface{})
				argumentStream.WriteString(stringValue(function["arguments"]))
			}
		}
	}
	if toolIDCount != 1 {
		t.Fatalf("tool ID was regenerated or repeated: %s", out)
	}
	if !json.Valid([]byte(argumentStream.String())) || argumentStream.String() != `{"a":1,"b":2}` || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("argument delta or terminal marker missing: %s", out)
	}
}

func TestGeminiStreamBadJSONEmitsProtocolError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {not-json}\n\n"))}
	recorder := httptest.NewRecorder()
	if err := streamGeminiAsChat(recorder, resp, "gemini"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), `"upstream_protocol_error"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("missing protocol error event: %s", recorder.Body.String())
	}
}

func TestResponsesRejectUnsupportedTargetFeatures(t *testing.T) {
	_, _, _, err := convertResponsesToGemini([]byte(`{"model":"gemini","input":"x","previous_response_id":"r1"}`))
	if err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("expected previous_response_id rejection, got %v", err)
	}
	_, _, _, err = convertResponsesToGemini([]byte(`{"model":"gemini","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/x.png"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "remote images") {
		t.Fatalf("expected Responses remote image rejection, got %v", err)
	}
}

func TestChatJSONToResponsesRejectsMultipleChoices(t *testing.T) {
	_, err := convertChatJSONToResponses([]byte(`{"choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}]}`), "gpt")
	if err == nil || !strings.Contains(err.Error(), "choices") {
		t.Fatalf("expected multiple choice rejection, got %v", err)
	}
}
