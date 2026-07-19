package service

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"

	"gorm.io/gorm"
)

var errDatabaseUnavailable = errors.New("database is not initialized")

const (
	accountErrorReauth    = "reauth_required"
	accountErrorForbidden = "forbidden"
	accountErrorRateLimit = "rate_limit"
	accountErrorUpstream  = "upstream"
	accountErrorNetwork   = "network"
)

type AccountPool struct {
	mu       sync.Mutex
	accounts []model.Account
	index    uint64
}

var pool *AccountPool
var poolStopMu sync.Mutex
var poolStop context.CancelFunc
var poolDone chan struct{}

func init() {
	pool = &AccountPool{accounts: make([]model.Account, 0)}
}

// InitAccountPool initializes the local OAuth account cache.
func InitAccountPool() {
	StopAccountPool()
	if err := pool.refresh(); err != nil {
		logging.Errorf("Initial account pool refresh failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	poolStopMu.Lock()
	poolStop = cancel
	poolDone = done
	poolStopMu.Unlock()
	go func() {
		defer close(done)
		pool.refreshLoop(ctx)
	}()
}

// StopAccountPool terminates the background cache refresh loop.
func StopAccountPool() {
	poolStopMu.Lock()
	cancel := poolStop
	done := poolDone
	poolStop = nil
	poolDone = nil
	poolStopMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// RefreshAccountPool makes account mutations visible immediately instead of
// waiting for the background refresh interval.
func RefreshAccountPool() {
	if pool != nil {
		if err := pool.refresh(); err != nil {
			logging.Errorf("Account pool refresh failed: %v", err)
		}
	}
}

func (p *AccountPool) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.refresh(); err != nil {
				logging.Errorf("Account pool refresh failed: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *AccountPool) refresh() error {
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	var accounts []model.Account
	result := db.
		Where("(credential_type = ? AND access_token != '') OR (credential_type = ? AND api_key != '')", model.CredentialOAuth, model.CredentialAPIKey).
		Find(&accounts)
	if result.Error != nil {
		return result.Error
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = accounts
	if len(p.accounts) == 0 {
		p.index = 0
	} else {
		p.index %= uint64(len(p.accounts))
	}
	return nil
}

// GetNextAccount selects OAuth accounts in round-robin order. Accounts are
// deliberately not leased or marked busy: concurrent and long-running streams
// may share an OAuth account. Returning a value copy prevents those requests
// from racing on the pool's cached object.
func GetNextAccount() (*model.Account, error) {
	return GetNextAccountContext(context.Background(), nil)
}

// GetNextAccountExcluding returns a healthy account not present in tried.
// Callers use it for bounded retries so a cooldown cannot cause a duplicate
// attempt against the same credential in one request.
func GetNextAccountExcluding(tried map[uint]struct{}) (*model.Account, error) {
	return GetNextAccountContext(context.Background(), tried)
}

// GetNextAccountContext propagates request cancellation through the database
// health recheck instead of allowing a locked SQLite connection to outlive the
// client request.
func GetNextAccountContext(ctx context.Context, tried map[uint]struct{}) (*model.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, ErrNoAvailableAccount
	}
	excluded := make(map[uint]struct{}, len(tried))
	for {
		pool.mu.Lock()
		accounts := append([]model.Account(nil), pool.accounts...)
		if len(accounts) == 0 {
			pool.mu.Unlock()
			return nil, ErrNoAvailableAccount
		}
		now := time.Now()
		start := pool.index
		var account *model.Account
		for offset := 0; offset < len(accounts); offset++ {
			position := int((start + uint64(offset)) % uint64(len(accounts)))
			candidate := accounts[position]
			if _, alreadyTried := tried[candidate.ID]; alreadyTried {
				continue
			}
			if _, alreadyExcluded := excluded[candidate.ID]; alreadyExcluded || !isAccountHealthy(candidate, now) {
				continue
			}
			// Reserve the position that was actually selected, not merely the
			// position after the previous start. This preserves fairness when
			// cached cooldowns skip one or more accounts.
			pool.index = uint64(position+1) % uint64(len(accounts))
			account = &candidate
			break
		}
		pool.mu.Unlock()
		if account == nil {
			return nil, ErrNoAvailableAccount
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if account.ID != 0 && !accountHealthCurrentContext(ctx, account) {
			excluded[account.ID] = struct{}{}
			continue
		}
		return account, nil
	}
}

func accountAttemptLimit() int {
	if pool == nil {
		return 1
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.accounts) == 0 {
		return 1
	}
	count := 0
	for _, account := range pool.accounts {
		if isAccountHealthy(account, time.Now()) {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	if count < maxAccountAttempts {
		return count
	}
	return maxAccountAttempts
}

// AccountPoolReady reports whether at least one credential can serve traffic.
// It intentionally excludes cooldown and re-authentication-required accounts.
func AccountPoolReady() bool {
	return AccountPoolReadyContext(context.Background())
}

// AccountPoolReadyContext is the cancellation-aware readiness variant used by
// HTTP health handlers so a locked SQLite database cannot hang readiness.
func AccountPoolReadyContext(ctx context.Context) bool {
	if pool == nil {
		return false
	}
	pool.mu.Lock()
	accounts := append([]model.Account(nil), pool.accounts...)
	pool.mu.Unlock()
	now := time.Now()
	for _, account := range accounts {
		if isAccountHealthy(account, now) && (account.ID == 0 || accountHealthCurrentContext(ctx, &account)) {
			return true
		}
	}
	return false
}

func isAccountHealthy(account model.Account, now time.Time) bool {
	if account.ReauthRequired || strings.EqualFold(account.HealthState, accountErrorReauth) {
		return false
	}
	return account.CooldownUntil == nil || !account.CooldownUntil.After(now)
}

func accountHealthCurrentContext(ctx context.Context, account *model.Account) bool {
	if account == nil {
		return false
	}
	db := database.GetDB()
	if db == nil {
		return false
	}
	var latest model.Account
	if err := db.WithContext(ctx).
		Select("health_revision", "health_state", "cooldown_until", "last_error_class", "last_error_at", "failure_count", "reauth_required").
		First(&latest, account.ID).Error; err != nil {
		return false
	}
	account.HealthRevision = latest.HealthRevision
	account.HealthState = latest.HealthState
	account.CooldownUntil = latest.CooldownUntil
	account.LastErrorClass = latest.LastErrorClass
	account.LastErrorAt = latest.LastErrorAt
	account.FailureCount = latest.FailureCount
	account.ReauthRequired = latest.ReauthRequired
	return isAccountHealthy(*account, time.Now())
}

// MarkAccountHealthy clears a transient failure after a successful upstream
// response. It updates only health columns and cannot overwrite usage/token
// fields from a stale pool copy.
func MarkAccountHealthy(account *model.Account) {
	if account == nil {
		return
	}
	if account.ID == 0 {
		if account.ReauthRequired {
			return
		}
		account.HealthRevision++
		account.HealthState = model.AccountHealthHealthy
		account.CooldownUntil = nil
		account.LastErrorClass = ""
		account.LastErrorAt = nil
		account.FailureCount = 0
		updatePoolHealth(*account)
		return
	}
	if db := database.GetDB(); db != nil {
		result := db.Model(&model.Account{}).
			Where("id = ? AND credential_revision = ? AND health_revision = ? AND reauth_required = ?", account.ID, account.CredentialRevision, account.HealthRevision, false).
			Updates(map[string]interface{}{
				"health_state":     model.AccountHealthHealthy,
				"cooldown_until":   gorm.Expr("NULL"),
				"last_error_class": "",
				"last_error_at":    gorm.Expr("NULL"),
				"failure_count":    0,
				"health_revision":  gorm.Expr("health_revision + 1"),
			})
		if result.Error != nil {
			logging.Errorf("update account health failed: %v", result.Error)
			return
		}
		if result.RowsAffected != 1 {
			return
		}
	} else {
		return
	}
	account.HealthRevision++
	account.HealthState = model.AccountHealthHealthy
	account.CooldownUntil = nil
	account.LastErrorClass = ""
	account.LastErrorAt = nil
	account.FailureCount = 0
	updatePoolHealth(*account)
}

// MarkAccountFailure persists a bounded cooldown. Retry-After wins over the
// exponential fallback and is capped so a malicious upstream cannot create an
// effectively permanent local outage.
func MarkAccountFailure(account *model.Account, status int, retryAfter time.Duration, cause error) {
	if account == nil {
		return
	}
	class, reauth := classifyAccountFailure(status, cause)
	now := time.Now()
	delay := retryAfter
	if delay <= 0 {
		n := account.FailureCount
		if n < 0 {
			n = 0
		}
		if n > 7 {
			n = 7
		}
		delay = time.Duration(1<<n) * 5 * time.Second
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	cooldown := now.Add(delay)
	if account.ID == 0 {
		if account.ReauthRequired && !reauth {
			return
		}
		account.HealthRevision++
		account.HealthState = class
		account.CooldownUntil = &cooldown
		account.LastErrorClass = class
		account.LastErrorAt = &now
		account.FailureCount++
		account.ReauthRequired = reauth
		updatePoolHealth(*account)
		return
	}
	if db := database.GetDB(); db != nil {
		updates := map[string]interface{}{
			"health_state":     class,
			"cooldown_until":   cooldown,
			"last_error_class": class,
			"last_error_at":    now,
			"failure_count":    gorm.Expr("failure_count + 1"),
			"health_revision":  gorm.Expr("health_revision + 1"),
		}
		query := db.Model(&model.Account{}).
			Where("id = ? AND credential_revision = ? AND health_revision = ?", account.ID, account.CredentialRevision, account.HealthRevision)
		if reauth {
			updates["reauth_required"] = true
		} else {
			query = query.Where("reauth_required = ?", false)
		}
		result := query.Updates(updates)
		if result.Error != nil {
			logging.Errorf("persist account health failed: %v", result.Error)
			return
		}
		if result.RowsAffected != 1 {
			return
		}
		account.HealthRevision++
		account.HealthState = class
		account.CooldownUntil = &cooldown
		account.LastErrorClass = class
		account.LastErrorAt = &now
		account.FailureCount++
		account.ReauthRequired = reauth
		var latest model.Account
		if err := db.Select("health_revision", "health_state", "cooldown_until", "last_error_class", "last_error_at", "failure_count", "reauth_required").First(&latest, account.ID).Error; err == nil {
			account.HealthRevision = latest.HealthRevision
			account.HealthState = latest.HealthState
			account.CooldownUntil = latest.CooldownUntil
			account.LastErrorClass = latest.LastErrorClass
			account.LastErrorAt = latest.LastErrorAt
			account.FailureCount = latest.FailureCount
			account.ReauthRequired = latest.ReauthRequired
		}
	} else {
		return
	}
	updatePoolHealth(*account)
}

func markAccountReauthRequired(account *model.Account, class string) {
	if account == nil {
		return
	}
	now := time.Now()
	cooldown := now.Add(30 * time.Minute)
	if account.ID != 0 {
		db := database.GetDB()
		if db == nil {
			return
		}
		result := db.Model(&model.Account{}).
			Where("id = ? AND credential_revision = ? AND health_revision = ?", account.ID, account.CredentialRevision, account.HealthRevision).
			Updates(map[string]interface{}{
				"health_state": accountErrorReauth, "last_error_class": class,
				"last_error_at": now, "cooldown_until": cooldown, "reauth_required": true,
				"health_revision": gorm.Expr("health_revision + 1"),
			})
		if result.Error != nil || result.RowsAffected != 1 {
			return
		}
	}
	account.HealthRevision++
	account.HealthState = accountErrorReauth
	account.LastErrorClass = class
	account.LastErrorAt = &now
	account.CooldownUntil = &cooldown
	account.ReauthRequired = true
	updatePoolHealth(*account)
}

func classifyAccountFailure(status int, cause error) (string, bool) {
	switch status {
	case http.StatusUnauthorized:
		return accountErrorReauth, true
	case http.StatusForbidden:
		return accountErrorForbidden, false
	case http.StatusTooManyRequests, 529:
		return accountErrorRateLimit, false
	case http.StatusRequestTimeout, http.StatusTooEarly:
		return accountErrorNetwork, false
	}
	if status >= 500 || status == 0 {
		if cause != nil {
			return accountErrorNetwork, false
		}
		return accountErrorUpstream, false
	}
	return accountErrorUpstream, false
}

func updatePoolHealth(account model.Account) {
	if pool == nil || account.ID == 0 {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for i := range pool.accounts {
		if pool.accounts[i].ID == account.ID {
			if pool.accounts[i].CredentialRevision != account.CredentialRevision || pool.accounts[i].HealthRevision > account.HealthRevision {
				continue
			}
			pool.accounts[i].HealthRevision = account.HealthRevision
			pool.accounts[i].HealthState = account.HealthState
			pool.accounts[i].CooldownUntil = account.CooldownUntil
			pool.accounts[i].LastErrorClass = account.LastErrorClass
			pool.accounts[i].LastErrorAt = account.LastErrorAt
			pool.accounts[i].FailureCount = account.FailureCount
			pool.accounts[i].ReauthRequired = account.ReauthRequired
		}
	}
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// UseCredit atomically records local usage for concurrent requests.
func UseCredit(account *model.Account, multiplier float64) {
	if account == nil || account.ID == 0 || !validMultiplier(multiplier) {
		return
	}
	db := database.GetDB()
	if db == nil {
		logging.Errorf("Record account usage failed: %v", errDatabaseUnavailable)
		return
	}
	result := db.Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"daily_used": gorm.Expr("daily_used + ?", multiplier),
		"total_used": gorm.Expr("total_used + ?", multiplier),
		"last_used":  time.Now(),
	})
	if result.Error != nil {
		logging.Errorf("Record account usage failed for account %d: %v", account.ID, result.Error)
	}
}

// UpdateAccountCreditsFromResponse atomically updates only usage-related
// fields, so one completed stream cannot overwrite another request's OAuth
// token or counters with a stale account copy.
func UpdateAccountCreditsFromResponse(account *model.Account, resp *http.Response, modelMultiplier float64) {
	if account == nil || account.ID == 0 || !validMultiplier(modelMultiplier) {
		return
	}
	if resp == nil || resp.Header == nil {
		UseCredit(account, modelMultiplier)
		return
	}

	periodLimit := resp.Header.Get("Zen-Pricing-Period-Limit")
	periodCost := resp.Header.Get("Zen-Pricing-Period-Cost")
	requestCost := resp.Header.Get("Zen-Request-Cost")
	periodEnd := resp.Header.Get("Zen-Pricing-Period-End")

	updates := map[string]interface{}{
		"daily_used": gorm.Expr("daily_used + ?", modelMultiplier),
		"total_used": gorm.Expr("total_used + ?", modelMultiplier),
		"last_used":  time.Now(),
	}
	absoluteUpdates := make(map[string]interface{})

	requestCostValue, requestCostOK := parseFloat(requestCost)
	hasGatewayUsage := requestCostOK && requestCostValue > 0
	periodCostValue, periodCostOK := parseFloat(periodCost)
	periodCostOK = periodCostOK && periodCostValue >= 0
	if periodCost != "" {
		if periodCostOK {
			absoluteUpdates["quota_used"] = periodCostValue
			hasGatewayUsage = true
		}
	}
	periodLimitValue, periodLimitOK := parseFloat(periodLimit)
	periodLimitOK = periodLimitOK && periodLimitValue > 0
	if periodLimit != "" {
		if periodLimitOK {
			absoluteUpdates["quota_limit"] = periodLimitValue
			absoluteUpdates["quota_limit_available"] = true
		}
	}
	if hasGatewayUsage {
		updates["usage_data_available"] = true
	}
	periodEndValue, periodEndOK := time.Time{}, false
	if periodEnd != "" {
		if value, err := time.Parse(time.RFC3339, periodEnd); err == nil {
			periodEndValue, periodEndOK = value, true
			absoluteUpdates["credit_refresh_time"] = value
		}
	}

	db := database.GetDB()
	if db == nil {
		logging.Errorf("Record gateway usage failed: %v", errDatabaseUnavailable)
		return
	}
	// Local counters belong to the account and must include completed requests
	// even when a credential was rotated while a stream was in flight.
	result := db.Model(&model.Account{}).Where("id = ?", account.ID).Updates(updates)
	if result.Error != nil {
		logging.Errorf("Record gateway usage failed for account %d: %v", account.ID, result.Error)
	}

	// Gateway quota values are absolute snapshots. They are written only for
	// the credential revision that started this request and only when the
	// pricing period is not older than the stored snapshot. Within one period,
	// quota_used is monotonic so a late response cannot decrease it.
	if len(absoluteUpdates) > 0 {
		where := "id = ? AND credential_revision = ?"
		args := []interface{}{account.ID, account.CredentialRevision}
		if periodEndOK {
			where += " AND (COALESCE(CAST(strftime('%s', credit_refresh_time) AS INTEGER), -9223372036854775808) < ? OR (COALESCE(CAST(strftime('%s', credit_refresh_time) AS INTEGER), -9223372036854775808) = ? AND (quota_used <= ? OR ? = 0) AND (quota_limit <= ? OR ? = 0)))"
			args = append(args,
				periodEndValue.Unix(), periodEndValue.Unix(),
				periodCostValue, boolToInt(periodCostOK),
				periodLimitValue, boolToInt(periodLimitOK),
			)
		} else {
			if periodCostOK {
				where += " AND quota_used <= ?"
				args = append(args, periodCostValue)
			}
			if periodLimitOK {
				where += " AND quota_limit <= ?"
				args = append(args, periodLimitValue)
			}
		}
		quotaResult := db.Model(&model.Account{}).Where(where, args...).Updates(absoluteUpdates)
		if quotaResult.Error != nil {
			logging.Errorf("Record gateway quota failed for account %d: %v", account.ID, quotaResult.Error)
		}
	}

	if IsDebugMode() && (requestCost != "" || periodCost != "") {
		logging.Debugf("gateway usage received: account_id=%d request_cost_present=%t period_cost_present=%t period_limit_present=%t period_end_present=%t",
			account.ID, requestCost != "", periodCost != "", periodLimit != "", periodEnd != "")
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func parseFloat(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func validMultiplier(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
