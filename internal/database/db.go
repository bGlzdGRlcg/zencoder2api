package database

import (
	"zencoder-2api/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func Init(dbPath string) error {
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // 完全关闭日志输出
	})
	if err != nil {
		return err
	}

	if err := DB.AutoMigrate(
		&model.Account{},
	); err != nil {
		return err
	}

	// OAuth is now the only supported Zencoder credential. Remove legacy
	// plaintext credential columns instead of leaving unused secrets in SQLite.
	for _, column := range []string{"api_key", "client_secret"} {
		if DB.Migrator().HasColumn(&model.Account{}, column) {
			if err := DB.Migrator().DropColumn(&model.Account{}, column); err != nil {
				return err
			}
		}
	}
	return nil
}

func GetDB() *gorm.DB {
	return DB
}
