package service

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func TestOperationCreditsRequestUsesOAuthBearer(t *testing.T) {
	setCreditsTestKey(t)
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path != "/api/v1/quotas/me/operations/op-oauth/credits" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"operationId":"op-oauth","totalOperationCredits":0,"turns":1,"totalUserConsumedCredits":4,"totalUserBudget":3}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	encrypted, err := secret.Encrypt("oauth-token")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{CredentialType: model.CredentialOAuth, AccessToken: encrypted, TokenExpiresAt: time.Now().Add(time.Hour)}
	req, err := newOperationCreditsRequest(context.Background(), account, "op-oauth")
	if err != nil {
		t.Fatal(err)
	}
	credits, err := fetchOperationCredits(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := seen.Get("zencoder-api-key"); got != "" {
		t.Fatalf("unexpected API key header: %q", got)
	}
	if credits.OperationCredits != 0 || credits.Remaining != 0 {
		t.Fatalf("zero/clamped credits = %#v", credits)
	}
}

func TestOperationCreditsRequestUsesAPIKey(t *testing.T) {
	setCreditsTestKey(t)
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"operationId":"op-key","totalOperationCredits":8,"turns":1,"totalUserConsumedCredits":9,"totalUserBudget":20}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{CredentialType: model.CredentialAPIKey, APIKey: encrypted}
	req, err := newOperationCreditsRequest(context.Background(), account, "op-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchOperationCredits(req); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("zencoder-api-key"); got != "zencoder-key" {
		t.Fatalf("zencoder-api-key = %q", got)
	}
	if got := seen.Get("Authorization"); got != "" {
		t.Fatalf("unexpected bearer header: %q", got)
	}
}

func TestParseOperationCreditsRejectsNonIntegerAndClampsRemaining(t *testing.T) {
	credits, err := parseOperationCredits([]byte(`{"operationId":"op","totalOperationCredits":2,"turns":1,"totalUserConsumedCredits":12,"totalUserBudget":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if credits.Remaining != 0 {
		t.Fatalf("remaining = %d, want clamp to zero", credits.Remaining)
	}
	for _, raw := range []string{`1.5`, `"2"`, `9223372036854775808`, `-1`} {
		body := `{"operationId":"op","totalOperationCredits":` + raw + `,"turns":1,"totalUserConsumedCredits":1,"totalUserBudget":2}`
		if _, err := parseOperationCredits([]byte(body)); err == nil {
			t.Fatalf("expected invalid integer %s", raw)
		}
	}
	if _, err := parseOperationCredits([]byte(`{"operationId":"op","totalOperationCredits":1,"turns":1,"totalUserConsumedCredits":1,"totalUserBudget":2} {}`)); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
	omitted, err := parseOperationCredits([]byte(`{"totalOperationCredits":4,"totalUserConsumedCredits":6,"totalUserBudget":10}`))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Turns != 0 || omitted.OperationID != "" || omitted.Remaining != 4 {
		t.Fatalf("optional operation fields were not defaulted: %#v", omitted)
	}
}

func TestNonStreamingCreditRefreshRecordsOperationBeforeBodyAndQueuesAfterEOF(t *testing.T) {
	StopUsageCreditsWorker()
	queue := make(chan creditRefreshJob, 1)
	creditRefreshWorkerMu.Lock()
	creditRefreshQueue = queue
	creditRefreshPending = make(map[uint]struct{})
	creditRefreshRunning = make(map[uint]struct{})
	creditRefreshFollowup = make(map[uint]struct{})
	creditRefreshWorkerMu.Unlock()
	t.Cleanup(func() {
		creditRefreshWorkerMu.Lock()
		if creditRefreshQueue == queue {
			creditRefreshQueue = nil
			creditRefreshPending = nil
			creditRefreshRunning = nil
			creditRefreshFollowup = nil
		}
		creditRefreshWorkerMu.Unlock()
	})
	setupCreditsTestDB(t, "non-stream-eof.db")
	account := model.Account{
		ClientID: "credits-body-eof", CredentialType: model.CredentialAPIKey,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ensureOperationID(context.Background())
	request := httptest.NewRequest(http.MethodPost, "https://gateway.example.test", nil).WithContext(ctx)
	response := &http.Response{
		Header:  make(http.Header),
		Body:    io.NopCloser(strings.NewReader("{}")),
		Request: request,
	}
	UpdateAccountCreditsFromResponse(&account, response, 1)

	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if want := operationIDFromContext(ctx); stored.UsageCreditsOperationID != want {
		t.Fatalf("operation ID = %q before body read, want %q", stored.UsageCreditsOperationID, want)
	}
	if len(queue) != 0 {
		t.Fatal("credit query was queued before the response body completed")
	}
	buffer := make([]byte, 1)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatal("credit query was queued after only a partial body read")
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 {
		t.Fatalf("queued credit queries = %d, want 1", len(queue))
	}
}

func TestCreditNoOperationCASDoesNotOverwriteNewOperation(t *testing.T) {
	setupCreditsTestDB(t, "no-operation-cas.db")
	account := model.Account{
		ClientID: "credits-no-operation-cas", CredentialType: model.CredentialAPIKey,
		CredentialRevision: 1, UsageCreditsStatus: UsageCreditsStateUnknown,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"usage_credits_operation_id": "new-operation",
		"usage_credits_status":       UsageCreditsStateUnknown,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, marked, err := markCreditNoOperation(context.Background(), account); err != nil {
		t.Fatal(err)
	} else if marked {
		t.Fatal("stale no-operation update unexpectedly won the CAS")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsOperationID != "new-operation" || stored.UsageCreditsStatus != UsageCreditsStateUnknown {
		t.Fatalf("new operation was overwritten: %#v", stored)
	}
}

func TestRememberCreditOperationClearsPreviousOperationDetails(t *testing.T) {
	setupCreditsTestDB(t, "remember-operation-clears-details.db")
	account := model.Account{
		ClientID: "remember-operation-clears-details", CredentialType: model.CredentialAPIKey,
		CredentialRevision: 1, UsageCreditsOperationID: "old-operation",
		UsageCreditsOperationCredits: 8, UsageCreditsTurns: 2, UsageCreditsOperationExists: true,
		UsageCreditsConsumed: 1179, UsageCreditsBudget: 5000, UsageCreditsRemaining: 3821,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if !rememberCreditOperation(context.Background(), &account, "new-operation") {
		t.Fatal("remembering the new operation failed")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsOperationID != "new-operation" || stored.UsageCreditsOperationCredits != 0 ||
		stored.UsageCreditsTurns != 0 || stored.UsageCreditsOperationExists ||
		stored.UsageCreditsConsumed != 1179 || stored.UsageCreditsRemaining != 3821 {
		t.Fatalf("new operation did not isolate operation details: %#v", stored)
	}
}

func TestCreditRefreshFailureDoesNotChangeAccountHealth(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "failure.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-failure", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-failure",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	_, err = RefreshAccountCredits(context.Background(), account.ID)
	if err == nil {
		t.Fatal("expected upstream error")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HealthState != model.AccountHealthHealthy || stored.ReauthRequired || stored.FailureCount != 0 {
		t.Fatalf("credit failure changed health: state=%q reauth=%t failures=%d", stored.HealthState, stored.ReauthRequired, stored.FailureCount)
	}
	if stored.UsageCreditsStatus != UsageCreditsStateError {
		t.Fatalf("status = %q", stored.UsageCreditsStatus)
	}
}

func TestExpiredOAuthCreditRefreshDoesNotRefreshTokenOrChangeHealth(t *testing.T) {
	setCreditsTestKey(t)
	var gatewayCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	var authCalls int
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls++
		http.Error(w, "refresh rejected", http.StatusUnauthorized)
	}))
	defer authServer.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", authServer.URL)
	setupCreditsTestDB(t, "expired-oauth.db")
	access, err := secret.Encrypt("expired-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-expired-oauth", CredentialType: model.CredentialOAuth,
		AccessToken: access, RefreshToken: refresh, TokenExpiresAt: time.Now().Add(-time.Hour),
		CredentialRevision: 1, HealthRevision: 3, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-expired",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshAccountCredits(context.Background(), account.ID); err == nil {
		t.Fatal("expected expired OAuth token to defer the optional credit query")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gatewayCalls != 0 || authCalls != 1 {
		t.Fatalf("unexpected optional OAuth calls: gateway=%d auth=%d", gatewayCalls, authCalls)
	}
	if stored.HealthRevision != 3 || stored.HealthState != model.AccountHealthHealthy || stored.ReauthRequired || stored.FailureCount != 0 {
		t.Fatalf("optional credit auth changed account health: %#v", stored)
	}
	if stored.CredentialRevision != 1 || stored.UsageCreditsStatus != UsageCreditsStateError {
		t.Fatalf("optional credit auth changed credentials or state incorrectly: %#v", stored)
	}
}

func TestExpiredOAuthCreditRefreshRotatesTokenWithoutChangingHealth(t *testing.T) {
	setCreditsTestKey(t)
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh_token" {
			t.Fatalf("unexpected auth path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`))
	}))
	defer authServer.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", authServer.URL)
	gatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path == "/api/v1/quotas/me/tokens" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/v1/quotas/me/operations/op-refresh/credits" {
			t.Fatalf("unexpected gateway path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"operationId":"op-refresh","totalOperationCredits":3,"turns":1,"totalUserConsumedCredits":13,"totalUserBudget":20}`))
	}))
	defer gatewayServer.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", gatewayServer.URL)
	setupCreditsTestDB(t, "oauth-refresh-success.db")
	access, err := secret.Encrypt("expired-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-oauth-refresh-success", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: access, RefreshToken: refresh,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
		HealthRevision: 4, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-refresh",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Remaining != 7 || result.Snapshot.State != UsageCreditsStateReady {
		t.Fatalf("unexpected refreshed OAuth snapshot: %#v", result.Snapshot)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRevision != 2 || stored.HealthRevision != 4 || stored.HealthState != model.AccountHealthHealthy {
		t.Fatalf("unexpected credential/health revisions: %#v", stored)
	}
	if got, err := secret.Decrypt(stored.AccessToken); err != nil || got != "new-access" {
		t.Fatalf("rotated access token = %q, err=%v", got, err)
	}
}

func TestRevokedOAuthCreditRefreshRetriesOnceWithoutChangingHealth(t *testing.T) {
	setCreditsTestKey(t)
	var authCalls, gatewayCalls int
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls++
		if r.URL.Path != "/refresh_token" {
			t.Fatalf("unexpected auth path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"renewed-access","refreshToken":"renewed-refresh","expiresIn":3600}`))
	}))
	defer authServer.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", authServer.URL)
	gatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls++
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected gateway path: %s", r.URL.Path)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer revoked-access":
			http.Error(w, "revoked", http.StatusUnauthorized)
		case "Bearer renewed-access":
			_, _ = w.Write([]byte(`{"totalConsumedByUser":4,"totalUserBudget":20,"remaining":15}`))
		default:
			t.Fatalf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
		}
	}))
	defer gatewayServer.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", gatewayServer.URL)
	setupCreditsTestDB(t, "oauth-revoked-retry.db")
	access, err := secret.Encrypt("revoked-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-oauth-revoked", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: access, RefreshToken: refresh,
		TokenExpiresAt: time.Now().Add(time.Hour), CredentialRevision: 1,
		HealthRevision: 7, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authCalls != 1 || gatewayCalls != 2 {
		t.Fatalf("calls: auth=%d gateway=%d", authCalls, gatewayCalls)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 15 {
		t.Fatalf("unexpected retry snapshot: %#v", result.Snapshot)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRevision != 2 || stored.HealthRevision != 7 ||
		stored.HealthState != model.AccountHealthHealthy || stored.ReauthRequired || stored.FailureCount != 0 {
		t.Fatalf("optional revoked-token recovery changed health: %#v", stored)
	}
}

func TestCreditRefreshPersistsBalanceWhenOperationHasNoTurns(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quotas/me/tokens" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/v1/quotas/me/operations/op-empty/credits" {
			t.Fatalf("unexpected gateway path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"operationId":"op-empty","totalOperationCredits":0,"turns":0,"totalUserConsumedCredits":1179,"totalUserBudget":5000}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "empty-operation.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-empty", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-empty",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.OperationExists ||
		result.Snapshot.Remaining != 3821 || result.Snapshot.OperationCredits != 0 {
		t.Fatalf("unexpected zero-turn snapshot: %#v", result.Snapshot)
	}
}

func TestCreditRefreshBindsOptionalResponseOperationFieldsToRequest(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quotas/me/tokens" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/v1/quotas/me/operations/optional-op/credits" {
			t.Fatalf("unexpected gateway path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"totalOperationCredits":4,"totalUserConsumedCredits":6,"totalUserBudget":10}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "optional-operation-fields.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-optional-fields", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "optional-op",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Snapshot.OperationExists || result.Snapshot.Remaining != 4 || result.Snapshot.State != UsageCreditsStateReady {
		t.Fatalf("optional operation fields were not bound safely: %#v", result.Snapshot)
	}
}

func TestCreditRefreshCASRejectsCredentialRotation(t *testing.T) {
	setCreditsTestKey(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quotas/me/tokens" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/v1/quotas/me/operations/op-cas/credits" {
			t.Fatalf("unexpected gateway path: %s", r.URL.Path)
		}
		once.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte(`{"operationId":"op-cas","totalOperationCredits":8,"turns":1,"totalUserConsumedCredits":8,"totalUserBudget":20}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "cas.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-cas", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-cas",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan error, 1)
	go func() {
		_, refreshErr := RefreshAccountCredits(context.Background(), account.ID)
		resultCh <- refreshErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("credits request did not start")
	}
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"credential_revision": 2,
		"health_state":        model.AccountHealthHealthy,
	}).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-resultCh; err == nil {
		t.Fatal("expected CAS rejection after credential rotation")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsConsumed != 0 || stored.HealthState != model.AccountHealthHealthy {
		t.Fatalf("stale response was persisted: consumed=%d health=%q", stored.UsageCreditsConsumed, stored.HealthState)
	}
}

func TestUsageCreditsWorkerStopCancelsInFlightRefreshAndReleasesLease(t *testing.T) {
	StopUsageCreditsWorker()
	setCreditsTestKey(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "worker-stop.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-worker-stop", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "op-stop",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	stop := StartUsageCreditsWorker()
	t.Cleanup(StopUsageCreditsWorker)
	TriggerAccountCreditsRefresh(context.Background(), &account, "op-stop")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start the credit request")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("worker stop did not cancel the in-flight credit request")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsLeaseID != "" || stored.UsageCreditsLeaseUntil != nil {
		t.Fatalf("credit refresh lease was not released: %#v", stored)
	}
}

func TestCreditCleanupContextDetachesCancellationWithBoundedDeadline(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatalf("cleanup context inherited cancellation: %v", ctx.Err())
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > creditCleanupTimeout {
		t.Fatalf("cleanup context has invalid deadline: %v %t", deadline, ok)
	}
}

func setupCreditsTestDB(t *testing.T, name string) {
	t.Helper()
	if err := database.Init(filepath.Join(t.TempDir(), name)); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
}

func setCreditsTestKey(t *testing.T) {
	t.Helper()
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))
}
