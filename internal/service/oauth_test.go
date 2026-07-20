package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
)

func TestOAuthExchangeContract(t *testing.T) {
	t.Setenv("ZENCODER_PLUGIN_VERSION", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/oauth/token" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["code"] != "code" || body["codeVerifier"] != "verifier" || len(body) != 2 {
			t.Fatalf("unexpected exchange body: %#v", body)
		}
		if r.Header.Get("x-zencoder-anonymous-id") != "anonymous" || r.Header.Get("x-zencoder-plugin-version") != "vsc-3.4.1" {
			t.Fatalf("missing OAuth metadata: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)

	tokens, err := NewOAuthService().exchangeCode(context.Background(), "frontegg", "code", "verifier", "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" || tokens.ExpiresAt.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
}

func TestOAuthWorkOSExchangeContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/token" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["code"] != "code" || body["codeVerifier"] != "verifier" || body["provider"] != "workos" || len(body) != 3 {
			t.Fatalf("unexpected WorkOS exchange body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)

	if _, err := NewOAuthService().exchangeCode(context.Background(), "workos", "code", "verifier", "anonymous"); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRefreshContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/refresh_token" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["refreshToken"] != "old-refresh" || len(body) != 1 {
			t.Fatalf("unexpected refresh body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","expiresIn":3600}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)

	tokens, err := refreshZencoderOAuthToken(context.Background(), &model.Account{
		OAuthProvider:    "frontegg",
		RefreshToken:     "old-refresh",
		OAuthAnonymousID: "anonymous",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "new-access" || tokens.RefreshToken != "old-refresh" {
		t.Fatalf("unexpected rotated tokens: %#v", tokens)
	}
}

func TestOAuthWorkOSRefreshContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/refresh" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["refreshToken"] != "old-refresh" || body["provider"] != "workos" || len(body) != 2 {
			t.Fatalf("unexpected WorkOS refresh body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","expiresIn":3600}`))
	}))
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)
	accessPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://api.workos.com/user_management/client"}`))

	tokens, err := refreshZencoderOAuthToken(context.Background(), &model.Account{
		AccessToken: "header." + accessPayload + ".signature", RefreshToken: "old-refresh", OAuthAnonymousID: "anonymous",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "new-access" || tokens.RefreshToken != "old-refresh" {
		t.Fatalf("unexpected rotated tokens: %#v", tokens)
	}
}

func TestParseOAuthTokensRequiresAccessToken(t *testing.T) {
	if _, err := parseOAuthTokens([]byte(`{"refreshToken":"refresh"}`)); err == nil {
		t.Fatal("expected missing access token error")
	}
}

func TestStartOAuthPersistsEncryptedVerifier(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v", 32))))
	setupOAuthSessionTestDatabase(t, "oauth-start.db")
	result, err := NewOAuthService().StartZencoderLogin("http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	var session model.OAuthSession
	if err := database.GetDB().Where("state = ?", result.State).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if !secret.IsEncrypted(session.CodeVerifier) {
		t.Fatalf("OAuth verifier was not encrypted: %q", session.CodeVerifier)
	}
	verifier, err := secret.Decrypt(session.CodeVerifier)
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(result.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if authorizationURL.Query().Get("code_challenge") != pkceChallenge(verifier) {
		t.Fatal("authorization challenge does not match persisted verifier")
	}
}

func TestOAuthProfileFromAccessToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user","tenantId":"tenant","email":"user@example.test","name":"User"}`))
	profile, err := oauthProfileFromAccessToken("header." + payload + ".signature")
	if err != nil {
		t.Fatal(err)
	}
	if profile.UserID != "user" || profile.TenantID != "tenant" || profile.Email != "user@example.test" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
}

func TestOAuthLogURLRedactsSecrets(t *testing.T) {
	value, err := url.Parse("https://user:password@auth.example.test/api/auth/token?code=secret#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if got := oauthLogURL(value); got != "https://auth.example.test/api/auth/token" {
		t.Fatalf("OAuth log URL = %q", got)
	}
}

func TestCompleteOAuthLoginUsesPKCEAndTokenClaims(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("e", 32))))
	setupOAuthSessionTestDatabase(t, "oauth-complete.db")
	session := createOAuthSessionTestRecord(t, "complete-state")
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user","tenantId":"tenant","email":"user@example.test"}`))
	accessToken := "header." + payload + ".signature"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/auth/token" {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["code"] != "code" || body["codeVerifier"] != "test-verifier" || body["provider"] != "workos" || len(body) != 3 {
			t.Fatalf("unexpected exchange body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  accessToken,
			"refresh_token": "refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)

	result, err := NewOAuthService().CompleteZencoderLogin(context.Background(), session.State, "code", "WORKOS")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("OAuth completion made %d upstream requests, want 1", requests.Load())
	}
	if result.Account == nil || result.Account.OAuthProvider != "workos" || result.Account.OAuthUserID != "user" || result.Account.OAuthTenantID != "tenant" || result.Account.AccessToken != accessToken {
		t.Fatalf("unexpected OAuth account: %#v", result.Account)
	}
	var sessionCount int64
	if err := database.GetDB().Model(&model.OAuthSession{}).Where("id = ?", session.ID).Count(&sessionCount).Error; err != nil {
		t.Fatal(err)
	}
	if sessionCount != 0 {
		t.Fatal("completed OAuth session was not deleted")
	}
}

func TestUpsertOAuthAccountUpdatesExistingIdentity(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 32))))
	if err := database.Init(filepath.Join(t.TempDir(), "oauth-upsert.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	profile := zencoderOAuthProfile{UserID: "user", TenantID: "tenant", Email: "old@example.test"}
	first, err := upsertOAuthAccount(context.Background(), "frontegg", "anonymous-old", zencoderOAuthTokens{
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
	}, profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.Email = "new@example.test"
	second, err := upsertOAuthAccount(context.Background(), "frontegg", "anonymous-new", zencoderOAuthTokens{
		AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: time.Now().Add(2 * time.Hour),
	}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.CredentialRevision != first.CredentialRevision+1 {
		t.Fatalf("OAuth upsert created/revisioned the wrong account: first=%+v second=%+v", first, second)
	}
	if second.OAuthEmail != profile.Email || second.OAuthAnonymousID != "anonymous-new" || second.AccessToken != "access-new" || second.RefreshToken != "refresh-new" {
		t.Fatalf("OAuth identity was not updated: %+v", second)
	}
	var count int64
	if err := database.GetDB().Model(&model.Account{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("OAuth upsert created %d accounts", count)
	}
}

func TestConsumedOAuthSessionCannotBeClaimedAgain(t *testing.T) {
	if err := database.Init(filepath.Join(t.TempDir(), "oauth-consumed.db")); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	session := &model.OAuthSession{
		State: "single-use-state", AnonymousID: "anonymous", Origin: "http://localhost:8080",
		RedirectURL: "http://localhost:8080/oauth/zencoder/callback/single-use-state",
		ExpiresAt:   time.Now().Add(time.Minute),
	}
	if err := database.GetDB().Create(session).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := claimOAuthSession(context.Background(), session.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeOAuthSession(context.Background(), claimed.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := claimOAuthSession(context.Background(), session.State); !errors.Is(err, ErrOAuthSessionInvalidOrExpired) {
		t.Fatalf("consumed session claim error = %v", err)
	}
	var stored model.OAuthSession
	if err := database.GetDB().First(&stored, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ConsumedAt == nil {
		t.Fatal("consumed session was not marked before cleanup")
	}
}

func TestOAuthAccountUpsertRollsBackWhenSessionConsumeFails(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	setupOAuthSessionTestDatabase(t, "oauth-atomic-complete.db")
	session := createOAuthSessionTestRecord(t, "atomic-complete-state")
	claimed, err := claimOAuthSession(context.Background(), session.State)
	if err != nil {
		t.Fatal(err)
	}
	tokens := zencoderOAuthTokens{
		AccessToken: "atomic-access", RefreshToken: "atomic-refresh",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	profile := zencoderOAuthProfile{
		UserID: "atomic-user", TenantID: "atomic-tenant", Email: "atomic@example.test",
	}
	if _, err := upsertAndConsumeOAuthAccount(
		context.Background(), claimed.ID, "wrong-claim", "frontegg", claimed.AnonymousID, tokens, profile,
	); !errors.Is(err, ErrOAuthSessionInvalidOrExpired) {
		t.Fatalf("completion error = %v, want invalid session", err)
	}
	var accountCount int64
	if err := database.GetDB().Model(&model.Account{}).Count(&accountCount).Error; err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("account upsert committed despite consume failure: %d", accountCount)
	}
	var storedSession model.OAuthSession
	if err := database.GetDB().First(&storedSession, claimed.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedSession.ConsumedAt != nil {
		t.Fatal("failed transaction consumed the OAuth session")
	}

	account, err := upsertAndConsumeOAuthAccount(
		context.Background(), claimed.ID, claimed.ClaimID, "frontegg", claimed.AnonymousID, tokens, profile,
	)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID == 0 {
		t.Fatal("successful atomic completion did not create an account")
	}
	if err := database.GetDB().First(&storedSession, claimed.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedSession.ConsumedAt == nil {
		t.Fatal("successful atomic completion did not consume the session")
	}
}

func TestOAuthSessionHeartbeatPreventsClaimTakeover(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 32))))
	setupOAuthSessionTestDatabase(t, "oauth-heartbeat.db")
	session := createOAuthSessionTestRecord(t, "heartbeat-state")
	claimed, err := claimOAuthSession(context.Background(), session.State)
	if err != nil {
		t.Fatal(err)
	}

	ctx, heartbeat := startOAuthSessionClaimHeartbeat(
		context.Background(),
		claimed.ID,
		claimed.ClaimID,
		10*time.Millisecond,
	)
	t.Cleanup(func() { _ = heartbeat.Stop() })
	select {
	case <-ctx.Done():
		t.Fatalf("heartbeat stopped early: %v", heartbeat.Err())
	case <-time.After(50 * time.Millisecond):
	}

	// Simulate a callback that has run longer than the fixed claim TTL. A
	// renewed ClaimedAt must keep a second instance from taking ownership.
	stale := time.Now().Add(-oauthSessionClaimTTL - time.Second)
	if err := database.GetDB().Model(&model.OAuthSession{}).
		Where("id = ? AND claim_id = ?", claimed.ID, claimed.ClaimID).
		Update("claimed_at", stale).Error; err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := claimOAuthSession(context.Background(), session.State); !errors.Is(err, ErrOAuthSessionInvalidOrExpired) {
		t.Fatalf("heartbeat allowed claim takeover: %v", err)
	}
	if err := heartbeat.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteOAuthLoginCancellationStopsHeartbeatAndReleasesClaim(t *testing.T) {
	setupOAuthSessionTestDatabase(t, "oauth-heartbeat-cancel.db")
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))
	session := createOAuthSessionTestRecord(t, "cancel-state")

	var exchangeStarted atomic.Bool
	releaseExchange := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/token" {
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
			return
		}
		exchangeStarted.Store(true)
		select {
		case <-r.Context().Done():
		case <-releaseExchange:
		}
	}))
	defer server.Close()
	defer close(releaseExchange)
	t.Setenv("ZENCODER_AUTH_BASE_URL", server.URL)

	service := NewOAuthService()
	service.claimHeartbeatInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := service.CompleteZencoderLogin(ctx, session.State, "code", "frontegg"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion error = %v, want context deadline", err)
	}
	if !exchangeStarted.Load() {
		t.Fatal("OAuth exchange did not start")
	}

	deadline := time.Now().Add(time.Second)
	for {
		reclaimed, err := claimOAuthSession(context.Background(), session.State)
		if err == nil {
			if releaseErr := releaseOAuthSession(context.Background(), reclaimed.ID, reclaimed.ClaimID); releaseErr != nil {
				t.Fatal(releaseErr)
			}
			break
		}
		if !errors.Is(err, ErrOAuthSessionInvalidOrExpired) {
			t.Fatalf("reclaim error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled callback claim was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func setupOAuthSessionTestDatabase(t *testing.T, name string) {
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

func createOAuthSessionTestRecord(t *testing.T, state string) *model.OAuthSession {
	t.Helper()
	verifier, err := secret.Encrypt("test-verifier")
	if err != nil {
		t.Fatal(err)
	}
	session := &model.OAuthSession{
		State:        state,
		CodeVerifier: verifier,
		AnonymousID:  "anonymous",
		Origin:       "http://localhost:8080",
		RedirectURL:  fmt.Sprintf("http://localhost:8080/oauth/zencoder/callback/%s", state),
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	if err := database.GetDB().Create(session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}
