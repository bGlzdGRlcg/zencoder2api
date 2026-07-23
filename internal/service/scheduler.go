package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
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
)

// StartCreditResetScheduler starts the daily reset loop and returns a cancel
// function for graceful shutdown. CREDIT_RESET_TIMEZONE defaults to UTC and
// CREDIT_RESET_AT defaults to 09:09.
func StartCreditResetScheduler() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	location, err := creditResetLocation()
	if err != nil {
		logging.Errorf("Credit reset scheduler disabled: %v", err)
		return cancel
	}
	hour, minute, err := creditResetClock()
	if err != nil {
		logging.Errorf("Credit reset scheduler disabled: %v", err)
		return cancel
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			now := time.Now().In(location)
			next := nextDailyTime(now, hour, minute, location)
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
	logging.Infof("Credit reset scheduler started: timezone=%s at=%02d:%02d", location, hour, minute)
	return func() {
		cancel()
		<-done
	}
}

func creditResetLocation() (*time.Location, error) {
	name := strings.TrimSpace(os.Getenv("CREDIT_RESET_TIMEZONE"))
	if name == "" {
		name = "UTC"
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid CREDIT_RESET_TIMEZONE %q: %w", name, err)
	}
	return location, nil
}

func creditResetClock() (int, int, error) {
	raw := strings.TrimSpace(os.Getenv("CREDIT_RESET_AT"))
	if raw == "" {
		raw = "09:09"
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("CREDIT_RESET_AT must use HH:MM")
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("CREDIT_RESET_AT must use a valid 24-hour HH:MM value")
	}
	return hour, minute, nil
}

// ValidateCreditResetConfig lets startup fail closed instead of silently
// running without maintenance when scheduler environment values are invalid.
func ValidateCreditResetConfig() error {
	if _, err := creditResetLocation(); err != nil {
		return err
	}
	_, _, err := creditResetClock()
	return err
}

func nextDailyTime(now time.Time, hour, minute int, location *time.Location) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, location)
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, location)
	}
	return next
}

func ResetAllCredits() error {
	location, err := creditResetLocation()
	if err != nil {
		return err
	}
	return resetAllCreditsAt(context.Background(), time.Now().In(location))
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
