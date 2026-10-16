package storage

import (
	"context"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"github.com/uptrace/bun"
)

// CanReadStatusVersion checks the audience and content version in one snapshot.
func (s *bunStorage) CanReadStatusVersion(ctx context.Context, status *models.Status, viewerID string) (bool, error) {
	return s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.id = ? AND status.updated_at = ?", status.ID, status.UpdatedAt).
		Where(`(status.account_id = ? OR EXISTS (SELECT 1 FROM mentions m WHERE m.status_id = status.id AND m.account_id = ?) OR (status.visibility = ? AND EXISTS (SELECT 1 FROM follows f WHERE f.account_id = ? AND f.target_account_id = status.account_id AND f.state = ?)))`, viewerID, viewerID, models.VisibilityPrivate, viewerID, models.FollowAccepted).Exists(ctx)
}

// EditStatusWithJobs commits only editable fields and their derived relationships.
func (s *bunStorage) EditStatusWithJobs(ctx context.Context, status *models.Status, expected time.Time, jobs []*models.Job) error {
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model(status).
			Column("content_html", "content_source", "content_type", "summary", "sensitive", "language", "edited_at", "updated_at", "is_blog_post", "blog_slug", "expires_at").
			WherePK().Where("updated_at = ?", expected).Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrStatusConflict
		}

		// Conditional writes claim attachments safely even when another post races us.
		ids := make([]string, 0, len(status.MediaAttachments))
		seen := make(map[string]bool, len(status.MediaAttachments))
		for _, media := range status.MediaAttachments {
			if media.ID == "" || seen[media.ID] {
				return ErrInvalidMedia
			}
			seen[media.ID] = true
			res, err := tx.NewUpdate().Model((*models.MediaAttachment)(nil)).Set("status_id = ?", status.ID).
				Set("description = ?", media.Description).
				Where("id = ? AND account_id = ?", media.ID, status.AccountID).
				Where("(status_id IS NULL OR status_id = ?)", status.ID).Exec(ctx)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return ErrInvalidMedia
			}
			ids = append(ids, media.ID)
		}
		q := tx.NewUpdate().Model((*models.MediaAttachment)(nil)).Set("status_id = NULL").Where("status_id = ?", status.ID)
		if len(ids) > 0 {
			q = q.Where("id NOT IN (?)", bun.In(ids))
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		tags := make([]*models.Tag, 0, len(status.Tags))
		for i := range status.Tags {
			tags = append(tags, &status.Tags[i])
		}
		if _, err := syncStatusTags(ctx, tx, status.ID, tags); err != nil {
			return err
		}
		mentions := make([]*models.Mention, 0, len(status.Mentions))
		for i := range status.Mentions {
			mentions = append(mentions, &status.Mentions[i])
		}
		if err := syncStatusMentions(ctx, tx, status.ID, mentions); err != nil {
			return err
		}
		// Rebuild tag-derived home membership inside the same transaction.
		if _, err := tx.NewDelete().Model((*models.HomeFeedEntry)(nil)).Where("status_id = ?", status.ID).Exec(ctx); err != nil {
			return err
		}
		viewers, err := homeTimelineViewers(ctx, tx, status)
		if err != nil {
			return err
		}
		for _, viewer := range viewers {
			entry := &models.HomeFeedEntry{AccountID: viewer, StatusID: status.ID, AuthorAccountID: status.AccountID, CreatedAt: status.CreatedAt}
			if _, err := tx.NewInsert().Model(entry).Exec(ctx); err != nil {
				return err
			}
		}
		if len(jobs) > 0 {
			_, err = tx.NewInsert().Model(&jobs).Exec(ctx)
		}
		return err
	})
	if err == nil {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return err
}
