package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Notification Endpoints

// NotificationsList returns the user's notifications.
// GET /api/v1/notifications
func (h *Handler) NotificationsList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)

	excludeTypes := r.URL.Query()["exclude_types[]"]
	if len(excludeTypes) == 0 {
		excludeTypes = r.URL.Query()["exclude_types"]
	}

	// Honor since_id/min_id: marker-based apps poll "what arrived after my
	// last-read notification" to compute the unread badge. Ignoring the
	// cursor returned the whole first page, so everything always looked
	// unread again seconds after marking as read.
	var notifs []*models.Notification
	var err error
	if cursor := params.SinceID; cursor != nil || params.MinID != nil {
		if cursor == nil {
			cursor = params.MinID
		}
		notifs, err = h.store.ListNotificationsSince(r.Context(), account.ID, *cursor, params.Limit, excludeTypes)
	} else {
		notifs, err = h.store.ListNotifications(r.Context(), account.ID, params.MaxID, params.Limit, excludeTypes)
	}
	if err != nil {
		slog.Error("api: notifications error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching notifications")
		return
	}

	result, err := h.notificationsToMastodonChecked(r.Context(), notifs, account)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering notifications")
		return
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// NotificationsUnreadCount returns the badge count without downloading a page
// of notifications, which is how clients poll it.
// GET /api/v1/notifications/unread_count
func (h *Handler) NotificationsUnreadCount(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	excludeTypes := r.URL.Query()["exclude_types[]"]
	if len(excludeTypes) == 0 {
		excludeTypes = r.URL.Query()["exclude_types"]
	}
	count, err := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, excludeTypes)
	if err != nil {
		slog.Error("api: unread notification count", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error counting notifications")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]interface{}{"count": count})
}

// NotificationGet returns a single notification.
// GET /api/v1/notifications/{id}
func (h *Handler) NotificationGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	notif, err := h.store.GetNotification(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}

	if notif.AccountID != account.ID {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}

	result, err := h.notificationsToMastodonChecked(r.Context(), []*models.Notification{notif}, account)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering notification")
		return
	}
	if len(result) == 0 {
		writeAPIError(w, http.StatusNotFound, "notification data error")
		return
	}

	writeAPIJSON(w, http.StatusOK, result[0])
}

// NotificationsClear clears all notifications.
// POST /api/v1/notifications/clear
func (h *Handler) NotificationsClear(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Mastodon clients have no portable read-state field: a notification that
	// remains in this endpoint's list is treated as active again on the next
	// poll. Clearing must therefore remove the rows, matching Mastodon's dismiss
	// semantics and the web UI's clear action.
	if err := h.store.DeleteAllNotifications(r.Context(), account.ID); err != nil {
		slog.Error("api: clear notifications error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error clearing notifications")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// NotificationDismiss dismisses a single notification.
// POST /api/v1/notifications/{id}/dismiss
func (h *Handler) NotificationDismiss(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	// Mastodon's dismiss operation removes the notification from subsequent
	// list responses. Keeping the row with read=true makes clients rediscover
	// it on their next poll because the v1 representation has no portable read
	// filtering semantics.
	if err := h.store.DeleteNotification(r.Context(), account.ID, id); err != nil {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// GroupedNotificationsList implements Mastodon's v2 grouped-notifications
// response. littleFedi currently exposes each event as a one-item group. This
// preserves the complete API semantics and stable group identity without
// pretending that independently dismissible rows form one persisted group.
// GET /api/v2/notifications
func (h *Handler) GroupedNotificationsList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)
	excludeTypes := r.URL.Query()["exclude_types[]"]
	if len(excludeTypes) == 0 {
		excludeTypes = r.URL.Query()["exclude_types"]
	}
	var notifs []*models.Notification
	var err error
	if cursor := params.SinceID; cursor != nil || params.MinID != nil {
		if cursor == nil {
			cursor = params.MinID
		}
		notifs, err = h.store.ListNotificationsSince(r.Context(), account.ID, *cursor, params.Limit, excludeTypes)
	} else {
		notifs, err = h.store.ListNotifications(r.Context(), account.ID, params.MaxID, params.Limit, excludeTypes)
	}
	if err != nil {
		slog.Error("api: grouped notifications error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching notifications")
		return
	}

	converted, err := h.notificationsToMastodonChecked(r.Context(), notifs, account)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering notifications")
		return
	}
	accounts := make([]*MastodonAccount, 0, len(converted))
	statuses := make([]*MastodonStatus, 0, len(converted))
	groups := make([]map[string]interface{}, 0, len(converted))
	seenAccounts := make(map[string]struct{}, len(converted))
	seenStatuses := make(map[string]struct{}, len(converted))
	for _, n := range converted {
		if n.Account != nil {
			if _, ok := seenAccounts[n.Account.ID]; !ok {
				seenAccounts[n.Account.ID] = struct{}{}
				accounts = append(accounts, n.Account)
			}
		}
		if n.Status != nil {
			if _, ok := seenStatuses[n.Status.ID]; !ok {
				seenStatuses[n.Status.ID] = struct{}{}
				statuses = append(statuses, n.Status)
			}
		}
		mostRecentID, _ := strconv.ParseInt(n.ID, 10, 64)
		group := map[string]interface{}{
			"group_key":                   "ungrouped-" + n.ID,
			"notifications_count":         1,
			"type":                        n.Type,
			"most_recent_notification_id": mostRecentID,
			"page_min_id":                 n.ID,
			"page_max_id":                 n.ID,
			"latest_page_notification_at": n.CreatedAt,
			"sample_account_ids":          []string{n.Account.ID},
		}
		if n.Status != nil {
			group["status_id"] = n.Status.ID
		}
		groups = append(groups, group)
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"accounts":            accounts,
		"statuses":            statuses,
		"notification_groups": groups,
	})
}

// NotificationPolicyGet returns littleFedi's current notification policy.
// Policy-based request filtering is not enabled, so all ordinary sources are
// accepted and private mentions retain Mastodon's safe default.
// GET /api/v2/notifications/policy
func (h *Handler) NotificationPolicyGet(w http.ResponseWriter, r *http.Request) {
	if getAuthAccount(r.Context()) == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"for_not_following":    "accept",
		"for_not_followers":    "accept",
		"for_new_accounts":     "accept",
		"for_private_mentions": "drop",
		"for_limited_accounts": "filter",
		"summary": map[string]int{
			"pending_requests_count":      0,
			"pending_notifications_count": 0,
		},
	})
}

// Conversion Helpers

// notificationsToMastodon converts notifications to API objects with a
// constant number of queries: all actor accounts at once, all target statuses
// at once, then a single batched EnrichStatuses over the deduplicated status
// set (an N+1 avoided - it hurts on low-power devices). Notifications whose
// actor account is missing are skipped, matching previous behaviour.
func (h *Handler) notificationsToMastodon(ctx context.Context, notifs []*models.Notification, currentAccount *models.Account) []*MastodonNotification {
	result, _ := h.notificationsToMastodonChecked(ctx, notifs, currentAccount)
	return result
}

func (h *Handler) notificationsToMastodonChecked(ctx context.Context, notifs []*models.Notification, currentAccount *models.Account) ([]*MastodonNotification, error) {
	accountIDs := make([]string, 0, len(notifs))
	statusIDs := make([]string, 0, len(notifs))
	seenAccounts := make(map[string]struct{}, len(notifs))
	seenStatuses := make(map[string]struct{}, len(notifs))
	for _, n := range notifs {
		if _, seen := seenAccounts[n.FromAccountID]; !seen {
			seenAccounts[n.FromAccountID] = struct{}{}
			accountIDs = append(accountIDs, n.FromAccountID)
		}
		if n.StatusID != nil && *n.StatusID != "" {
			if _, seen := seenStatuses[*n.StatusID]; !seen {
				seenStatuses[*n.StatusID] = struct{}{}
				statusIDs = append(statusIDs, *n.StatusID)
			}
		}
	}
	accounts, _ := h.store.GetAccountsByIDs(ctx, accountIDs)
	statuses, _ := h.store.GetStatusesByIDs(ctx, statusIDs)

	statusList := make([]*models.Status, 0, len(statuses))
	for id, st := range statuses {
		if !h.app.CanViewStatus(ctx, st, currentAccount) {
			delete(statuses, id)
			continue
		}
		statusList = append(statusList, st)
	}
	h.app.EnrichStatuses(ctx, statusList, currentAccount)
	statusList, err := h.app.FilterStatuses(ctx, "notifications", statusList, currentAccount)
	if err != nil {
		return nil, err
	}
	// Enrichment may replace slice elements (component-cache hits are
	// copies), so rebuild the lookup map from the enriched slice.
	clear(statuses)
	for _, st := range statusList {
		statuses[st.ID] = st
	}

	result := make([]*MastodonNotification, 0, len(notifs))
	for _, n := range notifs {
		fromAccount := accounts[n.FromAccountID]
		if fromAccount == nil {
			slog.Warn("api: notification from_account not found", "id", n.FromAccountID)
			continue
		}

		mn := &MastodonNotification{
			ID:        strconv.FormatInt(n.APIID, 10),
			Type:      n.Type,
			CreatedAt: mastodonTimestamp(n.CreatedAt),
			Account:   h.accountToMastodon(fromAccount, currentAccount, false),
			Read:      n.Read,
			Emoji:     n.Emoji,
		}
		if n.Type == "emoji_reaction" {
			mn.Type = "pleroma:emoji_reaction"
		}
		if kind, importID := models.ParseFollowImportNotificationType(n.Type); importID != "" {
			mn.Type = kind
			mn.URL = "https://" + h.domain + "/settings/import?import_id=" + url.QueryEscape(importID)
		}

		if n.StatusID != nil && *n.StatusID != "" {
			status := statuses[*n.StatusID]
			if status == nil {
				continue
			}
			mn.Status = h.statusToMastodon(status, currentAccount)
		}

		result = append(result, mn)
	}

	return result, nil
}
