package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

// Fork migration. bun names a migration by its numeric file prefix alone, so
// a fork migration sharing a number with a future upstream one would mark
// that upstream migration as already applied. Fork migrations therefore
// start at 9001, far above upstream's sequence.
//
// standard records whether a Web Push client asked for RFC 8291 aes128gcm;
// Mastodon clients that do not ask expect the legacy aesgcm draft encoding.
// access_token holds the subscribing app's bearer token (sealed by keycrypt
// when a key is configured) because Mastodon push payloads carry it.
func init() {
	Migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		for _, stmt := range []string{
			`ALTER TABLE push_subscriptions ADD COLUMN standard BOOLEAN NOT NULL DEFAULT false`,
			`ALTER TABLE push_subscriptions ADD COLUMN access_token TEXT NOT NULL DEFAULT ''`,
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		for _, stmt := range []string{
			`ALTER TABLE push_subscriptions DROP COLUMN access_token`,
			`ALTER TABLE push_subscriptions DROP COLUMN standard`,
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}
