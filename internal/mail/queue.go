package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	jobqueue "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// MaxSendAttempts is the MaxAttempts for send_email jobs. Three fit inside the
// queue's first two backoff steps, so a provider answering "452 4.4.5 Rate limit
// exceeded, try again later" saw all of them within a minute and the mail was
// dropped. Ten follow the same curve out to 1.4-2.8 hours, past a normal
// rate-limit window. Expiring messages, such as password resets, stop sooner at
// the expiry carried in their payload.
const MaxSendAttempts = 10

// EnqueueEmailVerification creates a one-use token and queues its email.
func EnqueueEmailVerification(ctx context.Context, store storage.Storage, webDomain string, account *models.Account) error {
	if account == nil || account.Email == "" {
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("mail: generate verification token: %w", err)
	}
	token := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	if err := store.CreateEmailVerification(ctx, &models.EmailVerification{
		AccountID: account.ID, Email: account.Email, TokenHash: hex.EncodeToString(hash[:]),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		return fmt.Errorf("mail: store verification token: %w", err)
	}
	return EnqueueEmail(ctx, store, EmailJobPayload{
		To:      account.Email,
		Subject: "Verify your littleFedi email address",
		Body: fmt.Sprintf("Hi @%s,\n\nVerify your email address:\n\nhttps://%s/auth/verify_email?token=%s\n\n- littleFedi",
			account.Username, webDomain, token),
	})
}

// EmailJobPayload is the payload for a send_email queue job.
type EmailJobPayload struct {
	To               string     `json:"to"`
	Subject          string     `json:"subject"`
	Body             string     `json:"body"`
	WebDomain        string     `json:"web_domain,omitempty"`
	AccountID        string     `json:"account_id,omitempty"`
	NotificationType string     `json:"notification_type,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// EnqueueEmail durably queues one email with the standard retry window.
func EnqueueEmail(ctx context.Context, store storage.Storage, payload EmailJobPayload) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return store.EnqueueJob(ctx, &models.Job{Kind: "send_email", Payload: string(b), MaxAttempts: MaxSendAttempts, State: "queued"})
}

// EnqueueEmailNotification checks if the recipient should get an email for
// the given notification and enqueues a send_email job if so.
func EnqueueEmailNotification(ctx context.Context, store storage.Storage, webDomain string, notif *models.Notification) {
	// The notification row already exists, so request preemption must not leave
	// it without its durable email job.
	ctx = context.WithoutCancel(ctx)
	job, err := NotificationJob(ctx, store, webDomain, notif)
	if err != nil {
		slog.Error("mail: build email notification job", "error", err)
		return
	}
	if job == nil {
		return
	}
	if err := store.EnqueueJob(ctx, job); err != nil {
		slog.Error("mail: enqueue email job", "error", err)
	}
}

// NotificationJob builds the durable email job owed by notif. A nil job means
// the recipient did not opt in; storage errors are returned so callers that
// commit notifications atomically can retry instead of dropping the email.
func NotificationJob(ctx context.Context, store storage.Storage, webDomain string, notif *models.Notification) (*models.Job, error) {
	if notif.AccountID == "" {
		return nil, nil
	}

	account, err := store.GetAccount(ctx, notif.AccountID)
	if err != nil {
		return nil, fmt.Errorf("mail: load recipient: %w", err)
	}
	if account == nil {
		return nil, fmt.Errorf("mail: recipient %s not found", notif.AccountID)
	}

	if account.Email == "" || !account.EmailVerified {
		return nil, nil
	}

	if !shouldEmailNotify(account, notif.Type) {
		return nil, nil
	}

	fromUsername := ""
	from, err := store.GetAccount(ctx, notif.FromAccountID)
	if err != nil {
		return nil, fmt.Errorf("mail: load source account: %w", err)
	}
	if from != nil {
		fromUsername = from.Username
	}

	subject, body := emailContent(account.Locale, account.Username, notif.Type, fromUsername, webDomain, notif.Emoji)
	if notif.Type == "blog.rebuilt" && notif.StatusID != nil && *notif.StatusID != "" {
		body = i18n.T(account.Locale, "mail.blog_rebuilt_discussion.body", account.Username,
			fmt.Sprintf("https://%s/@%s/blog", webDomain, account.Username))
	}

	payload := EmailJobPayload{
		To:               account.Email,
		Subject:          subject,
		Body:             body,
		WebDomain:        webDomain,
		AccountID:        account.ID,
		NotificationType: notif.Type,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mail: marshal email job payload: %w", err)
	}

	return &models.Job{
		Kind:        "send_email",
		Payload:     string(payloadBytes),
		MaxAttempts: MaxSendAttempts,
		State:       "queued",
	}, nil
}

// shouldEmailNotify returns true if the account wants email for this notification type.
func shouldEmailNotify(account *models.Account, notifType string) bool {
	switch notifType {
	case "mention":
		return account.EmailNotifyMention
	case "follow", "follow_accept", "follow_reject":
		return account.EmailNotifyFollow
	case "reblog":
		return account.EmailNotifyReblog
	case "favourite", "emoji_reaction":
		return account.EmailNotifyFavourite
	case "poll":
		return account.EmailNotifyPoll
	case "follow_request":
		return account.EmailNotifyFollowRequest
	case "quote":
		return account.EmailNotifyQuote
	case "admin.report":
		return account.EmailNotifyReport
	case "admin.approval":
		return account.EmailNotifyApproval
	case "admin.new_account":
		return account.EmailNotifyNewAccount
	case "admin.invite_signup":
		return account.EmailNotifyInviteSignup
	case "admin.backup_success":
		return account.EmailNotifyBackupSuccess
	case "admin.backup_failure":
		return account.EmailNotifyBackupFailure
	case "admin.instance":
		return account.EmailNotifyInstance
	case "admin.account_deleted", "admin.account_moved":
		return account.EmailNotifyAccountDeparture
	case "digest":
		return account.EmailNotifyDigest
	case "blog.rebuilt":
		return account.EmailNotifyBlogRebuild
	case "block", "unblock", "unfollow", "move":
		return account.EmailNotifyAccountActivity
	default:
		return false
	}
}

// NotifyAccountCreated emails admins who opted into all registrations and the
// owner of a used invite. One recipient gets at most one email for the event.
func NotifyAccountCreated(ctx context.Context, store storage.Storage, domain string, account *models.Account, inviteCode string) {
	if account == nil {
		return
	}

	type recipient struct {
		account   *models.Account
		notifType string
	}
	recipients := make(map[string]recipient)
	if admins, err := store.GetAdmins(ctx); err != nil {
		slog.Error("mail: get admins for new account notification", "error", err)
	} else {
		for _, admin := range admins {
			switch {
			case !account.Approved && admin.EmailNotifyApproval:
				recipients[admin.ID] = recipient{admin, "admin.approval"}
			case admin.EmailNotifyNewAccount:
				recipients[admin.ID] = recipient{admin, "admin.new_account"}
			}
		}
	}

	invited := account.InviteID != nil && inviteCode != ""
	if invited {
		if invite, err := store.GetInviteByCode(ctx, inviteCode); err != nil {
			slog.Error("mail: get used invite", "error", err)
		} else if inviter, err := store.GetAccount(ctx, invite.CreatedByID); err != nil {
			slog.Error("mail: get invite owner", "error", err)
		} else if inviter.EmailNotifyInviteSignup {
			if _, exists := recipients[inviter.ID]; !exists {
				recipients[inviter.ID] = recipient{inviter, "admin.invite_signup"}
			}
		}
	}

	for _, recipient := range recipients {
		if recipient.account.Email == "" || !recipient.account.EmailVerified {
			continue
		}
		subject := fmt.Sprintf("[%s] New account: @%s", domain, account.Username)
		body := fmt.Sprintf("A new account has been created on %s.\n\nUsername: @%s\nRegistration: open registration\n\nReview accounts: https://%s/admin/accounts\n\n- littleFedi", domain, account.Username, domain)
		if invited {
			body = fmt.Sprintf("A new account has been created on %s using an invite code.\n\nUsername: @%s\n\nReview accounts: https://%s/admin/accounts\n\n- littleFedi", domain, account.Username, domain)
		}
		if !account.Approved {
			subject = fmt.Sprintf("[%s] New account awaiting approval: @%s", domain, account.Username)
			body = fmt.Sprintf("A new account has registered on %s and requires admin approval.\n\nUsername: @%s\nRegistration: open registration\n\nReview: https://%s/admin/pending\n\n- littleFedi", domain, account.Username, domain)
			if invited {
				body = fmt.Sprintf("A new account has registered on %s using an invite code and requires admin approval.\n\nUsername: @%s\n\nReview: https://%s/admin/pending\n\n- littleFedi", domain, account.Username, domain)
			}
		}
		payload, err := json.Marshal(EmailJobPayload{
			To:               recipient.account.Email,
			Subject:          subject,
			Body:             body,
			AccountID:        recipient.account.ID,
			NotificationType: recipient.notifType,
		})
		if err != nil {
			slog.Error("mail: marshal new account email", "error", err)
			continue
		}
		if err := store.EnqueueJob(ctx, &models.Job{Kind: "send_email", Payload: string(payload), MaxAttempts: MaxSendAttempts, State: "queued"}); err != nil {
			slog.Error("mail: enqueue new account email", "error", err)
		}
	}
}

// Backup alerts are operational messages to administrators. Unlike user
// notifications, they must still flow when an existing admin address predates
// email verification or SMTP was unavailable while the account was created.
func requiresVerifiedEmail(notifType string) bool {
	return notifType != "admin.backup_success" && notifType != "admin.backup_failure"
}

func emailContent(locale, accountUsername, notifType, fromUsername, domain, emoji string) (string, string) {
	if fromUsername == "" {
		fromUsername = i18n.T(locale, "mail.someone")
	}
	notifURL := fmt.Sprintf("https://%s/notifications", domain)

	switch notifType {
	case "mention":
		return i18n.T(locale, "mail.mention.subject", domain, fromUsername),
			i18n.T(locale, "mail.mention.body", accountUsername, fromUsername, notifURL)
	case "follow":
		return i18n.T(locale, "mail.follow.subject", domain, fromUsername),
			i18n.T(locale, "mail.follow.body", accountUsername, fromUsername, notifURL)
	case "follow_request":
		return i18n.T(locale, "mail.follow_request.subject", domain, fromUsername),
			i18n.T(locale, "mail.follow_request.body", accountUsername, fromUsername, notifURL)
	case "reblog":
		return i18n.T(locale, "mail.reblog.subject", domain, fromUsername),
			i18n.T(locale, "mail.reblog.body", accountUsername, fromUsername, notifURL)
	case "favourite":
		return i18n.T(locale, "mail.favourite.subject", domain, fromUsername),
			i18n.T(locale, "mail.favourite.body", accountUsername, fromUsername, notifURL)
	case "emoji_reaction":
		return i18n.T(locale, "mail.emoji_reaction.subject", domain, fromUsername, emoji),
			i18n.T(locale, "mail.emoji_reaction.body", accountUsername, fromUsername, emoji, notifURL)
	case "quote":
		return i18n.T(locale, "mail.quote.subject", domain, fromUsername),
			i18n.T(locale, "mail.quote.body", accountUsername, fromUsername, notifURL)
	case "admin.report":
		return i18n.T(locale, "mail.report.subject", domain),
			i18n.T(locale, "mail.report.body", domain)
	case "admin.account_deleted":
		return i18n.T(locale, "mail.account_deleted.subject", domain, fromUsername),
			i18n.T(locale, "mail.account_deleted.body", accountUsername, fromUsername, notifURL)
	case "admin.account_moved":
		return i18n.T(locale, "mail.account_moved.subject", domain, fromUsername),
			i18n.T(locale, "mail.account_moved.body", accountUsername, fromUsername, notifURL)
	case "blog.rebuilt":
		blogURL := fmt.Sprintf("https://%s/@%s/blog", domain, accountUsername)
		return i18n.T(locale, "mail.blog_rebuilt.subject", domain),
			i18n.T(locale, "mail.blog_rebuilt.body", accountUsername, blogURL)
	case "block":
		return i18n.T(locale, "mail.block.subject", domain, fromUsername),
			i18n.T(locale, "mail.block.body", accountUsername, fromUsername, notifURL)
	case "unblock":
		return i18n.T(locale, "mail.unblock.subject", domain, fromUsername),
			i18n.T(locale, "mail.unblock.body", accountUsername, fromUsername, notifURL)
	case "unfollow":
		return i18n.T(locale, "mail.unfollow.subject", domain, fromUsername),
			i18n.T(locale, "mail.unfollow.body", accountUsername, fromUsername, notifURL)
	case "move":
		return i18n.T(locale, "mail.move.subject", domain, fromUsername),
			i18n.T(locale, "mail.move.body", accountUsername, fromUsername, notifURL)
	default:
		return i18n.T(locale, "mail.default.subject", domain),
			i18n.T(locale, "mail.default.body", accountUsername, fromUsername, notifURL)
	}
}

// HandleEmailJob processes a send_email job and delivers it via the mailer.
func HandleEmailJob(store storage.Storage, mailer *Mailer) func(ctx context.Context, payload string) error {
	return func(ctx context.Context, payload string) error {
		if mailer == nil {
			return nil
		}

		var jobPayload EmailJobPayload
		if err := json.Unmarshal([]byte(payload), &jobPayload); err != nil {
			return jobqueue.Permanent(fmt.Errorf("mail: unmarshal email payload: %w", err))
		}
		if jobPayload.ExpiresAt != nil && !time.Now().Before(*jobPayload.ExpiresAt) {
			return nil
		}
		// A durable job may wait while the user changes their preferences.
		// Re-check at delivery time and also pick up a changed email address.
		if jobPayload.AccountID != "" && jobPayload.NotificationType != "" {
			account, err := store.GetAccount(ctx, jobPayload.AccountID)
			if err != nil || account == nil || account.Email == "" || requiresVerifiedEmail(jobPayload.NotificationType) && !account.EmailVerified ||
				!shouldEmailNotify(account, jobPayload.NotificationType) {
				return nil
			}
			jobPayload.To = account.Email
		}

		err := mailer.Send(jobPayload.To, jobPayload.Subject, jobPayload.Body)
		if permanentSendError(err) {
			return jobqueue.Permanent(err)
		}
		return err
	}
}

// Admin notification helpers

// enqueueAdminEmail sends an email to all admins who have adminEmails set to true.
func enqueueAdminEmail(ctx context.Context, store storage.Storage, notifType string, adminEmails func(acc *models.Account) bool, subject, body string) {
	admins, err := store.GetAdmins(ctx)
	if err != nil {
		slog.Error("mail: get admins for notification", "error", err)
		return
	}
	for _, admin := range admins {
		switch {
		case admin.Email == "":
			slog.Debug("mail: skipped admin email", "admin", admin.Username, "type", notifType, "reason", "no email")
			continue
		case requiresVerifiedEmail(notifType) && !admin.EmailVerified:
			slog.Debug("mail: skipped admin email", "admin", admin.Username, "type", notifType, "reason", "email not verified")
			continue
		case !adminEmails(admin):
			slog.Debug("mail: skipped admin email", "admin", admin.Username, "type", notifType, "reason", "preference disabled")
			continue
		}
		payload := EmailJobPayload{
			To:               admin.Email,
			Subject:          subject,
			Body:             body,
			AccountID:        admin.ID,
			NotificationType: notifType,
		}
		payloadBytes, _ := json.Marshal(payload)
		job := &models.Job{
			Kind:        "send_email",
			Payload:     string(payloadBytes),
			MaxAttempts: MaxSendAttempts,
			State:       "queued",
		}
		if err := store.EnqueueJob(ctx, job); err != nil {
			slog.Error("mail: enqueue admin email", "admin", admin.Username, "error", err)
		} else {
			// Every event gets a fresh job ID; failed terminal jobs are never
			// consulted or reused, so earlier SMTP failures cannot suppress it.
			slog.Debug("mail: enqueued admin email", "admin", admin.Username, "type", notifType, "job", job.ID)
		}
	}
}

// NotifyAdminsBackup sends a backup success/failure email to admins.
func NotifyAdminsBackup(ctx context.Context, store storage.Storage, domain string, success bool, path string) {
	if success {
		subject := fmt.Sprintf("[%s] Backup completed successfully", domain)
		body := fmt.Sprintf(
			"Backup to %s completed successfully.\n\n- littleFedi", path,
		)
		enqueueAdminEmail(ctx, store, "admin.backup_success", func(a *models.Account) bool { return a.EmailNotifyBackupSuccess }, subject, body)
	} else {
		subject := fmt.Sprintf("[%s] Backup FAILED", domain)
		body := "Periodic backup failed. Check the server logs for details.\n\n- littleFedi"
		enqueueAdminEmail(ctx, store, "admin.backup_failure", func(a *models.Account) bool { return a.EmailNotifyBackupFailure }, subject, body)
	}
}

// NotifyAdminsInstanceUnreachable sends an email to admins about an unreachable instance.
func NotifyAdminsInstanceUnreachable(ctx context.Context, store storage.Storage, domain, instanceDomain string) {
	subject := fmt.Sprintf("[%s] Instance %s became unreachable", domain, instanceDomain)
	body := fmt.Sprintf(
		"The remote instance %s has become unreachable. Delivery to this instance has been suspended.\n\n"+
			"View instance health: https://%s/admin/instances\n\n"+
			"- littleFedi",
		instanceDomain, domain,
	)
	enqueueAdminEmail(ctx, store, "admin.instance", func(a *models.Account) bool { return a.EmailNotifyInstance }, subject, body)
}
