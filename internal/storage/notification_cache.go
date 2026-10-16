package storage

import (
	"context"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Notification caching on cachingStorage.
//
// The unread count runs on every authenticated web page render and the list
// head is the first thing both front ends ask for, so both are cached here.
// Notifications are per-recipient (never shared between viewers), so entries
// are keyed by recipient account and invalidated by the notification writes
// that flow through this same interface. Only the head page (maxID == nil) of
// the list is cached - deeper pagination is rare and always live. Block/mute
// writes drop the cached lists (the list query filters on them) but not the
// counts (the count query does not); suspension-driven visibility changes are
// covered by the entity TTL backstop.

// keys

func keyNotifCount(accountID string, excludeTypes []string) string {
	return "notifcount:" + accountID + ":" + strings.Join(excludeTypes, ",")
}

// notifHeadLimit is the size of the cached head window. Requests for any
// limit up to this are served by slicing one shared entry, so the web UI
// (20) and Mastodon apps (typically 15-40, varying per app) all hit the same
// cached window instead of missing on their private limit value.
const notifHeadLimit = 40

func keyNotifList(accountID string, excludeTypes []string) string {
	return "notiflist:" + accountID + ":" + strings.Join(excludeTypes, ",")
}

// copies and sizes

func copyNotifications(in []*models.Notification) []*models.Notification {
	if in == nil {
		return nil
	}
	out := make([]*models.Notification, len(in))
	for i, n := range in {
		if n == nil {
			continue
		}
		cp := *n
		if n.StatusID != nil {
			sid := *n.StatusID
			cp.StatusID = &sid
		}
		out[i] = &cp
	}
	return out
}

func notificationsSize(in []*models.Notification) int {
	n := 32
	for _, notif := range in {
		if notif != nil {
			n += 160 + len(notif.ID) + len(notif.Type) + len(notif.AccountID) + len(notif.FromAccountID)
		}
	}
	return n
}

// invalidation

// invalidateNotifications drops the cached unread counts and list heads for one
// recipient (all excludeTypes/limit variants).
func (cs *cachingStorage) invalidateNotifications(accountID string) {
	cs.c.DeletePrefix("notifcount:" + accountID + ":")
	cs.c.DeletePrefix("notiflist:" + accountID + ":")
}

// invalidateNotificationLists drops only the cached list heads for one
// recipient; used by block/mute writes, which change list visibility but not
// the unread count (the count query does not filter on blocks/mutes).
func (cs *cachingStorage) invalidateNotificationLists(accountID string) {
	cs.c.DeletePrefix("notiflist:" + accountID + ":")
}

// cached reads

func (cs *cachingStorage) CountUnreadNotifications(ctx context.Context, accountID string) (int, error) {
	key := keyNotifCount(accountID, nil)
	if v, ok := cs.c.Get(key); ok {
		return v.(int), nil
	}
	count, err := cs.Storage.CountUnreadNotifications(ctx, accountID)
	if err != nil {
		return 0, err
	}
	cs.c.Set(key, count, 64+len(key), cs.ttl)
	return count, nil
}

func (cs *cachingStorage) CountUnreadNotificationsFiltered(ctx context.Context, accountID string, excludeTypes []string) (int, error) {
	key := keyNotifCount(accountID, excludeTypes)
	if v, ok := cs.c.Get(key); ok {
		return v.(int), nil
	}
	count, err := cs.Storage.CountUnreadNotificationsFiltered(ctx, accountID, excludeTypes)
	if err != nil {
		return 0, err
	}
	cs.c.Set(key, count, 64+len(key), cs.ttl)
	return count, nil
}

func (cs *cachingStorage) ListNotifications(ctx context.Context, accountID string, maxID *string, limit int, excludeTypes []string) ([]*models.Notification, error) {
	if maxID != nil || limit > notifHeadLimit {
		// Deeper pagination and oversized pages are rare; always live.
		return cs.Storage.ListNotifications(ctx, accountID, maxID, limit, excludeTypes)
	}
	key := keyNotifList(accountID, excludeTypes)
	if v, ok := cs.c.Get(key); ok {
		head := v.([]*models.Notification)
		if len(head) > limit {
			head = head[:limit]
		}
		return copyNotifications(head), nil
	}
	version := cs.c.Version()
	notifs, err := cs.Storage.ListNotifications(ctx, accountID, nil, notifHeadLimit, excludeTypes)
	if err != nil {
		return nil, err
	}
	pristine := copyNotifications(notifs)
	cs.c.SetIfVersion(version, key, pristine, notificationsSize(pristine)+len(key), cs.ttl)
	if len(notifs) > limit {
		notifs = notifs[:limit]
	}
	return notifs, nil
}

func (cs *cachingStorage) SetMarker(ctx context.Context, accountID, timeline, lastReadID string) (*models.Marker, error) {
	marker, err := cs.Storage.SetMarker(ctx, accountID, timeline, lastReadID)
	if err == nil && timeline == "notifications" {
		cs.invalidateNotifications(accountID)
	}
	return marker, err
}

// invalidating writes

func (cs *cachingStorage) CreateNotification(ctx context.Context, notif *models.Notification) error {
	err := cs.Storage.CreateNotification(ctx, notif)
	if err == nil && notif != nil {
		cs.invalidateNotifications(notif.AccountID)
	}
	return err
}

func (cs *cachingStorage) CreateNotificationOnce(ctx context.Context, notif *models.Notification, jobs []*models.Job) (bool, error) {
	created, err := cs.Storage.CreateNotificationOnce(ctx, notif, jobs)
	if err == nil && created && notif != nil {
		cs.invalidateNotifications(notif.AccountID)
	}
	return created, err
}

func (cs *cachingStorage) UpsertNotification(ctx context.Context, notif *models.Notification) error {
	err := cs.Storage.UpsertNotification(ctx, notif)
	if err == nil && notif != nil {
		cs.invalidateNotifications(notif.AccountID)
	}
	return err
}

func (cs *cachingStorage) MarkNotificationRead(ctx context.Context, id string) error {
	// Resolve the recipient first so the invalidation can be precise; if the
	// lookup fails, fall back to dropping every recipient's notification keys.
	var accountID string
	if notif, err := cs.Storage.GetNotification(ctx, id); err == nil && notif != nil {
		accountID = notif.AccountID
	}
	err := cs.Storage.MarkNotificationRead(ctx, id)
	if err == nil {
		if accountID != "" {
			cs.invalidateNotifications(accountID)
		} else {
			cs.c.DeletePrefix("notifcount:")
			cs.c.DeletePrefix("notiflist:")
		}
	}
	return err
}

func (cs *cachingStorage) MarkNotificationsRead(ctx context.Context, accountID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	err := cs.Storage.MarkNotificationsRead(ctx, accountID, ids)
	if err == nil {
		cs.invalidateNotifications(accountID)
	}
	return err
}

func (cs *cachingStorage) DeleteNotification(ctx context.Context, accountID, id string) error {
	err := cs.Storage.DeleteNotification(ctx, accountID, id)
	if err == nil {
		cs.invalidateNotifications(accountID)
	}
	return err
}

func (cs *cachingStorage) MarkAllNotificationsRead(ctx context.Context, accountID string) error {
	err := cs.Storage.MarkAllNotificationsRead(ctx, accountID)
	if err == nil {
		cs.invalidateNotifications(accountID)
	}
	return err
}

func (cs *cachingStorage) DeleteAllNotifications(ctx context.Context, accountID string) error {
	err := cs.Storage.DeleteAllNotifications(ctx, accountID)
	if err == nil {
		cs.invalidateNotifications(accountID)
	}
	return err
}

func (cs *cachingStorage) DeleteNotifications(ctx context.Context, accountID, fromAccountID string, notifType string) error {
	err := cs.Storage.DeleteNotifications(ctx, accountID, fromAccountID, notifType)
	if err == nil {
		cs.invalidateNotifications(accountID)
	}
	return err
}

func (cs *cachingStorage) DeleteNotificationsForStatus(ctx context.Context, statusID string) error {
	err := cs.Storage.DeleteNotificationsForStatus(ctx, statusID)
	if err == nil {
		// Recipients are unknown here and the operation is rare (status
		// deletion): drop every recipient's notification keys.
		cs.c.DeletePrefix("notifcount:")
		cs.c.DeletePrefix("notiflist:")
	}
	return err
}

func (cs *cachingStorage) CreateBlock(ctx context.Context, block *models.Block) error {
	err := cs.Storage.CreateBlock(ctx, block)
	if err == nil && block != nil {
		cs.invalidateNotificationLists(block.AccountID)
	}
	return err
}

func (cs *cachingStorage) DeleteBlock(ctx context.Context, accountID, targetID string) error {
	err := cs.Storage.DeleteBlock(ctx, accountID, targetID)
	if err == nil {
		cs.invalidateNotificationLists(accountID)
	}
	return err
}

func (cs *cachingStorage) CreateMute(ctx context.Context, mute *models.Mute) error {
	err := cs.Storage.CreateMute(ctx, mute)
	if err == nil && mute != nil {
		cs.invalidateNotificationLists(mute.AccountID)
	}
	return err
}

func (cs *cachingStorage) DeleteMute(ctx context.Context, accountID, targetID string) error {
	err := cs.Storage.DeleteMute(ctx, accountID, targetID)
	if err == nil {
		cs.invalidateNotificationLists(accountID)
	}
	return err
}
