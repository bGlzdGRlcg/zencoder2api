package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func TestAccountSchedulingUsesFreshUsageCredits(t *testing.T) {
	now := time.Now()
	base := model.Account{
		CredentialRevision: 1,
		HealthState:        model.AccountHealthHealthy,
	}
	if priority := accountSchedulingPriority(base, now); priority != 0 {
		t.Fatalf("unknown account priority = %d, want bootstrap priority 0", priority)
	}

	updatedAt := now.Add(-time.Minute)
	positive := base
	positive.UsageCreditsAvailable = true
	positive.UsageCreditsStatus = UsageCreditsStateReady
	positive.UsageCreditsRemaining = 10
	positive.UsageCreditsUpdatedAt = &updatedAt
	positive.UsageCreditsCredentialRevision = positive.CredentialRevision
	if priority := accountSchedulingPriority(positive, now); priority != 1 {
		t.Fatalf("positive account priority = %d, want 1", priority)
	}

	exhausted := positive
	exhausted.UsageCreditsRemaining = 0
	if priority := accountSchedulingPriority(exhausted, now); priority != -1 {
		t.Fatalf("fresh exhausted account priority = %d, want excluded", priority)
	}
	periodEnded := now.Add(-time.Second)
	exhausted.UsageCreditsRemaining = 10
	exhausted.UsageCreditsPeriodEnd = &periodEnded
	if priority := accountSchedulingPriority(exhausted, now); priority != 2 {
		t.Fatalf("expired-period account priority = %d, want stale fallback 2", priority)
	}
	exhausted.UsageCreditsPeriodEnd = nil
	exhausted.UsageCreditsRemaining = 0

	staleAt := now.Add(-2*defaultUsageCreditsRefreshInterval - time.Second)
	exhausted.UsageCreditsUpdatedAt = &staleAt
	if priority := accountSchedulingPriority(exhausted, now); priority != 2 {
		t.Fatalf("stale exhausted account priority = %d, want fallback priority 2", priority)
	}
	failed := positive
	failed.UsageCreditsStatus = UsageCreditsStateError
	if priority := accountSchedulingPriority(failed, now); priority != 2 {
		t.Fatalf("failed snapshot priority = %d, want fallback priority 2", priority)
	}
}

func TestMergeAccountPoolRuntimePreservesNewerCachedState(t *testing.T) {
	older := time.Now().UTC().Add(-time.Minute)
	newer := time.Now().UTC()
	queried := []model.Account{
		{ID: 1, ClientID: "old-credential", CredentialRevision: 1},
		{
			ID: 2, CredentialRevision: 1, HealthRevision: 1, HealthState: model.AccountHealthHealthy,
			UsageCreditsQueryRevision: 5, UsageCreditsRemaining: 50,
		},
		{
			ID: 3, CredentialRevision: 1, HealthRevision: 2, HealthState: model.AccountHealthHealthy,
			UsageCreditsQueryRevision: 3, UsageCreditsRemaining: 30,
		},
		{
			ID: 4, CredentialRevision: 1, UsageCreditsQueryRevision: 5,
			UsageCreditsStatus: UsageCreditsStateRefreshing, UsageCreditsRemaining: 70, UsageCreditsLastAttemptAt: &older,
		},
	}
	cached := []model.Account{
		{ID: 1, ClientID: "new-credential", CredentialRevision: 2},
		{
			ID: 2, CredentialRevision: 1, HealthRevision: 2, HealthState: accountErrorRateLimit, ReauthRequired: true,
			UsageCreditsQueryRevision: 4, UsageCreditsRemaining: 40,
		},
		{
			ID: 3, CredentialRevision: 1, HealthRevision: 1, HealthState: accountErrorRateLimit,
			UsageCreditsQueryRevision: 4, UsageCreditsRemaining: 20,
		},
		{
			ID: 4, CredentialRevision: 1, UsageCreditsQueryRevision: 5,
			UsageCreditsStatus: UsageCreditsStateStale, UsageCreditsRemaining: 60, UsageCreditsLastAttemptAt: &newer,
		},
	}

	mergeAccountPoolRuntime(queried, cached)

	if queried[0].CredentialRevision != 2 || queried[0].ClientID != "new-credential" {
		t.Fatalf("newer credential was overwritten: %#v", queried[0])
	}
	if queried[1].HealthRevision != 2 || queried[1].HealthState != accountErrorRateLimit || !queried[1].ReauthRequired ||
		queried[1].UsageCreditsQueryRevision != 5 || queried[1].UsageCreditsRemaining != 50 {
		t.Fatalf("health merge replaced unrelated credit state: %#v", queried[1])
	}
	if queried[2].HealthRevision != 2 || queried[2].HealthState != model.AccountHealthHealthy ||
		queried[2].UsageCreditsQueryRevision != 4 || queried[2].UsageCreditsRemaining != 20 {
		t.Fatalf("credit merge replaced unrelated health state: %#v", queried[2])
	}
	if queried[3].UsageCreditsStatus != UsageCreditsStateStale || queried[3].UsageCreditsRemaining != 60 ||
		queried[3].UsageCreditsLastAttemptAt == nil || !queried[3].UsageCreditsLastAttemptAt.Equal(newer) {
		t.Fatalf("newer same-revision credit state was overwritten: %#v", queried[3])
	}
}

func TestAccountPoolSelectionUsesCreditTiers(t *testing.T) {
	setCreditsTestKey(t)
	setupCreditsTestDB(t, "pool-credit-tiers.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	accounts := []model.Account{
		{ClientID: "pool-unknown", CredentialType: model.CredentialAPIKey, APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy},
		{ClientID: "pool-positive", CredentialType: model.CredentialAPIKey, APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady, UsageCreditsRemaining: 10, UsageCreditsBudget: 20, UsageCreditsUpdatedAt: &now, UsageCreditsCredentialRevision: 1},
		{ClientID: "pool-exhausted", CredentialType: model.CredentialAPIKey, APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady, UsageCreditsRemaining: 0, UsageCreditsBudget: 20, UsageCreditsUpdatedAt: &now, UsageCreditsCredentialRevision: 1},
	}
	if err := database.GetDB().Create(&accounts).Error; err != nil {
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
	first, err := GetNextAccountContext(context.Background(), nil)
	if err != nil || first.ID != accounts[0].ID {
		t.Fatalf("first account = %#v, err=%v; want unknown bootstrap account", first, err)
	}
	second, err := GetNextAccountContext(context.Background(), map[uint]struct{}{first.ID: {}})
	if err != nil || second.ID != accounts[1].ID {
		t.Fatalf("second account = %#v, err=%v; want fresh positive account", second, err)
	}
	if _, err := GetNextAccountContext(context.Background(), map[uint]struct{}{first.ID: {}, second.ID: {}}); !errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("exhausted account was not excluded: %v", err)
	}
}

func TestAccountPoolSelectionMergesCredentialRotatedAfterPoolRefresh(t *testing.T) {
	setCreditsTestKey(t)
	setupCreditsTestDB(t, "pool-credential-rotation.db")
	oldEncrypted, err := secret.Encrypt("old-zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	newEncrypted, err := secret.Encrypt("new-zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "api-key-old", CredentialType: model.CredentialAPIKey,
		APIKey: oldEncrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
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
		"client_id":           "api-key-new",
		"api_key":             newEncrypted,
		"credential_revision": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}

	selected, err := GetNextAccountContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.CredentialRevision != 1 {
		t.Fatalf("selection masked the cached credential revision: %d", selected.CredentialRevision)
	}
	req := httptest.NewRequest(http.MethodGet, "https://gateway.example.test", nil)
	if err := ApplyZencoderAuth(context.Background(), req, selected); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("zencoder-api-key"); got != "new-zencoder-key" {
		t.Fatalf("selected account used stale API key %q", got)
	}
	if selected.CredentialRevision != 2 || selected.ClientID != "api-key-new" {
		t.Fatalf("credential merge did not update selected copy: revision=%d client_id=%q", selected.CredentialRevision, selected.ClientID)
	}

}

func TestAccountPoolSelectionRefreshesExhaustedCreditCache(t *testing.T) {
	setCreditsTestKey(t)
	setupCreditsTestDB(t, "pool-credit-cache-refresh.db")
	encrypted, err := secret.Encrypt("zencoder-key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	account := model.Account{
		ClientID: "pool-credit-cache-refresh", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
		UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
		UsageCreditsBudget: 20, UsageCreditsRemaining: 0, UsageCreditsUpdatedAt: &now,
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

	// Simulate another instance publishing a recovered balance while this
	// instance still holds an exhausted cache entry.
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"usage_credits_remaining":      10,
		"usage_credits_query_revision": 2,
		"usage_credits_updated_at":     time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if !AccountPoolReadyContext(context.Background()) {
		t.Fatal("readiness did not observe the recovered credit balance")
	}
	pool.mu.Lock()
	for index := range pool.accounts {
		if pool.accounts[index].ID == account.ID {
			pool.accounts[index].UsageCreditsRemaining = 0
		}
	}
	pool.mu.Unlock()

	selected, err := GetNextAccountContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != account.ID || selected.UsageCreditsRemaining != 10 {
		t.Fatalf("selected account = %#v, want recovered balance", selected)
	}
}

func TestAccountAttemptLimitDoesNotTrustSchedulingCache(t *testing.T) {
	now := time.Now().UTC()
	pool.mu.Lock()
	previousAccounts := pool.accounts
	previousIndex := pool.index
	pool.accounts = []model.Account{
		{ReauthRequired: true},
		{
			CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			UsageCreditsAvailable: true, UsageCreditsStatus: UsageCreditsStateReady,
			UsageCreditsRemaining: 0, UsageCreditsUpdatedAt: &now,
			UsageCreditsCredentialRevision: 1,
		},
	}
	pool.index = 0
	pool.mu.Unlock()
	t.Cleanup(func() {
		pool.mu.Lock()
		pool.accounts = previousAccounts
		pool.index = previousIndex
		pool.mu.Unlock()
	})

	if limit := accountAttemptLimit(); limit != 2 {
		t.Fatalf("account attempt limit = %d, want all 2 cached accounts", limit)
	}
}

func TestUsageCreditsRefreshIntervalValidation(t *testing.T) {
	for _, value := range []string{"1m", "15m", "24h"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("USAGE_CREDITS_REFRESH_INTERVAL", value)
			if _, err := usageCreditsRefreshInterval(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, value := range []string{"invalid", "59s", "25h"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("USAGE_CREDITS_REFRESH_INTERVAL", value)
			if _, err := usageCreditsRefreshInterval(); err == nil {
				t.Fatalf("expected %q to be rejected", value)
			}
		})
	}
}

func TestUsageCreditsRefreshDueUsesLastAttempt(t *testing.T) {
	now := time.Now().UTC()
	account := model.Account{}
	if !usageCreditsRefreshDue(account, now, 15*time.Minute) {
		t.Fatal("account without an attempt should be due")
	}
	recent := now.Add(-time.Minute)
	account.UsageCreditsLastAttemptAt = &recent
	if usageCreditsRefreshDue(account, now, 15*time.Minute) {
		t.Fatal("recent attempt was refreshed too early")
	}
	old := now.Add(-16 * time.Minute)
	account.UsageCreditsLastAttemptAt = &old
	if !usageCreditsRefreshDue(account, now, 15*time.Minute) {
		t.Fatal("stale attempt was not refreshed")
	}
	periodEnd := now.Add(-time.Second)
	account.UsageCreditsLastAttemptAt = &recent
	account.UsageCreditsPeriodEnd = &periodEnd
	if !usageCreditsRefreshDue(account, now, 15*time.Minute) {
		t.Fatal("expired billing period was not refreshed immediately")
	}
}

func TestScheduledUsageCreditsRefreshRunsDueAccounts(t *testing.T) {
	setCreditsTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quotas/me/tokens" {
			t.Fatalf("unexpected scheduled path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"remaining":8,"totalConsumedByUser":12,"totalUserBudget":20}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_GATEWAY_BASE_URL", server.URL)
	setupCreditsTestDB(t, "scheduled-refresh.db")
	encrypted, err := secret.Encrypt("scheduled-api-key")
	if err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ClientID: "credits-scheduled", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	refreshDueUsageCredits(context.Background(), time.Minute)

	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UsageCreditsStatus != UsageCreditsStateReady || stored.UsageCreditsRemaining != 8 {
		t.Fatalf("scheduled refresh did not persist the balance: %#v", stored)
	}
	var lease model.SchedulerLease
	if err := database.GetDB().Where("name = ?", usageCreditsRefreshJobName).First(&lease).Error; err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "" {
		t.Fatalf("scheduler lease was not released: %#v", lease)
	}
}
