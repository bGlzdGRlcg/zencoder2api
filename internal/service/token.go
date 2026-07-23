package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"

	"gorm.io/gorm"
)

const (
	oauthRefreshSkew           = time.Minute
	oauthDefaultExpiry         = 40 * time.Minute
	oauthRefreshLease          = 30 * time.Second
	oauthRefreshCleanupTimeout = 5 * time.Second
	oauthRefreshPollDelay      = 50 * time.Millisecond
)

var zencoderOAuthHTTPClient = newDirectHTTPClient(15 * time.Second)
var oauthRefreshLeaseHeartbeatInterval = oauthRefreshLease / 3

var (
	ErrCredentialInvalidated = errors.New("account credential was deleted or replaced")
	ErrAccountUnavailable    = errors.New("account is temporarily unavailable")
)

type refreshLockEntry struct {
	semaphore  chan struct{}
	references int
}

var refreshLocks = struct {
	sync.Mutex
	entries map[string]*refreshLockEntry
}{entries: make(map[string]*refreshLockEntry)}

func acquireRefreshLock(ctx context.Context, key string) (func(), error) {
	refreshLocks.Lock()
	entry := refreshLocks.entries[key]
	if entry == nil {
		entry = &refreshLockEntry{semaphore: make(chan struct{}, 1)}
		refreshLocks.entries[key] = entry
	}
	entry.references++
	refreshLocks.Unlock()

	select {
	case entry.semaphore <- struct{}{}:
		return func() {
			<-entry.semaphore
			releaseRefreshLockReference(key, entry)
		}, nil
	case <-ctx.Done():
		releaseRefreshLockReference(key, entry)
		return nil, ctx.Err()
	}
}

func releaseRefreshLockReference(key string, entry *refreshLockEntry) {
	refreshLocks.Lock()
	defer refreshLocks.Unlock()
	entry.references--
	if entry.references == 0 && refreshLocks.entries[key] == entry {
		delete(refreshLocks.entries, key)
	}
}

// InvalidateAccount drops an idle refresh lock after an account deletion or
// credential rotation. Active callers still fail the database existence check
// in ensureAccountUsable before they can send another request.
func InvalidateAccount(id uint) {
	if id == 0 {
		return
	}
	key := fmt.Sprintf("account-%d", id)
	refreshLocks.Lock()
	if entry := refreshLocks.entries[key]; entry != nil && entry.references == 0 {
		delete(refreshLocks.entries, key)
	}
	refreshLocks.Unlock()
}

type oauthRefreshHTTPError struct {
	statusCode int
	retryAfter string
}

// oauthRefreshLeaseHeartbeat keeps a database lease valid while the upstream
// refresh request is in flight. Losing the lease cancels the derived context
// so a stale response cannot be treated as authoritative.
type oauthRefreshLeaseHeartbeat struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	errMu    sync.Mutex
	err      error
}

func startOAuthRefreshLeaseHeartbeat(parent context.Context, id uint, holder string) (context.Context, *oauthRefreshLeaseHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &oauthRefreshLeaseHeartbeat{cancel: cancel, done: make(chan struct{})}
	interval := oauthRefreshLeaseHeartbeatInterval
	if interval <= 0 || interval >= oauthRefreshLease {
		interval = oauthRefreshLease / 3
	}
	go heartbeat.run(ctx, id, holder, interval)
	return ctx, heartbeat
}

func (h *oauthRefreshLeaseHeartbeat) run(ctx context.Context, id uint, holder string, interval time.Duration) {
	defer close(h.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renewOAuthRefreshLease(ctx, id, holder); err != nil {
				if ctx.Err() != nil {
					return
				}
				h.errMu.Lock()
				h.err = fmt.Errorf("renew OAuth refresh lease: %w", err)
				h.errMu.Unlock()
				h.cancel()
				return
			}
		}
	}
}

func (h *oauthRefreshLeaseHeartbeat) Err() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *oauthRefreshLeaseHeartbeat) Stop() error {
	h.stopOnce.Do(func() {
		h.cancel()
		<-h.done
	})
	return h.Err()
}

func (e *oauthRefreshHTTPError) Error() string {
	return fmt.Sprintf("refresh Zencoder OAuth token: HTTP %d", e.statusCode)
}

func zencoderAuthBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("ZENCODER_AUTH_BASE_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return "https://auth.zencoder.ai"
}

// ApplyZencoderAuth applies exactly one Gateway credential. OAuth accounts use
// Bearer while Zen CLI API-key accounts use zencoder-api-key.
func ApplyZencoderAuth(ctx context.Context, req *http.Request, account *model.Account) error {
	return applyZencoderAuth(ctx, req, account, true)
}

// applyZencoderAuthWithoutHealthMutation is used by optional background requests.
// It still refreshes an expired OAuth token, but a failed refresh cannot mark
// the account unhealthy or require re-authentication.
func applyZencoderAuthWithoutHealthMutation(ctx context.Context, req *http.Request, account *model.Account) error {
	return applyZencoderAuth(ctx, req, account, false)
}

func applyZencoderAuth(ctx context.Context, req *http.Request, account *model.Account, refreshFailureAffectsHealth bool) error {
	if err := ensureAccountUsable(ctx, account); err != nil {
		return err
	}
	req.Header.Del("Authorization")
	req.Header.Del("zencoder-api-key")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")

	switch account.CredentialType {
	case model.CredentialOAuth:
		if strings.TrimSpace(account.APIKey) != "" {
			return errors.New("OAuth account contains an API key")
		}
		accessToken, err := getAccessTokenWithHealthMode(ctx, account, refreshFailureAffectsHealth)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
		return nil
	case model.CredentialAPIKey:
		if strings.TrimSpace(account.AccessToken) != "" || strings.TrimSpace(account.RefreshToken) != "" {
			return errors.New("API-key account contains OAuth tokens")
		}
		apiKey, err := secret.Plaintext(account.APIKey)
		if err != nil {
			return fmt.Errorf("load Zencoder API key: %w", err)
		}
		if apiKey = strings.TrimSpace(apiKey); apiKey == "" {
			return errors.New("account has no Zencoder API key")
		}
		req.Header.Set("zencoder-api-key", apiKey)
		return nil
	default:
		return errors.New("account has an unsupported credential type")
	}
}

func getAccessTokenWithHealthMode(ctx context.Context, account *model.Account, refreshFailureAffectsHealth bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.CredentialType != model.CredentialOAuth {
		return "", errors.New("account is not a Zencoder OAuth account")
	}
	var err error
	account.AccessToken, err = secret.Plaintext(account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("load Zencoder OAuth access token: %w", err)
	}
	account.RefreshToken, err = secret.Plaintext(account.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("load Zencoder OAuth refresh token: %w", err)
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return "", errors.New("account has no Zencoder OAuth access token")
	}
	if !tokenNeedsRefresh(account.AccessToken, account.TokenExpiresAt) {
		return strings.TrimSpace(account.AccessToken), nil
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return "", errors.New("zencoder OAuth refresh token is missing")
	}

	lockKey := fmt.Sprintf("account-%d", account.ID)
	if account.ID == 0 {
		lockKey = "client-" + account.ClientID
	}
	unlock, err := acquireRefreshLock(ctx, lockKey)
	if err != nil {
		return "", fmt.Errorf("wait for Zencoder OAuth refresh: %w", err)
	}
	defer unlock()

	if account.ID == 0 {
		return refreshTransientOAuthAccount(ctx, account)
	}
	return refreshStoredOAuthAccount(ctx, account, refreshFailureAffectsHealth)
}

// ForceOAuthRefresh performs the single recovery refresh allowed after a
// Gateway 401. It is deliberately separate from normal expiry checks so a
// still-dated but revoked access token can be recovered once.
func ForceOAuthRefresh(ctx context.Context, account *model.Account) error {
	return forceOAuthRefresh(ctx, account, true)
}

// forceOAuthRefreshWithoutHealthMutation gives optional background requests a
// single recovery attempt without allowing a billing endpoint to disable an
// otherwise usable inference credential.
func forceOAuthRefreshWithoutHealthMutation(ctx context.Context, account *model.Account) error {
	return forceOAuthRefresh(ctx, account, false)
}

func forceOAuthRefresh(ctx context.Context, account *model.Account, refreshFailureAffectsHealth bool) error {
	if account == nil || account.CredentialType != model.CredentialOAuth {
		return errors.New("account is not a Zencoder OAuth account")
	}
	expired := time.Now().Add(-oauthRefreshSkew)
	account.TokenExpiresAt = expired
	if account.ID != 0 {
		db := database.GetDB()
		if db == nil {
			return errDatabaseUnavailable
		}
		result := db.WithContext(ctx).Model(&model.Account{}).Where("id = ? AND credential_revision = ?", account.ID, account.CredentialRevision).Update("token_expires_at", expired)
		if result.Error != nil {
			return fmt.Errorf("expire rejected OAuth access token: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrCredentialInvalidated
		}
	}
	_, err := getAccessTokenWithHealthMode(ctx, account, refreshFailureAffectsHealth)
	return err
}

func ensureAccountUsable(ctx context.Context, account *model.Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if account.ID == 0 {
		return nil
	}
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	var stored model.Account
	if err := db.WithContext(ctx).First(&stored, account.ID).Error; err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrCredentialInvalidated
		}
		return fmt.Errorf("verify account credential: %w", err)
	}
	if stored.ReauthRequired || stored.CooldownUntil != nil && stored.CooldownUntil.After(time.Now()) {
		return ErrAccountUnavailable
	}
	credentialsChanged := stored.ClientID != account.ClientID ||
		stored.CredentialType != account.CredentialType ||
		stored.CredentialRevision != account.CredentialRevision
	if credentialsChanged {
		if stored.CredentialType != account.CredentialType || stored.CredentialRevision <= account.CredentialRevision {
			return ErrCredentialInvalidated
		}
		*account = mergeAccountCredential(*account, stored)
		RefreshAccountPool()
	}
	return nil
}

func refreshTransientOAuthAccount(ctx context.Context, account *model.Account) (string, error) {
	if !tokenNeedsRefresh(account.AccessToken, account.TokenExpiresAt) {
		return strings.TrimSpace(account.AccessToken), nil
	}
	tokens, err := refreshZencoderOAuthToken(ctx, account)
	if err != nil {
		return "", err
	}
	account.AccessToken = tokens.AccessToken
	account.RefreshToken = tokens.RefreshToken
	account.TokenExpiresAt = tokens.ExpiresAt
	return strings.TrimSpace(tokens.AccessToken), nil
}

func refreshStoredOAuthAccount(ctx context.Context, account *model.Account, refreshFailureAffectsHealth bool) (string, error) {
	db := database.GetDB()
	if db == nil {
		return "", errDatabaseUnavailable
	}
	holder, err := randomURLToken(18)
	if err != nil {
		return "", fmt.Errorf("create OAuth refresh lease: %w", err)
	}
	for {
		latest, loadErr := loadOAuthAccount(ctx, account.ID)
		if loadErr != nil {
			return "", loadErr
		}
		*account = mergeAccountCredential(*account, *latest)
		if !tokenNeedsRefresh(account.AccessToken, account.TokenExpiresAt) {
			return strings.TrimSpace(account.AccessToken), nil
		}
		claimed, claimErr := claimOAuthRefreshLease(ctx, account.ID, holder)
		if claimErr != nil {
			return "", claimErr
		}
		if claimed {
			break
		}
		if err := waitForOAuthRefresh(ctx, account.ID, account.AccessToken); err != nil {
			return "", err
		}
	}

	leaseCtx, heartbeat := startOAuthRefreshLeaseHeartbeat(ctx, account.ID, holder)
	defer func() {
		if err := heartbeat.Stop(); err != nil {
			logging.Warnf("Stop OAuth refresh lease heartbeat: %v", err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthRefreshCleanupTimeout)
		defer cancel()
		if err := releaseOAuthRefreshLease(cleanupCtx, account.ID, holder); err != nil {
			logging.Warnf("Release OAuth refresh lease: %v", err)
		}
	}()
	latest, err := loadOAuthAccount(leaseCtx, account.ID)
	if err != nil {
		return "", err
	}
	if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
		return "", heartbeatErr
	}
	*account = mergeAccountCredential(*account, *latest)
	if !tokenNeedsRefresh(account.AccessToken, account.TokenExpiresAt) {
		return strings.TrimSpace(account.AccessToken), nil
	}
	refreshRevision := account.CredentialRevision
	refreshHealthRevision := account.HealthRevision
	tokens, err := refreshZencoderOAuthToken(leaseCtx, account)
	if err != nil {
		if heartbeatErr := heartbeat.Stop(); heartbeatErr != nil {
			return "", heartbeatErr
		}
		if refreshFailureAffectsHealth {
			var refreshErr *oauthRefreshHTTPError
			if errors.As(err, &refreshErr) {
				MarkAccountFailure(account, refreshErr.statusCode, parseRetryAfter(refreshErr.retryAfter), err)
				if refreshErr.statusCode == http.StatusBadRequest || refreshErr.statusCode == http.StatusUnauthorized {
					markAccountReauthRequired(account, "invalid_grant")
				}
			} else {
				MarkAccountFailure(account, 0, 0, err)
			}
		}
		return "", err
	}
	// Refill the lease immediately before the local writeback stage,
	// then stop the background renewer so it cannot race with the CAS that
	// clears lease ownership on success.
	if err := renewOAuthRefreshLease(leaseCtx, account.ID, holder); err != nil {
		return "", fmt.Errorf("renew OAuth refresh lease before writeback: %w", err)
	}
	if heartbeatErr := heartbeat.Stop(); heartbeatErr != nil {
		return "", heartbeatErr
	}

	updates := map[string]interface{}{
		"access_token":        tokens.AccessToken,
		"refresh_token":       tokens.RefreshToken,
		"token_expires_at":    tokens.ExpiresAt,
		"credential_revision": gorm.Expr("credential_revision + 1"),
		"refresh_lease_id":    "",
		"refresh_lease_until": gorm.Expr("NULL"),
	}
	if refreshFailureAffectsHealth {
		updates["health_state"] = model.AccountHealthHealthy
		updates["cooldown_until"] = gorm.Expr("NULL")
		updates["last_error_class"] = ""
		updates["last_error_at"] = gorm.Expr("NULL")
		updates["failure_count"] = 0
		updates["reauth_required"] = false
		updates["health_revision"] = gorm.Expr("health_revision + 1")
	}
	result := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND refresh_lease_id = ? AND credential_revision = ? AND health_revision = ? AND reauth_required = ?", account.ID, holder, refreshRevision, refreshHealthRevision, false).
		Updates(updates)
	if result.Error != nil {
		return "", fmt.Errorf("persist refreshed Zencoder OAuth token: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return "", ErrCredentialInvalidated
	}
	account.AccessToken = tokens.AccessToken
	account.RefreshToken = tokens.RefreshToken
	account.TokenExpiresAt = tokens.ExpiresAt
	if refreshFailureAffectsHealth {
		account.HealthState = model.AccountHealthHealthy
		account.CooldownUntil = nil
		account.LastErrorClass = ""
		account.LastErrorAt = nil
		account.FailureCount = 0
		account.ReauthRequired = false
		account.HealthRevision++
	}
	account.CredentialRevision++
	RefreshAccountPool()
	return strings.TrimSpace(tokens.AccessToken), nil
}

func loadOAuthAccount(ctx context.Context, id uint) (*model.Account, error) {
	db := database.GetDB()
	if db == nil {
		return nil, errDatabaseUnavailable
	}
	var latest model.Account
	if err := db.WithContext(ctx).First(&latest, id).Error; err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCredentialInvalidated
		}
		return nil, fmt.Errorf("load OAuth account: %w", err)
	}
	var err error
	latest.AccessToken, err = secret.Plaintext(latest.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("load latest access token: %w", err)
	}
	latest.RefreshToken, err = secret.Plaintext(latest.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("load latest refresh token: %w", err)
	}
	return &latest, nil
}

func mergeAccountCredential(dst, src model.Account) model.Account {
	dst.APIKey = src.APIKey
	dst.AccessToken = src.AccessToken
	dst.RefreshToken = src.RefreshToken
	dst.TokenExpiresAt = src.TokenExpiresAt
	dst.OAuthProvider = src.OAuthProvider
	dst.OAuthEmail = src.OAuthEmail
	dst.OAuthUserID = src.OAuthUserID
	dst.OAuthTenantID = src.OAuthTenantID
	dst.OAuthAnonymousID = src.OAuthAnonymousID
	dst.ClientID = src.ClientID
	dst.CredentialType = src.CredentialType
	dst.CredentialRevision = src.CredentialRevision
	dst.HealthRevision = src.HealthRevision
	dst.HealthState = src.HealthState
	dst.CooldownUntil = src.CooldownUntil
	dst.LastErrorClass = src.LastErrorClass
	dst.LastErrorAt = src.LastErrorAt
	dst.FailureCount = src.FailureCount
	dst.ReauthRequired = src.ReauthRequired
	return dst
}

func claimOAuthRefreshLease(ctx context.Context, id uint, holder string) (bool, error) {
	db := database.GetDB()
	if db == nil {
		return false, errDatabaseUnavailable
	}
	now := time.Now()
	until := now.Add(oauthRefreshLease)
	result := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND (refresh_lease_id = ? OR refresh_lease_until IS NULL OR refresh_lease_until <= ?)", id, holder, now).
		Updates(map[string]interface{}{"refresh_lease_id": holder, "refresh_lease_until": until})
	if result.Error != nil {
		return false, fmt.Errorf("claim OAuth refresh lease: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return true, nil
	}
	return false, nil
}

func renewOAuthRefreshLease(ctx context.Context, id uint, holder string) error {
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	now := time.Now()
	result := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND refresh_lease_id = ? AND refresh_lease_until > ?", id, holder, now).
		Update("refresh_lease_until", now.Add(oauthRefreshLease))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCredentialInvalidated
	}
	return nil
}

func releaseOAuthRefreshLease(ctx context.Context, id uint, holder string) error {
	if db := database.GetDB(); db != nil {
		return db.WithContext(ctx).Model(&model.Account{}).Where("id = ? AND refresh_lease_id = ?", id, holder).
			Updates(map[string]interface{}{"refresh_lease_id": "", "refresh_lease_until": gorm.Expr("NULL")}).Error
	}
	return errDatabaseUnavailable
}

func waitForOAuthRefresh(ctx context.Context, id uint, previousAccessToken string) error {
	timer := time.NewTimer(oauthRefreshPollDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	latest, err := loadOAuthAccount(ctx, id)
	if err != nil {
		return err
	}
	if latest.AccessToken != previousAccessToken || !tokenNeedsRefresh(latest.AccessToken, latest.TokenExpiresAt) {
		return nil
	}
	return nil
}

func tokenNeedsRefresh(accessToken string, expiresAt time.Time) bool {
	if strings.TrimSpace(accessToken) == "" {
		return true
	}
	if expiresAt.IsZero() {
		expiresAt = jwtExpiresAt(accessToken)
	}
	return expiresAt.IsZero() || time.Now().Add(oauthRefreshSkew).After(expiresAt)
}

type zencoderOAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func refreshZencoderOAuthToken(ctx context.Context, account *model.Account) (zencoderOAuthTokens, error) {
	provider := account.OAuthProvider
	if provider == "" {
		provider = detectOAuthProviderFromJWT(account.AccessToken)
	}
	body := map[string]string{
		"refreshToken": strings.TrimSpace(account.RefreshToken),
	}
	endpoint := "/refresh_token"
	if provider == "workos" {
		body["provider"] = provider
		endpoint = "/api/auth/refresh"
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zencoderAuthBaseURL()+endpoint, bytes.NewReader(payload))
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	setOAuthServiceHeaders(req, account.OAuthAnonymousID)
	logging.Debugf(
		"Zencoder OAuth refresh request request_id=%s provider=%s endpoint=%s plugin_version=%s refresh_token_bytes=%d",
		logging.RequestIDFromContext(ctx), provider, oauthLogURL(req.URL), req.Header.Get("x-zencoder-plugin-version"), len(strings.TrimSpace(account.RefreshToken)),
	)
	resp, err := zencoderOAuthHTTPClient.Do(req)
	if err != nil {
		return zencoderOAuthTokens{}, fmt.Errorf("refresh Zencoder OAuth token: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	logOAuthHTTPResponse(ctx, "refresh", resp, responseBody)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return zencoderOAuthTokens{}, &oauthRefreshHTTPError{
			statusCode: resp.StatusCode,
			retryAfter: resp.Header.Get("Retry-After"),
		}
	}
	tokens, err := parseOAuthTokens(responseBody)
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = account.RefreshToken
	}
	return tokens, nil
}

func parseOAuthTokens(body []byte) (zencoderOAuthTokens, error) {
	var raw struct {
		AccessToken       string          `json:"accessToken"`
		AccessTokenSnake  string          `json:"access_token"`
		RefreshToken      string          `json:"refreshToken"`
		RefreshTokenSnake string          `json:"refresh_token"`
		ExpiresIn         json.RawMessage `json:"expiresIn"`
		ExpiresInSnake    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return zencoderOAuthTokens{}, fmt.Errorf("decode zencoder OAuth token response: %w", err)
	}
	accessToken := firstNonEmpty(raw.AccessToken, raw.AccessTokenSnake)
	refreshToken := firstNonEmpty(raw.RefreshToken, raw.RefreshTokenSnake)
	if accessToken == "" {
		return zencoderOAuthTokens{}, errors.New("zencoder OAuth response has no access token")
	}
	var expiresAt time.Time
	if seconds := numberSeconds(raw.ExpiresIn); seconds > 0 {
		expiresAt = expiryFromSeconds(seconds)
	} else if seconds := numberSeconds(raw.ExpiresInSnake); seconds > 0 {
		expiresAt = expiryFromSeconds(seconds)
	} else {
		expiresAt = jwtExpiresAt(accessToken)
		if expiresAt.IsZero() {
			expiresAt = time.Now().Add(oauthDefaultExpiry)
		}
	}
	return zencoderOAuthTokens{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt}, nil
}

func expiryFromSeconds(seconds int64) time.Time {
	maxSeconds := int64(^uint64(0)>>1) / int64(time.Second)
	if seconds <= 0 || seconds > maxSeconds {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

func setOAuthServiceHeaders(req *http.Request, anonymousID string) {
	if anonymousID == "" {
		anonymousID = "zencoder2api"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-zencoder-anonymous-id", anonymousID)
	req.Header.Set("x-zencoder-plugin-version", zencoderPluginVersion())
}

func zencoderPluginVersion() string {
	value := strings.TrimSpace(os.Getenv("ZENCODER_PLUGIN_VERSION"))
	if value == "" {
		value = "3.4.1"
	}
	if strings.HasPrefix(value, "vsc-") {
		return value
	}
	return "vsc-" + value
}

func detectOAuthProviderFromJWT(token string) string {
	payload, err := decodeJWTPayload(token)
	if err != nil {
		return "frontegg"
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) == nil && strings.HasPrefix(claims.Issuer, "https://api.workos.com/") {
		return "workos"
	}
	return "frontegg"
}

func logOAuthHTTPResponse(ctx context.Context, operation string, resp *http.Response, body []byte) {
	if !logging.Enabled(logging.LevelDebug) || resp == nil {
		return
	}
	endpoint := ""
	if resp.Request != nil {
		endpoint = oauthLogURL(resp.Request.URL)
	}
	logging.Debugf(
		"Zencoder OAuth %s response request_id=%s status=%d endpoint=%s content_type=%q server=%q upstream_request_id=%q location_present=%t body_bytes=%d",
		operation,
		logging.RequestIDFromContext(ctx),
		resp.StatusCode,
		endpoint,
		resp.Header.Get("Content-Type"),
		resp.Header.Get("Server"),
		firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("x-amzn-requestid"), resp.Header.Get("cf-ray")),
		resp.Header.Get("Location") != "",
		len(body),
	)
}

func oauthLogURL(value *url.URL) string {
	if value == nil {
		return ""
	}
	cleanURL := *value
	cleanURL.User = nil
	cleanURL.RawQuery = ""
	cleanURL.ForceQuery = false
	cleanURL.Fragment = ""
	return cleanURL.String()
}

func jwtExpiresAt(token string) time.Time {
	payload, err := decodeJWTPayload(token)
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		ExpiresAt json.RawMessage `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}
	expiresAt := numberSeconds(claims.ExpiresAt)
	if expiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(expiresAt, 0)
}

func decodeJWTPayload(token string) ([]byte, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil, errors.New("invalid JWT")
	}
	segment := strings.TrimRight(parts[1], "=")
	return base64.RawURLEncoding.DecodeString(segment)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func numberSeconds(value json.RawMessage) int64 {
	raw := strings.Trim(strings.TrimSpace(string(value)), `"`)
	if raw == "" || raw == "null" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return seconds
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 || seconds > float64(^uint64(0)>>1) {
		return 0
	}
	return int64(seconds)
}
