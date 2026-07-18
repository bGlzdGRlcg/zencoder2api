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
	"os"
	"strings"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
)

const oauthRefreshSkew = time.Minute

var oauthRefreshLocks sync.Map

func zencoderAuthBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("ZENCODER_AUTH_BASE_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return "https://auth.zencoder.ai"
}

// ApplyZencoderAuth applies the OAuth Bearer token used by the VSCode
// extension. Expired access tokens are refreshed and persisted first.
func ApplyZencoderAuth(ctx context.Context, req *http.Request, account *model.Account) error {
	accessToken, err := GetAccessToken(ctx, account)
	if err != nil {
		return err
	}
	req.Header.Del("zencoder-api-key")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return nil
}

// GetAccessToken returns a valid OAuth access token, refreshing it when it is
// expired or will expire within one minute.
func GetAccessToken(ctx context.Context, account *model.Account) (string, error) {
	if account == nil || strings.TrimSpace(account.AccessToken) == "" {
		return "", errors.New("account has no Zencoder OAuth access token")
	}
	if account.CredentialType != "" && account.CredentialType != model.CredentialOAuth {
		return "", errors.New("account is not a Zencoder OAuth account")
	}
	if !tokenNeedsRefresh(account.AccessToken, account.TokenExpiresAt) {
		return strings.TrimSpace(account.AccessToken), nil
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return "", errors.New("zencoder OAuth refresh token is missing")
	}

	lockKey := account.ClientID
	if lockKey == "" {
		lockKey = fmt.Sprintf("account-%d", account.ID)
	}
	lockValue, _ := oauthRefreshLocks.LoadOrStore(lockKey, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	// A concurrent request may already have refreshed the database row.
	if account.ID != 0 && database.GetDB() != nil {
		var latest model.Account
		if err := database.GetDB().First(&latest, account.ID).Error; err == nil {
			account.AccessToken = latest.AccessToken
			account.RefreshToken = latest.RefreshToken
			account.TokenExpiresAt = latest.TokenExpiresAt
			account.OAuthProvider = latest.OAuthProvider
			account.OAuthAnonymousID = latest.OAuthAnonymousID
		}
	}
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
	if account.ID != 0 && database.GetDB() != nil {
		if err := database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
			"access_token":     account.AccessToken,
			"refresh_token":    account.RefreshToken,
			"token_expires_at": account.TokenExpiresAt,
		}).Error; err != nil {
			return "", fmt.Errorf("persist refreshed Zencoder OAuth token: %w", err)
		}
	}
	return account.AccessToken, nil
}

func tokenNeedsRefresh(accessToken string, expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		expiresAt = jwtExpiresAt(accessToken)
	}
	return !expiresAt.IsZero() && time.Now().Add(oauthRefreshSkew).After(expiresAt)
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
	endpoint := "/refresh_token"
	body := map[string]interface{}{"refreshToken": account.RefreshToken}
	if provider == "workos" {
		endpoint = "/api/auth/refresh"
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
	setOAuthServiceHeaders(req, account.OAuthAnonymousID)
	resp, err := newDirectHTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return zencoderOAuthTokens{}, fmt.Errorf("refresh Zencoder OAuth token: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zencoderOAuthTokens{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return zencoderOAuthTokens{}, fmt.Errorf("refresh Zencoder OAuth token: HTTP %d", resp.StatusCode)
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
		AccessToken       string      `json:"accessToken"`
		AccessTokenSnake  string      `json:"access_token"`
		RefreshToken      string      `json:"refreshToken"`
		RefreshTokenSnake string      `json:"refresh_token"`
		ExpiresIn         interface{} `json:"expiresIn"`
		ExpiresInSnake    interface{} `json:"expires_in"`
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
		expiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if seconds := numberSeconds(raw.ExpiresInSnake); seconds > 0 {
		expiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	} else {
		expiresAt = jwtExpiresAt(accessToken)
	}
	return zencoderOAuthTokens{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt}, nil
}

func setOAuthServiceHeaders(req *http.Request, anonymousID string) {
	if anonymousID == "" {
		anonymousID = "zencoder2api"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-zencoder-anonymous-id", anonymousID)
	req.Header.Set("x-zencoder-plugin-version", "vsc-3.68.0")
}

func detectOAuthProviderFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "frontegg"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
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

func jwtExpiresAt(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.ExpiresAt, 0)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func numberSeconds(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case string:
		var seconds int64
		_, _ = fmt.Sscanf(typed, "%d", &seconds)
		return seconds
	default:
		return 0
	}
}
