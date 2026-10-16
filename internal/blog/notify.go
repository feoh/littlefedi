package blog

import (
	"context"
	"log/slog"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// NotificationType is the notification type recorded after a background
// rebuild finishes.
const NotificationType = "blog.rebuilt"

// NotifyRebuilt records the "your site was regenerated" notification and, if
// the account opted in, queues its email. UpsertNotification collapses
// repeated rebuilds onto one row, so a busy publisher gets a single refreshed
// unread entry rather than one per post.
//
// The error is returned rather than only logged: on a loaded single-writer
// SQLite instance the insert can lose to a busy database right after a
// rebuild, and dropping it there leaves a regenerated site with no
// notification at all. Failing the job instead lets the queue retry it.
func NotifyRebuilt(ctx context.Context, store storage.Storage, webDomain string, req RebuildRequest) error {
	notif := &models.Notification{Type: NotificationType, AccountID: req.AccountID, FromAccountID: req.AccountID}
	if req.PostID != "" {
		notif.StatusID = &req.PostID
	}
	if err := store.UpsertNotification(ctx, notif); err != nil {
		slog.Warn("blog: record rebuild notification", "account", req.AccountID, "error", err)
		return err
	}
	push.EnqueuePushNotification(ctx, store, notif)
	mail.EnqueueEmailNotification(ctx, store, webDomain, notif)
	return nil
}
