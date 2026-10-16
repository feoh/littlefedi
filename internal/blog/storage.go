package blog

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

type rebuildStorage struct {
	storage.Storage
	cfg config.Blog
}

// NewRebuildStorage hooks the common persistence boundary below the normal
// cache decorators, so inbox, import, backfill and expiry need no special hooks.
func NewRebuildStorage(inner storage.Storage, cfg config.Blog) storage.Storage {
	if !cfg.Enabled {
		return inner
	}
	return &rebuildStorage{Storage: inner, cfg: cfg}
}

func (s *rebuildStorage) CreateStatus(ctx context.Context, status *models.Status) error {
	if err := s.Storage.CreateStatus(ctx, status); err != nil {
		return err
	}
	s.enqueueAffected(ctx, status)
	return nil
}

func (s *rebuildStorage) CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.CreateStatusWithJobs(ctx, status, jobs); err != nil {
		return err
	}
	s.enqueueAffected(ctx, status)
	return nil
}

func (s *rebuildStorage) UpdateStatus(ctx context.Context, status *models.Status) error {
	before, _ := s.Storage.GetStatus(ctx, status.ID)
	beforeReq, beforeOK := s.affected(ctx, before)
	if err := s.Storage.UpdateStatus(ctx, status); err != nil {
		return err
	}
	if beforeOK {
		s.enqueue(ctx, beforeReq)
	}
	s.enqueueAffected(ctx, status)
	return nil
}

func (s *rebuildStorage) SetStatusBlog(ctx context.Context, statusID string, isBlogPost bool, slug string) error {
	before, _ := s.Storage.GetStatus(ctx, statusID)
	beforeReq, beforeOK := s.affected(ctx, before)
	if err := s.Storage.SetStatusBlog(ctx, statusID, isBlogPost, slug); err != nil {
		return err
	}
	if beforeOK {
		s.enqueue(ctx, beforeReq)
	}
	status, _ := s.Storage.GetStatus(ctx, statusID)
	s.enqueueAffected(ctx, status)
	return nil
}

func (s *rebuildStorage) UpdateStatusContent(ctx context.Context, status *models.Status) error {
	if err := s.Storage.UpdateStatusContent(ctx, status); err != nil {
		return err
	}
	stored, _ := s.Storage.GetStatus(ctx, status.ID)
	s.enqueueAffected(ctx, stored)
	return nil
}

func (s *rebuildStorage) DeleteStatus(ctx context.Context, id string) error {
	before, _ := s.Storage.GetStatus(ctx, id)
	req, ok := s.affected(ctx, before)
	if err := s.Storage.DeleteStatus(ctx, id); err != nil {
		return err
	}
	if ok {
		s.enqueue(ctx, req)
	}
	return nil
}

func (s *rebuildStorage) enqueueAffected(ctx context.Context, status *models.Status) {
	if req, ok := s.affected(ctx, status); ok {
		s.enqueue(ctx, req)
	}
}

func (s *rebuildStorage) affected(ctx context.Context, status *models.Status) (RebuildRequest, bool) {
	if status == nil {
		return RebuildRequest{}, false
	}
	if status.IsBlogPost {
		return RebuildRequest{AccountID: status.AccountID}, true
	}
	if status.InReplyToID == nil || (status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted) {
		return RebuildRequest{}, false
	}
	ancestors, _, err := s.Storage.GetStatusContext(ctx, status.ID)
	if err != nil {
		return RebuildRequest{}, false
	}
	for _, ancestor := range ancestors {
		if !ancestor.IsBlogPost || ancestor.Visibility != models.VisibilityPublic {
			continue
		}
		account, err := s.Storage.GetAccount(ctx, ancestor.AccountID)
		if err == nil && account.Domain == nil && s.cfg.AccountAllowed(account.BlogAllowed) && account.BlogRepliesEnabled {
			return RebuildRequest{AccountID: account.ID, PostID: ancestor.ID}, true
		}
	}
	return RebuildRequest{}, false
}

func (s *rebuildStorage) enqueue(ctx context.Context, req RebuildRequest) {
	EnqueueRebuild(ctx, s.Storage, req)
}

// EnqueueRebuild queues and coalesces a full-account or single-post rebuild.
func EnqueueRebuild(ctx context.Context, store storage.Storage, req RebuildRequest) {
	key := req.Key()
	if !Claim(key) {
		return
	}
	payload, _ := json.Marshal(req)
	if err := store.EnqueueJob(ctx, &models.Job{Kind: RebuildJobKind, Payload: string(payload)}); err != nil {
		Cancel(key)
		slog.Warn("blog: failed to enqueue rebuild", "key", key, "error", err)
	}
}

func (s *rebuildStorage) EditStatusWithJobs(ctx context.Context, st *models.Status, expected time.Time, jobs []*models.Job) error {
	before, _ := s.Storage.GetStatus(ctx, st.ID)
	if err := s.Storage.EditStatusWithJobs(ctx, st, expected, jobs); err != nil {
		return err
	}
	s.enqueueAffected(ctx, before)
	s.enqueueAffected(ctx, st)
	return nil
}

func (s *rebuildStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.PublishScheduledStatusWithJobs(ctx, expected, status, jobs); err != nil {
		return err
	}
	s.enqueueAffected(ctx, status)
	return nil
}

func (s *rebuildStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	id := expected.ID
	before, _ := s.Storage.GetStatus(ctx, id)
	req, ok := s.affected(ctx, before)
	if err := s.Storage.DeleteStatusWithJobs(ctx, expected, expiredOnly, tombstone, jobs); err != nil {
		return err
	}
	if ok {
		s.enqueue(ctx, req)
	}
	return nil
}
