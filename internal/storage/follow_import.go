package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

var ErrAccountImportActive = errors.New("an account import is already in progress")

func checkAccountImportTx(ctx context.Context, tx bun.Tx, accountID string) error {
	// Lock the account before checking, including when no import row exists yet.
	res, err := tx.NewUpdate().Model((*models.Account)(nil)).
		Set("updated_at = updated_at").Where("id = ?", accountID).
		Where("domain IS NULL").Where("is_service_actor = ?", false).
		Where("deletion_state = ''").Exec(ctx)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("account import is unavailable")
	}
	active, err := tx.NewSelect().Model((*models.AccountImport)(nil)).
		Where("account_id = ?", accountID).Where("state IN ('queued', 'running')").Exists(ctx)
	if err != nil {
		return err
	}
	if active {
		return ErrAccountImportActive
	}
	return nil
}

func (s *bunStorage) CreateFollowImport(ctx context.Context, imp *models.AccountImport, blocks []string, job *models.Job) error {
	if imp.ID == "" || len(blocks) == 0 {
		return fmt.Errorf("invalid follow import")
	}
	now := time.Now().UTC()
	imp.CreatedAt, imp.UpdatedAt, imp.State = now, now, "queued"
	applyJobDefaults(job)
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := checkAccountImportTx(ctx, tx, imp.AccountID); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(imp).Exec(ctx); err != nil {
			return err
		}
		payloads := make([]models.JobPayload, len(blocks))
		for i, body := range blocks {
			payloads[i] = models.JobPayload{ID: imp.ID + ":" + strconv.Itoa(i), Body: body, CreatedAt: now}
		}
		// Keep each INSERT below SQLite's parameter limit for large CSV uploads.
		for start := 0; start < len(payloads); start += 100 {
			end := min(start+100, len(payloads))
			part := payloads[start:end]
			if _, err := tx.NewInsert().Model(&part).Exec(ctx); err != nil {
				return err
			}
		}
		_, err := tx.NewInsert().Model(job).Exec(ctx)
		return err
	})
	if err == nil {
		metrics.IncrementQueueEnqueued()
	}
	return err
}

// AdvanceFollowImport commits the cursor, follow and delivery together. An old
// worker cannot commit over a newer cursor, even if its lease was recovered.
func (s *bunStorage) AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expectedResult string, follow *models.Follow, jobs []*models.Job) (bool, error) {
	if follow != nil {
		prepareFollow(follow)
	}
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	advanced := false
	notificationCreated := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		advanced, notificationCreated = false, false
		res, err := tx.NewUpdate().Model((*models.AccountImport)(nil)).
			Set("result_json = ?", imp.ResultJSON).Set("state = ?", imp.State).
			Set("last_error = ?", imp.LastError).Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", imp.ID).Where("account_id = ?", imp.AccountID).
			Where("state IN ('queued', 'running')").Where("result_json = ?", expectedResult).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if follow != nil {
			if follow.AccountID != imp.AccountID {
				return fmt.Errorf("follow import account mismatch")
			}
			if err := s.createFollowTx(ctx, tx, follow); err != nil {
				return err
			}
		}
		if len(jobs) > 0 {
			if _, err := tx.NewInsert().Model(&jobs).Exec(ctx); err != nil {
				return err
			}
		}
		if imp.State == "complete" {
			var err error
			notificationCreated, err = createFollowImportNotification(ctx, tx, imp)
			if err != nil {
				return err
			}
			if _, err := tx.NewDelete().Model((*models.JobPayload)(nil)).Where("id LIKE ?", imp.ID+":%").Exec(ctx); err != nil {
				return err
			}
		}
		advanced = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if advanced {
		if follow != nil {
			s.rels.invalidateViewers(follow.AccountID)
		}
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
		if notificationCreated {
			metrics.IncrementQueueEnqueued()
		}
	}
	return advanced, nil
}

func createFollowImportNotification(ctx context.Context, tx bun.IDB, imp *models.AccountImport) (bool, error) {
	notif := &models.Notification{
		ID: imp.ID, AccountID: imp.AccountID, FromAccountID: imp.AccountID,
		Type: "follow_import." + imp.State + ":" + imp.ID,
	}
	prepareNotification(notif)
	return createNotificationOnce(ctx, tx, notif, []*models.Job{{Kind: models.NotificationCreatedJobKind, Payload: notif.ID}})
}
