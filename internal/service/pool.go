package service

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"

	"gorm.io/gorm"
)

type AccountPool struct {
	mu       sync.Mutex
	accounts []model.Account
	index    uint64
}

var pool *AccountPool

func init() {
	pool = &AccountPool{accounts: make([]model.Account, 0)}
}

// InitAccountPool initializes the local OAuth account cache.
func InitAccountPool() {
	pool.refresh()
	go pool.refreshLoop()
}

// RefreshAccountPool makes account mutations visible immediately instead of
// waiting for the background refresh interval.
func RefreshAccountPool() {
	if pool != nil {
		pool.refresh()
	}
}

func (p *AccountPool) refreshLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.refresh()
	}
}

func (p *AccountPool) refresh() {
	var accounts []model.Account
	result := database.GetDB().
		Where("credential_type = ? AND access_token != ''", model.CredentialOAuth).
		Find(&accounts)
	if result.Error != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = accounts
	if len(p.accounts) == 0 {
		p.index = 0
	} else {
		p.index %= uint64(len(p.accounts))
	}
}

// GetNextAccount selects OAuth accounts in round-robin order. Accounts are
// deliberately not leased or marked busy: concurrent and long-running streams
// may share an OAuth account. Returning a value copy prevents those requests
// from racing on the pool's cached object.
func GetNextAccount() (*model.Account, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if len(pool.accounts) == 0 {
		return nil, ErrNoAvailableAccount
	}

	position := int(pool.index % uint64(len(pool.accounts)))
	account := pool.accounts[position]
	pool.index = uint64(position+1) % uint64(len(pool.accounts))
	return &account, nil
}

// UseCredit atomically records local usage for concurrent requests.
func UseCredit(account *model.Account, multiplier float64) {
	if account == nil {
		return
	}
	database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"daily_used": gorm.Expr("daily_used + ?", multiplier),
		"total_used": gorm.Expr("total_used + ?", multiplier),
		"last_used":  time.Now(),
	})
}

// UpdateAccountCreditsFromResponse atomically updates only usage-related
// fields, so one completed stream cannot overwrite another request's OAuth
// token or counters with a stale account copy.
func UpdateAccountCreditsFromResponse(account *model.Account, resp *http.Response, modelMultiplier float64) {
	if account == nil {
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

	hasGatewayUsage := requestCost != "" && parseFloat(requestCost) > 0
	if periodCost != "" {
		if value := parseFloat(periodCost); value >= 0 {
			updates["quota_used"] = value
			hasGatewayUsage = true
		}
	}
	if periodLimit != "" {
		if value := parseFloat(periodLimit); value > 0 {
			updates["quota_limit"] = value
			updates["quota_limit_available"] = true
		}
	}
	if hasGatewayUsage {
		updates["usage_data_available"] = true
	}
	if periodEnd != "" {
		if value, err := time.Parse(time.RFC3339, periodEnd); err == nil {
			updates["credit_refresh_time"] = value
		}
	}

	database.GetDB().Model(&model.Account{}).Where("id = ?", account.ID).Updates(updates)

	if IsDebugMode() && (requestCost != "" || periodCost != "") {
		log.Printf("[DEBUG] 使用API积分: 账号=%s, RequestCost=%s, PeriodCost=%s, PeriodLimit=%s, PeriodEnd=%s",
			account.OAuthEmail, requestCost, periodCost, periodLimit, periodEnd)
	}
}

func parseFloat(value string) float64 {
	if value == "" {
		return 0
	}
	var parsed float64
	_, _ = fmt.Sscanf(value, "%f", &parsed)
	return parsed
}
