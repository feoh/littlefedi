package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

var htmlTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

// EnqueuePushNotification checks every push subscription the account holds
// (one per logged-in app or web/PWA session) and enqueues a
// push delivery job for each one whose alert preferences allow this
// notification type, so a user with several apps installed gets paged on
// all of them.
func EnqueuePushNotification(ctx context.Context, store storage.Storage, notif *models.Notification) {
	// The notification row already exists, so request preemption must not leave
	// it without its durable delivery jobs.
	ctx = context.WithoutCancel(ctx)
	jobs, err := NotificationJobs(ctx, store, notif)
	if err != nil {
		slog.Error("push: build notification jobs", "error", err)
		return
	}
	if err := store.EnqueueJobs(ctx, jobs); err != nil {
		slog.Error("push: enqueue jobs", "error", err)
	}
}

// NotificationJobs builds every durable push job owed by notif. Storage
// errors are returned so an atomic notification writer can retry the whole
// operation without leaving the UI row ahead of its push delivery.
func NotificationJobs(ctx context.Context, store storage.Storage, notif *models.Notification) ([]*models.Job, error) {
	// Only local accounts can have push subscriptions
	if notif.AccountID == "" {
		return nil, nil
	}

	subs, err := store.ListPushSubscriptionsByAccountID(ctx, notif.AccountID)
	if err != nil {
		return nil, fmt.Errorf("push: list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil, nil
	}
	hide, warn, err := filterPushStatus(ctx, store, notif.AccountID, notif.StatusID)
	if err != nil {
		return nil, err
	}
	if hide {
		return nil, nil
	}

	// Get the account that triggered the notification
	fromAccount, err := store.GetAccount(ctx, notif.FromAccountID)
	if err != nil {
		return nil, fmt.Errorf("push: load source account: %w", err)
	}

	jobs := make([]*models.Job, 0, len(subs))
	for _, sub := range subs {
		if !shouldNotify(sub, notif.Type) {
			continue
		}
		allowed, err := policyAllows(ctx, store, sub, notif.FromAccountID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}

		jobPayload := PushJobPayload{
			SubscriptionID:  sub.ID,
			NotifType:       notif.Type,
			NotifID:         notif.ID,
			TargetAccountID: notif.AccountID,
			FromAccountID:   notif.FromAccountID,
			FromUsername:    fromAccount.Username,
			FromDisplay:     fromAccount.DisplayName,
			FromAvatar:      fromAccount.Avatar,
			StatusID:        notif.StatusID,
			Emoji:           notif.Emoji,
			SuppressBody:    warn,
		}

		payloadBytes, err := json.Marshal(jobPayload)
		if err != nil {
			return nil, fmt.Errorf("push: marshal job payload: %w", err)
		}

		jobs = append(jobs, &models.Job{
			Kind:        "push_notification",
			Payload:     string(payloadBytes),
			RunAt:       notif.CreatedAt, // send immediately
			MaxAttempts: 5,
			State:       "queued",
		})
	}
	return jobs, nil
}

// EnqueuePushNotifications bulk-loads subscriptions and source accounts, then
// inserts all eligible jobs in one queue transaction.
func EnqueuePushNotifications(ctx context.Context, store storage.Storage, notifs []*models.Notification) {
	// The notification rows already exist, so finish the short queue write even
	// if the originating request or worker is preempted.
	ctx = context.WithoutCancel(ctx)
	accountIDs := make([]string, 0, len(notifs))
	fromIDs := make([]string, 0, len(notifs))
	for _, notif := range notifs {
		if notif != nil && notif.AccountID != "" {
			accountIDs = append(accountIDs, notif.AccountID)
			fromIDs = append(fromIDs, notif.FromAccountID)
		}
	}
	subsByAccount, err := store.ListPushSubscriptionsByAccountIDs(ctx, accountIDs)
	if err != nil {
		slog.Warn("push: list subscriptions for batch", "error", err)
		return
	}
	fromAccounts, err := store.GetAccountsByIDs(ctx, fromIDs)
	if err != nil {
		slog.Warn("push: list source accounts for batch", "error", err)
		return
	}
	jobs := make([]*models.Job, 0)
	for _, notif := range notifs {
		if notif == nil {
			continue
		}
		hide, warn, err := filterPushStatus(ctx, store, notif.AccountID, notif.StatusID)
		if err != nil || hide {
			continue
		}
		from := fromAccounts[notif.FromAccountID]
		if from == nil {
			continue
		}
		for _, sub := range subsByAccount[notif.AccountID] {
			if !shouldNotify(sub, notif.Type) {
				continue
			}
			allowed, err := policyAllows(ctx, store, sub, notif.FromAccountID)
			if err != nil {
				slog.Warn("push: check source policy", "error", err)
				continue
			}
			if !allowed {
				continue
			}
			payload, err := json.Marshal(PushJobPayload{
				SubscriptionID: sub.ID, NotifType: notif.Type, NotifID: notif.ID,
				TargetAccountID: notif.AccountID, FromAccountID: notif.FromAccountID,
				FromUsername: from.Username, FromDisplay: from.DisplayName, FromAvatar: from.Avatar,
				StatusID: notif.StatusID, Emoji: notif.Emoji, SuppressBody: warn,
			})
			if err != nil {
				continue
			}
			jobs = append(jobs, &models.Job{
				Kind: "push_notification", Payload: string(payload), RunAt: notif.CreatedAt,
				MaxAttempts: 5, State: "queued",
			})
		}
	}
	if len(jobs) > 0 {
		if err := store.EnqueueJobs(ctx, jobs); err != nil {
			slog.Error("push: enqueue batch", "jobs", len(jobs), "error", err)
		}
	}
}

// ValidPolicy reports whether a push source policy is supported.
func ValidPolicy(policy string) bool {
	return policy == "all" || policy == "followed" || policy == "follower" || policy == "none"
}

// policyAllows checks which accounts may trigger a subscription. Unknown
// policies fail closed so a malformed value cannot broaden notification scope.
func policyAllows(ctx context.Context, store storage.Storage, sub *models.PushSubscription, fromAccountID string) (bool, error) {
	switch sub.Policy {
	case "all":
		return true, nil
	case "none":
		return false, nil
	case "followed":
		following, err := store.IsFollowing(ctx, sub.AccountID, fromAccountID)
		if err != nil {
			return false, fmt.Errorf("push: check followed policy: %w", err)
		}
		return following, nil
	case "follower":
		// Mastodon's "follower" policy: only accounts that follow the recipient.
		follower, err := store.IsFollowing(ctx, fromAccountID, sub.AccountID)
		if err != nil {
			return false, fmt.Errorf("push: check follower policy: %w", err)
		}
		return follower, nil
	default:
		return false, nil
	}
}

// PushJobPayload is the payload for a push notification delivery job.
type PushJobPayload struct {
	SubscriptionID  string  `json:"subscription_id"`
	NotifType       string  `json:"notif_type"`
	NotifID         string  `json:"notif_id"`
	TargetAccountID string  `json:"target_account_id"`
	FromAccountID   string  `json:"from_account_id"`
	FromUsername    string  `json:"from_username"`
	FromDisplay     string  `json:"from_display"`
	FromAvatar      string  `json:"from_avatar"`
	StatusID        *string `json:"status_id,omitempty"`
	Emoji           string  `json:"emoji,omitempty"`
	SuppressBody    bool    `json:"suppress_body,omitempty"`
}

// shouldNotify checks whether the subscription's alert preferences allow
// sending a push for the given notification type.
func shouldNotify(sub *models.PushSubscription, notifType string) bool {
	// An app subscription speaks the Mastodon API: its app has no way to show
	// littleFedi's own notification types. The web UI's subscriptions do.
	if sub.OAuthTokenID != nil && !models.IsMastodonNotificationType(notifType) {
		return false
	}
	switch notifType {
	case "mention":
		return sub.NotifyMention
	case "follow", "unfollow", "block", "unblock", "move", "follow_accept", "follow_reject":
		return sub.NotifyFollow
	case "reblog":
		return sub.NotifyReblog
	case "favourite", "emoji_reaction":
		return sub.NotifyFavourite
	case "poll":
		return sub.NotifyPoll
	case "follow_request":
		return sub.NotifyFollowRequest
	case "status":
		return sub.NotifyStatus
	case "quote":
		return sub.NotifyQuote
	case "blog.rebuilt":
		return sub.NotifyBlogRebuild
	default:
		return false
	}
}

// HandlePushJob processes a push notification job.
// This is the worker handler registered in main.go.
func HandlePushJob(ctx context.Context, store storage.Storage, sender *Sender, domain string) func(ctx context.Context, payload string) error {
	return func(ctx context.Context, payload string) error {
		var jobPayload PushJobPayload
		if err := json.Unmarshal([]byte(payload), &jobPayload); err != nil {
			return err
		}

		// Get the subscription
		sub, err := store.GetPushSubscription(ctx, jobPayload.SubscriptionID)
		if err != nil {
			// Subscription was deleted, job is no longer relevant
			slog.Warn("push: subscription not found, skipping", "id", jobPayload.SubscriptionID)
			return nil
		}

		// Verify the subscription still belongs to the same account
		if sub.AccountID != jobPayload.TargetAccountID {
			slog.Warn("push: subscription account mismatch, skipping")
			return nil
		}

		// Clients fetch the notification by its API ID; one that has since
		// been dismissed or deleted is not worth announcing.
		notification, err := store.GetNotification(ctx, jobPayload.NotifID)
		if err != nil || notification == nil {
			slog.Debug("push: notification no longer exists, skipping", "id", jobPayload.NotifID)
			return nil
		}

		notifPayload := buildNotificationPayload(ctx, store, jobPayload, domain)
		notifPayload.NotificationID = notification.APIID
		if sub.AccessToken != "" {
			token, err := keycrypt.Open(sub.AccessToken)
			if err != nil {
				return fmt.Errorf("push: open subscription access token: %w", err)
			}
			notifPayload.AccessToken = token
		}

		subData := &Subscription{
			Endpoint: sub.Endpoint,
			P256DH:   sub.P256DH,
			Auth:     sub.Auth,
			Standard: sub.Standard,
		}

		if err := sender.Send(subData, notifPayload, "normal"); err != nil {
			if errors.Is(err, ErrSubscriptionExpired) {
				// Subscription is invalid, clean it up
				slog.Info("push: subscription expired, removing", "account_id", sub.AccountID)
				if delErr := store.DeletePushSubscription(ctx, sub.ID); delErr != nil {
					slog.Error("push: failed to delete expired subscription", "error", delErr)
				}
				return nil // Don't retry expired subscriptions
			}
			if errors.Is(err, ErrRateLimited) {
				// Retry with backoff
				return err
			}
			return err
		}

		slog.Debug("push: notification sent", "type", jobPayload.NotifType, "to", jobPayload.TargetAccountID)
		return nil
	}
}

func buildNotificationPayload(ctx context.Context, store storage.Storage, job PushJobPayload, domain string) *NotificationPayload {
	body, statusURL := notificationBodyAndStatusURL(ctx, store, job, domain)
	if job.SuppressBody {
		body = ""
	}
	clickURL := statusURL
	if clickURL == "" {
		clickURL = "https://" + domain + "/notifications"
	}
	// The rebuild notification has no status; the useful destination is the
	// regenerated site itself (the account is its own notification actor).
	if job.NotifType == "blog.rebuilt" && job.FromUsername != "" {
		clickURL = "https://" + domain + "/@" + job.FromUsername + "/blog"
	}
	accountAcct := job.FromUsername
	if accountAcct != "" && !strings.Contains(accountAcct, "@") {
		if fromAccount, err := store.GetAccount(ctx, job.FromAccountID); err == nil && fromAccount.Domain != nil && *fromAccount.Domain != "" {
			accountAcct = fromAccount.Username + "@" + *fromAccount.Domain
		}
	}
	accountName := job.FromDisplay
	if accountName == "" {
		accountName = job.FromUsername
	}
	locale := i18n.DefaultLocale
	if recipient, err := store.GetAccount(ctx, job.TargetAccountID); err == nil && i18n.IsSupported(recipient.Locale) {
		locale = recipient.Locale
	}
	// Mastodon always sends an icon, and some clients reject a payload
	// without one.
	icon := absoluteURL(domain, job.FromAvatar)
	if icon == "" {
		icon = "https://" + domain + "/avatars/original/missing.png"
	}
	return &NotificationPayload{
		Title:                    notificationTitle(locale, job),
		Body:                     body,
		Icon:                     icon,
		Badge:                    "",
		Tag:                      "littlefedi:" + job.NotifID,
		URL:                      clickURL,
		Type:                     job.NotifType,
		NotificationType:         job.NotifType,
		PreferredLocale:          locale,
		AccountID:                job.FromAccountID,
		AccountAcct:              accountAcct,
		AccountName:              accountName,
		AccountAvatar:            absoluteURL(domain, job.FromAvatar),
		StatusID:                 stringPtrValue(job.StatusID),
		StatusURL:                statusURL,
		RecipientAccountID:       job.TargetAccountID,
		RecipientAccountUsername: "",
	}
}

func filterPushStatus(ctx context.Context, store storage.Storage, accountID string, statusID *string) (hide, warn bool, err error) {
	if statusID == nil || *statusID == "" {
		return false, false, nil
	}
	status, err := store.GetStatus(ctx, *statusID)
	if err != nil {
		return true, false, nil
	}
	if status.BoostOfID != nil {
		status.BoostOf, err = store.GetStatus(ctx, *status.BoostOfID)
		if err != nil {
			return true, false, nil
		}
	}
	if status.QuoteOfID != nil {
		status.QuoteOf, err = store.GetStatus(ctx, *status.QuoteOfID)
		if err != nil {
			return true, false, nil
		}
	}
	if status.BoostOf != nil && status.BoostOf.QuoteOfID != nil {
		status.BoostOf.QuoteOf, err = store.GetStatus(ctx, *status.BoostOf.QuoteOfID)
		if err != nil {
			return true, false, nil
		}
	}
	candidates := []*models.Status{status, status.BoostOf, status.QuoteOf}
	if status.BoostOf != nil {
		candidates = append(candidates, status.BoostOf.QuoteOf)
	}
	for _, candidate := range candidates {
		if candidate != nil {
			candidate.Poll, _ = store.GetPollForStatus(ctx, candidate.ID, accountID)
		}
	}
	filters, err := store.ListFilters(ctx, accountID)
	if err != nil {
		return false, false, err
	}
	filtered := statusfilter.Apply(filters, "notifications", []*models.Status{status}, time.Now())
	return len(filtered) == 0, len(filtered) == 1 && len(filtered[0].FilterMatches) > 0, nil
}

func notificationBodyAndStatusURL(ctx context.Context, store storage.Storage, job PushJobPayload, domain string) (string, string) {
	if job.StatusID == nil || *job.StatusID == "" {
		return "", ""
	}
	status, err := store.GetStatus(ctx, *job.StatusID)
	if err != nil && job.NotifType == "reblog" {
		return "", "https://" + domain + "/notifications"
	}
	if err != nil {
		return "", ""
	}
	target := status
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		if boosted, err := store.GetStatus(ctx, *status.BoostOfID); err == nil {
			target = boosted
		}
	}
	body := plainTextPreview(target.ContentHTML, 180)
	statusURL := target.URL
	if statusURL == "" {
		statusURL = target.URI
	}
	return body, absoluteURL(domain, statusURL)
}

func plainTextPreview(raw string, limit int) string {
	text := htmlTagPattern.ReplaceAllString(raw, " ")
	text = html.UnescapeString(text)
	text = strings.Join(strings.Fields(text), " ")
	if limit > 0 && len([]rune(text)) > limit {
		runes := []rune(text)
		text = string(runes[:limit-1]) + "…"
	}
	return text
}

func stringPtrValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func absoluteURL(domain, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return "https://" + domain + raw
}

// notificationTitle generates the title for a push notification.
func notificationTitle(locale string, job PushJobPayload) string {
	display := job.FromDisplay
	if display == "" {
		display = "@" + job.FromUsername
	}

	switch job.NotifType {
	case "mention":
		return display + " " + i18n.T(locale, "notif.type.mention")
	case "follow":
		return display + " " + i18n.T(locale, "notif.type.follow")
	case "reblog":
		return display + " " + i18n.T(locale, "notif.type.reblog")
	case "favourite":
		return display + " " + i18n.T(locale, "notif.type.favourite")
	case "emoji_reaction":
		return display + " " + i18n.T(locale, "notif.type.emoji_reaction", job.Emoji)
	case "quote":
		return display + " " + i18n.T(locale, "notif.type.quote")
	case "follow_request":
		return display + " " + i18n.T(locale, "notif.type.follow_request")
	case "move":
		return display + " " + i18n.T(locale, "notif.type.move")
	case "unfollow":
		return display + " " + i18n.T(locale, "notif.type.unfollow")
	case "block":
		return display + " " + i18n.T(locale, "notif.type.block")
	case "unblock":
		return display + " " + i18n.T(locale, "notif.type.unblock")
	case "follow_accept":
		return display + " " + i18n.T(locale, "notif.type.follow_accept")
	case "follow_reject":
		return display + " " + i18n.T(locale, "notif.type.follow_reject")
	case "poll":
		return i18n.T(locale, "push.poll_ended")
	case "status":
		return display + " " + i18n.T(locale, "notif.type.status")
	case "blog.rebuilt":
		// Self-triggered: prefixing the account's own display name would read
		// as if somebody else rebuilt their site.
		if job.StatusID != nil && *job.StatusID != "" {
			return i18n.T(locale, "push.blog_rebuilt_discussion")
		}
		return i18n.T(locale, "push.blog_rebuilt")
	default:
		return i18n.T(locale, "push.new_notification_from", display)
	}
}
