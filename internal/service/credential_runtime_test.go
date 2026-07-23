package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"

	"gorm.io/gorm"
)

func TestApplyZencoderAPIKeyIsMutuallyExclusive(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	encrypted, err := secret.Encrypt("zen-api-key")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "https://gateway.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{CredentialType: model.CredentialAPIKey, APIKey: encrypted}
	if err := ApplyZencoderAuth(context.Background(), request, account); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("zencoder-api-key") != "zen-api-key" || request.Header.Get("Authorization") != "" {
		t.Fatalf("unexpected API-key headers: %#v", request.Header)
	}
	account.AccessToken = "oauth-token"
	if err := ApplyZencoderAuth(context.Background(), request, account); err == nil {
		t.Fatal("expected mixed OAuth/API-key credentials to be rejected")
	}
}

func TestRefreshLockHonorsContextAndReclaimsEntry(t *testing.T) {
	key := "lock-test"
	first, err := acquireRefreshLock(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := acquireRefreshLock(ctx, key); err == nil {
		t.Fatal("expected context cancellation while waiting for refresh lock")
	}
	first()
	refreshLocks.Lock()
	_, exists := refreshLocks.entries[key]
	refreshLocks.Unlock()
	if exists {
		t.Fatal("idle refresh lock was not reclaimed")
	}
}

func TestOAuthRefreshSingleflightAndRotation(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32))))
	var calls atomic.Int32
	server := newTestOAuthServer(t, &calls)
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	path := filepath.Join(t.TempDir(), "refresh.db")
	if err := database.Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	access, err := secret.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := secret.Encrypt("old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{ClientID: "refresh-singleflight", CredentialType: model.CredentialOAuth, OAuthProvider: "frontegg", AccessToken: access, RefreshToken: refresh, TokenExpiresAt: time.Now().Add(-time.Hour), HealthState: model.AccountHealthHealthy}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *account
			if _, err := GetAccessToken(context.Background(), &copy); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh endpoint called %d times", calls.Load())
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := secret.Decrypt(stored.AccessToken); err != nil || got != "new-access" {
		t.Fatalf("rotated access token not persisted: %q %v", got, err)
	}
	if got, err := secret.Decrypt(stored.RefreshToken); err != nil || got != "new-refresh" {
		t.Fatalf("rotated refresh token not persisted: %q %v", got, err)
	}
	previousRevision := stored.CredentialRevision
	if err := ForceOAuthRefresh(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("forced 401 recovery refresh called endpoint %d times", calls.Load())
	}
	stored = model.Account{}
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRevision != previousRevision+1 {
		t.Fatalf("credential revision did not advance: %d -> %d", previousRevision, stored.CredentialRevision)
	}
}

func TestOAuthRefreshDoesNotOverwriteNewerCredentials(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))
	started := make(chan struct{})
	release := make(chan struct{}, 1)
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"stale-access","refreshToken":"stale-refresh","expiresIn":3600}`))
	}))
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
		server.Close()
	})
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "refresh-cas.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	oldAccess, err := secret.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	oldRefresh, err := secret.Encrypt("old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "refresh-cas", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: oldAccess, RefreshToken: oldRefresh,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		copy := *account
		_, err := GetAccessToken(context.Background(), &copy)
		errCh <- err
	}()
	<-started
	newAccess, err := secret.Encrypt("login-access")
	if err != nil {
		t.Fatal(err)
	}
	newRefresh, err := secret.Encrypt("login-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"access_token": newAccess, "refresh_token": newRefresh,
		"token_expires_at": time.Now().Add(time.Hour), "credential_revision": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	if err := <-errCh; !errors.Is(err, ErrCredentialInvalidated) {
		t.Fatalf("refresh error = %v, want ErrCredentialInvalidated", err)
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := secret.Decrypt(stored.AccessToken); err != nil || got != "login-access" {
		t.Fatalf("newer access token was overwritten: %q %v", got, err)
	}
	if stored.CredentialRevision != 2 {
		t.Fatalf("credential revision = %d, want 2", stored.CredentialRevision)
	}
}

func TestOAuthRefreshLeaseHeartbeatRenewsAndCancelsOnOwnershipLoss(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("l", 32))))
	previousInterval := oauthRefreshLeaseHeartbeatInterval
	oauthRefreshLeaseHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { oauthRefreshLeaseHeartbeatInterval = previousInterval })

	started := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseUpstream) }) })
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "refresh-heartbeat.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	accessToken, err := secret.Encrypt("expired-access")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := secret.Encrypt("single-use-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "refresh-heartbeat", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: accessToken, RefreshToken: refreshToken,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}

	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	t.Cleanup(cancelRefresh)
	errCh := make(chan error, 1)
	go func() {
		copy := *account
		_, err := GetAccessToken(refreshCtx, &copy)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refresh request")
	}
	var initial model.Account
	if err := database.GetDB().Select("refresh_lease_until").First(&initial, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if initial.RefreshLeaseUntil == nil {
		t.Fatal("refresh lease was not claimed")
	}
	initialUntil := *initial.RefreshLeaseUntil
	renewed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var latest model.Account
		if err := database.GetDB().Select("refresh_lease_until").First(&latest, account.ID).Error; err != nil {
			t.Fatal(err)
		}
		if latest.RefreshLeaseUntil != nil && latest.RefreshLeaseUntil.After(initialUntil) {
			renewed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !renewed {
		t.Fatal("refresh lease heartbeat did not renew the lease")
	}

	otherUntil := time.Now().Add(time.Minute)
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"refresh_lease_id": "other-instance", "refresh_lease_until": otherUntil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrCredentialInvalidated) {
			t.Fatalf("refresh error = %v, want lease ownership error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was not canceled after lease ownership loss")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RefreshLeaseID != "other-instance" || stored.ReauthRequired {
		t.Fatalf("stale cleanup or refresh failure corrupted ownership/health: %+v", stored)
	}
}

func TestCanceledOAuthRefreshStillReleasesDatabaseLease(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	started := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseUpstream) }) })
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "refresh-cancel-cleanup.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	accessToken, err := secret.Encrypt("expired-access")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := secret.Encrypt("cancel-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "refresh-cancel-cleanup", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: accessToken, RefreshToken: refreshToken,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		copy := *account
		_, err := GetAccessToken(ctx, &copy)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refresh request")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh error = %v, want canceled context", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled refresh did not return")
	}
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RefreshLeaseID != "" || stored.RefreshLeaseUntil != nil {
		t.Fatalf("canceled refresh left database lease behind: %+v", stored)
	}
}

func TestOAuthRefreshDoesNotClearConcurrentReauth(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("j", 32))))
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"stale-access","refreshToken":"stale-refresh","expiresIn":3600}`))
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	if err := database.Init(filepath.Join(t.TempDir(), "refresh-health-cas.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	oldAccess, err := secret.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	oldRefresh, err := secret.Encrypt("old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "refresh-health-cas", CredentialType: model.CredentialOAuth,
		OAuthProvider: "frontegg", AccessToken: oldAccess, RefreshToken: oldRefresh,
		TokenExpiresAt: time.Now().Add(-time.Hour), CredentialRevision: 1,
		HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		copy := *account
		_, err := GetAccessToken(context.Background(), &copy)
		errCh <- err
	}()
	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("refresh returned before reaching upstream: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OAuth refresh request")
	}

	concurrent := *account
	markAccountReauthRequired(&concurrent, "concurrent_reauth")
	releaseOnce.Do(func() { close(release) })
	if err := <-errCh; !errors.Is(err, ErrCredentialInvalidated) {
		t.Fatalf("refresh error = %v, want ErrCredentialInvalidated", err)
	}

	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.ReauthRequired || stored.HealthState != accountErrorReauth || stored.HealthRevision != account.HealthRevision+1 {
		t.Fatalf("refresh cleared newer reauth state: %+v", stored)
	}
	if got, err := secret.Decrypt(stored.AccessToken); err != nil || got != "old-access" {
		t.Fatalf("stale refresh token was persisted: %q %v", got, err)
	}
}

func TestApplyZencoderAuthReloadsNewerCredentialRevision(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("u", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "credential-reload.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	oldKey, err := secret.Encrypt("old-key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{ClientID: "old-client", CredentialType: model.CredentialAPIKey, APIKey: oldKey, CredentialRevision: 1}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	stale := *account
	newKey, err := secret.Encrypt("new-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"client_id": "new-client", "api_key": newKey, "credential_revision": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://gateway.invalid", nil)
	if err := ApplyZencoderAuth(context.Background(), request, &stale); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("zencoder-api-key") != "new-key" || stale.ClientID != "new-client" || stale.CredentialRevision != 2 {
		t.Fatalf("stale credential was not reloaded: account=%+v headers=%#v", stale, request.Header)
	}
}

func newTestOAuthServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh_token" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`))
	}))
}

func TestAccountHealthPersistsRetryAfterAndClears(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 32))))
	path := filepath.Join(t.TempDir(), "health.db")
	if err := database.Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	account := &model.Account{ClientID: "health-test", CredentialType: model.CredentialAPIKey, APIKey: "enc:v1:placeholder"}
	// Store an actual encrypted value without ever writing a real credential to
	// logs; the value is only a fixture for the database row.
	account.APIKey, err = secret.Encrypt("fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	MarkAccountFailure(account, http.StatusTooManyRequests, 2*time.Second, nil)
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HealthState != accountErrorRateLimit || stored.ReauthRequired || stored.CooldownUntil == nil {
		t.Fatalf("unexpected persisted health: %+v", stored)
	}
	MarkAccountHealthy(account)
	stored = model.Account{}
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HealthState != model.AccountHealthHealthy || stored.CooldownUntil != nil || stored.FailureCount != 0 {
		t.Fatalf("health did not clear: %+v", stored)
	}
}

func TestStaleHealthWriteDoesNotPoisonRotatedCredential(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "health-revision.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	encrypted, err := secret.Encrypt("old-key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "health-revision", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	stale := *account
	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"credential_revision": 2, "health_state": model.AccountHealthHealthy, "reauth_required": false,
	}).Error; err != nil {
		t.Fatal(err)
	}

	MarkAccountFailure(&stale, http.StatusUnauthorized, 0, nil)
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRevision != 2 || stored.ReauthRequired || stored.HealthState != model.AccountHealthHealthy {
		t.Fatalf("stale failure poisoned rotated credential: %+v", stored)
	}
}

func TestSuccessfulOldRequestDoesNotClearReauthRequired(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("w", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "health-reauth.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	encrypted, err := secret.Encrypt("key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "health-reauth", CredentialType: model.CredentialAPIKey, APIKey: encrypted,
		CredentialRevision: 1, HealthState: accountErrorReauth, ReauthRequired: true,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	MarkAccountFailure(account, http.StatusTooManyRequests, 0, nil)
	MarkAccountHealthy(account)
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.ReauthRequired || stored.HealthState != accountErrorReauth {
		t.Fatalf("successful stale request cleared permanent reauth state: %+v", stored)
	}
}

func TestHealthRevisionRejectsOutOfOrderResults(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "health-event-cas.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	encrypted, err := secret.Encrypt("key")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{
		ClientID: "health-event-cas", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	if account.HealthRevision != 1 {
		t.Fatalf("initial health revision = %d, want 1", account.HealthRevision)
	}
	RefreshAccountPool()

	staleSuccess := *account
	current := *account
	MarkAccountFailure(&current, http.StatusTooManyRequests, time.Minute, nil)
	if current.HealthRevision != 2 {
		t.Fatalf("failure health revision = %d, want 2", current.HealthRevision)
	}
	MarkAccountHealthy(&staleSuccess)

	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HealthRevision != 2 || stored.HealthState != accountErrorRateLimit || stored.CooldownUntil == nil {
		t.Fatalf("stale success cleared newer failure: %+v", stored)
	}

	staleFailure := current
	MarkAccountHealthy(&current)
	if current.HealthRevision != 3 {
		t.Fatalf("success health revision = %d, want 3", current.HealthRevision)
	}
	MarkAccountFailure(&staleFailure, http.StatusInternalServerError, time.Minute, nil)
	stored = model.Account{}
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HealthRevision != 3 || stored.HealthState != model.AccountHealthHealthy || stored.CooldownUntil != nil {
		t.Fatalf("stale failure replaced newer success: %+v", stored)
	}

	// Simulate a delayed cache publication after the newer database write.
	updatePoolHealth(staleFailure)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, cached := range pool.accounts {
		if cached.ID == account.ID {
			if cached.HealthRevision != 3 || cached.HealthState != model.AccountHealthHealthy || cached.CooldownUntil != nil {
				t.Fatalf("stale cache publication replaced newer health: %+v", cached)
			}
			return
		}
	}
	t.Fatal("test account missing from pool cache")
}

func TestConcurrentRoundRobinReservesDistinctStarts(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("q", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "round-robin.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	future := time.Now().Add(time.Hour)
	for index := 0; index < 3; index++ {
		encrypted, err := secret.Encrypt("round-robin-key-" + strconv.Itoa(index))
		if err != nil {
			t.Fatal(err)
		}
		if err := database.GetDB().Create(&model.Account{
			ClientID: "round-robin-" + strconv.Itoa(index), CredentialType: model.CredentialAPIKey,
			APIKey: encrypted, CredentialRevision: 1, HealthState: model.AccountHealthHealthy,
			CooldownUntil: func() *time.Time {
				if index == 0 {
					return &future
				}
				return nil
			}(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	RefreshAccountPool()
	const callers = 32
	start := make(chan struct{})
	ids := make(chan uint, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			account, err := GetNextAccount()
			if err != nil {
				errs <- err
				return
			}
			ids <- account.ID
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	counts := make(map[uint]int)
	for id := range ids {
		counts[id]++
	}
	if len(counts) != 2 {
		t.Fatalf("concurrent round-robin selected %d accounts: %v", len(counts), counts)
	}
	var values []int
	for _, count := range counts {
		values = append(values, count)
	}
	if difference := values[0] - values[1]; difference < -1 || difference > 1 {
		t.Fatalf("concurrent round-robin was imbalanced: %v", counts)
	}
}

func TestResetAllCreditsUsesDailyLease(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	path := filepath.Join(t.TempDir(), "scheduler.db")
	if err := database.Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	encrypted, err := secret.Encrypt("scheduler-fixture")
	if err != nil {
		t.Fatal(err)
	}
	account := &model.Account{ClientID: "scheduler-test", CredentialType: model.CredentialAPIKey, APIKey: encrypted, DailyUsed: 9, LastResetDate: ""}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 19, 9, 9, 0, 0, time.UTC)
	if err := resetAllCreditsAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().First(account, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if account.DailyUsed != 0 || account.LastResetDate != "2026-07-19" {
		t.Fatalf("credits were not reset: %+v", account)
	}
	account.DailyUsed = 7
	if err := database.GetDB().Save(account).Error; err != nil {
		t.Fatal(err)
	}
	if err := resetAllCreditsAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().First(account, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if account.DailyUsed != 7 {
		t.Fatal("second scheduler instance reran the same daily job")
	}
}

func TestGatewayQuotaRejectsOldPeriodsAndCredentialRevisions(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("q", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "quota-order.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	encrypted, err := secret.Encrypt("quota-fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC)
	account := &model.Account{
		ClientID: "quota-order", CredentialType: model.CredentialAPIKey,
		APIKey: encrypted, CredentialRevision: 1,
		QuotaUsed: 10, QuotaLimit: 100, CreditRefreshTime: periodEnd,
	}
	if err := database.GetDB().Create(account).Error; err != nil {
		t.Fatal(err)
	}

	oldResponse := &http.Response{Header: http.Header{
		"Zen-Pricing-Period-Cost":  []string{"2"},
		"Zen-Pricing-Period-Limit": []string{"50"},
		"Zen-Pricing-Period-End":   []string{periodEnd.Add(-24 * time.Hour).Format(time.RFC3339)},
	}}
	UpdateAccountCreditsFromResponse(account, oldResponse, 1)
	var stored model.Account
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.QuotaUsed != 10 || stored.QuotaLimit != 100 {
		t.Fatalf("old pricing period overwrote quota: used=%v limit=%v", stored.QuotaUsed, stored.QuotaLimit)
	}
	if stored.DailyUsed != 1 || stored.TotalUsed != 1 {
		t.Fatalf("local usage was not recorded: daily=%v total=%v", stored.DailyUsed, stored.TotalUsed)
	}

	newerResponse := &http.Response{Header: http.Header{
		"Zen-Pricing-Period-Cost":  []string{"12"},
		"Zen-Pricing-Period-Limit": []string{"100"},
		"Zen-Pricing-Period-End":   []string{periodEnd.Format(time.RFC3339)},
	}}
	UpdateAccountCreditsFromResponse(account, newerResponse, 1)
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.QuotaUsed != 12 {
		t.Fatalf("newer quota snapshot was not stored: %v", stored.QuotaUsed)
	}
	lateSamePeriod := &http.Response{Header: http.Header{
		"Zen-Pricing-Period-Cost":  []string{"11"},
		"Zen-Pricing-Period-Limit": []string{"90"},
		"Zen-Pricing-Period-End":   []string{periodEnd.Format(time.RFC3339)},
	}}
	UpdateAccountCreditsFromResponse(account, lateSamePeriod, 1)
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.QuotaUsed != 12 || stored.QuotaLimit != 100 {
		t.Fatalf("late same-period snapshot regressed quota: used=%v limit=%v", stored.QuotaUsed, stored.QuotaLimit)
	}

	if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).
		Update("credential_revision", gorm.Expr("credential_revision + 1")).Error; err != nil {
		t.Fatal(err)
	}
	staleCredentialResponse := &http.Response{Header: http.Header{
		"Zen-Pricing-Period-Cost": []string{"99"},
		"Zen-Pricing-Period-End":  []string{periodEnd.Add(24 * time.Hour).Format(time.RFC3339)},
	}}
	UpdateAccountCreditsFromResponse(account, staleCredentialResponse, 1)
	if err := database.GetDB().First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.QuotaUsed != 12 {
		t.Fatalf("stale credential revision overwrote quota: %v", stored.QuotaUsed)
	}
	if stored.DailyUsed != 4 || stored.TotalUsed != 4 {
		t.Fatalf("credential rotation dropped completed request usage: daily=%v total=%v", stored.DailyUsed, stored.TotalUsed)
	}
}
