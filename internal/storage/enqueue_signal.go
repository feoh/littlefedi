package storage

import (
	"context"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// enqueueSignalStorage notifies the worker pool after every successful local
// enqueue, so just-enqueued jobs run immediately instead of waiting out the
// workers' poll interval. Every other method passes through.
type enqueueSignalStorage struct {
	Storage
	notifyN func(int)
}

// NewEnqueueSignalStorage wraps inner so notify fires after each successful
// EnqueueJob/EnqueueJobs. A nil notify returns inner unchanged.
func NewEnqueueSignalStorage(inner Storage, notify func()) Storage {
	if notify == nil {
		return inner
	}
	return NewEnqueueSignalStorageN(inner, func(int) { notify() })
}

// NewEnqueueSignalStorageN wakes enough workers to consume a successful batch.
func NewEnqueueSignalStorageN(inner Storage, notifyN func(int)) Storage {
	if notifyN == nil {
		return inner
	}
	return &enqueueSignalStorage{Storage: inner, notifyN: notifyN}
}

func (s *enqueueSignalStorage) EnqueueJob(ctx context.Context, job *models.Job) error {
	if err := s.Storage.EnqueueJob(ctx, job); err != nil {
		return err
	}
	s.notifyN(1)
	return nil
}

func (s *enqueueSignalStorage) CreateFollowImport(ctx context.Context, imp *models.AccountImport, blocks []string, job *models.Job) error {
	if err := s.Storage.CreateFollowImport(ctx, imp, blocks, job); err != nil {
		return err
	}
	s.notifyN(1)
	return nil
}

func (s *enqueueSignalStorage) AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expected string, follow *models.Follow, jobs []*models.Job) (bool, error) {
	advanced, err := s.Storage.AdvanceFollowImport(ctx, imp, expected, follow, jobs)
	if err == nil && advanced && len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return advanced, err
}

func (s *enqueueSignalStorage) EnqueueJobs(ctx context.Context, jobs []*models.Job) error {
	if err := s.Storage.EnqueueJobs(ctx, jobs); err != nil {
		return err
	}
	if len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return nil
}

func (s *enqueueSignalStorage) CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.CreateStatusWithJobs(ctx, status, jobs); err != nil {
		return err
	}
	s.notifyN(len(jobs))
	return nil
}

func (s *enqueueSignalStorage) CreateNotificationOnce(ctx context.Context, notif *models.Notification, jobs []*models.Job) (bool, error) {
	created, err := s.Storage.CreateNotificationOnce(ctx, notif, jobs)
	if err == nil && created && len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return created, err
}

// enqueueSignalByKindStorage is like enqueueSignalStorage but routes the wake
// by job kind, so a worker pool dedicated to a kind subset (e.g. delivery/push/email)
// isn't woken by unrelated enqueues it can't process anyway.
type enqueueSignalByKindStorage struct {
	Storage
	notify func(kind string, n int)
}

// NewEnqueueSignalStorageByKind wraps inner so notify(job.Kind, n) fires
// after each successful EnqueueJob/EnqueueJobs, letting the caller wake only
// the pool(s) that own that kind. A nil notify returns inner unchanged.
func NewEnqueueSignalStorageByKind(inner Storage, notify func(kind string, n int)) Storage {
	if notify == nil {
		return inner
	}
	return &enqueueSignalByKindStorage{Storage: inner, notify: notify}
}

func (s *enqueueSignalByKindStorage) EnqueueJob(ctx context.Context, job *models.Job) error {
	if err := s.Storage.EnqueueJob(ctx, job); err != nil {
		return err
	}
	s.notify(job.Kind, 1)
	return nil
}

func (s *enqueueSignalByKindStorage) CreateFollowImport(ctx context.Context, imp *models.AccountImport, blocks []string, job *models.Job) error {
	if err := s.Storage.CreateFollowImport(ctx, imp, blocks, job); err != nil {
		return err
	}
	s.notify(job.Kind, 1)
	return nil
}

func (s *enqueueSignalByKindStorage) AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expected string, follow *models.Follow, jobs []*models.Job) (bool, error) {
	advanced, err := s.Storage.AdvanceFollowImport(ctx, imp, expected, follow, jobs)
	if err == nil && advanced {
		for _, job := range jobs {
			s.notify(job.Kind, 1)
		}
	}
	return advanced, err
}

// EnqueueJobs assumes a single batch is one job kind, true of every current
// caller (delivery fan-out, push notification batches, housekeeping) which
// each build a homogeneous-kind slice.
func (s *enqueueSignalByKindStorage) EnqueueJobs(ctx context.Context, jobs []*models.Job) error {
	if err := s.Storage.EnqueueJobs(ctx, jobs); err != nil {
		return err
	}
	if len(jobs) > 0 {
		s.notify(jobs[0].Kind, len(jobs))
	}
	return nil
}

func (s *enqueueSignalByKindStorage) CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.CreateStatusWithJobs(ctx, status, jobs); err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, job := range jobs {
		counts[job.Kind]++
	}
	for kind, n := range counts {
		s.notify(kind, n)
	}
	return nil
}

func (s *enqueueSignalByKindStorage) CreateNotificationOnce(ctx context.Context, notif *models.Notification, jobs []*models.Job) (bool, error) {
	created, err := s.Storage.CreateNotificationOnce(ctx, notif, jobs)
	if err != nil || !created {
		return created, err
	}
	counts := make(map[string]int)
	for _, job := range jobs {
		counts[job.Kind]++
	}
	for kind, n := range counts {
		s.notify(kind, n)
	}
	return true, nil
}

func (s *enqueueSignalStorage) EditStatusWithJobs(ctx context.Context, st *models.Status, expected time.Time, jobs []*models.Job) error {
	if err := s.Storage.EditStatusWithJobs(ctx, st, expected, jobs); err != nil {
		return err
	}
	if len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return nil
}

func (s *enqueueSignalByKindStorage) EditStatusWithJobs(ctx context.Context, st *models.Status, expected time.Time, jobs []*models.Job) error {
	if err := s.Storage.EditStatusWithJobs(ctx, st, expected, jobs); err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, job := range jobs {
		counts[job.Kind]++
	}
	for kind, n := range counts {
		s.notify(kind, n)
	}
	return nil
}

func (s *enqueueSignalStorage) CreateFavouriteWithNotification(ctx context.Context, f *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateFavouriteWithNotification(ctx, f, notif, jobs)
	if err == nil && notified && len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return notified, err
}

func (s *enqueueSignalStorage) CreateBoostWithNotification(ctx context.Context, st *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateBoostWithNotification(ctx, st, notif, jobs)
	if err == nil && notified && len(jobs) > 0 {
		s.notifyN(len(jobs))
	}
	return notified, err
}

func (s *enqueueSignalByKindStorage) CreateFavouriteWithNotification(ctx context.Context, f *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateFavouriteWithNotification(ctx, f, notif, jobs)
	if err == nil && notified {
		counts := make(map[string]int)
		for _, job := range jobs {
			counts[job.Kind]++
		}
		for kind, n := range counts {
			s.notify(kind, n)
		}
	}
	return notified, err
}

func (s *enqueueSignalByKindStorage) CreateBoostWithNotification(ctx context.Context, st *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateBoostWithNotification(ctx, st, notif, jobs)
	if err == nil && notified {
		counts := make(map[string]int)
		for _, job := range jobs {
			counts[job.Kind]++
		}
		for kind, n := range counts {
			s.notify(kind, n)
		}
	}
	return notified, err
}

func (s *enqueueSignalStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.PublishScheduledStatusWithJobs(ctx, expected, status, jobs); err != nil {
		return err
	}
	s.notifyN(len(jobs))
	return nil
}

func (s *enqueueSignalByKindStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error {
	if err := s.Storage.PublishScheduledStatusWithJobs(ctx, expected, status, jobs); err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, job := range jobs {
		counts[job.Kind]++
	}
	for kind, n := range counts {
		s.notify(kind, n)
	}
	return nil
}

func (s *enqueueSignalStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	if err := s.Storage.DeleteStatusWithJobs(ctx, expected, expiredOnly, tombstone, jobs); err != nil {
		return err
	}
	s.notifyN(len(jobs))
	return nil
}

func (s *enqueueSignalStorage) CreateScheduledStatus(ctx context.Context, status *models.ScheduledStatus) error {
	if err := s.Storage.CreateScheduledStatus(ctx, status); err != nil {
		return err
	}
	s.notifyN(1)
	return nil
}

func (s *enqueueSignalStorage) UpdateScheduledStatusTime(ctx context.Context, id, accountID string, at time.Time) error {
	if err := s.Storage.UpdateScheduledStatusTime(ctx, id, accountID, at); err != nil {
		return err
	}
	s.notifyN(1)
	return nil
}

func (s *enqueueSignalByKindStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	if err := s.Storage.DeleteStatusWithJobs(ctx, expected, expiredOnly, tombstone, jobs); err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, job := range jobs {
		counts[job.Kind]++
	}
	for kind, n := range counts {
		s.notify(kind, n)
	}
	return nil
}

func (s *enqueueSignalByKindStorage) CreateScheduledStatus(ctx context.Context, status *models.ScheduledStatus) error {
	if err := s.Storage.CreateScheduledStatus(ctx, status); err != nil {
		return err
	}
	s.notify("scheduled_status", 1)
	return nil
}

func (s *enqueueSignalByKindStorage) UpdateScheduledStatusTime(ctx context.Context, id, accountID string, at time.Time) error {
	if err := s.Storage.UpdateScheduledStatusTime(ctx, id, accountID, at); err != nil {
		return err
	}
	s.notify("scheduled_status", 1)
	return nil
}
