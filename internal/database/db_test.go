package database

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInitEncryptsLegacyAPIKey(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))))
	path := filepath.Join(t.TempDir(), "migration.db")
	legacy, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.AutoMigrate(&model.Account{}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Create(&model.Account{ClientID: "legacy-key", CredentialType: model.CredentialAPIKey, APIKey: "legacy-fixture"}).Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, err := legacy.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacySQL.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var account model.Account
	if err := GetDB().Where("credential_type = ?", model.CredentialAPIKey).First(&account).Error; err != nil {
		t.Fatal(err)
	}
	if !secret.IsEncrypted(account.APIKey) {
		t.Fatal("legacy API key remained plaintext")
	}
	wantIndex, err := secret.BlindIndex("zencoder-api-key", "legacy-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if account.ClientID != "api-key-"+wantIndex {
		t.Fatalf("legacy API key retained an unkeyed identity: %q", account.ClientID)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("database permissions = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestInitRejectsSQLiteFileURI(t *testing.T) {
	if err := Init("file:" + filepath.Join(t.TempDir(), "database.db")); err == nil {
		t.Fatal("expected file: SQLite DSN to be rejected")
	}
}

func TestInitAddsCodeVerifierToVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-one.db")
	legacy, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.AutoMigrate(&model.OAuthSession{}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Migrator().DropColumn(&model.OAuthSession{}, "code_verifier"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`CREATE TABLE zencoder_schema_migrations (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		version INTEGER NOT NULL,
		holder TEXT NOT NULL,
		lease_until DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`INSERT INTO zencoder_schema_migrations
		(id, version, holder, lease_until, updated_at) VALUES (1, 1, '', ?, ?)`,
		time.Unix(1, 0).UTC(), time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, err := legacy.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacySQL.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if !GetDB().Migrator().HasColumn(&model.OAuthSession{}, "code_verifier") {
		t.Fatal("version 2 migration did not add OAuth session code_verifier")
	}
	session := model.OAuthSession{
		State:        "state",
		CodeVerifier: "verifier",
		AnonymousID:  "anonymous",
		Origin:       "http://localhost:8080",
		RedirectURL:  "http://localhost:8080/oauth/zencoder/callback/state",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	if err := GetDB().Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	var stored model.OAuthSession
	if err := GetDB().First(&stored, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CodeVerifier != session.CodeVerifier {
		t.Fatalf("code verifier = %q, want %q", stored.CodeVerifier, session.CodeVerifier)
	}
}

func TestInitHelperProcess(t *testing.T) {
	if os.Getenv("ZENCODER_DB_INIT_HELPER") != "1" {
		return
	}
	path := os.Getenv("ZENCODER_DB_INIT_PATH")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentProcessesSerializeSchemaMigration(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))
	path := filepath.Join(t.TempDir(), "concurrent-migration.db")
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, len(commands))
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestInitHelperProcess$", "-test.count=1")
		command.Env = append(os.Environ(), "ZENCODER_DB_INIT_HELPER=1", "ZENCODER_DB_INIT_PATH="+path)
		command.Stdout = &outputs[index]
		command.Stderr = &outputs[index]
		commands[index] = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("migration helper %d failed: %v\n%s", index, err, outputs[index].String())
		}
	}

	verification, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := verification.Raw("SELECT version FROM zencoder_schema_migrations WHERE id = 1").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	sqlDB, err := verification.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
}
