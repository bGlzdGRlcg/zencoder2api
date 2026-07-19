package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"zencoder-2api/internal/model"
)

func TestStreamingAccountFinalization(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		closeOnly bool
		wantState string
	}{
		{name: "terminal", stream: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", wantState: model.AccountHealthHealthy},
		{name: "truncated", stream: "data: {\"choices\":[", wantState: accountErrorNetwork},
		{name: "client close", stream: "data: {\"choices\":[", closeOnly: true, wantState: "pending"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := &model.Account{HealthState: "pending"}
			response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.stream))}
			finalizeStreamingAccount(context.Background(), response, account, 1, streamOpenAIChat)
			if test.closeOnly {
				buffer := make([]byte, 1)
				_, _ = response.Body.Read(buffer)
				_ = response.Body.Close()
			} else {
				_, _ = io.Copy(io.Discard, response.Body)
			}
			if account.HealthState != test.wantState {
				t.Fatalf("health state = %q, want %q", account.HealthState, test.wantState)
			}
		})
	}
}

func TestStreamTerminalMarkersIgnoreModelText(t *testing.T) {
	if streamTerminalSeen(streamOpenAIResponses, []byte(`data: {"delta":"response.completed"}`)) {
		t.Fatal("Responses model text was treated as a terminal event")
	}
	if streamTerminalSeen(streamAnthropic, []byte(`data: {"text":"message_stop"}`)) {
		t.Fatal("Anthropic model text was treated as a terminal event")
	}
	if !streamTerminalSeen(streamOpenAIResponses, []byte("event: response.completed\n")) {
		t.Fatal("Responses terminal event was not detected")
	}
}
