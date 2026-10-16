package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func scheduledStatusJob(id, accountID string, at time.Time) *models.Job {
	payload, _ := json.Marshal(map[string]string{"id": id, "account_id": accountID})
	job := &models.Job{Kind: "scheduled_status", Payload: string(payload), RunAt: at, MaxAttempts: 3, State: "queued"}
	applyJobDefaults(job)
	return job
}

func (s *bunStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error {
	if expected.AccountID != status.AccountID {
		return ErrScheduleChanged
	}
	prepareStatus(status)
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewDelete().Model((*models.ScheduledStatus)(nil)).
			Where("id = ? AND account_id = ?", expected.ID, expected.AccountID).
			Where("scheduled_at = ? AND scheduled_at <= ?", expected.ScheduledAt, time.Now()).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			exists, err := tx.NewSelect().Model((*models.ScheduledStatus)(nil)).Where("id = ?", expected.ID).Exists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				return sql.ErrNoRows
			}
			return ErrScheduleChanged
		}
		if _, err := tx.NewInsert().Model(status).Exec(ctx); err != nil {
			return err
		}
		return persistStatusRows(ctx, tx, status, jobs)
	})
	if err == nil {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return err
}
