//go:build postgres

package storage

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// SQLiteArenaConstrained is always false in the PostgreSQL build; see the
// SQLite driver files for the WASM-arena case it guards against.
func SQLiteArenaConstrained() bool { return false }

// SQLiteMaxPoolSize leaves the pool alone: PostgreSQL has no per-connection
// arena to reserve.
func SQLiteMaxPoolSize(requested int) int { return requested }

// SQLiteArenaBytes is zero for the same reason: the database server owns its
// own memory, in another process.
func SQLiteArenaBytes(int) int64 { return 0 }

func openStorageDB(dsn string) (*sql.DB, *bun.DB, string, error) {
	if !isPostgresDSN(dsn) {
		return nil, nil, "", fmt.Errorf("storage: SQLite support is not included in the PostgreSQL binary; use a postgres:// DSN or rebuild without -tags postgres")
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, "", fmt.Errorf("storage: open postgres: %w", err)
	}
	return sqldb, bun.NewDB(sqldb, pgdialect.New()), "pg", nil
}

func sqliteBackup(context.Context, *sql.DB, string, string) error {
	return fmt.Errorf("storage: SQLite support is not included in the PostgreSQL binary")
}
