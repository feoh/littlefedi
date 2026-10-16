package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if db.Dialect().Name().String() != "sqlite" {
			return nil
		}
		// UNIQUE(uri) already maintains the same index.
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_statuses_uri`)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		if db.Dialect().Name().String() != "sqlite" {
			return nil
		}
		_, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_statuses_uri ON statuses(uri)`)
		return err
	})
}
