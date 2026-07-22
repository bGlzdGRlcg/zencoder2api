package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func TestTokenCreditsRequestUsesOAuthBearerAndNeedsNoOperationID(t *testing.T) {
	setCreditsTestKey(t)
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"periodEnd":"2026-07-25T17:56:32Z","remaining":4991,"totalConsumedByUser":9,"totalUserBudget":5000}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)

	encrypted, err := secret.Encrypt("oauth-token")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		CredentialType: model.CredentialOAuth,
		AccessToken:    encrypted,
		TokenExpiresAt: time.Now().Add(time.Hour),
	}
	req, err := newTokenCreditsRequest(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	credits, err := fetchTokenCredits(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := seen.Get("zencoder-api-key"); got != "" {
		t.Fatalf("unexpected API key header: %q", got)
	}
	if got := seen.Get("zen-operation-id"); got != "" {
		t.Fatalf("token query unexpectedly used operation ID: %q", got)
	}
	if credits.Consumed != 9 || credits.Budget != 5000 || credits.Remaining != 4991 {
		t.Fatalf("unexpected token balance: %#v", credits)
	}
	wantPeriodEnd := time.Date(2026, time.July, 25, 17, 56, 32, 0, time.UTC)
	if credits.PeriodEnd == nil || !credits.PeriodEnd.Equal(wantPeriodEnd) {
		t.Fatalf("period end = %v, want %v", credits.PeriodEnd, wantPeriodEnd)
	}
}

func TestTokenCreditsRequestUsesAPIKey(t *testing.T) {
	setCreditsTestKey(t)
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"totalConsumedByUser":3,"totalUserBudget":10}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)

	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{CredentialType: model.CredentialAPIKey, APIKey: encrypted}
	req, err := newTokenCreditsRequest(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	credits, err := fetchTokenCredits(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("zencoder-api-key"); got != "zencoder-key" {
		t.Fatalf("zencoder-api-key = %q", got)
	}
	if got := seen.Get("Authorization"); got != "" {
		t.Fatalf("unexpected bearer header: %q", got)
	}
	if credits.Consumed != 3 || credits.Budget != 10 || credits.Remaining != 7 {
		t.Fatalf("unexpected derived token balance: %#v", credits)
	}
}

func TestParseTokenCreditsClampsAndRejectsInvalidValues(t *testing.T) {
	credits, err := parseTokenCredits([]byte(`{"totalConsumedByUser":12,"totalUserBudget":5,"remaining":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if credits.Remaining != 0 {
		t.Fatalf("remaining = %d, want zero", credits.Remaining)
	}
	authoritative, err := parseTokenCredits([]byte(`{"totalConsumedByUser":1,"totalUserBudget":5,"remaining":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if authoritative.Remaining != 7 {
		t.Fatalf("explicit remaining was not preserved: %d", authoritative.Remaining)
	}
	for _, body := range []string{
		`{"totalConsumedByUser":-1,"totalUserBudget":5}`,
		`{"totalConsumedByUser":1.5,"totalUserBudget":5}`,
		`{"totalConsumedByUser":"1","totalUserBudget":5}`,
		`{"totalConsumedByUser":1}`,
		`{"totalConsumedByUser":1,"totalUserBudget":-1}`,
		`{"totalConsumedByUser":1,"totalUserBudget":5,"remaining":-1}`,
		`{"totalConsumedByUser":1,"totalUserBudget":5,"periodEnd":"not-a-time"}`,
		`{"totalConsumedByUser":1,"totalUserBudget":5,"periodEnd":123}`,
	} {
		if _, err := parseTokenCredits([]byte(body)); err == nil {
			t.Fatalf("expected invalid token balance: %s", body)
		}
	}
}

func TestFetchTokenCreditsMapsUnsupportedStatus(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetchTokenCredits(req)
	if err == nil {
		t.Fatal("expected unsupported endpoint error")
	}
	var httpErr *creditHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusNotFound || httpErr.state != UsageCreditsStateUnsupported {
		t.Fatalf("unexpected HTTP error: %v", err)
	}
}

func TestRefreshAccountCreditsUsesTokensWithoutOperationID(t *testing.T) {
	setCreditsTestKey(t)
	periodEnd := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"periodEnd":%q,"totalConsumedByUser":9,"totalUserBudget":5000,"remaining":4991}`, periodEnd.Format(time.RFC3339))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-no-operation.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-no-operation", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 4991 {
		t.Fatalf("unexpected snapshot: %#v", result.Snapshot)
	}
	if result.Snapshot.PeriodEnd == nil || !result.Snapshot.PeriodEnd.Equal(periodEnd) {
		t.Fatalf("period end was not returned: %v", result.Snapshot.PeriodEnd)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsConsumed != 9 || stored.UsageCreditsBudget != 5000 || stored.UsageCreditsRemaining != 4991 {
		t.Fatalf("token snapshot was not persisted: %#v", stored)
	}
}

func TestRefreshAccountCreditsRecoversAfterAutomaticOAuthRefreshIsRejected(t *testing.T) {
	setCreditsTestKey(t)
	var refreshCalls atomic.Int32
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh_token" {
			http.NotFound(w, r)
			return
		}
		call := refreshCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"accessToken":"refreshed-%d","refreshToken":"refresh-%d","expiresIn":3600}`, call, call)
	}))
	defer authServer.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", authServer.URL)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-2" {
			http.Error(w, "rejected token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"totalConsumedByUser":2,"totalUserBudget":10,"remaining":8}`))
	}))
	defer gateway.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", gateway.URL)
	setupCreditsTestDB(t, "oauth-refresh-recovery.db")
	access, err := secret.Encrypt("expired-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("expired-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "oauth-credit-recovery", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: access, RefreshToken: refresh,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 8 {
		t.Fatalf("unexpected recovered snapshot: %#v", result.Snapshot)
	}
	if refreshCalls.Load() != 2 {
		t.Fatalf("OAuth refresh calls = %d, want automatic plus forced recovery", refreshCalls.Load())
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRevision != 3 {
		t.Fatalf("credential revision = %d, want 3 after two token rotations", stored.CredentialRevision)
	}
}

func TestRefreshAccountCreditsRecoversOAuth401OnOperationFallback(t *testing.T) {
	setCreditsTestKey(t)
	var refreshCalls atomic.Int32
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh_token" {
			http.NotFound(w, r)
			return
		}
		call := refreshCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"accessToken":"fallback-%d","refreshToken":"fallback-refresh-%d","expiresIn":3600}`, call, call)
	}))
	defer authServer.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", authServer.URL)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/quotas/me/tokens":
			http.NotFound(w, r)
		case "/api/v1/quotas/me/operations/fallback-op/credits":
			if r.Header.Get("Authorization") != "Bearer fallback-1" {
				http.Error(w, "rejected token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"operationId":"fallback-op","totalOperationCredits":3,"turns":1,"totalUserConsumedCredits":7,"totalUserBudget":10}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer gateway.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", gateway.URL)
	setupCreditsTestDB(t, "oauth-fallback-recovery.db")
	access, err := secret.Encrypt("valid-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("valid-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "oauth-fallback-recovery", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: access, RefreshToken: refresh,
		TokenExpiresAt: time.Now().Add(time.Hour), CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy, UsageCreditsOperationID: "fallback-op",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 3 || !result.Snapshot.OperationExists {
		t.Fatalf("unexpected fallback recovery snapshot: %#v", result.Snapshot)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("OAuth refresh calls = %d, want one fallback recovery", refreshCalls.Load())
	}
}

func TestTokenCreditSnapshotPreservesKnownPeriodWhenResponseOmitsIt(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"totalConsumedByUser":4,"totalUserBudget":20,"remaining":15}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-preserve-period.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	updatedAt := time.Now().UTC()
	account := model.Account{
		ClientID: "tokens-preserve-period", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
		UsageCreditsConsumed: 3, UsageCreditsBudget: 20, UsageCreditsRemaining: 16,
		UsageCreditsUpdatedAt: &updatedAt, UsageCreditsPeriodEnd: &periodEnd,
		UsageCreditsCredentialRevision: 1,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := pool.refresh(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.mu.Lock()
		pool.accounts = nil
		pool.index = 0
		pool.mu.Unlock()
	})

	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.PeriodEnd == nil || !result.Snapshot.PeriodEnd.Equal(periodEnd) || result.Snapshot.Remaining != 15 {
		t.Fatalf("known period was not preserved: %#v", result.Snapshot)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, cached := range pool.accounts {
		if cached.ID == account.ID {
			if cached.UsageCreditsPeriodEnd == nil || !cached.UsageCreditsPeriodEnd.Equal(periodEnd) {
				t.Fatalf("pool lost the known period when response omitted it: %v", cached.UsageCreditsPeriodEnd)
			}
			return
		}
	}
	t.Fatal("test account missing from pool cache")
}

func TestUpdatePoolCreditsPublishesDatabaseCredentialRevisionAndState(t *testing.T) {
	setCreditsTestKey(t)
	setupCreditsTestDB(t, "pool-credit-state.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Now().UTC()
	account := model.Account{
		ClientID: "pool-credit-state", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
		UsageCreditsConsumed: 5, UsageCreditsBudget: 10, UsageCreditsRemaining: 0,
		UsageCreditsUpdatedAt: &updatedAt, UsageCreditsCredentialRevision: 1,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := pool.refresh(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.mu.Lock()
		pool.accounts = nil
		pool.index = 0
		pool.mu.Unlock()
	})
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"usage_credits_status":          UsageCreditsStateError,
		"usage_credits_query_revision":  2,
		"usage_credits_last_attempt_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	updatePoolCredits(account.ID)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, cached := range pool.accounts {
		if cached.ID == account.ID {
			if cached.CredentialRevision != 1 || cached.UsageCreditsStatus != UsageCreditsStateError {
				t.Fatalf("pool did not publish the current credit state: %#v", cached)
			}
			return
		}
	}
	t.Fatal("test account missing from pool cache")
}

func TestTokenCreditSnapshotRejectsOlderBillingPeriod(t *testing.T) {
	setCreditsTestKey(t)
	oldPeriod := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"periodEnd":%q,"totalConsumedByUser":99,"totalUserBudget":100,"remaining":1}`, oldPeriod.Format(time.RFC3339))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-older-period.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	newPeriod := oldPeriod.Add(24 * time.Hour)
	updatedAt := time.Now().UTC()
	account := model.Account{
		ClientID: "tokens-older-period", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
		UsageCreditsConsumed: 10, UsageCreditsBudget: 100, UsageCreditsRemaining: 90,
		UsageCreditsUpdatedAt: &updatedAt, UsageCreditsPeriodEnd: &newPeriod,
		UsageCreditsCredentialRevision: 1,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if !errors.Is(err, errCreditRefreshSuperseded) {
		t.Fatalf("error = %v, want superseded", err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 90 ||
		result.Snapshot.PeriodEnd == nil || !result.Snapshot.PeriodEnd.Equal(newPeriod) {
		t.Fatalf("older period replaced current snapshot: %#v", result.Snapshot)
	}
}

func TestRefreshAccountCreditsFallsBackWhenTokensAreUnsupported(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/quotas/me/tokens":
			http.NotFound(w, r)
		case "/api/v1/quotas/me/operations/legacy-op/credits":
			_, _ = w.Write([]byte(`{"operationId":"legacy-op","totalOperationCredits":4,"turns":1,"totalUserConsumedCredits":6,"totalUserBudget":10}`))
		default:
			t.Fatalf("unexpected fallback path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-fallback.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-fallback", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "legacy-op",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.Remaining != 4 || !result.Snapshot.OperationExists {
		t.Fatalf("unexpected fallback snapshot: %#v", result.Snapshot)
	}
}

func TestOperationFallbackClearsStaleTokenPeriodEnd(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/quotas/me/tokens":
			http.NotFound(w, r)
		case "/api/v1/quotas/me/operations/legacy-period/credits":
			_, _ = w.Write([]byte(`{"operationId":"legacy-period","totalOperationCredits":2,"turns":1,"totalUserConsumedCredits":4,"totalUserBudget":10}`))
		default:
			t.Fatalf("unexpected fallback path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-fallback-period.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour)
	account := model.Account{
		ClientID: "tokens-fallback-period", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "legacy-period", UsageCreditsPeriodEnd: &expired,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateReady || result.Snapshot.PeriodEnd != nil {
		t.Fatalf("fallback retained stale token period: %#v", result.Snapshot)
	}
}

func TestTokenAuthFailureDoesNotClearActiveCreditLease(t *testing.T) {
	setCreditsTestKey(t)
	setupCreditsTestDB(t, "tokens-auth-active-lease.db")
	leaseUntil := time.Now().Add(time.Minute).UTC()
	account := model.Account{
		ClientID: "tokens-auth-active-lease", CredentialType: model.CredentialAPIKey,
		APIKey: "not-an-encrypted-api-key", CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy, UsageCreditsStatus: UsageCreditsStateRefreshing,
		UsageCreditsLeaseID: "active-holder", UsageCreditsLeaseUntil: &leaseUntil,
		UsageCreditsQueryRevision: 3,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshAccountCredits(context.Background(), account.ID); err == nil {
		t.Fatal("expected API-key decryption failure")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsLeaseID != "active-holder" || stored.UsageCreditsLeaseUntil == nil ||
		!stored.UsageCreditsLeaseUntil.Equal(leaseUntil) || stored.UsageCreditsStatus != UsageCreditsStateRefreshing {
		t.Fatalf("pre-lease auth failure modified an active refresh: %#v", stored)
	}
}

func TestRefreshAccountCreditsMarksNoOperationForUnsupportedTokenEndpoint(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-unsupported-no-operation.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-unsupported-no-operation", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RefreshAccountCredits(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != UsageCreditsStateNoOperation {
		t.Fatalf("state = %q, want no_operation", result.Snapshot.State)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsStatus != UsageCreditsStateNoOperation || stored.UsageCreditsLastAttemptAt == nil {
		t.Fatalf("no-operation state was not persisted: %#v", stored)
	}
}

func TestRefreshAccountCreditsDoesNotTreatMalformedTokenResponseAsUnsupported(t *testing.T) {
	setCreditsTestKey(t)
	var operationCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/quotas/me/tokens":
			_, _ = w.Write([]byte(`{"totalConsumedByUser":1}`))
		case "/api/v1/quotas/me/operations/legacy-op/credits":
			operationCalls++
			_, _ = w.Write([]byte(`{"operationId":"legacy-op","totalOperationCredits":1,"totalUserConsumedCredits":1,"totalUserBudget":2}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-malformed-response.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-malformed-response", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "legacy-op",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshAccountCredits(context.Background(), account.ID); err == nil {
		t.Fatal("expected malformed token response to fail")
	}
	if operationCalls != 0 {
		t.Fatalf("malformed token response unexpectedly used operation fallback: %d", operationCalls)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsStatus != UsageCreditsStateError {
		t.Fatalf("status = %q, want error", stored.UsageCreditsStatus)
	}
}

func TestRefreshAccountCreditsPreservesOperationFallbackError(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/quotas/me/tokens":
			http.NotFound(w, r)
		case "/api/v1/quotas/me/operations/legacy-op/credits":
			http.Error(w, "denied", http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-fallback-error.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-fallback-error", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsOperationID: "legacy-op",
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if result, err := RefreshAccountCredits(context.Background(), account.ID); err == nil {
		t.Fatal("expected operation fallback error")
	} else if result.Snapshot.State == UsageCreditsStateNoOperation {
		t.Fatalf("existing operation was misclassified as no_operation: %#v", result.Snapshot)
	}
}

func TestEnqueueAccountCreditsRefreshCoalescesSameAccount(t *testing.T) {
	StopUsageCreditsWorker()
	queue := make(chan creditRefreshJob, 8)
	creditRefreshWorkerMu.Lock()
	previous := creditRefreshQueue
	previousPending := creditRefreshPending
	previousRunning := creditRefreshRunning
	previousFollowup := creditRefreshFollowup
	creditRefreshQueue = queue
	creditRefreshPending = make(map[uint]struct{})
	creditRefreshRunning = make(map[uint]struct{})
	creditRefreshFollowup = make(map[uint]struct{})
	creditRefreshWorkerMu.Unlock()
	t.Cleanup(func() {
		creditRefreshWorkerMu.Lock()
		if creditRefreshQueue == queue {
			creditRefreshQueue = previous
			creditRefreshPending = previousPending
			creditRefreshRunning = previousRunning
			creditRefreshFollowup = previousFollowup
		}
		creditRefreshWorkerMu.Unlock()
	})

	account := &model.Account{ID: 41, CredentialRevision: 3}
	for i := 0; i < 3; i++ {
		enqueueAccountCreditsRefresh(account)
	}
	if got := len(queue); got != 1 {
		t.Fatalf("queued refresh jobs = %d, want one coalesced job", got)
	}
	job := <-queue
	if job.accountID != account.ID {
		t.Fatalf("unexpected coalesced job: %#v", job)
	}
}

func TestUsageCreditsWorkerQueuesFollowupDuringRefresh(t *testing.T) {
	StopUsageCreditsWorker()
	setCreditsTestKey(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		} else if call == 2 {
			close(secondStarted)
		}
		_, _ = w.Write([]byte(`{"remaining":7,"totalConsumedByUser":3,"totalUserBudget":10}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-worker-followup.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-worker-followup", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	stop := StartUsageCreditsWorker()
	t.Cleanup(stop)
	enqueueAccountCreditsRefresh(&account)
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first credit refresh did not start")
	}
	enqueueAccountCreditsRefresh(&account)
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh completed during an active query was not queued as a follow-up")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("credit refresh calls = %d, want 2", got)
	}
}

func TestUsageCreditsWorkerRetriesAfterExternalRefreshLeaseBusy(t *testing.T) {
	StopUsageCreditsWorker()
	setCreditsTestKey(t)
	var calls atomic.Int32
	completed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		call := calls.Add(1)
		_, _ = w.Write([]byte(`{"remaining":8,"totalConsumedByUser":2,"totalUserBudget":10}`))
		if call == 1 {
			close(completed)
		}
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "tokens-worker-external-lease.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "tokens-worker-external-lease", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	holder := "manual-refresh-holder"
	if _, claimed, err := claimCreditRefreshLease(context.Background(), stored, "", holder); err != nil || !claimed {
		t.Fatalf("claim external refresh lease: claimed=%t err=%v", claimed, err)
	}
	t.Cleanup(func() { releaseCreditRefreshLease(context.Background(), account.ID, holder) })

	stop := StartUsageCreditsWorker()
	t.Cleanup(stop)
	TriggerAccountCreditsRefresh(context.Background(), &account, "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		creditRefreshWorkerMu.Lock()
		_, followup := creditRefreshFollowup[account.ID]
		creditRefreshWorkerMu.Unlock()
		if followup {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not retain a follow-up after an external lease conflict")
		}
		time.Sleep(5 * time.Millisecond)
	}
	releaseCreditRefreshLease(context.Background(), account.ID, holder)
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not retry after the external refresh lease was released")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("credit refresh calls = %d, want one retried query", got)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
			t.Fatal(err)
		}
		if stored.UsageCreditsStatus == UsageCreditsStateReady && stored.UsageCreditsRemaining == 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry did not publish the latest credit snapshot: %#v", stored)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
