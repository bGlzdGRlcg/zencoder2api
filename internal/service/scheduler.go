package service

import (
	"context"
	"fmt"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	creditResetJobName = "daily-credit-reset"
	jobLeaseDuration   = 2 * time.Minute
	creditResetHour    = 9
	creditResetMinute  = 9
)

// StartCreditResetScheduler starts the daily UTC 09:09 reset loop and returns
// a cancel function for graceful shutdown.
func StartCreditResetScheduler() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	location := time.UTC
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			now := time.Now().In(location)
			next := nextDailyTime(now, creditResetHour, creditResetMinute, location)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-timer.C:
				if err := resetAllCreditsAt(ctx, time.Now().In(location)); err != nil && ctx.Err() == nil {
					logging.Errorf("Credits reset failed: %v", err)
				}
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}
	}()
	logging.Infof("Credit reset scheduler started: timezone=UTC at=%02d:%02d", creditResetHour, creditResetMinute)
	return func() {
		cancel()
		<-done
	}
}

func nextDailyTime(now time.Time, hour, minute int, location *time.Location) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, location)
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, location)
	}
	return next
}

func resetAllCreditsAt(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	holder, err := randomURLToken(18)
	if err != nil {
		return fmt.Errorf("create scheduler lease holder: %w", err)
	}
	today := now.Format("2006-01-02")
	claimed, err := claimDailyJobLease(db.WithContext(ctx), creditResetJobName, holder, today, time.Now().UTC())
	if err != nil || !claimed {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			releaseDailyJobLease(db, creditResetJobName, holder)
		}
	}()

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Account{}).
			Where("last_reset_date != ? OR last_reset_date IS NULL", today).
			Updates(map[string]interface{}{"daily_used": 0, "last_reset_date": today}).Error; err != nil {
			return err
		}
		result := tx.Model(&model.SchedulerLease{}).
			Where("name = ? AND holder = ?", creditResetJobName, holder).
			Updates(map[string]interface{}{"last_run_date": today, "lease_until": time.Now().UTC(), "holder": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("scheduler lease was lost before completion")
		}
		return nil
	})
	if err != nil {
		return err
	}
	succeeded = true
	logging.Infof("Credits reset completed: date=%s timezone=%s", today, now.Location())
	return nil
}

func claimDailyJobLease(db *gorm.DB, name, holder, today string, now time.Time) (bool, error) {
	lease := model.SchedulerLease{Name: name, Holder: holder, LeaseUntil: now.Add(jobLeaseDuration)}
	created := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&lease)
	if created.Error != nil {
		return false, fmt.Errorf("create scheduler lease: %w", created.Error)
	}
	if created.RowsAffected == 1 {
		return true, nil
	}
	result := db.Model(&model.SchedulerLease{}).
		Where("name = ? AND last_run_date != ? AND lease_until <= ?", name, today, now).
		Updates(map[string]interface{}{"holder": holder, "lease_until": now.Add(jobLeaseDuration)})
	if result.Error != nil {
		return false, fmt.Errorf("claim scheduler lease: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func releaseDailyJobLease(db *gorm.DB, name, holder string) {
	_ = db.Model(&model.SchedulerLease{}).Where("name = ? AND holder = ?", name, holder).
		Updates(map[string]interface{}{"holder": "", "lease_until": time.Now().UTC()}).Error
}
