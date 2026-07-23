package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"zencoder-2api/internal/database"
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

func TestStreamingCompletionSupersedesRefreshStartedMidStream(t *testing.T) {
	StopUsageCreditsWorker()
	setupCreditsTestDB(t, "stream-credit-supersede.db")
	account := model.Account{
		ClientID: "stream-credit-supersede", CredentialType: model.CredentialAPIKey,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
		UsageCreditsConsumed: 30, UsageCreditsBudget: 100, UsageCreditsRemaining: 70,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ensureOperationID(context.Background())
	response := &http.Response{
		Header: make(http.Header),
		Body:   io.NopCloser(strings.NewReader("data: {}\n\ndata: [DONE]\n\n")),
	}
	finalizeStreamingAccount(ctx, response, &account, 1, streamOpenAIChat)
	var during model.Account
	if err := database.GetDB().First(&during, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	revision, claimed, err := claimCreditRefreshLease(context.Background(), during, "", "stream-holder")
	if err != nil || !claimed {
		t.Fatalf("claiming mid-stream refresh: revision=%d claimed=%t err=%v", revision, claimed, err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	var after model.Account
	if err := database.GetDB().First(&after, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.UsageCreditsQueryRevision != revision+1 || after.UsageCreditsLeaseID != "" ||
		after.UsageCreditsStatus != UsageCreditsStateStale {
		t.Fatalf("stream completion did not supersede the in-flight refresh: %#v", after)
	}
	if err := persistTokenCreditSnapshot(context.Background(), during, "stream-holder", revision, parsedTokenCredits{
		Consumed: 20, Budget: 100, Remaining: 80,
	}); !errors.Is(err, errCreditRefreshSuperseded) {
		t.Fatalf("mid-stream refresh CAS error = %v, want superseded", err)
	}
}

func TestStreamingCloseInvalidatesRefreshAfterUnderlyingClose(t *testing.T) {
	StopUsageCreditsWorker()
	setupCreditsTestDB(t, "stream-credit-close-order.db")
	account := model.Account{
		ClientID: "stream-credit-close-order", CredentialType: model.CredentialAPIKey,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ensureOperationID(context.Background())
	underlyingClosed := false
	response := &http.Response{Header: make(http.Header)}
	response.Body = &closeObserverBody{onClose: func() {
		underlyingClosed = true
		var duringClose model.Account
		if err := database.GetDB().First(&duringClose, account.ID).Error; err != nil {
			t.Fatal(err)
		}
		if duringClose.UsageCreditsLeaseID != "stream-close-holder" {
			t.Fatalf("refresh was invalidated before the upstream body closed: %#v", duringClose)
		}
	}}
	finalizeStreamingAccount(ctx, response, &account, 1, streamOpenAIChat)
	var during model.Account
	if err := database.GetDB().First(&during, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	revision, claimed, err := claimCreditRefreshLease(context.Background(), during, "", "stream-close-holder")
	if err != nil || !claimed {
		t.Fatalf("claiming refresh before close: revision=%d claimed=%t err=%v", revision, claimed, err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !underlyingClosed {
		t.Fatal("underlying response body was not closed")
	}
	var after model.Account
	if err := database.GetDB().First(&after, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.UsageCreditsQueryRevision != revision+1 || after.UsageCreditsLeaseID != "" ||
		after.UsageCreditsStatus != UsageCreditsStateStale {
		t.Fatalf("stream close did not invalidate the refresh after close: %#v", after)
	}
}

type closeObserverBody struct {
	onClose func()
}

func (*closeObserverBody) Read([]byte) (int, error) { return 0, io.EOF }

func (body *closeObserverBody) Close() error {
	body.onClose()
	return nil
}
