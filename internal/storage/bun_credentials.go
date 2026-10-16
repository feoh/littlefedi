package storage

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func revokeAccountCredentials(ctx context.Context, tx bun.Tx, accountID string) error {
	for _, model := range []any{(*models.PushSubscription)(nil), (*models.Session)(nil)} {
		if _, err := tx.NewDelete().Model(model).Where("account_id = ?", accountID).Exec(ctx); err != nil {
			return err
		}
	}
	_, err := tx.NewUpdate().Model((*models.OAuthToken)(nil)).Set("revoked_at = ?", time.Now()).Where("account_id = ?", accountID).Where("revoked_at IS NULL").Exec(ctx)
	return err
}
