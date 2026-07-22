package database

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

const (
	currentSchemaVersion       = 4
	migrationBusyTimeoutMillis = 30000
	runtimeBusyTimeoutMillis   = 5000
	migrationLeaseDuration     = 10 * time.Minute
)

func Init(dbPath string) (retErr error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(dbPath)), "file:") {
		return fmt.Errorf("file: SQLite DSNs are not supported; DB_PATH must be a filesystem path or :memory:")
	}
	if err := prepareDatabasePath(dbPath); err != nil {
		return err
	}
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // 完全关闭日志输出
	})
	if err != nil {
		return err
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("get database connection pool: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = sqlDB.Close()
			DB = nil
		}
	}()
	if err := secureDatabaseFile(dbPath); err != nil {
		return err
	}
	// SQLite has one writer. Serializing this small local database avoids
	// transient SQLITE_BUSY failures during token refresh and usage updates.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := DB.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", migrationBusyTimeoutMillis)).Error; err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	if err := DB.Exec("PRAGMA journal_mode = WAL").Error; err != nil {
		return fmt.Errorf("enable sqlite WAL: %w", err)
	}
	if err := secureDatabaseFile(dbPath); err != nil {
		return err
	}
	if err := DB.Exec("PRAGMA synchronous = NORMAL").Error; err != nil {
		return fmt.Errorf("configure sqlite synchronization: %w", err)
	}
	if err := DB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return fmt.Errorf("enable sqlite foreign keys: %w", err)
	}
	if err := runSchemaMigrations(DB); err != nil {
		return err
	}
	if err := DB.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", runtimeBusyTimeoutMillis)).Error; err != nil {
		return fmt.Errorf("configure runtime sqlite busy timeout: %w", err)
	}
	return nil
}

func runSchemaMigrations(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS zencoder_schema_migrations (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		version INTEGER NOT NULL,
		holder TEXT NOT NULL,
		lease_until DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		return fmt.Errorf("create schema migration state: %w", err)
	}
	now := time.Now().UTC()
	if err := db.Exec(`INSERT OR IGNORE INTO zencoder_schema_migrations
		(id, version, holder, lease_until, updated_at) VALUES (1, 0, '', ?, ?)`, time.Unix(1, 0).UTC(), now).Error; err != nil {
		return fmt.Errorf("initialize schema migration state: %w", err)
	}
	holder, err := randomMigrationHolder()
	if err != nil {
		return fmt.Errorf("create schema migration holder: %w", err)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		claimed := tx.Exec(`UPDATE zencoder_schema_migrations
			SET holder = ?, lease_until = ?, updated_at = ?
			WHERE id = 1 AND (holder = '' OR holder = ? OR lease_until <= ?)`,
			holder, now.Add(migrationLeaseDuration), now, holder, now)
		if claimed.Error != nil {
			return fmt.Errorf("claim schema migration lease: %w", claimed.Error)
		}
		if claimed.RowsAffected != 1 {
			return errors.New("schema migration lease is held by another process")
		}
		var version int
		if err := tx.Raw("SELECT version FROM zencoder_schema_migrations WHERE id = 1 AND holder = ?", holder).Scan(&version).Error; err != nil {
			return fmt.Errorf("read schema migration version: %w", err)
		}
		if version < 1 {
			if err := migrateSchemaVersionOne(tx); err != nil {
				return err
			}
			version = 1
		}
		if version < 2 {
			if err := migrateSchemaVersionTwo(tx); err != nil {
				return err
			}
			version = 2
		}
		if version < 3 {
			if err := migrateSchemaVersionThree(tx); err != nil {
				return err
			}
			version = 3
		}
		if version < 4 {
			if err := migrateSchemaVersionFour(tx); err != nil {
				return err
			}
			version = 4
		}
		completed := tx.Exec(`UPDATE zencoder_schema_migrations
			SET version = ?, holder = '', lease_until = ?, updated_at = ?
			WHERE id = 1 AND holder = ?`, version, time.Now().UTC(), time.Now().UTC(), holder)
		if completed.Error != nil {
			return fmt.Errorf("complete schema migration: %w", completed.Error)
		}
		if completed.RowsAffected != 1 {
			return errors.New("schema migration lease was lost before completion")
		}
		return nil
	})
}

func migrateSchemaVersionOne(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.Account{},
		&model.OAuthSession{},
		&model.AdminSession{},
		&model.SchedulerLease{},
	); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := migrateAccountSecrets(db); err != nil {
		return err
	}
	if err := db.Model(&model.Account{}).Where("credential_type = '' AND access_token != ''").Update("credential_type", model.CredentialOAuth).Error; err != nil {
		return fmt.Errorf("normalize OAuth credential type: %w", err)
	}
	if err := db.Model(&model.Account{}).Where("credential_type = '' AND api_key != ''").Update("credential_type", model.CredentialAPIKey).Error; err != nil {
		return fmt.Errorf("normalize API-key credential type: %w", err)
	}
	if err := db.Model(&model.Account{}).Where("credential_revision = 0").Update("credential_revision", 1).Error; err != nil {
		return fmt.Errorf("initialize credential revisions: %w", err)
	}

	// client_secret is a retired legacy field. APIKey is retained and migrated
	// through the same encrypted credential store as OAuth tokens.
	if db.Migrator().HasColumn(&model.Account{}, "client_secret") {
		if err := db.Migrator().DropColumn(&model.Account{}, "client_secret"); err != nil {
			return fmt.Errorf("drop legacy account column client_secret: %w", err)
		}
	}
	// Schema v1 did not persist the PKCE verifier. Keep its cleanup here so v2
	// exclusively owns the encrypted verifier column added below.
	if db.Migrator().HasColumn(&model.OAuthSession{}, "code_verifier") {
		if err := db.Migrator().DropColumn(&model.OAuthSession{}, "code_verifier"); err != nil {
			return fmt.Errorf("drop retired OAuth session verifier: %w", err)
		}
	}
	return nil
}

func migrateSchemaVersionTwo(db *gorm.DB) error {
	if db.Migrator().HasColumn(&model.OAuthSession{}, "code_verifier") {
		return nil
	}
	if err := db.Migrator().AddColumn(&model.OAuthSession{}, "CodeVerifier"); err != nil {
		return fmt.Errorf("add OAuth session verifier: %w", err)
	}
	return nil
}

func migrateSchemaVersionThree(db *gorm.DB) error {
	if err := db.AutoMigrate(&model.Account{}); err != nil {
		return fmt.Errorf("migrate usage-based credit snapshot: %w", err)
	}
	if err := db.Model(&model.Account{}).
		Where("usage_credits_status = '' OR usage_credits_status IS NULL").
		Update("usage_credits_status", "unknown").Error; err != nil {
		return fmt.Errorf("initialize usage-based credit status: %w", err)
	}
	return nil
}

func migrateSchemaVersionFour(db *gorm.DB) error {
	if err := db.AutoMigrate(&model.Account{}); err != nil {
		return fmt.Errorf("migrate usage-based credit period: %w", err)
	}
	return nil
}

func randomMigrationHolder() (string, error) {
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func migrateAccountSecrets(db *gorm.DB) error {
	var accounts []model.Account
	if err := db.Where("access_token != '' OR refresh_token != '' OR api_key != ''").Find(&accounts).Error; err != nil {
		return fmt.Errorf("load account secrets for migration: %w", err)
	}
	for _, account := range accounts {
		accessToken, err := secret.Encrypt(account.AccessToken)
		if err != nil {
			return fmt.Errorf("encrypt access token for account %d: %w", account.ID, err)
		}
		refreshToken, err := secret.Encrypt(account.RefreshToken)
		if err != nil {
			return fmt.Errorf("encrypt refresh token for account %d: %w", account.ID, err)
		}
		apiKey, err := secret.Encrypt(account.APIKey)
		if err != nil {
			return fmt.Errorf("encrypt API key for account %d: %w", account.ID, err)
		}
		clientID := account.ClientID
		if account.APIKey != "" {
			plaintextAPIKey, err := secret.Decrypt(account.APIKey)
			if err != nil {
				return fmt.Errorf("decrypt API key for account %d: %w", account.ID, err)
			}
			index, err := secret.BlindIndex("zencoder-api-key", strings.TrimSpace(plaintextAPIKey))
			if err != nil {
				return fmt.Errorf("index API key for account %d: %w", account.ID, err)
			}
			clientID = "api-key-" + index
		}
		if accessToken == account.AccessToken && refreshToken == account.RefreshToken && apiKey == account.APIKey && clientID == account.ClientID {
			continue
		}
		if err := db.Model(&model.Account{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"api_key":       apiKey,
			"client_id":     clientID,
		}).Error; err != nil {
			return fmt.Errorf("persist encrypted tokens for account %d: %w", account.ID, err)
		}
	}
	return nil
}

func prepareDatabasePath(dbPath string) error {
	if !isFilesystemDatabasePath(dbPath) {
		return nil
	}
	directory := filepath.Dir(dbPath)
	if directory == "." {
		return nil
	}
	if info, err := os.Stat(directory); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("database parent path is not a directory: %s", directory)
		}
		if err := securePathPermissions(directory, true); err != nil {
			return fmt.Errorf("secure database directory: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect database directory: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := securePathPermissions(directory, true); err != nil {
		return fmt.Errorf("secure database directory: %w", err)
	}
	return nil
}

func secureDatabaseFile(dbPath string) error {
	if !isFilesystemDatabasePath(dbPath) {
		return nil
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := securePathPermissions(path, false); err != nil {
			if errors.Is(err, os.ErrNotExist) && path != dbPath {
				continue
			}
			return fmt.Errorf("secure database file %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func isFilesystemDatabasePath(dbPath string) bool {
	trimmed := strings.TrimSpace(dbPath)
	return trimmed != "" && trimmed != ":memory:"
}

func GetDB() *gorm.DB {
	return DB
}
