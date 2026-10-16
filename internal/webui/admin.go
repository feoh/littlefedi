package webui

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/backup"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/instancemeta"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Admin Middleware

// AdminMiddleware checks that the authenticated user is an admin.
func (h *Handler) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := getAuthAccount(r.Context())
		if account == nil {
			w.Header().Set("Cache-Control", "private, no-store")
			http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
			return
		}
		if !account.IsAdmin {
			h.renderPage(w, r, "Forbidden", map[string]interface{}{"Message": "Admin privileges required"}, "error.html")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) adminAudit(ctx context.Context, targetType, targetID, action, note string) {
	admin := getAuthAccount(ctx)
	if admin == nil {
		return
	}
	if err := h.store.CreateModerationLog(ctx, &models.ModerationLog{AdminAccountID: admin.ID, TargetType: targetType, TargetID: targetID, Action: action, Note: note}); err != nil {
		slog.Error("admin: create audit entry", "action", action, "target", targetID, "error", err)
	}
}

// Admin Dashboard

type adminAttention struct {
	Count                     int
	Title, Detail, URL, Level string
}

type adminPagination struct {
	Page, Pages, Total int
	PrevURL, NextURL   string
}

func adminPaginate(r *http.Request, total, perPage int) (adminPagination, int, int) {
	if perPage <= 0 {
		perPage = 50
	}
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	start, end := (page-1)*perPage, page*perPage
	if end > total {
		end = total
	}
	pageURL := func(n int) string {
		values := r.URL.Query()
		values.Set("page", strconv.Itoa(n))
		return r.URL.Path + "?" + values.Encode()
	}
	pagination := adminPagination{Page: page, Pages: pages, Total: total}
	if page > 1 {
		pagination.PrevURL = pageURL(page - 1)
	}
	if page < pages {
		pagination.NextURL = pageURL(page + 1)
	}
	return pagination, start, end
}

type adminOverviewData struct {
	Account                                                                               *models.Account
	CSRFToken                                                                             string
	GeneratedAt                                                                           time.Time
	Health, HealthDetail                                                                  string
	LocalUserCount, PendingCount, StatusCount, DomainCount, UnreachableCount, OpenReports int
	MaxStatusChars, BroadcastSent, BroadcastFailed                                        int
	BroadcastDone, WelcomeEnabled                                                         bool
	WelcomeMessage, WelcomeResult                                                         string
	Queue                                                                                 *models.QueueStats
	QueueLevel, QueueDetail                                                               string
	Metrics                                                                               metrics.MetricsSnapshot
	Attention                                                                             []adminAttention
	RecentAudit                                                                           []auditEntry
	Unavailable                                                                           []string
}

const (
	queueWarningAge  = 5 * time.Minute
	queueCriticalAge = 15 * time.Minute
)

// assessQueue keeps diagnostic failures visible without treating routine
// remote delivery rejection as an instance-wide operational failure.
func assessQueue(stats *models.QueueStats, snapshot metrics.MetricsSnapshot, now time.Time) (level, detail string, actionable int) {
	if stats == nil {
		return "unavailable", "Queue health is unavailable.", 0
	}
	if stats.StaleRunning > 0 {
		return "danger", fmt.Sprintf("%d running jobs exceeded the stale timeout.", stats.StaleRunning), stats.StaleRunning
	}
	if stats.Ready > 0 && snapshot.ConfiguredWorkers == 0 {
		return "danger", "Jobs are ready but no queue workers are configured.", stats.Ready
	}
	if stats.OldestReadyAt != nil {
		age := now.Sub(*stats.OldestReadyAt)
		if age >= queueCriticalAge {
			return "danger", fmt.Sprintf("The oldest ready job has waited %s.", formatDuration(age)), stats.Ready
		}
		if age >= queueWarningAge {
			return "warn", fmt.Sprintf("The oldest ready job has waited %s; monitor the backlog.", formatDuration(age)), 0
		}
	}
	if stats.Failed > 0 {
		return "ok", fmt.Sprintf("Processing is current. %d terminal failures are retained for diagnosis.", stats.Failed), 0
	}
	return "ok", "Background processing is current.", 0
}

func queueStaleTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Queue.StaleRunningTimeout > 0 {
		return cfg.Queue.StaleRunningTimeout
	}
	return 15 * time.Minute
}

// AdminDashboard shows the admin overview.
func (h *Handler) AdminDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)
	now := time.Now()
	data := adminOverviewData{Account: account, CSRFToken: getCSRFToken(ctx), GeneratedAt: now, Metrics: metrics.Snapshot(), Health: "Operational", HealthDetail: "No conditions currently require attention.", MaxStatusChars: h.maxStatusChars()}
	if _, data.BroadcastDone = r.URL.Query()["broadcast_sent"]; data.BroadcastDone {
		data.BroadcastSent, _ = strconv.Atoi(r.URL.Query().Get("broadcast_sent"))
		data.BroadcastFailed, _ = strconv.Atoi(r.URL.Query().Get("broadcast_failed"))
	}
	if saved := r.URL.Query().Get("welcome_saved"); saved != "" {
		data.WelcomeResult = saved
	}
	if welcome, err := h.store.GetWelcomeDirectMessage(ctx); err == nil {
		data.WelcomeEnabled = true
		data.WelcomeMessage = welcome.Content
	} else if !errors.Is(err, sql.ErrNoRows) {
		data.Unavailable = append(data.Unavailable, "welcome message")
	}
	if n, err := h.store.CountLocalAccounts(ctx); err == nil {
		data.LocalUserCount = n
	} else {
		data.Unavailable = append(data.Unavailable, "local account count")
	}
	if pending, err := h.store.GetPendingApprovals(ctx); err == nil {
		data.PendingCount = len(pending)
	} else {
		data.Unavailable = append(data.Unavailable, "pending approvals")
	}
	if n, err := h.store.CountStatuses(ctx); err == nil {
		data.StatusCount = n
	} else {
		data.Unavailable = append(data.Unavailable, "post count")
	}
	if n, err := h.store.CountDomains(ctx); err == nil {
		data.DomainCount = n
	} else {
		data.Unavailable = append(data.Unavailable, "domain count")
	}
	if n, err := h.store.CountUnreachableInstances(ctx); err == nil {
		data.UnreachableCount = n
	} else {
		data.Unavailable = append(data.Unavailable, "instance health")
	}
	if n, err := h.store.CountOpenReports(ctx); err == nil {
		data.OpenReports = n
	} else {
		data.Unavailable = append(data.Unavailable, "open reports")
	}
	queueStats, err := h.store.GetQueueStats(ctx, now, now.Add(-queueStaleTimeout(h.cfg)))
	if err != nil {
		data.Unavailable = append(data.Unavailable, "queue health")
	} else {
		data.Queue = queueStats
		data.QueueLevel, data.QueueDetail, _ = assessQueue(queueStats, data.Metrics, now)
	}
	if logs, err := h.store.ListModerationLog(ctx, nil, 5); err == nil {
		for _, log := range logs {
			name := log.AdminAccountID
			if a, getErr := h.store.GetAccount(ctx, log.AdminAccountID); getErr == nil && a != nil {
				name = "@" + a.Username
			}
			data.RecentAudit = append(data.RecentAudit, auditEntry{CreatedAt: formatTimeRelative(log.CreatedAt), AdminName: name, TargetType: log.TargetType, TargetID: log.TargetID, Action: log.Action, Note: log.Note})
		}
	} else {
		data.Unavailable = append(data.Unavailable, "recent audit activity")
	}
	if data.OpenReports > 0 {
		data.Attention = append(data.Attention, adminAttention{data.OpenReports, "Open moderation reports", "Review reports awaiting a decision.", "/admin/reports", "danger"})
	}
	if data.PendingCount > 0 {
		data.Attention = append(data.Attention, adminAttention{data.PendingCount, "Pending account approvals", "Approve or reject registration requests.", "/admin/pending", "warn"})
	}
	if data.Queue != nil {
		_, detail, actionable := assessQueue(data.Queue, data.Metrics, now)
		if actionable > 0 {
			data.Attention = append(data.Attention, adminAttention{actionable, "Queue requires intervention", detail, "/admin/queue", "danger"})
		}
	}
	// Offline instances are a normal part of federation and do not require operator
	// action, so they are surfaced as an informational count (data.UnreachableCount)
	// rather than an "attention"/"action required" item.
	if len(data.Attention) > 0 {
		data.Health = "Action required"
		data.HealthDetail = "One or more operational or moderation conditions need review."
	} else if len(data.Unavailable) > 0 {
		data.Health = "Degraded"
		data.HealthDetail = "Some dashboard data could not be loaded."
	} else if data.QueueLevel == "warn" {
		data.Health = "Degraded"
		data.HealthDetail = data.QueueDetail
	}
	h.renderPage(w, r, "Admin", data, "admin_dashboard.html")
}

// AdminBroadcastDirectMessage sends separate DMs so recipients cannot see one another.
func (h *Handler) AdminBroadcastDirectMessage(w http.ResponseWriter, r *http.Request) {
	admin := getAuthAccount(r.Context())
	message := strings.TrimSpace(r.FormValue("message"))
	if message == "" || len([]rune(message)) > h.maxStatusChars() {
		http.Error(w, "Message must be between 1 and the configured character limit", http.StatusUnprocessableEntity)
		return
	}
	recipients, err := h.store.GetLocalAccounts(r.Context())
	if err != nil {
		http.Error(w, "Could not list local users", http.StatusInternalServerError)
		return
	}

	sent, failed := 0, 0
	// Synchronous fan-out is simplest; queue batches if large instances make this request slow.
	for _, recipient := range recipients {
		if recipient.ID == admin.ID {
			continue
		}
		if _, err := h.app.CreateLocalDirectMessage(r.Context(), admin, recipient, message); err != nil {
			failed++
			slog.Error("admin: broadcast direct message", "recipient", recipient.ID, "error", err)
			continue
		}
		sent++
	}
	if sent > 0 {
		h.adminAudit(r.Context(), "local_accounts", h.domain, "broadcast_direct_message", fmt.Sprintf("sent %d, failed %d", sent, failed))
	}
	values := url.Values{"broadcast_sent": {strconv.Itoa(sent)}, "broadcast_failed": {strconv.Itoa(failed)}}
	http.Redirect(w, r, "/admin?"+values.Encode(), http.StatusSeeOther)
}

// AdminWelcomeDirectMessage saves or disables the DM sent after local account creation.
func (h *Handler) AdminWelcomeDirectMessage(w http.ResponseWriter, r *http.Request) {
	admin := getAuthAccount(r.Context())
	message := strings.TrimSpace(r.FormValue("message"))
	if len([]rune(message)) > h.maxStatusChars() {
		http.Error(w, "Message exceeds the configured character limit", http.StatusUnprocessableEntity)
		return
	}
	result := "disabled"
	if message == "" {
		if err := h.store.DeleteWelcomeDirectMessage(r.Context()); err != nil {
			http.Error(w, "Could not disable welcome message", http.StatusInternalServerError)
			return
		}
		h.adminAudit(r.Context(), "welcome_direct_message", h.domain, "disable", "")
	} else {
		if err := h.store.SetWelcomeDirectMessage(r.Context(), admin.ID, message); err != nil {
			http.Error(w, "Could not save welcome message", http.StatusInternalServerError)
			return
		}
		result = "enabled"
		h.adminAudit(r.Context(), "welcome_direct_message", h.domain, "configure", "")
	}
	http.Redirect(w, r, "/admin?welcome_saved="+result, http.StatusSeeOther)
}

type adminQueueData struct {
	Account                                      *models.Account
	CSRFToken                                    string
	GeneratedAt                                  time.Time
	Stats                                        *models.QueueStats
	Problems                                     []models.QueueProblemJob
	Metrics                                      metrics.MetricsSnapshot
	StaleTimeout, Retention, MaintenanceInterval time.Duration
	Error                                        string
	QueueLevel, QueueDetail                      string
	RunningJobs                                  []adminQueueJobView
	QueuedGroups                                 []adminQueueJobGroup
	ActiveJobsTotal, ActiveJobsShown             int
	ContentsError                                string
	Result                                       string
	// WorkerPools is Metrics.WorkerPools as a name-sorted slice: the optional
	// dedicated light-jobs pool (cfg.Queue.LightWorkers) runs alongside the
	// main pool, and the admin view needs a stable per-pool breakdown rather
	// than the single combined "busy/configured" figure, which can't show
	// that one pool is saturated while another sits idle.
	WorkerPools []adminWorkerPoolView
}

type adminWorkerPoolView struct {
	Name               string
	Active, Configured int64
}

type adminQueueJobView struct {
	Kind, Summary, State, Timing string
	Attempts, MaxAttempts        int
}

type adminQueueJobGroup struct {
	Kind, Label string
	Jobs        []adminQueueJobView
}

func queueKindLabel(kind string) string {
	switch kind {
	case "delivery":
		return "Federation deliveries"
	case "inbox":
		return "Inbox activities"
	case "push_notification":
		return "Push notifications"
	case "media_cache":
		return "Remote media cache"
	case "actor_refresh":
		return "Remote actor refresh"
	default:
		label := strings.ReplaceAll(kind, "_", " ")
		if label == "" {
			return "Other jobs"
		}
		return strings.ToUpper(label[:1]) + label[1:]
	}
}

func safeQueueHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "a remote server"
	}
	return parsed.Hostname()
}

// summarizeQueueJob extracts a small, non-sensitive description
// for known payloads. Raw payloads and identifiers never reach the template.
func summarizeQueueJob(job models.Job) string {
	switch job.Kind {
	case "delivery":
		var payload struct {
			Inbox    string          `json:"inbox"`
			Activity json.RawMessage `json:"activity"`
		}
		if json.Unmarshal([]byte(job.Payload), &payload) == nil {
			var activity struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload.Activity, &activity)
			if activity.Type != "" {
				return "Deliver " + activity.Type + " to " + safeQueueHost(payload.Inbox)
			}
			return "Deliver activity to " + safeQueueHost(payload.Inbox)
		}
		return "Deliver federated activity"
	case "inbox":
		var payload struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
		}
		if json.Unmarshal([]byte(job.Payload), &payload) == nil {
			activityType := payload.Type
			if activityType == "" {
				activityType = "activity"
			}
			if payload.Actor != "" {
				return "Process " + activityType + " from " + safeQueueHost(payload.Actor)
			}
			return "Process " + activityType
		}
		return "Process inbox activity"
	case "actor_refresh":
		return "Refresh remote actor profile"
	case "push_notification":
		var payload struct {
			NotifType string `json:"notif_type"`
		}
		if json.Unmarshal([]byte(job.Payload), &payload) == nil && payload.NotifType != "" {
			return "Send " + payload.NotifType + " notification"
		}
		return "Send push notification"
	case "media_cache":
		var payload struct {
			RemoteURL string `json:"remote_url"`
		}
		if json.Unmarshal([]byte(job.Payload), &payload) == nil {
			return "Cache media from " + safeQueueHost(payload.RemoteURL)
		}
		return "Cache remote media"
	default:
		return "Process " + queueKindLabel(job.Kind)
	}
}

func queueJobView(job models.Job, now time.Time) adminQueueJobView {
	timing := "Ready now"
	if job.State == "running" {
		timing = "Running"
		if job.TakenAt != nil {
			timing = "Running for " + formatDuration(now.Sub(*job.TakenAt))
		}
	} else if job.RunAt.After(now) {
		timing = "Scheduled in " + formatDuration(job.RunAt.Sub(now))
	} else if !job.RunAt.IsZero() {
		timing = "Ready for " + formatDuration(now.Sub(job.RunAt))
	}
	return adminQueueJobView{Kind: job.Kind, Summary: summarizeQueueJob(job), State: job.State, Timing: timing, Attempts: job.Attempts, MaxAttempts: job.MaxAttempts}
}

// AdminQueue shows durable queue state without exposing job payloads.
func (h *Handler) AdminQueue(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), time.Now()
	stale := queueStaleTimeout(h.cfg)
	data := adminQueueData{Account: getAuthAccount(ctx), CSRFToken: getCSRFToken(ctx), GeneratedAt: now, Metrics: metrics.Snapshot(), StaleTimeout: stale}
	poolNames := make([]string, 0, len(data.Metrics.WorkerPools))
	for name := range data.Metrics.WorkerPools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)
	for _, name := range poolNames {
		s := data.Metrics.WorkerPools[name]
		data.WorkerPools = append(data.WorkerPools, adminWorkerPoolView{Name: name, Active: s.Active, Configured: s.Configured})
	}
	if r.URL.Query().Get("result") == "deleted" {
		data.Result = "Problem job deleted."
	}
	if r.URL.Query().Get("result") == "protected" {
		data.Error = "That job is running, no longer exists, or is not eligible for deletion."
	}
	if h.cfg != nil {
		data.Retention = h.cfg.Queue.TerminalJobRetention
		data.MaintenanceInterval = h.cfg.Queue.MaintenanceInterval
	}
	stats, err := h.store.GetQueueStats(ctx, now, now.Add(-stale))
	if err != nil {
		data.Error = "Queue statistics are currently unavailable."
		slog.Error("admin: queue stats", "error", err)
	} else {
		data.Stats = stats
		data.QueueLevel, data.QueueDetail, _ = assessQueue(stats, data.Metrics, now)
	}
	if jobs, err := h.store.ListQueueProblemJobs(ctx, 50); err != nil {
		if data.Error == "" {
			data.Error = "Problem jobs are currently unavailable."
		}
		slog.Error("admin: queue problem jobs", "error", err)
	} else {
		data.Problems = jobs
	}
	if jobs, err := h.store.ListActiveQueueJobs(ctx, 200); err != nil {
		data.ContentsError = "Active queue contents are currently unavailable."
		slog.Error("admin: active queue jobs", "error", err)
	} else {
		data.ActiveJobsShown = len(jobs)
		if data.Stats != nil {
			data.ActiveJobsTotal = data.Stats.Ready + data.Stats.Scheduled + data.Stats.Running
		}
		groupIndexes := make(map[string]int)
		for _, job := range jobs {
			view := queueJobView(job, now)
			if job.State == "running" {
				data.RunningJobs = append(data.RunningJobs, view)
				continue
			}
			index, ok := groupIndexes[job.Kind]
			if !ok {
				index = len(data.QueuedGroups)
				groupIndexes[job.Kind] = index
				data.QueuedGroups = append(data.QueuedGroups, adminQueueJobGroup{Kind: job.Kind, Label: queueKindLabel(job.Kind)})
			}
			data.QueuedGroups[index].Jobs = append(data.QueuedGroups[index].Jobs, view)
		}
	}
	h.renderPage(w, r, "Admin - Queue", data, "admin_queue.html")
}

// AdminQueueDeleteProblem removes a retained failure or cancels a queued retry.
func (h *Handler) AdminQueueDeleteProblem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/queue?result=protected", http.StatusFound)
		return
	}
	deleted, err := h.store.DeleteQueueProblemJob(r.Context(), id)
	if err != nil {
		slog.Error("admin: delete queue problem job", "id", id, "error", err)
		http.Redirect(w, r, "/admin/queue?result=protected", http.StatusFound)
		return
	}
	if !deleted {
		http.Redirect(w, r, "/admin/queue?result=protected", http.StatusFound)
		return
	}
	h.adminAudit(r.Context(), "queue_job", id, "delete", "removed retained failure or queued retry")
	http.Redirect(w, r, "/admin/queue?result=deleted", http.StatusFound)
}

// AdminAccountsList shows all local accounts.
func (h *Handler) AdminAccountsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	localAccounts, err := h.store.GetLocalAccounts(ctx)
	if err != nil {
		slog.Error("admin: list accounts", "error", err)
		h.renderError(w, r, "Error fetching accounts")
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	filtered := make([]*models.Account, 0, len(localAccounts))
	for _, item := range localAccounts {
		if query != "" && !strings.Contains(strings.ToLower(item.Username+" "+item.Email), query) {
			continue
		}
		matches := state == ""
		switch state {
		case "admin":
			matches = item.IsAdmin
		case "pending":
			matches = !item.Approved
		case "moderated":
			matches = item.SuspendedAt != nil || item.ModerationState != ""
		case "active":
			matches = item.Approved && item.SuspendedAt == nil && item.ModerationState == ""
		}
		if matches {
			filtered = append(filtered, item)
		}
	}
	pagination, start, end := adminPaginate(r, len(filtered), 50)
	filtered = filtered[start:end]

	data := map[string]interface{}{
		"Accounts":  filtered,
		"Account":   account,
		"CSRFToken": getCSRFToken(ctx),
		"Query":     query, "State": state,
		"Pagination":  pagination,
		"BlogCapable": h.blogCapable(),
		// With [blog] all_accounts on, the per-account button would toggle a
		// column nothing reads, so the page states the policy instead.
		"BlogAllAccounts": h.cfg != nil && h.cfg.Blog.AllAccounts,
	}

	h.renderPage(w, r, "Admin - Accounts", data, "admin_accounts.html")
}

// AdminToggleAdmin toggles admin status for an account.
func (h *Handler) AdminToggleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/accounts", http.StatusFound)
		return
	}

	target, err := h.store.GetAccount(ctx, id)
	if err != nil {
		http.Redirect(w, r, "/admin/accounts", http.StatusFound)
		return
	}

	// Prevent removing the last admin
	if target.IsAdmin {
		admins, _ := h.store.GetAdmins(ctx)
		if len(admins) <= 1 {
			slog.Warn("admin: attempted to demote last admin", "username", target.Username)
			http.Redirect(w, r, "/admin/accounts?error=last-admin", http.StatusFound)
			return
		}
	}

	if err := h.store.SetAdmin(ctx, id, !target.IsAdmin); err != nil {
		slog.Error("admin: toggle admin", "account_id", id, "error", err)
	} else {
		action := "grant_admin"
		if target.IsAdmin {
			action = "revoke_admin"
		}
		h.adminAudit(ctx, "account", id, action, "")
	}

	http.Redirect(w, r, "/admin/accounts", http.StatusFound)
}

// AdminToggleBlogAllowed toggles per-account permission for the optional
// blog feature (in addition to the instance-wide [blog] enabled switch).
func (h *Handler) AdminToggleBlogAllowed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/accounts", http.StatusFound)
		return
	}
	// Nothing reads the per-account column while the instance grants blogging
	// to everyone, so flipping it here would only look like it did something.
	if h.cfg != nil && h.cfg.Blog.AllAccounts {
		http.Error(w, "Blog publishing is granted to every account by [blog] all_accounts", http.StatusConflict)
		return
	}

	target, err := h.store.GetAccount(ctx, id)
	if err != nil {
		http.Redirect(w, r, "/admin/accounts", http.StatusFound)
		return
	}

	if err := h.store.SetBlogAllowed(ctx, id, !target.BlogAllowed); err != nil {
		slog.Error("admin: toggle blog allowed", "account_id", id, "error", err)
	} else {
		action := "allow_blog"
		if target.BlogAllowed {
			action = "disallow_blog"
		}
		h.adminAudit(ctx, "account", id, action, "")
	}

	http.Redirect(w, r, "/admin/accounts", http.StatusFound)
}

func (h *Handler) AdminModerateAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "Missing account", http.StatusBadRequest)
		return
	}
	account, err := h.store.GetAccount(ctx, id)
	if err != nil {
		http.Error(w, "Account not found", http.StatusNotFound)
		return
	}
	action := r.FormValue("action")
	if err := h.app.ModerateAccount(ctx, getAuthAccount(ctx), account, action, r.FormValue("text")); err != nil {
		slog.Error("admin: moderate account", "account_id", id, "action", action, "error", err)
		status := http.StatusInternalServerError
		if errors.Is(err, app.ErrModerationRejected) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/admin/accounts", http.StatusFound)
}

func (h *Handler) AdminDeleteAccountConfirm(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccount(r.Context(), chi.URLParam(r, "id"))
	if err != nil || account.Domain != nil || account.IsServiceActor {
		http.Redirect(w, r, "/admin/accounts", http.StatusFound)
		return
	}
	h.renderPage(w, r, "Delete account - Admin", map[string]interface{}{
		"Account": getAuthAccount(r.Context()), "Target": account,
		"CSRFToken": getCSRFToken(r.Context()),
	}, "admin_account_delete.html")
}

func (h *Handler) AdminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	target, err := h.store.GetAccount(r.Context(), chi.URLParam(r, "id"))
	if err != nil || target.Domain != nil || r.FormValue("confirm") != target.Username {
		http.Redirect(w, r, "/admin/accounts?error=delete-confirmation", http.StatusFound)
		return
	}
	if _, _, err := h.app.RequestAccountDeletion(r.Context(), getAuthAccount(r.Context()), target); err != nil {
		slog.Error("admin: request account deletion", "account_id", target.ID, "error", err)
		http.Redirect(w, r, "/admin/accounts?error=delete-failed", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/accounts", http.StatusFound)
}

type adminReport struct {
	*models.Report
	Statuses           []*models.Status
	MissingStatusCount int
}

// AdminReports lists moderation reports for triage.
func (h *Handler) AdminReports(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	state := "open"
	if r.URL.Query().Get("resolved") == "true" {
		state = "resolved"
	}
	reports, err := h.store.ListReports(r.Context(), state, nil, 500)
	if err != nil {
		slog.Error("webui: list reports error", "error", err)
		reports = []*models.Report{}
	}
	pagination, start, end := adminPaginate(r, len(reports), 25)
	pageReports := make([]adminReport, 0, end-start)
	for _, rep := range reports[start:end] {
		rep.Target, _ = h.store.GetAccount(r.Context(), rep.TargetAccountID)
		if rep.AccountID != nil {
			rep.Reporter, _ = h.store.GetAccount(r.Context(), *rep.AccountID)
		}
		view := adminReport{Report: rep}
		for _, id := range strings.Split(rep.StatusIDs, ",") {
			if id == "" {
				continue
			}
			status, err := h.store.GetStatus(r.Context(), id)
			if err != nil {
				view.MissingStatusCount++
				continue
			}
			if status.Account == nil && rep.Target != nil && status.AccountID == rep.Target.ID {
				status.Account = rep.Target
			}
			if status.Account == nil {
				view.MissingStatusCount++
				continue
			}
			view.Statuses = append(view.Statuses, status)
		}
		pageReports = append(pageReports, view)
	}
	h.renderPage(w, r, "Reports - Admin", map[string]interface{}{
		"Account":         account,
		"Reports":         pageReports,
		"ShowingResolved": state == "resolved",
		"CSRFToken":       getCSRFToken(r.Context()),
		"Pagination":      pagination,
	}, "admin_reports.html")
}

// AdminReportResolveWeb resolves a report, optionally applying a follow-on action.
func (h *Handler) AdminReportResolveWeb(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	action := r.FormValue("action")
	if err := h.app.ResolveReport(ctx, getAuthAccount(ctx), id, action); err != nil {
		slog.Error("webui: resolve report error", "error", err)
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		} else if errors.Is(err, app.ErrModerationRejected) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/admin/reports", http.StatusFound)
}

// auditEntry is a moderation-log row decorated with display names for the table.
type auditEntry struct {
	CreatedAt  string
	AdminName  string
	TargetType string
	TargetID   string
	Action     string
	Note       string
}

// AdminAuditLog lists the moderation audit trail.
func (h *Handler) AdminAuditLog(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	logs, err := h.store.ListModerationLog(r.Context(), nil, 1000)
	if err != nil {
		slog.Error("webui: list moderation log error", "error", err)
		logs = nil
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	entries := make([]auditEntry, 0, len(logs))
	for _, l := range logs {
		if query != "" && !strings.Contains(strings.ToLower(l.Action+" "+l.TargetType+" "+l.TargetID+" "+l.Note), query) {
			continue
		}
		name := l.AdminAccountID
		if a, err := h.store.GetAccount(r.Context(), l.AdminAccountID); err == nil && a != nil {
			name = "@" + a.Username
		}
		entries = append(entries, auditEntry{
			CreatedAt:  l.CreatedAt.Format("Jan 2, 2006, 15:04"),
			AdminName:  name,
			TargetType: l.TargetType,
			TargetID:   l.TargetID,
			Action:     l.Action,
			Note:       l.Note,
		})
	}
	pagination, start, end := adminPaginate(r, len(entries), 50)
	entries = entries[start:end]
	h.renderPage(w, r, "Audit log - Admin", map[string]interface{}{
		"Account":    account,
		"Entries":    entries,
		"CSRFToken":  getCSRFToken(r.Context()),
		"Query":      query,
		"Pagination": pagination,
	}, "admin_audit.html")
}

// AdminApproveAccount approves a pending account.
func (h *Handler) AdminApproveAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id != "" {
		account, _ := h.store.GetAccount(ctx, id)
		if err := h.store.ApproveAccount(ctx, id); err != nil {
			slog.Error("admin: approve account", "account_id", id, "error", err)
		} else {
			h.adminAudit(ctx, "account", id, "approve", "")
			if h.mailer != nil && account != nil && account.Email != "" && account.EmailVerified {
				_ = h.mailer.SendApprovalGranted(account.Locale, account.Email, account.Username)
			}
		}
	}
	http.Redirect(w, r, "/admin/pending", http.StatusFound)
}

// AdminRejectAccount rejects (deletes) a pending account.
func (h *Handler) AdminRejectAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id != "" {
		account, _ := h.store.GetAccount(ctx, id)
		if err := h.store.RejectAccount(ctx, id); err != nil {
			slog.Error("admin: reject account", "account_id", id, "error", err)
		} else {
			h.adminAudit(ctx, "account", id, "reject", "")
			if h.mailer != nil && account != nil && account.Email != "" && account.EmailVerified {
				_ = h.mailer.SendApprovalRejected(account.Locale, account.Email, account.Username)
			}
		}
	}
	http.Redirect(w, r, "/admin/pending", http.StatusFound)
}

// AdminPendingApprovals shows pending account approvals.
func (h *Handler) AdminPendingApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	pendingAccounts, err := h.store.GetPendingApprovals(ctx)
	if err != nil {
		slog.Error("admin: pending approvals", "error", err)
		h.renderError(w, r, "Error fetching pending approvals")
		return
	}

	data := map[string]interface{}{
		"Accounts":   pendingAccounts,
		"Account":    account,
		"CSRFToken":  getCSRFToken(ctx),
		"IsPending":  true,
		"Pagination": adminPagination{Page: 1, Pages: 1, Total: len(pendingAccounts)},
	}

	h.renderPage(w, r, "Admin - Pending Approvals", data, "admin_accounts.html")
}

// Admin Invites

// AdminInvitesList shows all invites.
func (h *Handler) AdminInvitesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	invites, err := h.store.ListInvites(ctx)
	if err != nil {
		slog.Error("admin: list invites", "error", err)
		h.renderError(w, r, "Error fetching invites")
		return
	}

	data := map[string]interface{}{
		"Invites":   invites,
		"Account":   account,
		"CSRFToken": getCSRFToken(ctx),
	}

	h.renderPage(w, r, "Admin - Invites", data, "admin_invites.html")
}

// AdminInvitesCreate creates a new invite.
func (h *Handler) AdminInvitesCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/invites", http.StatusFound)
		return
	}

	ctx := r.Context()
	account := getAuthAccount(ctx)
	if account == nil {
		http.Redirect(w, r, "/admin/invites", http.StatusFound)
		return
	}

	code := r.FormValue("code")
	maxUsesStr := r.FormValue("max_uses")
	maxUses := 0
	if maxUsesStr != "" {
		if n, err := strconv.Atoi(maxUsesStr); err == nil && n > 0 {
			maxUses = n
		}
	}

	expiresStr := r.FormValue("expires_at")
	var expiresAt *time.Time
	if expiresStr != "" {
		if t, err := time.Parse("2006-01-02", expiresStr); err == nil {
			expiresAt = &t
		}
	}

	if invite, err := accounts.CreateInvite(ctx, h.store, account.ID, code, maxUses, expiresAt); err != nil {
		slog.Error("admin: create invite", "error", err)
	} else {
		h.adminAudit(ctx, "invite", invite.ID, "create", "")
	}

	http.Redirect(w, r, "/admin/invites", http.StatusFound)
}

// AdminInvitesRevoke revokes an invite.
func (h *Handler) AdminInvitesRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id != "" {
		if err := h.store.DeleteInvite(r.Context(), id); err != nil {
			slog.Error("admin: revoke invite", "error", err)
		} else {
			h.adminAudit(r.Context(), "invite", id, "revoke", "")
		}
	}
	http.Redirect(w, r, "/admin/invites", http.StatusFound)
}

// Admin federation policy

// AdminDomainBlocks shows domain blocks.
func (h *Handler) AdminDomainBlocks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	blocks, err := h.store.ListDomainBlocks(ctx)
	if err != nil {
		slog.Error("admin: list domain blocks", "error", err)
		h.renderError(w, r, "Error fetching domain blocks")
		return
	}
	allows, err := h.store.ListDomainAllows(ctx)
	if err != nil {
		h.renderError(w, r, "Error fetching domain allows")
		return
	}
	state, err := h.store.GetFederationPolicyState(ctx)
	if err != nil {
		h.renderError(w, r, "Error fetching federation policy")
		return
	}
	commercial, _ := h.store.ListCommercialDomainBlocks(ctx)
	conflicts := make(map[string]string)
	for _, block := range blocks {
		if block.Severity == "suspend" {
			conflicts[block.Domain] = "manual suspension wins"
		}
	}
	if state.CommercialBigTechEnabled {
		for _, block := range commercial {
			if conflicts[block.Domain] == "" {
				conflicts[block.Domain] = "allowed exception to automatic block"
			}
		}
	}

	data := map[string]interface{}{
		"DomainBlocks":            blocks,
		"DomainAllows":            allows,
		"Policy":                  state,
		"CommercialCount":         len(commercial),
		"ConfiguredCommercialURL": h.cfg.Federation.CommercialBigTechListURL,
		"MaintenanceEnabled":      h.cfg.BackgroundMaintenance,
		"AllowConflicts":          conflicts,
		"Account":                 account,
		"CSRFToken":               getCSRFToken(ctx),
	}

	h.renderPage(w, r, "Admin - Federation policy", data, "admin_domain_blocks.html")
}

func (h *Handler) AdminFederationMode(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("mode")
	if err := h.store.SetFederationMode(r.Context(), mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.adminAudit(r.Context(), "federation_policy", h.domain, "mode_"+mode, "")
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusSeeOther)
}

func (h *Handler) AdminDomainAllowCreate(w http.ResponseWriter, r *http.Request) {
	allow := &models.DomainAllow{Domain: r.FormValue("domain")}
	if err := h.store.CreateDomainAllow(r.Context(), allow); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.adminAudit(r.Context(), "domain", allow.Domain, "allow", "")
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusSeeOther)
}

func (h *Handler) AdminDomainAllowDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	allow, _ := h.store.GetDomainAllow(r.Context(), id)
	if err := h.store.DeleteDomainAllow(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if allow != nil {
		h.adminAudit(r.Context(), "domain", allow.Domain, "unallow", "")
	}
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusSeeOther)
}

func (h *Handler) AdminFederationImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	var formErr error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		formErr = r.ParseMultipartForm(1 << 20)
	} else {
		formErr = r.ParseForm()
	}
	if formErr != nil {
		http.Error(w, "Invalid import form", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") == "1" {
		h.confirmFederationImport(w, r)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Invalid import file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	kind := r.FormValue("kind")
	type previewRow struct{ Domain, Detail, Value string }
	rows := make([]previewRow, 0)
	if kind == "allow" {
		var allows []*models.DomainAllow
		allows, err = federation.ParseDomainAllowImport(file)
		if err == nil {
			for _, allow := range allows {
				var domain string
				domain, err = storage.NormalizeFederationDomain(allow.Domain)
				if err != nil {
					break
				}
				rows = append(rows, previewRow{Domain: domain, Value: domain})
			}
		}
	} else {
		var blocks []*models.DomainBlock
		blocks, err = federation.ParseDomainBlockImport(file)
		if err == nil {
			for _, block := range blocks {
				var domain string
				domain, err = storage.NormalizeFederationDomain(block.Domain)
				if err != nil {
					break
				}
				switch block.Severity {
				case "noop", "silence", "suspend":
				default:
					err = fmt.Errorf("invalid severity %q", block.Severity)
				}
				if err != nil {
					break
				}
				block.Domain = domain
				encoded, marshalErr := json.Marshal(block)
				if marshalErr != nil {
					err = marshalErr
					break
				}
				rows = append(rows, previewRow{Domain: domain, Detail: block.Severity, Value: base64.RawURLEncoding.EncodeToString(encoded)})
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.renderPage(w, r, "Admin - Review federation import", map[string]interface{}{"Account": getAuthAccount(r.Context()), "CSRFToken": getCSRFToken(r.Context()), "Kind": kind, "Rows": rows}, "admin_federation_import_preview.html")
}

func (h *Handler) confirmFederationImport(w http.ResponseWriter, r *http.Request) {
	kind, values := r.FormValue("kind"), r.Form["rows"]
	if len(values) == 0 {
		http.Error(w, "Select at least one domain", http.StatusBadRequest)
		return
	}
	inserted, err := 0, error(nil)
	if kind == "allow" {
		allows := make([]*models.DomainAllow, 0, len(values))
		for _, value := range values {
			allows = append(allows, &models.DomainAllow{Domain: value})
		}
		inserted, err = h.store.ImportDomainAllows(r.Context(), allows)
	} else {
		blocks := make([]*models.DomainBlock, 0, len(values))
		for _, value := range values {
			body, decodeErr := base64.RawURLEncoding.DecodeString(value)
			block := new(models.DomainBlock)
			if decodeErr != nil {
				err = decodeErr
				break
			}
			if decodeErr = json.Unmarshal(body, block); decodeErr != nil {
				err = decodeErr
				break
			}
			blocks = append(blocks, block)
		}
		if err == nil {
			inserted, err = h.store.ImportDomainBlocks(r.Context(), blocks)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.adminAudit(r.Context(), "federation_policy", kind, "import", fmt.Sprintf("inserted %d", inserted))
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusSeeOther)
}

func (h *Handler) AdminCommercialBigTech(w http.ResponseWriter, r *http.Request) {
	action := r.FormValue("action")
	var err error
	switch action {
	case "disable":
		err = h.store.SetCommercialBigTechEnabled(r.Context(), false)
	case "refresh", "enable":
		var syncer *federation.BigTechListSyncer
		syncer, err = federation.NewBigTechListSyncer(h.store, h.cfg.Federation.CommercialBigTechListURL)
		if err == nil {
			err = syncer.Refresh(r.Context())
		}
		if err == nil && action == "enable" {
			err = h.store.SetCommercialBigTechEnabled(r.Context(), true)
			if err == nil && h.cfg.BackgroundMaintenance {
				err = h.store.EnqueueJob(r.Context(), &models.Job{Kind: federation.CommercialBigTechRefreshJobKind, Payload: "{}", RunAt: time.Now().Add(24 * time.Hour), MaxAttempts: 3, Priority: -10})
			}
		}
	default:
		err = fmt.Errorf("invalid action")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.adminAudit(r.Context(), "federation_policy", "commercial_bigtech", action, "")
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusSeeOther)
}

// AdminDomainBlockCreate creates a new domain block.
func (h *Handler) AdminDomainBlockCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/domain_blocks", http.StatusFound)
		return
	}

	domain := r.FormValue("domain")
	severity := r.FormValue("severity")
	comment := r.FormValue("comment")
	on := func(v string) bool { return v == "on" || v == "true" || v == "1" }

	block := &models.DomainBlock{
		Domain:        domain,
		Severity:      severity,
		Comment:       comment,
		Public:        on(r.FormValue("public")),
		RejectMedia:   on(r.FormValue("reject_media")),
		RejectReports: on(r.FormValue("reject_reports")),
		Obfuscate:     on(r.FormValue("obfuscate")),
	}
	if err := h.store.CreateDomainBlock(r.Context(), block); err != nil {
		slog.Error("admin: create domain block", "domain", domain, "error", err)
		status := http.StatusInternalServerError
		if errors.Is(err, storage.ErrInvalidDomainBlock) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	h.adminAudit(r.Context(), "domain", block.Domain, "block_"+severity, comment)

	http.Redirect(w, r, "/admin/domain_blocks", http.StatusFound)
}

// AdminDomainBlockDelete removes a domain block.
func (h *Handler) AdminDomainBlockDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "Missing domain block", http.StatusBadRequest)
		return
	}
	block, _ := h.store.GetDomainBlockByID(r.Context(), id)
	if err := h.store.DeleteDomainBlock(r.Context(), id); err != nil {
		slog.Error("admin: delete domain block", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if block != nil {
		h.adminAudit(r.Context(), "domain", block.Domain, "unblock", "")
	}
	http.Redirect(w, r, "/admin/domain_blocks", http.StatusFound)
}

// Admin Bubble Instances

// AdminBubbles shows the bubble instances management page.
func (h *Handler) AdminBubbles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	instances, err := h.store.ListBubbleInstances(ctx)
	if err != nil {
		slog.Error("admin: list bubble instances", "error", err)
		h.renderError(w, r, "Error fetching bubble instances")
		return
	}

	data := map[string]interface{}{
		"BubbleInstances": instances,
		"Account":         account,
		"CSRFToken":       getCSRFToken(ctx),
	}

	h.renderPage(w, r, "Admin - Bubbles", data, "admin_bubbles.html")
}

// AdminBubbleCreate adds a domain to the bubble.
func (h *Handler) AdminBubbleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/bubbles", http.StatusFound)
		return
	}

	domain := r.FormValue("domain")
	note := r.FormValue("note")

	if domain != "" {
		b := &models.BubbleInstance{
			Domain: domain,
			Note:   note,
		}
		if err := h.store.CreateBubbleInstance(r.Context(), b); err != nil {
			slog.Error("admin: create bubble instance", "domain", domain, "error", err)
		} else {
			h.adminAudit(r.Context(), "bubble", domain, "add", note)
		}
	}

	http.Redirect(w, r, "/admin/bubbles", http.StatusFound)
}

// AdminBubbleDelete removes a domain from the bubble.
func (h *Handler) AdminBubbleDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id != "" {
		if err := h.store.DeleteBubbleInstance(r.Context(), id); err != nil {
			slog.Error("admin: delete bubble instance", "error", err)
		} else {
			h.adminAudit(r.Context(), "bubble", id, "remove", "")
		}
	}
	http.Redirect(w, r, "/admin/bubbles", http.StatusFound)
}

// Admin Instance Health

// instanceWithCounts adds follower/following counts to an Instance for display.
type instanceWithCounts struct {
	*models.Instance
	FollowersFromUs int  `json:"followers_from_us"` // our local accounts following them
	FollowingToUs   int  `json:"following_to_us"`   // their accounts following us
	Offline         bool `json:"offline"`           // unreachable past the offline window (delivery stopped)
}

// AdminInstancesList shows the instance health management page.
func (h *Handler) AdminInstancesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	instances, err := h.store.ListInstances(ctx)
	if err != nil {
		slog.Error("admin: list instances", "error", err)
		h.renderError(w, r, "Error fetching instances")
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	state := strings.TrimSpace(r.URL.Query().Get("state"))

	enriched := make([]instanceWithCounts, 0, len(instances))
	for _, inst := range instances {
		if query != "" && !strings.Contains(strings.ToLower(inst.Domain+" "+inst.Software+" "+inst.Version), query) {
			continue
		}
		// Offline = we have stopped delivering (continuously unreachable past the
		// window). Failing = a recent/open outage we are still delivering through.
		offline := !inst.Suspended && inst.UnreachableSince != nil &&
			time.Since(*inst.UnreachableSince) >= storage.InstanceOfflineWindow
		failing := !inst.Suspended && inst.UnreachableSince != nil && !offline
		matches := state == ""
		switch state {
		case "healthy":
			matches = !inst.Suspended && inst.UnreachableSince == nil
		case "degraded":
			matches = failing
		case "unreachable":
			matches = offline
		case "suspended":
			matches = inst.Suspended
		}
		if !matches {
			continue
		}
		followersFromUs, _ := h.store.CountFollowersByDomain(ctx, inst.Domain)
		followingToUs, _ := h.store.CountFollowingByDomain(ctx, inst.Domain)
		enriched = append(enriched, instanceWithCounts{
			Instance:        inst,
			FollowersFromUs: followersFromUs,
			FollowingToUs:   followingToUs,
			Offline:         offline,
		})
	}
	pagination, start, end := adminPaginate(r, len(enriched), 50)
	enriched = enriched[start:end]

	data := map[string]interface{}{
		"Instances": enriched,
		"Account":   account,
		"CSRFToken": getCSRFToken(ctx),
		"Query":     query, "State": state,
		"Pagination": pagination,
	}

	h.renderPage(w, r, "Admin - Instance Health", data, "admin_instances.html")
}

// AdminInstanceReset marks an instance as reachable again, clearing its
// unreachable status and backoff count.
func (h *Handler) AdminInstanceReset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}

	// Get the instance to find its domain
	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("admin: get instance for reset", "id", id, "error", err)
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}

	if err := h.store.MarkInstanceReachable(r.Context(), instance.Domain); err != nil {
		slog.Error("admin: reset instance", "domain", instance.Domain, "error", err)
	} else {
		h.adminAudit(r.Context(), "instance", instance.Domain, "reset", "")
	}

	http.Redirect(w, r, "/admin/instances", http.StatusFound)
}

// AdminInstanceToggleSuspend toggles the suspended flag on an instance.
func (h *Handler) AdminInstanceToggleSuspend(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}

	// Get current state
	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("admin: get instance for suspend toggle", "id", id, "error", err)
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}

	if err := h.store.SuspendInstance(r.Context(), id, !instance.Suspended); err != nil {
		slog.Error("admin: toggle instance suspend", "id", id, "error", err)
	} else {
		action := "suspend"
		if instance.Suspended {
			action = "unsuspend"
		}
		h.adminAudit(r.Context(), "instance", instance.Domain, action, "")
	}

	http.Redirect(w, r, "/admin/instances", http.StatusFound)
}

// AdminInstancePurgeConfirm requires the operator to type the affected domain.
func (h *Handler) AdminInstancePurgeConfirm(w http.ResponseWriter, r *http.Request) {
	instance, err := h.store.GetInstanceByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}
	h.renderPage(w, r, "Confirm instance purge - Admin", map[string]interface{}{"Account": getAuthAccount(r.Context()), "CSRFToken": getCSRFToken(r.Context()), "Instance": instance}, "admin_instance_purge.html")
}

// AdminInstancePurge removes all data related to a remote instance.
// This is useful when an instance changes its keys and we want to start fresh.
func (h *Handler) AdminInstancePurge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}

	// Get the instance for domain info (for logging)
	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("admin: get instance for purge", "id", id, "error", err)
		http.Redirect(w, r, "/admin/instances", http.StatusFound)
		return
	}
	if strings.TrimSpace(r.FormValue("confirm_domain")) != instance.Domain {
		h.renderPage(w, r, "Confirm instance purge - Admin", map[string]interface{}{"Account": getAuthAccount(r.Context()), "CSRFToken": getCSRFToken(r.Context()), "Instance": instance, "Error": "The domain did not match. No data was deleted."}, "admin_instance_purge.html")
		return
	}

	if err := h.store.DeleteInstanceData(r.Context(), id); err != nil {
		slog.Error("admin: purge instance data", "domain", instance.Domain, "error", err)
		h.renderError(w, r, "Could not purge instance data")
		return
	}

	slog.Info("admin: purged all data for instance", "domain", instance.Domain)
	h.adminAudit(r.Context(), "instance", instance.Domain, "purge", "all remote data removed")
	http.Redirect(w, r, "/admin/instances", http.StatusFound)
}

// Admin Settings

// AdminSettings shows instance settings and sends an SMTP test on POST.
func (h *Handler) AdminSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)
	if h.cfg == nil {
		h.renderError(w, r, "Configuration is unavailable")
		return
	}
	result, sendError := "", ""
	if r.Method == http.MethodPost {
		switch {
		case h.mailer == nil:
			sendError = "SMTP is not configured."
		case account.Email == "":
			sendError = "Add an email address to your account before testing SMTP."
		default:
			if err := h.mailer.Send(account.Email, "littleFedi SMTP test", "This test email confirms that littleFedi can send mail through the configured SMTP server."); err != nil {
				slog.Error("admin: SMTP test failed", "error", err)
				sendError = "The SMTP test failed. Check the server logs for details."
			} else {
				result = "Test email sent to " + account.Email + "."
			}
		}
	}
	data := map[string]interface{}{
		"Account": account, "CSRFToken": getCSRFToken(ctx),
		"Result": result, "Error": sendError,
		"LocalDomain": h.localDomain, "WebDomain": h.domain,
		"Registrations":        h.cfg != nil && h.cfg.Auth.Registrations,
		"InviteRequired":       h.cfg != nil && h.cfg.Auth.InviteRequired,
		"RequireApproval":      h.cfg != nil && h.cfg.Auth.RequireApproval,
		"MinPasswordLength":    h.cfg.Auth.MinPasswordLength,
		"OAuthScopes":          h.cfg.Auth.EnforceOAuthScopes,
		"MaxStatusChars":       h.cfg.Instance.MaxStatusChars,
		"QueueWorkers":         h.cfg.Queue.Workers,
		"QueueRetention":       h.cfg.Queue.TerminalJobRetention,
		"QueueStaleTimeout":    h.cfg.Queue.StaleRunningTimeout,
		"QueueMaintenance":     h.cfg.Queue.MaintenanceInterval,
		"SecureMode":           h.cfg.Federation.SecureMode,
		"MaxRetries":           models.DeliveryMaxAttempts,
		"ActorRefreshInterval": h.cfg.Federation.ActorRefreshInterval,
		"ActorStaleAge":        h.cfg.Federation.ActorStaleAge,
		"MediaCacheMode":       h.cfg.Media.CacheMode(),
		"MediaProxy":           h.cfg.Media.ProxyRemote,
		"MediaMaxSize":         h.cfg.Media.MaxFileSize,
		"StripEXIF":            h.cfg.Media.StripEXIF,
		"RenameMedia":          h.cfg.Media.RenameMedia,
		"MetricsEnabled":       h.cfg.Observability.MetricsEnabled,
		"LogLevel":             h.cfg.Observability.LogLevel,
		"SMTPConfigured":       h.cfg.SMTP.Host != "",
		"PushConfigured":       h.vapidPublicKey != "",
		"PruneRemoteDays":      h.cfg.Housekeeping.PruneRemoteStatusesAfterDays,
		"ServerPruneDays":      h.cfg.Housekeeping.PruneOwnStatusesAfterDays,
		"ServerPruneMinLikes":  h.cfg.Housekeeping.PruneOwnStatusesMinLikes,
		"ServerPruneMinBoosts": h.cfg.Housekeeping.PruneOwnStatusesMinBoosts,
		"ServerPruneInterval":  h.cfg.Housekeeping.Interval,
		"ServerPruneMaxPerRun": h.cfg.Housekeeping.MaxDeletePerRun,
	}

	h.renderPage(w, r, "Admin - Settings", data, "admin_settings.html")
}

// Admin Instance Profile

// AdminInstanceMeta renders the editable instance profile: the description
// text, rules, and contact address published on /about and by the instance
// APIs. Unlike /admin/settings this is not a view of the config file - it
// writes to the database, so changes apply without a restart.
func (h *Handler) AdminInstanceMeta(w http.ResponseWriter, r *http.Request) {
	h.renderInstanceMeta(w, r, "", "")
}

func (h *Handler) renderInstanceMeta(w http.ResponseWriter, r *http.Request, result, errMsg string) {
	ctx := r.Context()
	meta := h.instanceMeta(ctx)
	stored, _ := h.store.GetInstanceMeta(ctx)

	h.renderPage(w, r, "Admin - Instance profile", map[string]interface{}{
		"Account": getAuthAccount(ctx), "CSRFToken": getCSRFToken(ctx),
		"Result": result, "Error": errMsg,
		"InstanceName":        h.instanceName(),
		"ShortDescription":    meta.ShortDescription,
		"Description":         meta.Description,
		"ExtendedDescription": meta.ExtendedDescription,
		"TermsOfService":      meta.TermsOfService,
		"ContactEmail":        meta.ContactEmail,
		"Rules":               instancemeta.JoinRules(meta.Rules),
		// Saved reports whether these values are the operator's own or still
		// the config file's, which is the difference between editing the form
		// and editing littlefedi.toml.
		"Saved":     stored != nil,
		"UpdatedAt": meta.UpdatedAt,
	}, "admin_instance.html")
}

// AdminInstanceMetaSave persists the instance profile.
func (h *Handler) AdminInstanceMetaSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderInstanceMeta(w, r, "", "The form could not be read.")
		return
	}

	email := strings.TrimSpace(r.FormValue("contact_email"))
	if email != "" && !strings.Contains(email, "@") {
		h.renderInstanceMeta(w, r, "", "The contact address is not a valid email address.")
		return
	}

	meta := &models.InstanceMeta{
		ShortDescription:    strings.TrimSpace(r.FormValue("short_description")),
		Description:         strings.TrimSpace(r.FormValue("description")),
		ExtendedDescription: strings.TrimSpace(r.FormValue("extended_description")),
		TermsOfService:      strings.TrimSpace(r.FormValue("terms_of_service")),
		ContactEmail:        email,
		Rules:               instancemeta.JoinRules(strings.Split(r.FormValue("rules"), "\n")),
	}
	if err := h.store.SaveInstanceMeta(r.Context(), meta); err != nil {
		slog.Error("admin: save instance profile", "error", err)
		h.renderInstanceMeta(w, r, "", "The instance profile could not be saved.")
		return
	}

	h.adminAudit(r.Context(), "instance_profile", h.localDomain, "update", "")
	h.renderInstanceMeta(w, r, "Instance profile saved.", "")
}

// Admin Housekeeping

// AdminHousekeeping shows the housekeeping control panel.
func (h *Handler) AdminHousekeeping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	// Collect current housekeeping configuration info
	data := map[string]interface{}{
		"Account":           account,
		"CSRFToken":         getCSRFToken(ctx),
		"PruneRemoteDays":   h.cfg.Housekeeping.PruneRemoteStatusesAfterDays,
		"PruneOwnDays":      h.cfg.Housekeeping.PruneOwnStatusesAfterDays,
		"PruneOwnMinLikes":  h.cfg.Housekeeping.PruneOwnStatusesMinLikes,
		"PruneOwnMinBoosts": h.cfg.Housekeeping.PruneOwnStatusesMinBoosts,
		"PruneMaxPerRun":    h.cfg.Housekeeping.MaxDeletePerRun,
		"MediaPath":         h.mediaPath,
		"HasHousekeeper":    h.housekeeper != nil,
		"BlogCapable":       h.blogCapable(),
	}

	// Check if there are any results to display from a previous action
	if msg := r.URL.Query().Get("result"); msg != "" {
		data["Result"] = msg
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		data["Error"] = errMsg
	}

	h.renderPage(w, r, "Admin - Housekeeping", data, "admin_housekeeping.html")
}

// AdminHousekeepingPruneRemote prunes remote statuses older than the configured threshold.
func (h *Handler) AdminHousekeepingPruneRemote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.housekeeper == nil {
		http.Redirect(w, r, "/admin/housekeeping?error=no-housekeeper", http.StatusFound)
		return
	}

	if err := h.housekeeper.PruneRemoteStatuses(ctx); err != nil {
		slog.Error("admin: housekeeping prune remote", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=prune-remote-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - remote status pruning triggered")
	h.adminAudit(ctx, "housekeeping", "remote_statuses", "prune", "")
	http.Redirect(w, r, "/admin/housekeeping?result=remote-pruned", http.StatusFound)
}

// AdminHousekeepingPruneOwn prunes own low-interaction statuses.
func (h *Handler) AdminHousekeepingPruneOwn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.housekeeper == nil {
		http.Redirect(w, r, "/admin/housekeeping?error=no-housekeeper", http.StatusFound)
		return
	}

	if err := h.housekeeper.PruneOwnStatuses(ctx); err != nil {
		slog.Error("admin: housekeeping prune own", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=prune-own-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - own status pruning triggered")
	h.adminAudit(ctx, "housekeeping", "local_statuses", "prune", "")
	http.Redirect(w, r, "/admin/housekeeping?result=own-pruned", http.StatusFound)
}

// AdminHousekeepingPruneStaleMedia removes unattached media older than 1 hour.
// For admin-triggered cleanup, the threshold is shorter (1h) than the automatic
// background job (24h) so that the admin can clean up abandoned uploads promptly.
func (h *Handler) AdminHousekeepingPruneStaleMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	count, err := medialib.PruneStaleMedia(ctx, h.store, h.mediaPath, 1*time.Hour, h.mediaStore)
	if err != nil {
		slog.Error("admin: housekeeping prune stale media", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=stale-media-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - stale media pruned", "count", count)
	h.adminAudit(ctx, "housekeeping", "stale_media", "prune", fmt.Sprintf("removed %d", count))
	http.Redirect(w, r, "/admin/housekeeping?result=stale-media-pruned&count="+strconv.Itoa(count), http.StatusFound)
}

// AdminHousekeepingRemoveOrphanedMedia removes media whose status no longer exists.
func (h *Handler) AdminHousekeepingRemoveOrphanedMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	count, err := medialib.RemoveOrphanedMedia(ctx, h.store, h.mediaPath, h.mediaStore)
	if err != nil {
		slog.Error("admin: housekeeping remove orphaned media", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=orphaned-media-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - orphaned media removed", "count", count)
	h.adminAudit(ctx, "housekeeping", "orphaned_media", "remove", fmt.Sprintf("removed %d", count))
	http.Redirect(w, r, "/admin/housekeeping?result=orphaned-media-removed&count="+strconv.Itoa(count), http.StatusFound)
}

// AdminHousekeepingRemoveUnreferencedMedia removes media files from disk with no DB record.
func (h *Handler) AdminHousekeepingRemoveUnreferencedMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	count, err := medialib.RemoveUnreferencedMediaFromDisk(ctx, h.store, h.mediaPath)
	if err != nil {
		slog.Error("admin: housekeeping remove unreferenced media", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=unreferenced-media-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - unreferenced media removed", "count", count)
	h.adminAudit(ctx, "housekeeping", "unreferenced_media", "remove", fmt.Sprintf("removed %d", count))
	http.Redirect(w, r, "/admin/housekeeping?result=unreferenced-media-removed&count="+strconv.Itoa(count), http.StatusFound)
}

// AdminRegenerateAllBlogs queues a full static-site rebuild for every local
// account currently allowed to blog.
func (h *Handler) AdminRegenerateAllBlogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !h.blogCapable() {
		http.Redirect(w, r, "/admin/housekeeping?error=blog-not-enabled", http.StatusFound)
		return
	}

	count, err := h.enqueueBlogRebuildAll(ctx)
	if err != nil {
		slog.Error("admin: regenerate all blogs", "error", err)
		http.Redirect(w, r, "/admin/housekeeping?error=regenerate-blogs-failed", http.StatusFound)
		return
	}

	slog.Info("admin: housekeeping - all blogs queued for regeneration", "count", count)
	h.adminAudit(ctx, "housekeeping", "blogs", "regenerate_all", fmt.Sprintf("queued %d", count))
	http.Redirect(w, r, "/admin/housekeeping?result=blogs-queued&count="+strconv.Itoa(count), http.StatusFound)
}

// Admin Backup

type backupEntry struct {
	Timestamp     string
	Driver        string
	MediaIncluded bool
	MediaCount    int
}

type backupManifest struct {
	Version        int       `json:"version"`
	Timestamp      time.Time `json:"timestamp"`
	DatabaseDriver string    `json:"database_driver"`
	MediaIncluded  bool      `json:"media_included"`
	MediaCount     int       `json:"media_count"`
}

func (h *Handler) AdminBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := getAuthAccount(ctx)

	backupPath := h.cfg.Backup.ResolvedPath()

	var backups []backupEntry
	if entries, err := os.ReadDir(backupPath); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			manifestPath := filepath.Join(backupPath, e.Name(), "manifest.json")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				continue
			}
			var m backupManifest
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			backups = append(backups, backupEntry{
				Timestamp:     m.Timestamp.Format("2006-01-02 15:04:05 UTC"),
				Driver:        m.DatabaseDriver,
				MediaIncluded: m.MediaIncluded,
				MediaCount:    m.MediaCount,
			})
		}
		sort.Slice(backups, func(i, j int) bool {
			return backups[i].Timestamp > backups[j].Timestamp
		})
	}

	var nextBackup string
	if h.cfg.Backup.Enabled && h.cfg.BackgroundMaintenance {
		interval := h.cfg.Backup.IntervalDuration()
		if t, ok := backup.LastBackupTime(backupPath); ok {
			nextBackup = t.Add(interval).Format("2006-01-02 15:04:05 UTC")
		}
	}

	data := map[string]interface{}{
		"Account":            account,
		"CSRFToken":          getCSRFToken(ctx),
		"BackupEnabled":      h.cfg.Backup.Enabled,
		"MaintenanceEnabled": h.cfg.BackgroundMaintenance,
		"MediaBackupAllowed": h.cfg.Media.OwnedBackend() == "local",
		"BackupInterval":     h.cfg.Backup.IntervalDuration().String(),
		"BackupPath":         backupPath,
		"BackupIncludeMedia": h.cfg.Backup.IncludeMedia,
		"BackupRetain":       h.cfg.Backup.Retain,
		"NextBackup":         nextBackup,
		"Backups":            backups,
	}

	if msg := r.URL.Query().Get("result"); msg != "" {
		data["Result"] = msg
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		data["Error"] = errMsg
	}

	h.renderPage(w, r, "Admin - Backup", data, "admin_backup.html")
}

func (h *Handler) AdminBackupCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	includeMedia := r.FormValue("media") == "true"
	if includeMedia && h.cfg.Media.OwnedBackend() != "local" {
		http.Redirect(w, r, "/admin/backup?error=media-backup-requires-local-storage", http.StatusFound)
		return
	}

	payload, err := json.Marshal(map[string]bool{"include_media": includeMedia})
	if err != nil {
		http.Redirect(w, r, "/admin/backup?error=payload-encode-failed", http.StatusFound)
		return
	}

	job := &models.Job{
		Kind:        "backup",
		Payload:     string(payload),
		RunAt:       time.Now(),
		MaxAttempts: 1,
		State:       "queued",
	}

	if err := h.store.EnqueueJob(ctx, job); err != nil {
		slog.Error("admin: backup enqueue", "error", err)
		http.Redirect(w, r, "/admin/backup?error=enqueue-failed", http.StatusFound)
		return
	}

	mediaLabel := "database"
	if includeMedia {
		mediaLabel = "with-media"
	}
	slog.Info("admin: backup job enqueued", "include_media", includeMedia)
	h.adminAudit(ctx, "backup", mediaLabel, "create", "")
	http.Redirect(w, r, "/admin/backup?result=backup-queued", http.StatusFound)
}

func HandleBackupJob(store storage.Storage, cfg *config.Config, domain string) func(ctx context.Context, payload string) error {
	return func(ctx context.Context, payload string) error {
		var p struct {
			IncludeMedia bool `json:"include_media"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}

		backupCfg := *cfg
		backupCfg.Backup.IncludeMedia = p.IncludeMedia
		path, err := backup.CreateBackup(ctx, store, &backupCfg)
		if err != nil {
			slog.Error("backup: job failed", "error", err, "include_media", p.IncludeMedia)
			mail.NotifyAdminsBackup(ctx, store, domain, false, "")
			return err
		}
		slog.Info("backup: job completed", "path", path, "include_media", p.IncludeMedia)
		mail.NotifyAdminsBackup(ctx, store, domain, true, path)
		return nil
	}
}
