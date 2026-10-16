package storage

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// GetCatchupAffinity derives, in three indexed queries, who the account has
// been paying attention to lately: authors it favourited (favourites walked on
// the (account_id, status_id) unique index), authors it boosted or replied to,
// and the parent statuses of its own replies. Every row read belongs to this
// account, so the query cost scales with the reader's own activity, not with
// the size of the instance.
func (s *bunStorage) GetCatchupAffinity(ctx context.Context, accountID string, since time.Time, limit int) (*models.CatchupAffinity, error) {
	if limit <= 0 {
		limit = 200
	}

	authors := make(map[string]struct{}, limit)
	out := &models.CatchupAffinity{}

	var favouritedAuthors []string
	if err := s.db.NewSelect().
		Table("favourites").
		ColumnExpr("DISTINCT target.account_id").
		Join("JOIN statuses AS target ON target.id = favourites.status_id").
		Where("favourites.account_id = ?", accountID).
		Where("favourites.created_at >= ?", since).
		Limit(limit).
		Scan(ctx, &favouritedAuthors); err != nil {
		return nil, err
	}
	for _, id := range favouritedAuthors {
		if id != "" && id != accountID {
			authors[id] = struct{}{}
		}
	}

	// The account's own recent boosts and replies name the statuses it engaged
	// with; one batch lookup then turns those ids into their authors.
	var refs []struct {
		BoostOfID   *string `bun:"boost_of_id"`
		InReplyToID *string `bun:"in_reply_to_id"`
	}
	if err := s.db.NewSelect().
		Table("statuses").
		Column("boost_of_id", "in_reply_to_id").
		Where("account_id = ?", accountID).
		Where("created_at >= ?", since).
		Where("(boost_of_id IS NOT NULL OR in_reply_to_id IS NOT NULL)").
		OrderExpr("created_at DESC").
		Limit(limit).
		Scan(ctx, &refs); err != nil {
		return nil, err
	}

	engaged := make(map[string]struct{}, len(refs)*2)
	threads := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.BoostOfID != nil && *ref.BoostOfID != "" {
			engaged[*ref.BoostOfID] = struct{}{}
		}
		if ref.InReplyToID != nil && *ref.InReplyToID != "" {
			engaged[*ref.InReplyToID] = struct{}{}
			threads[*ref.InReplyToID] = struct{}{}
		}
	}

	if len(engaged) > 0 {
		ids := make([]string, 0, len(engaged))
		for id := range engaged {
			ids = append(ids, id)
		}
		var engagedAuthors []string
		if err := s.db.NewSelect().
			Table("statuses").
			ColumnExpr("DISTINCT account_id").
			Where("id IN (?)", bun.In(ids)).
			Scan(ctx, &engagedAuthors); err != nil {
			return nil, err
		}
		for _, id := range engagedAuthors {
			if id != "" && id != accountID {
				authors[id] = struct{}{}
			}
		}
	}

	out.AuthorIDs = make([]string, 0, len(authors))
	for id := range authors {
		out.AuthorIDs = append(out.AuthorIDs, id)
	}
	out.ThreadIDs = make([]string, 0, len(threads))
	for id := range threads {
		out.ThreadIDs = append(out.ThreadIDs, id)
	}
	return out, nil
}
