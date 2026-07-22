package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	usageCreditsRefreshJobName         = "usage-based-credits-refresh"
	defaultUsageCreditsRefreshInterval = 15 * time.Minute
	minimumUsageCreditsRefreshInterval = time.Minute
	maximumUsageCreditsRefreshInterval = 24 * time.Hour
)

// ValidateUsageCreditsRefreshConfig makes an invalid refresh cadence a startup
// error instead of silently disabling balance-based scheduling.
func ValidateUsageCreditsRefreshConfig() error {
	_, err := usageCreditsRefreshInterval()
	return err
}

func usageCreditsRefreshInterval() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("USAGE_CREDITS_REFRESH_INTERVAL"))
	if raw == "" {
		return defaultUsageCreditsRefreshInterval, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval < minimumUsageCreditsRefreshInterval || interval > maximumUsageCreditsRefreshInterval {
		return 0, fmt.Errorf("usage credits refresh interval (USAGE_CREDITS_REFRESH_INTERVAL) must be between %s and %s", minimumUsageCreditsRefreshInterval, maximumUsageCreditsRefreshInterval)
	}
	return interval, nil
}

// StartUsageCreditsRefreshScheduler refreshes stale account snapshots at a
// configurable interval. A database lease makes the scan single-owner across
// multiple service instances; per-account leases protect the actual queries.
func StartUsageCreditsRefreshScheduler() context.CancelFunc {
	interval, err := usageCreditsRefreshInterval()
	if err != nil {
		logging.Errorf("Usage-based credit scheduler disabled: %v", err)
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshDueUsageCredits(ctx, interval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshDueUsageCredits(ctx, interval)
			case <-ctx.Done():
				return
			}
		}
	}()
	logging.Infof("Usage-based credit scheduler started: interval=%s", interval)
	return func() {
		cancel()
		<-done
	}
}

func refreshDueUsageCredits(ctx context.Context, interval time.Duration) {
	if ctx.Err() != nil {
		return
	}
	db := database.GetDB()
	if db == nil {
		return
	}
	holder, err := randomURLToken(18)
	if err != nil {
		logging.Errorf("Create usage-based credit scheduler lease holder: %v", err)
		return
	}
	now := time.Now().UTC()
	claimed, err := claimRecurringJobLease(db.WithContext(ctx), usageCreditsRefreshJobName, holder, now, jobLeaseDuration)
	if err != nil {
		logging.Errorf("Claim usage-based credit scheduler lease: %v", err)
		return
	}
	if !claimed {
		return
	}
	defer releaseRecurringJobLease(db, usageCreditsRefreshJobName, holder)
	leaseCtx, stopLeaseHeartbeat := startUsageCreditsLeaseHeartbeat(ctx, db, holder)
	defer stopLeaseHeartbeat()

	var accounts []model.Account
	if err := db.WithContext(ctx).
		Where("(credential_type = ? AND access_token != '') OR (credential_type = ? AND api_key != '')",
			model.CredentialOAuth, model.CredentialAPIKey).
		Find(&accounts).Error; err != nil {
		logging.Errorf("Load accounts for usage-based credit refresh: %v", err)
		return
	}
	ids := make([]uint, 0, len(accounts))
	for index := range accounts {
		if ctx.Err() != nil {
			return
		}
		account := &accounts[index]
		if !usageCreditsRefreshDue(*account, now, interval) {
			continue
		}
		ids = append(ids, account.ID)
	}
	if len(ids) == 0 {
		return
	}
	results, err := RefreshAccountsCredits(leaseCtx, ids, false)
	if err != nil && ctx.Err() == nil {
		logging.Errorf("Refresh usage-based credits: %v", err)
		return
	}
	for _, result := range results {
		if result.Err != nil && !errors.Is(result.Err, errCreditRefreshBusy) && !errors.Is(result.Err, errCreditRefreshSuperseded) {
			logging.Debugf("Scheduled usage-based credit refresh failed: account_id=%d error=%v", result.AccountID, result.Err)
		}
	}
}

func startUsageCreditsLeaseHeartbeat(ctx context.Context, db *gorm.DB, holder string) (context.Context, func()) {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(jobLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				result := db.WithContext(heartbeatCtx).Model(&model.SchedulerLease{}).
					Where("name = ? AND holder = ?", usageCreditsRefreshJobName, holder).
					Updates(map[string]interface{}{"lease_until": time.Now().UTC().Add(jobLeaseDuration)})
				if result.Error != nil || result.RowsAffected != 1 {
					if result.Error != nil {
						logging.Warnf("Renew usage-based credit scheduler lease: %v", result.Error)
					} else {
						logging.Warnf("Usage-based credit scheduler lease was lost")
					}
					cancel()
					return
				}
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()
	return heartbeatCtx, func() {
		cancel()
		<-done
	}
}

func usageCreditsRefreshDue(account model.Account, now time.Time, interval time.Duration) bool {
	if account.UsageCreditsPeriodEnd != nil && !account.UsageCreditsPeriodEnd.After(now) {
		return true
	}
	last := account.UsageCreditsLastAttemptAt
	return last == nil || last.IsZero() || !last.Add(interval).After(now)
}

func claimRecurringJobLease(db *gorm.DB, name, holder string, now time.Time, duration time.Duration) (bool, error) {
	lease := model.SchedulerLease{Name: name, Holder: holder, LeaseUntil: now.Add(duration)}
	created := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&lease)
	if created.Error != nil {
		return false, created.Error
	}
	if created.RowsAffected == 1 {
		return true, nil
	}
	result := db.Model(&model.SchedulerLease{}).
		Where("name = ? AND (holder = ? OR lease_until <= ?)", name, holder, now).
		Updates(map[string]interface{}{"holder": holder, "lease_until": now.Add(duration)})
	return result.RowsAffected == 1, result.Error
}

func releaseRecurringJobLease(db *gorm.DB, name, holder string) {
	_ = db.Model(&model.SchedulerLease{}).
		Where("name = ? AND holder = ?", name, holder).
		Updates(map[string]interface{}{"holder": "", "lease_until": time.Now().UTC()}).Error
}
