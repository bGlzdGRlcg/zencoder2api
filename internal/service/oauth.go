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
	"zencoder-2api/internal/model"

	"gorm.io/gorm"
)

const oauthSessionTTL = 10 * time.Minute

var ErrOAuthSessionInvalidOrExpired = errors.New("OAuth session is invalid or expired")

type oauthSession struct {
	CodeVerifier string
	AnonymousID  string
	Origin       string
	ExpiresAt    time.Time
}

type OAuthStartResult struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
}

type OAuthService struct {
	mu       sync.Mutex
	sessions map[string]oauthSession
	client   *http.Client
}

func NewOAuthService() *OAuthService {
	return &OAuthService{
		sessions: make(map[string]oauthSession),
		client:   newDirectHTTPClient(15 * time.Second),
	}
}

// StartZencoderLogin creates the PKCE login URL used by the VSCode extension.
// The random state is embedded in the callback path, making the unauthenticated
// callback both one-time and unguessable.
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

	s.mu.Lock()
	now := time.Now()
	for key, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
	s.sessions[state] = oauthSession{
		CodeVerifier: verifier,
		AnonymousID:  anonymousID,
		Origin:       strings.TrimRight(origin, "/"),
		ExpiresAt:    now.Add(oauthSessionTTL),
	}
	s.mu.Unlock()

	signInURL, err := url.Parse(zencoderAuthBaseURL() + "/extension/signin")
	if err != nil {
		return OAuthStartResult{}, err
	}
	query := signInURL.Query()
	query.Set("version", "2")
	query.Set("redirect_uri", callbackURL)
	query.Set("code_challenge", pkceChallenge(verifier))
	signInURL.RawQuery = query.Encode()
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
	s.mu.Lock()
	session, exists := s.sessions[state]
	if exists {
		delete(s.sessions, state)
	}
	s.mu.Unlock()
	if !exists || time.Now().After(session.ExpiresAt) {
		return OAuthCompleteResult{}, ErrOAuthSessionInvalidOrExpired
	}
	if strings.TrimSpace(code) == "" {
		return OAuthCompleteResult{Origin: session.Origin}, errors.New("OAuth callback has no authorization code")
	}
	if provider != "workos" {
		provider = "frontegg"
	}

	tokens, err := s.exchangeCode(ctx, provider, code, session.CodeVerifier, session.AnonymousID)
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	profile, err := s.fetchProfile(ctx, tokens.AccessToken)
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	account, err := upsertOAuthAccount(provider, session.AnonymousID, tokens, profile)
	if err != nil {
		return OAuthCompleteResult{Origin: session.Origin}, err
	}
	RefreshAccountPool()
	return OAuthCompleteResult{Account: account, Origin: session.Origin}, nil
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
	codeVerifier string,
	anonymousID string,
) (zencoderOAuthTokens, error) {
	endpoint := "/api/oauth/token"
	body := map[string]interface{}{"code": code, "codeVerifier": codeVerifier}
	if provider == "workos" {
		endpoint = "/api/auth/token"
		body["provider"] = "workos"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zencoderAuthBaseURL()+endpoint, bytes.NewReader(payload))
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
	if resp.StatusCode != http.StatusOK {
		return zencoderOAuthTokens{}, fmt.Errorf("exchange Zencoder OAuth code: HTTP %d", resp.StatusCode)
	}
	return parseOAuthTokens(responseBody)
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
	provider string,
	anonymousID string,
	tokens zencoderOAuthTokens,
	profile zencoderOAuthProfile,
) (*model.Account, error) {
	digest := sha256.Sum256([]byte(provider + ":" + profile.TenantID + ":" + profile.UserID))
	clientID := "oauth-" + hex.EncodeToString(digest[:8])
	db := database.GetDB()
	var account model.Account
	result := db.Where("client_id = ?", clientID).First(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		account = model.Account{
			ClientID:         clientID,
			CredentialType:   model.CredentialOAuth,
			OAuthProvider:    provider,
			OAuthEmail:       profile.Email,
			OAuthUserID:      profile.UserID,
			OAuthTenantID:    profile.TenantID,
			OAuthAnonymousID: anonymousID,
			AccessToken:      tokens.AccessToken,
			RefreshToken:     tokens.RefreshToken,
			TokenExpiresAt:   tokens.ExpiresAt,
		}
		if err := db.Create(&account).Error; err != nil {
			return nil, fmt.Errorf("create OAuth account: %w", err)
		}
		return &account, nil
	}
	if result.Error != nil {
		return nil, fmt.Errorf("find OAuth account: %w", result.Error)
	}

	account.CredentialType = model.CredentialOAuth
	account.OAuthProvider = provider
	account.OAuthEmail = profile.Email
	account.OAuthUserID = profile.UserID
	account.OAuthTenantID = profile.TenantID
	account.OAuthAnonymousID = anonymousID
	account.AccessToken = tokens.AccessToken
	account.RefreshToken = tokens.RefreshToken
	account.TokenExpiresAt = tokens.ExpiresAt
	if err := db.Save(&account).Error; err != nil {
		return nil, fmt.Errorf("update OAuth account: %w", err)
	}
	return &account, nil
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
