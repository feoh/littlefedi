package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func (s *Service) DeleteStatus(ctx context.Context, account *models.Account, statusID string) error {
	status, err := s.store.GetStatus(ctx, statusID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.AccountID != account.ID {
		return fmt.Errorf("not your post")
	}
	tombstone, jobs, err := federation.PrepareStatusDeletion(ctx, s.store, s.domain, account, status)
	if err != nil {
		return err
	}
	attachments, err := s.store.GetMediaForStatus(ctx, status.ID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteStatusWithJobs(ctx, status, false, tombstone, jobs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	for _, attachment := range attachments {
		if err := medialib.DeleteAttachmentFiles(ctx, s.mediaStore, s.mediaPath, attachment); err != nil {
			slog.Warn("app: failed to remove media object", "id", attachment.ID, "error", err)
		}
	}
	status.Account = account
	s.pub().StatusDeleted(ctx, status)
	s.invalidateStatusAndViewer(account.ID, statusID)
	return nil
}
