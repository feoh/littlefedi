package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

func (s *bunStorage) CreatePoll(ctx context.Context, p *models.Poll) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return insertPoll(ctx, tx, p.StatusID, p)
	})
}
func (s *bunStorage) loadPoll(ctx context.Context, q *bun.SelectQuery, viewerID string) (*models.Poll, error) {
	p := new(models.Poll)
	if err := q.Scan(ctx, p); err != nil {
		return nil, err
	}
	if err := s.db.NewSelect().Model(&p.Options).Where("poll_id = ?", p.ID).Order("position ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if viewerID != "" {
		var pos []int
		err := s.db.NewSelect().Model((*models.PollVote)(nil)).Column("o.position").Join("JOIN poll_options AS o ON o.id = poll_vote.option_id").Where("poll_vote.poll_id = ? AND poll_vote.account_id = ?", p.ID, viewerID).Scan(ctx, &pos)
		if err == nil {
			p.OwnVotes = pos
			p.Voted = len(pos) > 0
		}
	}
	if p.ExpiresAt != nil && time.Now().After(*p.ExpiresAt) {
		p.Expired = true
	}
	return p, nil
}
func (s *bunStorage) GetPoll(ctx context.Context, id, viewerID string) (*models.Poll, error) {
	return s.loadPoll(ctx, s.db.NewSelect().Model((*models.Poll)(nil)).Where("id = ?", id), viewerID)
}
func (s *bunStorage) GetPollForStatus(ctx context.Context, statusID, viewerID string) (*models.Poll, error) {
	return s.loadPoll(ctx, s.db.NewSelect().Model((*models.Poll)(nil)).Where("status_id = ?", statusID), viewerID)
}

func (s *bunStorage) GetPollsForStatuses(ctx context.Context, ids []string, viewer string) (map[string]*models.Poll, error) {
	out := make(map[string]*models.Poll, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var polls []*models.Poll
	if err := s.db.NewSelect().Model(&polls).Where("status_id IN (?)", bun.In(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	if len(polls) == 0 {
		return out, nil
	}
	pids := make([]string, 0, len(polls))
	byID := map[string]*models.Poll{}
	for _, p := range polls {
		out[p.StatusID] = p
		byID[p.ID] = p
		pids = append(pids, p.ID)
	}
	var opts []models.PollOption
	if err := s.db.NewSelect().Model(&opts).Where("poll_id IN (?)", bun.In(pids)).Order("poll_id ASC", "position ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, o := range opts {
		p := byID[o.PollID]
		p.Options = append(p.Options, o)
	}
	if viewer != "" {
		var votes []models.PollVote
		if err := s.db.NewSelect().Model(&votes).Where("poll_id IN (?) AND account_id = ?", bun.In(pids), viewer).Scan(ctx); err == nil {
			pos := map[string]int{}
			for _, p := range polls {
				for _, o := range p.Options {
					pos[o.ID] = o.Position
				}
			}
			for _, v := range votes {
				p := byID[v.PollID]
				p.Voted = true
				p.OwnVotes = append(p.OwnVotes, pos[v.OptionID])
			}
		}
	}
	return out, nil
}

func (s *bunStorage) SyncPoll(ctx context.Context, p *models.Poll) error {
	if p == nil || p.StatusID == "" {
		return fmt.Errorf("poll missing status")
	}
	existing, err := s.GetPollForStatus(ctx, p.StatusID, "")
	if err != nil {
		return s.CreatePoll(ctx, p)
	}
	p.ID = existing.ID
	p.StatusID = existing.StatusID
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.Poll)(nil)).
			Set("expires_at = ?", p.ExpiresAt).
			Set("expired = ?", p.Expired).
			Set("multiple = ?", p.Multiple).
			Set("hide_totals = ?", p.HideTotals).
			Set("votes_count = ?", p.VotesCount).
			Set("voters_count = ?", p.VotersCount).
			Where("id = ?", p.ID).
			Exec(ctx); err != nil {
			return err
		}

		existingByPos := make(map[int]models.PollOption, len(existing.Options))
		for _, option := range existing.Options {
			existingByPos[option.Position] = option
		}
		for i := range p.Options {
			option := &p.Options[i]
			option.PollID = p.ID
			option.Position = i
			if existingOption, ok := existingByPos[i]; ok {
				option.ID = existingOption.ID
				if _, err := tx.NewUpdate().Model((*models.PollOption)(nil)).
					Set("title = ?", option.Title).
					Set("votes_count = ?", option.VotesCount).
					Where("id = ?", option.ID).
					Exec(ctx); err != nil {
					return err
				}
				continue
			}
			if option.ID == "" {
				option.ID = uuid.NewString()
			}
			if _, err := tx.NewInsert().Model(option).Exec(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.NewDelete().Model((*models.PollOption)(nil)).
			Where("poll_id = ? AND position >= ?", p.ID, len(p.Options)).
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
}

func (s *bunStorage) VotePoll(ctx context.Context, pollID, accountID string, choices []int) (*models.Poll, error) {
	p, err := s.GetPoll(ctx, pollID, accountID)
	if err != nil {
		return nil, err
	}
	if p.Expired || p.ExpiresAt != nil && time.Now().After(*p.ExpiresAt) {
		return nil, fmt.Errorf("poll expired")
	}
	if len(choices) == 0 || (!p.Multiple && len(choices) != 1) {
		return nil, fmt.Errorf("invalid choices")
	}
	sort.Ints(choices)
	for i, c := range choices {
		if c < 0 || c >= len(p.Options) || i > 0 && c == choices[i-1] {
			return nil, fmt.Errorf("invalid choices")
		}
	}
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		count, err := tx.NewSelect().Model((*models.PollVote)(nil)).Where("poll_id = ? AND account_id = ?", pollID, accountID).Count(ctx)
		if err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("already voted")
		}
		for _, c := range choices {
			v := &models.PollVote{ID: uuid.NewString(), PollID: pollID, OptionID: p.Options[c].ID, AccountID: accountID, CreatedAt: time.Now()}
			if _, err := tx.NewInsert().Model(v).Exec(ctx); err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*models.PollOption)(nil)).Set("votes_count = votes_count + 1").Where("id = ?", v.OptionID).Exec(ctx); err != nil {
				return err
			}
		}
		_, err = tx.NewUpdate().Model((*models.Poll)(nil)).Set("votes_count = votes_count + ?", len(choices)).Set("voters_count = voters_count + 1").Where("id = ?", pollID).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetPoll(ctx, pollID, accountID)
}
func (s *bunStorage) ClosePoll(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*models.Poll)(nil)).Set("expired = ?", true).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) ListLocalPollVoterIDs(ctx context.Context, pollID string) ([]string, error) {
	var ids []string
	err := s.db.NewSelect().
		TableExpr("poll_votes AS pv").
		ColumnExpr("DISTINCT pv.account_id").
		Join("JOIN accounts AS a ON a.id = pv.account_id").
		Where("pv.poll_id = ?", pollID).
		Where("a.domain IS NULL").
		Scan(ctx, &ids)
	return ids, err
}

func hydrateFilter(f *models.Filter) {
	if err := json.Unmarshal([]byte(f.ContextJSON), &f.Context); err != nil {
		slog.Warn("storage: filter context_json is corrupt, returning empty context", "filter_id", f.ID, "error", err)
	}
	if err := json.Unmarshal([]byte(f.LanguagesJSON), &f.Languages); err != nil {
		slog.Warn("storage: filter languages_json is corrupt, returning empty languages", "filter_id", f.ID, "error", err)
	}
	f.Languages = statusfilter.NormalizeLanguages(f.Languages)
	f.Statuses = []any{}
}
func (s *bunStorage) loadFilterKeywords(ctx context.Context, f *models.Filter) error {
	hydrateFilter(f)
	return s.db.NewSelect().Model(&f.Keywords).Where("filter_id = ?", f.ID).Order("id ASC").Scan(ctx)
}
func (s *bunStorage) CreateFilter(ctx context.Context, f *models.Filter) error {
	if f.ID == "" {
		f.ID = uuid.NewString()
	}
	b, _ := json.Marshal(f.Context)
	f.ContextJSON = string(b)
	f.Languages = statusfilter.NormalizeLanguages(f.Languages)
	f.InvertLanguages = f.InvertLanguages && len(f.Languages) > 0
	f.ShowUnknownLanguages = f.ShowUnknownLanguages && f.InvertLanguages
	b, _ = json.Marshal(f.Languages)
	f.LanguagesJSON = string(b)
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(f).Exec(ctx); err != nil {
			return err
		}
		for i := range f.Keywords {
			f.Keywords[i].ID = uuid.NewString()
			f.Keywords[i].FilterID = f.ID
		}
		if len(f.Keywords) > 0 {
			_, err := tx.NewInsert().Model(&f.Keywords).Exec(ctx)
			return err
		}
		return nil
	})
}
func (s *bunStorage) GetFilter(ctx context.Context, id, accountID string) (*models.Filter, error) {
	f := new(models.Filter)
	if err := s.db.NewSelect().Model(f).Where("id = ? AND account_id = ?", id, accountID).Scan(ctx); err != nil {
		return nil, err
	}
	return f, s.loadFilterKeywords(ctx, f)
}
func (s *bunStorage) ListFilters(ctx context.Context, accountID string) ([]*models.Filter, error) {
	var fs []*models.Filter
	if err := s.db.NewSelect().Model(&fs).Where("account_id = ?", accountID).Order("id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, f := range fs {
		if err := s.loadFilterKeywords(ctx, f); err != nil {
			return nil, err
		}
	}
	return fs, nil
}
func (s *bunStorage) UpdateFilter(ctx context.Context, f *models.Filter) error {
	b, _ := json.Marshal(f.Context)
	f.ContextJSON = string(b)
	f.Languages = statusfilter.NormalizeLanguages(f.Languages)
	f.InvertLanguages = f.InvertLanguages && len(f.Languages) > 0
	f.ShowUnknownLanguages = f.ShowUnknownLanguages && f.InvertLanguages
	b, _ = json.Marshal(f.Languages)
	f.LanguagesJSON = string(b)
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model(f).Column("title", "context_json", "languages_json", "invert_languages", "show_unknown_languages", "filter_action", "expires_at").Where("id = ? AND account_id = ?", f.ID, f.AccountID).Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		if _, err = tx.NewDelete().Model((*models.FilterKeyword)(nil)).Where("filter_id = ?", f.ID).Exec(ctx); err != nil {
			return err
		}
		for i := range f.Keywords {
			f.Keywords[i].ID = uuid.NewString()
			f.Keywords[i].FilterID = f.ID
		}
		if len(f.Keywords) > 0 {
			_, err = tx.NewInsert().Model(&f.Keywords).Exec(ctx)
		}
		return err
	})
}
func (s *bunStorage) DeleteFilter(ctx context.Context, id, accountID string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.FilterKeyword)(nil)).Where("filter_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*models.Filter)(nil)).Where("id = ? AND account_id = ?", id, accountID).Exec(ctx)
		return err
	})
}

func (s *bunStorage) CreateScheduledStatus(ctx context.Context, v *models.ScheduledStatus) error {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	job := scheduledStatusJob(v.ID, v.AccountID, v.ScheduledAt)
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(v).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(job).Exec(ctx)
		return err
	})
	if err == nil {
		metrics.IncrementQueueEnqueued()
	}
	return err
}
func (s *bunStorage) GetScheduledStatus(ctx context.Context, id, a string) (*models.ScheduledStatus, error) {
	v := new(models.ScheduledStatus)
	err := s.db.NewSelect().Model(v).Where("id = ? AND account_id = ?", id, a).Scan(ctx)
	return v, err
}
func (s *bunStorage) ListScheduledStatuses(ctx context.Context, a string, limit int) ([]*models.ScheduledStatus, error) {
	if limit <= 0 || limit > 40 {
		limit = 40
	}
	var v []*models.ScheduledStatus
	err := s.db.NewSelect().Model(&v).Where("account_id = ?", a).Order("scheduled_at ASC").Limit(limit).Scan(ctx)
	return v, err
}
func (s *bunStorage) UpdateScheduledStatusTime(ctx context.Context, id, a string, scheduledAt time.Time) error {
	job := scheduledStatusJob(id, a, scheduledAt)
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*models.ScheduledStatus)(nil)).Set("scheduled_at = ?", scheduledAt).Where("id = ? AND account_id = ?", id, a).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.NewInsert().Model(job).Exec(ctx)
		return err
	})
	if err == nil {
		metrics.IncrementQueueEnqueued()
	}
	return err
}
func (s *bunStorage) DeleteScheduledStatus(ctx context.Context, id, a string) error {
	_, err := s.db.NewDelete().Model((*models.ScheduledStatus)(nil)).Where("id = ? AND account_id = ?", id, a).Exec(ctx)
	return err
}
func (s *bunStorage) ListAnnouncements(ctx context.Context, now time.Time) ([]*models.Announcement, error) {
	var v []*models.Announcement
	err := s.db.NewSelect().Model(&v).Where("(starts_at IS NULL OR starts_at <= ?) AND (ends_at IS NULL OR ends_at > ?)", now, now).Order("published_at DESC").Scan(ctx)
	return v, err
}
