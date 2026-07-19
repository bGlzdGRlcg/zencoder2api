package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	oauthSessionTTL                    = 10 * time.Minute
	oauthSessionClaimTTL               = 2 * time.Minute
	oauthSessionClaimHeartbeatInterval = oauthSessionClaimTTL / 3
	oauthSessionCleanupTimeout         = 5 * time.Second
)

var ErrOAuthSessionInvalidOrExpired = errors.New("OAuth session is invalid or expired")

type OAuthStartResult struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
}

type OAuthService struct {
	client                 *http.Client
	claimHeartbeatInterval time.Duration
}

func NewOAuthService() *OAuthService {
	return &OAuthService{
		client:                 zencoderOAuthHTTPClient,
		claimHeartbeatInterval: oauthSessionClaimHeartbeatInterval,
	}
}

// StartZencoderLogin creates the browser login URL used by the VSCode extension.
// The random state is embedded in the callback path, making the unauthenticated
// callback both one-time and unguessable. The Zencoder exchange contract uses
// code/provider/redirectUrl and intentionally does not receive the verifier.
func (s *OAuthService) StartZencoderLogin(origin string) (OAuthStartResult, error) {
	state, err := randomURLToken(32)
	if err != nil {
		return OAuthStartResult{}, err
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return OAuthStartResult{}, err
	}
	anonymousID, err := randomURLToken(18)
	if err != nil {
		return OAuthStartResult{}, err
	}
	callbackURL, err := loopbackCallbackURL(origin, state)
	if err != nil {
		return OAuthStartResult{}, err
	}
	signInURL, err := url.Parse(zencoderAuthBaseURL() + "/extension/signin")
	if err != nil {
		return OAuthStartResult{}, err
	}
	query := signInURL.Query()
	query.Set("version", "2")
	query.Set("redirect_uri", callbackURL)
	query.Set("code_challenge", pkceChallenge(verifier))
	signInURL.RawQuery = query.Encode()

	now := time.Now()
	db := database.GetDB()
	if db == nil {
		return OAuthStartResult{}, errors.New("database is not initialized")
	}
	if err := db.Where("expires_at <= ?", now).Delete(&model.OAuthSession{}).Error; err != nil {
		return OAuthStartResult{}, fmt.Errorf("delete expired OAuth sessions: %w", err)
	}
	if err := db.Create(&model.OAuthSession{
		State:       state,
		AnonymousID: anonymousID,
		Origin:      strings.TrimRight(origin, "/"),
		RedirectURL: callbackURL,
		ExpiresAt:   now.Add(oauthSessionTTL),
	}).Error; err != nil {
		return OAuthStartResult{}, fmt.Errorf("persist OAuth session: %w", err)
	}

	return OAuthStartResult{AuthorizationURL: signInURL.String(), State: state}, nil
}

type OAuthCompleteResult struct {
	Account *model.Account
	Origin  string
}

func (s *OAuthService) CompleteZencoderLogin(
	ctx context.Context,
	state string,
	code string,
	provider string,
) (OAuthCompleteResult, error) {
	if strings.TrimSpace(code) == "" {
		return OAuthCompleteResult{}, errors.New("OAuth callback has no authorization code")
	}
	if provider != "workos" {
		provider = "frontegg"
	}
	session, err := claimOAuthSession(ctx, state)
	if err != nil {
		return OAuthCompleteResult{}, err
	}
	claimCtx, heartbeat := startOAuthSessionClaimHeartbeat(
		ctx,
		session.ID,
		session.ClaimID,
		s.claimHeartbeatInterval,
	)
	release := true
	defer func() {
		_ = heartbeat.Stop()
		if !release {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthSessionCleanupTimeout)
		defer cancel()
		if err := releaseOAuthSession(cleanupCtx, session.ID, session.ClaimID); err != nil {
			logging.Errorf("Release OAuth session: %v", err)
		}
	}()

	tokens, err := s.exchangeCode(claimCtx, provider, code, session.RedirectURL, session.AnonymousID)
	if claimErr := heartbeat.Err(); claimErr != nil {
		return OAuthCompleteResult{Origin: session.Origin}, claimErr
	}
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	profile, err := s.fetchProfile(claimCtx, tokens.AccessToken)
	if claimErr := heartbeat.Err(); claimErr != nil {
		return OAuthCompleteResult{Origin: session.Origin}, claimErr
	}
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	// Stop renewal before the final transaction. The transaction only performs
	// narrow local writes: account upsert and session consume are atomic, while
	// all external HTTP work above remains outside the transaction.
	if err := heartbeat.Stop(); err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	account, err := upsertAndConsumeOAuthAccount(ctx, session.ID, session.ClaimID, provider, session.AnonymousID, tokens, profile)
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	release = false
	if err := deleteOAuthSession(ctx, session.ID, session.ClaimID); err != nil {
		logging.Errorf("Delete completed OAuth session: %v", err)
	}
	RefreshAccountPool()
	return OAuthCompleteResult{Account: account, Origin: session.Origin}, nil
}

func claimOAuthSession(ctx context.Context, state string) (*model.OAuthSession, error) {
	if strings.TrimSpace(state) == "" {
		return nil, ErrOAuthSessionInvalidOrExpired
	}
	db := database.GetDB()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}

	claimID, err := randomURLToken(18)
	if err != nil {
		return nil, fmt.Errorf("create OAuth session claim: %w", err)
	}
	var session model.OAuthSession
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		staleClaim := now.Add(-oauthSessionClaimTTL)
		if err := tx.Where("state = ? AND consumed_at IS NULL AND expires_at > ? AND (claimed_at IS NULL OR claimed_at <= ?)", state, now, staleClaim).
			First(&session).Error; err != nil {
			return err
		}
		result := tx.Model(&model.OAuthSession{}).
			Where("id = ? AND consumed_at IS NULL AND expires_at > ? AND (claimed_at IS NULL OR claimed_at <= ?)", session.ID, now, staleClaim).
			Updates(map[string]interface{}{"claim_id": claimID, "claimed_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrOAuthSessionInvalidOrExpired
		}
		session.ClaimID = claimID
		session.ClaimedAt = &now
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrOAuthSessionInvalidOrExpired) {
		return nil, ErrOAuthSessionInvalidOrExpired
	}
	if err != nil {
		return nil, fmt.Errorf("claim OAuth session: %w", err)
	}
	return &session, nil
}

type oauthSessionClaimHeartbeat struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	errMu    sync.Mutex
	err      error
}

func startOAuthSessionClaimHeartbeat(
	parent context.Context,
	id uint,
	claimID string,
	interval time.Duration,
) (context.Context, *oauthSessionClaimHeartbeat) {
	if interval <= 0 || interval >= oauthSessionClaimTTL {
		interval = oauthSessionClaimHeartbeatInterval
	}
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &oauthSessionClaimHeartbeat{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go heartbeat.run(ctx, id, claimID, interval)
	return ctx, heartbeat
}

func (h *oauthSessionClaimHeartbeat) run(ctx context.Context, id uint, claimID string, interval time.Duration) {
	defer close(h.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renewOAuthSessionClaim(ctx, id, claimID); err != nil {
				if ctx.Err() != nil {
					return
				}
				h.errMu.Lock()
				h.err = fmt.Errorf("renew OAuth session claim: %w", err)
				h.errMu.Unlock()
				h.cancel()
				return
			}
		}
	}
}

func (h *oauthSessionClaimHeartbeat) Err() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *oauthSessionClaimHeartbeat) Stop() error {
	h.stopOnce.Do(func() {
		h.cancel()
		<-h.done
	})
	return h.Err()
}

func renewOAuthSessionClaim(ctx context.Context, id uint, claimID string) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	now := time.Now()
	result := db.WithContext(ctx).Model(&model.OAuthSession{}).
		Where("id = ? AND claim_id = ? AND consumed_at IS NULL AND expires_at > ?", id, claimID, now).
		Update("claimed_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrOAuthSessionInvalidOrExpired
	}
	return nil
}

func releaseOAuthSession(ctx context.Context, id uint, claimID string) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	return db.WithContext(ctx).Model(&model.OAuthSession{}).
		Where("id = ? AND claim_id = ? AND consumed_at IS NULL", id, claimID).
		Updates(map[string]interface{}{"claim_id": "", "claimed_at": gorm.Expr("NULL")}).Error
}

func deleteOAuthSession(ctx context.Context, id uint, claimID string) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	return db.WithContext(ctx).Where("id = ? AND claim_id = ?", id, claimID).Delete(&model.OAuthSession{}).Error
}

func consumeOAuthSession(ctx context.Context, id uint, claimID string) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database is not initialized")
	}
	now := time.Now()
	result := db.WithContext(ctx).Model(&model.OAuthSession{}).
		Where("id = ? AND claim_id = ? AND consumed_at IS NULL AND expires_at > ?", id, claimID, now).
		Update("consumed_at", now)
	if result.Error != nil {
		return fmt.Errorf("consume OAuth session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrOAuthSessionInvalidOrExpired
	}
	return nil
}

func loopbackCallbackURL(origin string, state string) (string, error) {
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Host == "" {
		return "", errors.New("invalid OAuth deployment origin")
	}
	host := "localhost"
	if port := parsedOrigin.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	callback := url.URL{
		Scheme: "http",
		Host:   host,
		Path:   "/oauth/zencoder/callback/" + state,
	}
	return callback.String(), nil
}

func (s *OAuthService) exchangeCode(
	ctx context.Context,
	provider string,
	code string,
	redirectURL string,
	anonymousID string,
) (zencoderOAuthTokens, error) {
	body := map[string]string{
		"providerType": provider,
		"code":         code,
		"redirectUrl":  redirectURL,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zencoderAuthBaseURL()+"/api/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	setOAuthServiceHeaders(req, anonymousID)
	resp, err := s.client.Do(req)
	if err != nil {
		return zencoderOAuthTokens{}, fmt.Errorf("exchange Zencoder OAuth code: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return zencoderOAuthTokens{}, fmt.Errorf("exchange Zencoder OAuth code: HTTP %d", resp.StatusCode)
	}
	tokens, err := parseOAuthTokens(responseBody)
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	if tokens.RefreshToken == "" {
		return zencoderOAuthTokens{}, errors.New("zencoder OAuth response has no refresh token")
	}
	return tokens, nil
}

type zencoderOAuthProfile struct {
	UserID   string
	TenantID string
	Email    string
	Name     string
}

func (s *OAuthService) fetchProfile(ctx context.Context, accessToken string) (zencoderOAuthProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zencoderAuthBaseURL()+"/api/auth/me", nil)
	if err != nil {
		return zencoderOAuthProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return zencoderOAuthProfile{}, fmt.Errorf("fetch Zencoder OAuth profile: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zencoderOAuthProfile{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return zencoderOAuthProfile{}, fmt.Errorf("fetch zencoder OAuth profile: HTTP %d", resp.StatusCode)
	}
	var raw struct {
		UserID   string `json:"user_id"`
		TenantID string `json:"org_id"`
		Email    string `json:"email"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return zencoderOAuthProfile{}, fmt.Errorf("decode zencoder OAuth profile: %w", err)
	}
	if raw.UserID == "" || raw.TenantID == "" {
		return zencoderOAuthProfile{}, errors.New("zencoder OAuth profile is missing account identity")
	}
	return zencoderOAuthProfile{UserID: raw.UserID, TenantID: raw.TenantID, Email: raw.Email, Name: raw.Name}, nil
}

func upsertOAuthAccount(
	ctx context.Context,
	provider string,
	anonymousID string,
	tokens zencoderOAuthTokens,
	profile zencoderOAuthProfile,
) (*model.Account, error) {
	db := database.GetDB()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	var account model.Account
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		account, err = upsertOAuthAccountTx(tx, provider, anonymousID, tokens, profile)
		return err
	}); err != nil {
		return nil, err
	}
	if err := decryptOAuthAccountTokens(&account); err != nil {
		return nil, err
	}
	return &account, nil
}

// upsertAndConsumeOAuthAccount commits the credential mutation and one-time
// session transition together. No network operation belongs in this
// transaction; callers must complete exchange/profile HTTP first.
func upsertAndConsumeOAuthAccount(
	ctx context.Context,
	sessionID uint,
	claimID string,
	provider string,
	anonymousID string,
	tokens zencoderOAuthTokens,
	profile zencoderOAuthProfile,
) (*model.Account, error) {
	db := database.GetDB()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	var account model.Account
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		account, err = upsertOAuthAccountTx(tx, provider, anonymousID, tokens, profile)
		if err != nil {
			return err
		}
		now := time.Now()
		result := tx.Model(&model.OAuthSession{}).
			Where("id = ? AND claim_id = ? AND consumed_at IS NULL AND expires_at > ?", sessionID, claimID, now).
			Update("consumed_at", now)
		if result.Error != nil {
			return fmt.Errorf("consume OAuth session: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrOAuthSessionInvalidOrExpired
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := decryptOAuthAccountTokens(&account); err != nil {
		return nil, err
	}
	return &account, nil
}

func upsertOAuthAccountTx(
	db *gorm.DB,
	provider string,
	anonymousID string,
	tokens zencoderOAuthTokens,
	profile zencoderOAuthProfile,
) (model.Account, error) {
	digest := sha256.Sum256([]byte(provider + ":" + profile.TenantID + ":" + profile.UserID))
	clientID := "oauth-" + hex.EncodeToString(digest[:])
	encryptedAccessToken, err := secret.Encrypt(tokens.AccessToken)
	if err != nil {
		return model.Account{}, fmt.Errorf("encrypt OAuth access token: %w", err)
	}
	encryptedRefreshToken, err := secret.Encrypt(tokens.RefreshToken)
	if err != nil {
		return model.Account{}, fmt.Errorf("encrypt OAuth refresh token: %w", err)
	}
	account := model.Account{
		ClientID:           clientID,
		CredentialType:     model.CredentialOAuth,
		OAuthProvider:      provider,
		OAuthEmail:         profile.Email,
		OAuthUserID:        profile.UserID,
		OAuthTenantID:      profile.TenantID,
		OAuthAnonymousID:   anonymousID,
		AccessToken:        encryptedAccessToken,
		RefreshToken:       encryptedRefreshToken,
		CredentialRevision: 1,
		TokenExpiresAt:     tokens.ExpiresAt,
	}
	updates := map[string]interface{}{
		"credential_type":     model.CredentialOAuth,
		"o_auth_provider":     provider,
		"o_auth_email":        profile.Email,
		"o_auth_user_id":      profile.UserID,
		"o_auth_tenant_id":    profile.TenantID,
		"o_auth_anonymous_id": anonymousID,
		"access_token":        encryptedAccessToken,
		"api_key":             "",
		"token_expires_at":    tokens.ExpiresAt,
		"health_state":        model.AccountHealthHealthy,
		"cooldown_until":      gorm.Expr("NULL"),
		"last_error_class":    "",
		"last_error_at":       gorm.Expr("NULL"),
		"failure_count":       0,
		"reauth_required":     false,
		"credential_revision": gorm.Expr("credential_revision + 1"),
		"updated_at":          time.Now(),
	}
	if tokens.RefreshToken != "" {
		updates["refresh_token"] = encryptedRefreshToken
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&account).Error; err != nil {
		return model.Account{}, fmt.Errorf("upsert OAuth account: %w", err)
	}
	if err := db.Where("client_id = ?", clientID).First(&account).Error; err != nil {
		return model.Account{}, fmt.Errorf("load OAuth account after upsert: %w", err)
	}
	return account, nil
}

func decryptOAuthAccountTokens(account *model.Account) error {
	if account == nil {
		return errors.New("OAuth account is nil")
	}
	var err error
	account.AccessToken, err = secret.Decrypt(account.AccessToken)
	if err != nil {
		return fmt.Errorf("decrypt OAuth access token after upsert: %w", err)
	}
	account.RefreshToken, err = secret.Decrypt(account.RefreshToken)
	if err != nil {
		return fmt.Errorf("decrypt OAuth refresh token after upsert: %w", err)
	}
	return nil
}

func randomURLToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
