package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// AccountDeletionDeadline leaves margin beyond the queue's normal federation
// retry window. After it, local privacy rights win over an unreachable peer.
const AccountDeletionDeadline = 72 * time.Hour

func (s *Service) RequestAccountDeletion(ctx context.Context, requester, target *models.Account) (*models.AccountDeletionRun, bool, error) {
	if requester == nil || target == nil || (requester.ID != target.ID && !requester.IsAdmin) {
		return nil, false, fmt.Errorf("account deletion is not authorized")
	}
	run, created, err := s.store.ScheduleAccountDeletion(ctx, target.ID, time.Now().Add(AccountDeletionDeadline))
	if err != nil || !created {
		return run, created, err
	}
	_ = s.store.CreateModerationLog(ctx, &models.ModerationLog{
		AdminAccountID: requester.ID, TargetType: "account", TargetID: target.ID,
		Action: "delete_requested", Note: "durable account deletion requested",
	})
	s.NotifyAdminsAccountDeleted(ctx, target)
	return run, true, nil
}

func (s *Service) NotifyAdminsAccountDeleted(ctx context.Context, account *models.Account) {
	s.notifyAdminsAccountDeparture(ctx, account, "admin.account_deleted")
}

func (s *Service) NotifyAdminsAccountMoved(ctx context.Context, account *models.Account) {
	s.notifyAdminsAccountDeparture(ctx, account, "admin.account_moved")
}

func (s *Service) notifyAdminsAccountDeparture(ctx context.Context, account *models.Account, notifType string) {
	if account == nil {
		return
	}
	admins, err := s.store.GetAdmins(ctx)
	if err != nil {
		slog.Warn("app: failed to list admins for account departure", "error", err)
		return
	}
	for _, admin := range admins {
		if admin.ID == account.ID {
			continue
		}
		fromID := account.ID
		if notifType == "admin.account_deleted" {
			// The account row will be purged, so keep the notification attached
			// to its recipient. The queued email still names the departing user.
			fromID = admin.ID
		}
		notif := &models.Notification{Type: notifType, AccountID: admin.ID, FromAccountID: fromID}
		if err := s.store.CreateNotification(ctx, notif); err != nil {
			slog.Warn("app: failed to create account departure notification", "admin", admin.ID, "error", err)
			continue
		}
		s.streamNotification(ctx, notif)
		mail.EnqueueEmailNotification(ctx, s.store, s.domain, &models.Notification{
			ID: notif.ID, Type: notif.Type, AccountID: admin.ID, FromAccountID: account.ID,
		})
	}
}

func HandleAccountDeletionDeliveryJob(store storage.Storage, federator *federation.Federator, mediaStore objectstore.Store) func(context.Context, string) error {
	return func(ctx context.Context, body string) error {
		var payload models.AccountDeletionDeliveryPayload
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return fmt.Errorf("decode account deletion delivery: %w", err)
		}
		run, destination, err := store.GetAccountDeletionDelivery(ctx, payload.RunID, payload.DestinationID)
		if err != nil {
			// A completed run removes its destination snapshot; its old terminal
			// job is therefore already satisfied.
			if completed, getErr := store.GetAccountDeletionRun(ctx, payload.RunID); getErr == nil && completed.State == "complete" {
				return nil
			}
			return err
		}
		if destination.DeliveredAt == nil {
			activity, err := json.Marshal(map[string]any{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       run.ActorURI + "#delete", "type": "Delete",
				"actor": run.ActorURI, "object": run.ActorURI,
				"to": []string{"https://www.w3.org/ns/activitystreams#Public"},
			})
			if err != nil {
				return err
			}
			if federator == nil {
				return fmt.Errorf("account deletion federator is unavailable")
			}
			if err := federator.DeliverRemovalRaw(ctx, destination.Inbox, activity, run.ActorURI); err != nil {
				return err
			}
			if err := store.MarkAccountDeletionDelivered(ctx, run.ID, destination.Inbox); err != nil {
				return err
			}
		}
		return finalizeAccountDeletion(ctx, store, mediaStore, run.ID, false)
	}
}

func HandleAccountDeletionFinalizeJob(store storage.Storage, mediaStore objectstore.Store) func(context.Context, string) error {
	return func(ctx context.Context, body string) error {
		var payload struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return fmt.Errorf("decode account deletion finalizer: %w", err)
		}
		return finalizeAccountDeletion(ctx, store, mediaStore, payload.RunID, true)
	}
}

func finalizeAccountDeletion(ctx context.Context, store storage.Storage, mediaStore objectstore.Store, runID string, deadlineJob bool) error {
	run, err := store.GetAccountDeletionRun(ctx, runID)
	if err != nil || run.State == "complete" {
		return err
	}
	if run.DeliveredCount < run.DestinationCount && (!deadlineJob || time.Now().Before(run.DeadlineAt)) {
		return nil
	}
	media, err := store.GetMediaForAccount(ctx, run.AccountID)
	if err != nil {
		return err
	}
	if mediaStore == nil && len(media) > 0 {
		return fmt.Errorf("account deletion media store is unavailable")
	}
	deleted := make(map[string]bool, len(media)*2)
	exports, err := store.ListAccountExports(ctx, run.AccountID)
	if err != nil {
		return err
	}
	for _, export := range exports {
		if export.StorageKey != "" {
			if mediaStore == nil {
				return fmt.Errorf("account deletion media store is unavailable")
			}
			if err := mediaStore.Delete(ctx, export.StorageKey); err != nil {
				return fmt.Errorf("delete account export: %w", err)
			}
		}
	}
	// Account imports have no separate download step, so a completed or
	// abandoned attempt's staged archive is normally already deleted; this
	// only cleans up one left behind by a run that failed every retry (see
	// HandleAccountImportJob).
	imports, err := store.ListAccountImports(ctx, run.AccountID)
	if err != nil {
		return err
	}
	for _, imp := range imports {
		if mediaStore == nil {
			return fmt.Errorf("account deletion media store is unavailable")
		}
		if err := mediaStore.Delete(ctx, accountImportKey(run.AccountID, imp.ID)); err != nil {
			return fmt.Errorf("delete account import: %w", err)
		}
	}
	for _, attachment := range media {
		for _, key := range []string{attachment.StorageKey, attachment.PreviewStorageKey} {
			if key == "" || deleted[key] {
				continue
			}
			if err := mediaStore.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete owned media: %w", err)
			}
			deleted[key] = true
		}
	}
	if err := store.CompleteAccountDeletion(ctx, runID); err != nil && !errors.Is(err, storage.ErrAccountDeletionPending) {
		return err
	}
	return nil
}

// NotifyAdminsSignUp sends Mastodon's "admin.sign_up" notification to every
// administrator when someone registers. Administrators are already emailed
// about new accounts, so this adds the in-app notification only.
func (s *Service) NotifyAdminsSignUp(ctx context.Context, account *models.Account) {
	if account == nil {
		return
	}
	admins, err := s.store.GetAdmins(ctx)
	if err != nil {
		slog.Warn("app: failed to list admins for sign-up", "error", err)
		return
	}
	for _, admin := range admins {
		if admin.ID == account.ID {
			continue
		}
		notif := &models.Notification{Type: "admin.sign_up", AccountID: admin.ID, FromAccountID: account.ID}
		created, err := s.store.CreateNotificationOnce(ctx, notif, nil)
		if err != nil {
			slog.Warn("app: failed to create sign-up notification", "admin", admin.ID, "error", err)
			continue
		}
		if created {
			s.streamNotification(ctx, notif)
		}
	}
}
