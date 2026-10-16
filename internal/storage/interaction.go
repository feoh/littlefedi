package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"github.com/uptrace/bun"
)

func insertJobs(ctx context.Context, tx bun.IDB, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	_, err := tx.NewInsert().Model(&jobs).Exec(ctx)
	return err
}

func interactionNotification(ctx context.Context, tx bun.IDB, fresh bool, notif *models.Notification, jobs []*models.Job) (bool, error) {
	if notif == nil {
		if fresh {
			return true, insertJobs(ctx, tx, jobs)
		}
		return false, nil
	}
	if !fresh {
		return createNotificationOnce(ctx, tx, notif, jobs)
	}
	if err := upsertNotification(ctx, tx, notif); err != nil {
		return false, err
	}
	// Upsert retains the existing notification ID after Undo followed by a new event.
	for _, job := range jobs {
		if job.Kind != "push_notification" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return false, err
		}
		payload["notif_id"] = notif.ID
		body, err := json.Marshal(payload)
		if err != nil {
			return false, err
		}
		job.Payload = string(body)
	}
	return true, insertJobs(ctx, tx, jobs)
}

func (s *bunStorage) CreateFavouriteWithNotification(ctx context.Context, fave *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error) {
	if fave.ID == "" {
		fave.ID = newID()
	}
	if fave.CreatedAt.IsZero() {
		fave.CreatedAt = time.Now()
	}
	prepareNotification(notif)
	notified := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewInsert().Model(fave).On("CONFLICT (account_id, status_id) DO NOTHING").Exec(ctx)
		if err != nil {
			return err
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if inserted > 0 {
			if _, err := tx.NewUpdate().Model((*models.Status)(nil)).Set("favourites_count = favourites_count + 1").Where("id = ?", fave.StatusID).Exec(ctx); err != nil {
				return err
			}
		}
		notified, err = interactionNotification(ctx, tx, inserted > 0, notif, jobs)
		return err
	})
	if err != nil {
		return false, err
	}
	if notified {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return notified, nil
}

func (s *bunStorage) CreateBoostWithNotification(ctx context.Context, boost *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error) {
	prepareStatus(boost)
	prepareNotification(notif)
	notified := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewInsert().Model(boost).On("CONFLICT (uri) DO NOTHING").Exec(ctx)
		if err != nil {
			return err
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if inserted > 0 {
			if err := persistStatusRows(ctx, tx, boost, nil); err != nil {
				return err
			}
		} else {
			accountID, targetID := boost.AccountID, boost.BoostOfID
			if err := tx.NewSelect().Model(boost).Where("status.uri = ?", boost.URI).Scan(ctx); err != nil {
				return err
			}
			if boost.AccountID != accountID || boost.BoostOfID == nil || targetID == nil || *boost.BoostOfID != *targetID {
				return fmt.Errorf("boost activity id was reused")
			}
		}
		notified, err = interactionNotification(ctx, tx, inserted > 0, notif, jobs)
		return err
	})
	if err != nil {
		return false, err
	}
	if notified {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return notified, nil
}
