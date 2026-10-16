//go:build !postgres && (darwin || (freebsd && (386 || amd64 || arm || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (netbsd && amd64) || windows || (openbsd && (amd64 || arm64)))

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"modernc.org/sqlite"
)

// The native modernc driver runs SQLite against real process memory, so the
// larger read-path defaults are safe here. The WASM fallback driver keeps them
// off: its interpreter owns a hard-capped 32MB memory arena on 32-bit hosts
// and mmap goes through an emulated VFS.
const (
	sqliteWASMDriver        = false
	sqliteMmapDefaultBytes  = 64 << 20 // mmap_size: page reads become mmap hits (SD-card win)
	sqliteCacheDefaultPages = "-8000"  // cache_size: ~8MB page cache vs SQLite's ~2MB default

	// No arena: this driver allocates against process memory, not a slab of
	// the Go heap, so nothing sizing the Go heap has to account for it.
	sqliteConnectionMemoryLimitBytes int64 = 0
)

func openStorageDB(dsn string) (*sql.DB, *bun.DB, string, error) {
	if isPostgresDSN(dsn) {
		return nil, nil, "", fmt.Errorf("storage: PostgreSQL support is not included in the default SQLite binary; rebuild with -tags postgres")
	}
	sqldb, err := sql.Open("sqlite", normalizeSQLiteDSN(dsn))
	if err != nil {
		return nil, nil, "", fmt.Errorf("storage: open sqlite: %w", err)
	}
	return sqldb, bun.NewDB(sqldb, sqlitedialect.New()), "sqlite", nil
}

func sqliteBackup(ctx context.Context, db *sql.DB, dsn, path string) error {
	if !strings.Contains(dsn, ":memory:") && !strings.Contains(dsn, "mode=memory") {
		var err error
		db, err = sql.Open("sqlite", normalizeSQLiteDSN(dsn))
		if err != nil {
			return err
		}
		defer db.Close()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("storage: SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(path)
		if err != nil {
			return err
		}
		if _, err := backup.Step(-1); err != nil {
			_ = backup.Finish()
			return err
		}
		return backup.Finish()
	})
}
