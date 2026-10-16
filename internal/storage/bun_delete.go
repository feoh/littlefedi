package storage

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func (s *bunStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	if tombstone != nil {
		if tombstone.ID == "" {
			tombstone.ID = newID()
		}
		if tombstone.CreatedAt.IsZero() {
			tombstone.CreatedAt = time.Now()
		}
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		// The conditional write locks the version on both backends until deletion.
		q := tx.NewUpdate().Model((*models.Status)(nil)).Set("updated_at = updated_at").
			Where("id = ? AND account_id = ? AND updated_at = ?", expected.ID, expected.AccountID, expected.UpdatedAt)
		if expiredOnly {
			q = q.Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now())
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			exists, err := tx.NewSelect().Model((*models.Status)(nil)).Where("id = ?", expected.ID).Exists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				return sql.ErrNoRows
			}
			return ErrStatusConflict
		}
		if err := deleteStatusRows(ctx, tx, expected.ID); err != nil {
			return err
		}
		if tombstone != nil {
			if _, err := tx.NewInsert().Model(tombstone).On("CONFLICT (uri) DO NOTHING").Exec(ctx); err != nil {
				return err
			}
		}
		if len(jobs) > 0 {
			if _, err := tx.NewInsert().Model(&jobs).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return err
}

func deleteStatusRows(ctx context.Context, tx bun.Tx, id string) error {
	var targets []struct {
		ID          string  `bun:"id"`
		AccountID   string  `bun:"account_id"`
		URI         string  `bun:"uri"`
		InReplyToID *string `bun:"in_reply_to_id"`
		BoostOfID   *string `bun:"boost_of_id"`
	}
	if err := tx.NewSelect().Table("statuses").
		Column("id", "account_id", "uri", "in_reply_to_id", "boost_of_id").
		Where("id = ? OR boost_of_id = ?", id, id).
		Scan(ctx, &targets); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	statusIDs := make([]string, 0, len(targets))
	statusURIs := make([]string, 0, len(targets))
	accountIDs := make([]string, 0, len(targets))
	parentIDs := make([]string, 0, len(targets))
	originalIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		statusIDs = append(statusIDs, target.ID)
		statusURIs = append(statusURIs, target.URI)
		accountIDs = append(accountIDs, target.AccountID)
		if target.InReplyToID != nil && *target.InReplyToID != "" {
			parentIDs = append(parentIDs, *target.InReplyToID)
		}
		if target.BoostOfID != nil && *target.BoostOfID != "" {
			originalIDs = append(originalIDs, *target.BoostOfID)
		}
	}

	// Capture linked tags before removing the pivots so their denormalized
	// counts can be repaired exactly after the delete.
	var tagIDs []string
	if err := tx.NewSelect().Model((*models.StatusTag)(nil)).
		Column("tag_id").
		Where("status_id IN (?)", bun.In(statusIDs)).
		Scan(ctx, &tagIDs); err != nil && err != sql.ErrNoRows {
		return err
	}

	// Remove every dependent row, including Announce wrappers of an original
	// status. The only materialized data is the small set of directly affected
	// status IDs; poll and vote rows stay inside SQL.
	if _, err := tx.NewDelete().Model((*models.PollVote)(nil)).
		Where("poll_id IN (SELECT id FROM polls WHERE status_id IN (?))", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*models.PollOption)(nil)).
		Where("poll_id IN (SELECT id FROM polls WHERE status_id IN (?))", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*models.Poll)(nil)).Where("status_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*models.QuoteAuthorization)(nil)).
		Where("quoted_status_id IN (?) OR quoting_uri IN (?)", bun.In(statusIDs), bun.In(statusURIs)).Exec(ctx); err != nil {
		return err
	}
	for _, m := range []interface{}{
		(*models.StatusTag)(nil),
		(*models.Mention)(nil),
		(*models.Favourite)(nil),
		(*models.EmojiReaction)(nil),
		(*models.Bookmark)(nil),
		(*models.StatusPreview)(nil),
		(*models.HomeFeedEntry)(nil),
	} {
		if _, err := tx.NewDelete().Model(m).Where("status_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := tx.NewDelete().Model((*models.MediaAttachment)(nil)).
		Where("status_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*models.Notification)(nil)).
		Where("status_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	// Replies and quotes retain their canonical URI but no longer point to a
	// missing local row.
	if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
		Set("in_reply_to_id = NULL").Where("in_reply_to_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
		Set("quote_of_id = NULL").Where("quote_of_id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*models.Status)(nil)).Where("id IN (?)", bun.In(statusIDs)).Exec(ctx); err != nil {
		return err
	}

	if err := recountTagCounts(ctx, tx, tagIDs); err != nil {
		return err
	}
	if len(parentIDs) > 0 {
		if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("replies_count = (SELECT COUNT(*) FROM statuses AS child WHERE child.in_reply_to_id = status.id AND child.visibility <> 'direct')").
			Where("id IN (?)", bun.In(parentIDs)).Exec(ctx); err != nil {
			return err
		}
	}
	if len(originalIDs) > 0 {
		if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("reblogs_count = (SELECT COUNT(*) FROM statuses AS boost WHERE boost.boost_of_id = status.id)").
			Where("id IN (?)", bun.In(originalIDs)).Exec(ctx); err != nil {
			return err
		}
	}
	_, err := tx.NewUpdate().Model((*models.Account)(nil)).
		Set("statuses_count = (SELECT COUNT(*) FROM statuses WHERE statuses.account_id = account.id)").
		Set("updated_at = ?", time.Now()).
		Where("id IN (?)", bun.In(accountIDs)).
		Where("domain IS NULL").
		Exec(ctx)
	return err
}
