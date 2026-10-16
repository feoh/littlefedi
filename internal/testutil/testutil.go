// Package testutil holds fixtures shared by the test suites: a migrated
// SQLite store in a temporary directory and a loaded configuration. It
// depends only on config and storage so any package above them can use it.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Domain is the instance domain used by every fixture.
const Domain = "test.example"

// SQLiteDSN returns the production SQLite DSN pointed at a file in dir.
func SQLiteDSN(dir string) string {
	return filepath.Join(dir, "littlefedi.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
}

// NewStore opens a fresh SQLite database under t.TempDir, runs every
// migration and closes it when the test ends.
func NewStore(t testing.TB) storage.Storage {
	t.Helper()
	store, err := storage.NewBunStorage(SQLiteDSN(t.TempDir()), 4, 4, false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RunMigrations(context.Background()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return store
}

// CreateLocalAccount stores an approved local account on Domain. It has no
// signing keys or password; use accounts.BootstrapLocalAccount when a test
// needs a fully provisioned account.
func CreateLocalAccount(t testing.TB, store storage.Storage, username string) *models.Account {
	t.Helper()
	base := "https://" + Domain + "/users/" + username
	now := time.Now()
	account := &models.Account{
		Username: username, DisplayName: username, Approved: true,
		ActorURI: base, InboxURI: base + "/inbox", OutboxURI: base + "/outbox",
		FollowersURI: base + "/followers", FollowingURI: base + "/following",
		PublicKeyPem: "pem", PublicKeyID: base + "#main-key",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAccount(context.Background(), account); err != nil {
		t.Fatalf("create account %s: %v", username, err)
	}
	return account
}

// NewConfig loads a configuration for Domain through config.Load, so all of
// the normal defaults and derived fields apply. Extra TOML is appended to the
// file after local_domain.
func NewConfig(t testing.TB, extraTOML string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "littlefedi.toml")
	body := "local_domain = \"" + Domain + "\"\n" + extraTOML
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.DB.DSN = SQLiteDSN(dir)
	cfg.Media.Path = filepath.Join(dir, "media")
	return cfg
}
