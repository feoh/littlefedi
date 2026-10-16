package webui

// Server-rendered scheduled-posts UI: list pending scheduled posts, cancel,
// and reschedule. Every action is a real POST form with a csrf_token so the
// feature works without JavaScript.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// scheduledDateTimeLocalFormat matches the HTML datetime-local input format.
const scheduledDateTimeLocalFormat = "2006-01-02T15:04"

// ScheduledPostView is the view model for one pending scheduled post.
type ScheduledPostView struct {
	ID               string
	Preview          string
	ScheduledAt      time.Time
	ScheduledAtLocal string // server-local, the no-JS value of <input type="datetime-local">
	ScheduledAtUTC   string // RFC3339, for the browser to render in the viewer's timezone
}

// ScheduledPostsData is the view model for the scheduled-posts page.
type ScheduledPostsData struct {
	Title     string
	Account   *models.Account
	Posts     []ScheduledPostView
	CSRFToken string
	Error     string
}

func scheduledPostView(v *models.ScheduledStatus) ScheduledPostView {
	var input app.StatusInput
	_ = json.Unmarshal([]byte(v.ParamsJSON), &input)
	preview := truncateText(input.Content, 120)
	if preview == "" {
		preview = "(no text)"
	}
	local := v.ScheduledAt.Local()
	return ScheduledPostView{
		ID:               v.ID,
		Preview:          preview,
		ScheduledAt:      local,
		ScheduledAtLocal: local.Format(scheduledDateTimeLocalFormat),
		ScheduledAtUTC:   formatTimeISO(v.ScheduledAt),
	}
}

// ScheduledPostsPage lists the user's pending scheduled posts.
func (h *Handler) ScheduledPostsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	scheduled, err := h.store.ListScheduledStatuses(r.Context(), account.ID, 40)
	if err != nil {
		slog.Error("webui: list scheduled statuses error", "error", err)
		h.renderError(w, r, "Error fetching scheduled posts")
		return
	}

	posts := make([]ScheduledPostView, 0, len(scheduled))
	for _, v := range scheduled {
		posts = append(posts, scheduledPostView(v))
	}

	data := &ScheduledPostsData{
		Title:     "Scheduled posts",
		Account:   account,
		Posts:     posts,
		CSRFToken: getCSRFToken(r.Context()),
		Error:     r.URL.Query().Get("error"),
	}
	h.renderPage(w, r, h.pageTitle(r, "scheduled.heading"), data, "scheduled.html")
}

// ScheduledPostCancelWeb cancels a pending scheduled post.
func (h *Handler) ScheduledPostCancelWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.app.CancelScheduledStatus(r.Context(), account.ID, chi.URLParam(r, "id")); err != nil {
		slog.Error("webui: cancel scheduled status error", "error", err)
	}
	http.Redirect(w, r, "/scheduled", http.StatusFound)
}

// ScheduledPostRescheduleWeb changes a pending scheduled post's publish time.
func (h *Handler) ScheduledPostRescheduleWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	newAt, ok := parseComposerScheduledAt(r)
	if !ok {
		http.Redirect(w, r, "/scheduled?error="+url.QueryEscape("Pick a new time"), http.StatusFound)
		return
	}
	if err := h.app.RescheduleStatus(r.Context(), account.ID, id, newAt); err != nil {
		http.Redirect(w, r, "/scheduled?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/scheduled", http.StatusFound)
}
