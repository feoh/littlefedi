package storage

import (
	"context"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// This file holds batch ("...ForStatuses" / "...ByIDs") loaders used to enrich whole
// timelines in a constant number of queries instead of one-per-status. They keep
// EnrichStatus free of N+1 queries: a 40-item timeline must
// not issue hundreds of serialized round-trips (which serialize hard on SQLite's
// single writer and amplify under load on PostgreSQL). Each loader returns a map
// keyed by status/account id; callers stitch the relations together in Go.

// GetStatusesByIDs loads statuses for the given ids, returned as a map keyed by
// status id. Authors are loaded by the enrichment pass in one shared batch.
func (s *bunStorage) GetStatusesByIDs(ctx context.Context, ids []string) (map[string]*models.Status, error) {
	out := make(map[string]*models.Status, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var statuses []*models.Status
	err := s.db.NewSelect().Model(&statuses).
		Where("status.id IN (?)", bun.In(ids)).
		Scan(ctx)
	if err != nil {
		return out, err
	}
	for _, st := range statuses {
		out[st.ID] = st
	}
	return out, nil
}

// GetAccountsByIDs loads accounts for the given ids, returned as a map keyed by
// account id. Missing ids are simply absent.
func (s *bunStorage) GetAccountsByIDs(ctx context.Context, ids []string) (map[string]*models.Account, error) {
	out := make(map[string]*models.Account, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("id IN (?)", bun.In(ids)).
		Scan(ctx)
	if err != nil {
		return out, err
	}
	for _, a := range accounts {
		out[a.ID] = a
	}
	return out, nil
}

// GetMentionsForStatuses returns the mentions of each status id, grouped by
// status id. Every requested id is present in the map (empty slice when none).
func (s *bunStorage) GetMentionsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Mention, error) {
	out := make(map[string][]*models.Mention, len(statusIDs))
	for _, id := range statusIDs {
		out[id] = nil
	}
	if len(statusIDs) == 0 {
		return out, nil
	}
	var mentions []*models.Mention
	err := s.db.NewSelect().Model(&mentions).
		Where("status_id IN (?)", bun.In(statusIDs)).
		Scan(ctx)
	if err != nil {
		return out, err
	}
	for _, m := range mentions {
		out[m.StatusID] = append(out[m.StatusID], m)
	}
	return out, nil
}

// GetMediaForStatuses returns the media attachments of each status id, grouped by
// status id and ordered by creation time. Every requested id is present.
func (s *bunStorage) GetMediaForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.MediaAttachment, error) {
	out := make(map[string][]*models.MediaAttachment, len(statusIDs))
	for _, id := range statusIDs {
		out[id] = nil
	}
	if len(statusIDs) == 0 {
		return out, nil
	}
	var media []*models.MediaAttachment
	err := s.db.NewSelect().Model(&media).
		Where("status_id IN (?)", bun.In(statusIDs)).
		Order("created_at ASC").
		Scan(ctx)
	if err != nil {
		return out, err
	}
	for _, m := range media {
		if m.StatusID == nil {
			continue
		}
		out[*m.StatusID] = append(out[*m.StatusID], m)
	}
	return out, nil
}

// GetTagsForStatuses returns the hashtags of each status id, grouped by status
// id. Every requested id is present in the map (empty slice when none).
func (s *bunStorage) GetTagsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Tag, error) {
	out := make(map[string][]*models.Tag, len(statusIDs))
	for _, id := range statusIDs {
		out[id] = nil
	}
	if len(statusIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		StatusID      string `bun:"status_id"`
		ID            string `bun:"id"`
		Name          string `bun:"name"`
		URL           string `bun:"url"`
		StatusesCount int    `bun:"statuses_count"`
	}
	err := s.db.NewSelect().
		ColumnExpr("st.status_id AS status_id").
		ColumnExpr("tag.id AS id").
		ColumnExpr("tag.name AS name").
		ColumnExpr("tag.url AS url").
		ColumnExpr("tag.statuses_count AS statuses_count").
		TableExpr("status_tags AS st").
		Join("JOIN tags AS tag ON tag.id = st.tag_id").
		Where("st.status_id IN (?)", bun.In(statusIDs)).
		Scan(ctx, &rows)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.StatusID] = append(out[r.StatusID], &models.Tag{
			ID:            r.ID,
			Name:          r.Name,
			URL:           r.URL,
			StatusesCount: r.StatusesCount,
		})
	}
	return out, nil
}

// ViewerStatusInteractions returns which of statusIDs the viewer has
// favourited, bookmarked, and boosted, as three sets, in a single query
// (UNION ALL over favourites/bookmarks/statuses) instead of three serialized
// round-trips per timeline build. The reblog leg matches the per-row
// GetReblogByAccountStatus check: a status authored by accountID whose
// boost_of_id points at the target.
func (s *bunStorage) ViewerStatusInteractions(ctx context.Context, accountID string, statusIDs []string) (faved, booked, reblogged map[string]bool, err error) {
	faved = make(map[string]bool)
	booked = make(map[string]bool)
	reblogged = make(map[string]bool)
	if accountID == "" || len(statusIDs) == 0 {
		return faved, booked, reblogged, nil
	}
	var rows []struct {
		Kind     string `bun:"kind"`
		StatusID string `bun:"sid"`
	}
	err = s.db.NewRaw(
		`SELECT 'f' AS kind, status_id AS sid FROM favourites WHERE account_id = ? AND status_id IN (?)
		 UNION ALL
		 SELECT 'b' AS kind, status_id AS sid FROM bookmarks WHERE account_id = ? AND status_id IN (?)
		 UNION ALL
		 SELECT 'r' AS kind, boost_of_id AS sid FROM statuses WHERE account_id = ? AND boost_of_id IN (?)`,
		accountID, bun.In(statusIDs),
		accountID, bun.In(statusIDs),
		accountID, bun.In(statusIDs),
	).Scan(ctx, &rows)
	if err != nil {
		return faved, booked, reblogged, err
	}
	for _, r := range rows {
		switch r.Kind {
		case "f":
			faved[r.StatusID] = true
		case "b":
			booked[r.StatusID] = true
		case "r":
			reblogged[r.StatusID] = true
		}
	}
	return faved, booked, reblogged, nil
}
