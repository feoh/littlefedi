package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// minScheduleLeadTime mirrors Mastodon's minimum lead time for scheduled posts.
const minScheduleLeadTime = 5 * time.Minute

// ScheduleStatus persists input for later publication instead of posting it
// now, and enqueues a delayed job to publish it at scheduledAt.
func (s *Service) ScheduleStatus(ctx context.Context, account *models.Account, input StatusInput, scheduledAt time.Time) (*models.ScheduledStatus, error) {
	if !scheduledAt.After(time.Now().Add(minScheduleLeadTime)) {
		return nil, fmt.Errorf("scheduled_at must be at least %s in the future", minScheduleLeadTime)
	}
	if err := ValidateStatusInput(input); err != nil {
		return nil, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	scheduled := &models.ScheduledStatus{AccountID: account.ID, ParamsJSON: string(body), ScheduledAt: scheduledAt}
	if err := s.store.CreateScheduledStatus(ctx, scheduled); err != nil {
		return nil, err
	}
	return scheduled, nil
}

// PublishScheduledStatus prepares outside the transaction, then consumes the
// expected due schedule with the post. Only a missing schedule counts as already
// finished; storage failures and concurrent rescheduling are returned to the worker.
func (s *Service) PublishScheduledStatus(ctx context.Context, id, accountID string) error {
	v, err := s.store.GetScheduledStatus(ctx, id, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if time.Now().Before(v.ScheduledAt) {
		return nil
	}
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	var input StatusInput
	if err := json.Unmarshal([]byte(v.ParamsJSON), &input); err != nil {
		return err
	}
	prepared, err := s.prepareStatus(ctx, account, input)
	if err != nil {
		return err
	}
	if err := s.store.PublishScheduledStatusWithJobs(ctx, v, prepared.status, prepared.jobs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	s.finishStatus(ctx, prepared)
	return nil
}

// RescheduleStatus moves a pending scheduled status to a new time and
// enqueues a fresh publish job for it.
func (s *Service) RescheduleStatus(ctx context.Context, accountID, id string, newAt time.Time) error {
	if !newAt.After(time.Now().Add(minScheduleLeadTime)) {
		return fmt.Errorf("scheduled_at must be at least %s in the future", minScheduleLeadTime)
	}
	return s.store.UpdateScheduledStatusTime(ctx, id, accountID, newAt)
}

// CancelScheduledStatus deletes a pending scheduled status. Any still-queued
// job for it will find the row gone and no-op in PublishScheduledStatus.
func (s *Service) CancelScheduledStatus(ctx context.Context, accountID, id string) error {
	return s.store.DeleteScheduledStatus(ctx, id, accountID)
}
