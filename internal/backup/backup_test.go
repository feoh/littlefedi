package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/backup"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.NewConfig(t, "")
	cfg.Backup.Path = filepath.Join(t.TempDir(), "backups")

	store, err := storage.NewBunStorage(cfg.DB.DSN, 4, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	alice := testutil.CreateLocalAccount(t, store, "alice")

	if _, ok := backup.LastBackupTime(cfg.Backup.Path); ok {
		t.Fatal("LastBackupTime reported a backup before any was made")
	}
	dir, err := backup.CreateBackup(ctx, store, cfg)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	for _, name := range []string{"manifest.json", "config.toml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("backup lacks %s: %v", name, err)
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("backup directory permissions = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	when, ok := backup.LastBackupTime(cfg.Backup.Path)
	if !ok || time.Since(when) > time.Hour {
		t.Errorf("LastBackupTime = %v, %v", when, ok)
	}

	// A SQLite import closes the store and replaces its file, so the restored
	// database is read back through a fresh connection.
	restoreCfg := testutil.NewConfig(t, "")
	target, err := storage.NewBunStorage(restoreCfg.DB.DSN, 4, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.RestoreBackup(ctx, dir, target, restoreCfg); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	reopened, err := storage.NewBunStorage(restoreCfg.DB.DSN, 4, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.GetAccountByUsernameDomain(ctx, "alice", nil)
	if err != nil || restored.ID != alice.ID {
		t.Errorf("restored account = %+v, %v", restored, err)
	}
}

func TestRestoreRejectsNonBackupDirectory(t *testing.T) {
	cfg := testutil.NewConfig(t, "")
	if err := backup.RestoreBackup(context.Background(), t.TempDir(), testutil.NewStore(t), cfg); err == nil {
		t.Error("restoring a directory without a manifest succeeded")
	}
}
