package webui

import (
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const maxComposerVisualHTML = 1 << 20

// Compose Status

// ComposeStatus handles creating a new status (htmx post).
func (h *Handler) ComposeStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := h.parseComposeForm(w, r); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	statusSource, ok := composerStatusSource(w, r)
	if !ok {
		return
	}
	spoilerText := r.FormValue("spoiler_text")
	visibility := r.FormValue("visibility")
	inReplyToID := r.FormValue("in_reply_to_id")
	redraftStatusID := r.FormValue("redraft_status_id")
	quotedStatusID := r.FormValue("quoted_status_id")
	var redraftOriginal *models.Status
	if redraftStatusID != "" {
		var err error
		redraftOriginal, err = h.store.GetStatus(r.Context(), redraftStatusID)
		if err != nil || redraftOriginal.AccountID != account.ID || redraftOriginal.BoostOfID != nil && *redraftOriginal.BoostOfID != "" {
			http.Error(w, "Invalid redraft", http.StatusForbidden)
			return
		}
	}
	mediaIDs := r.Form["media_ids"]
	pollOptions := []string{}
	for _, v := range r.Form["poll_options"] {
		if v = strings.TrimSpace(v); v != "" {
			pollOptions = append(pollOptions, v)
		}
	}
	if len(mediaIDs)+h.composerUploadCount(r) > h.maxMedia() {
		http.Error(w, "Too many media attachments", http.StatusUnprocessableEntity)
		return
	}
	uploadedMediaIDs, err := h.createComposerMedia(r, account)
	if err != nil {
		slog.Error("webui: media upload error", "error", err)
		http.Error(w, composerUploadErrorMessage(err), http.StatusUnprocessableEntity)
		return
	}
	mediaIDs = append(mediaIDs, uploadedMediaIDs...)

	if strings.TrimSpace(statusSource) == "" && len(mediaIDs) == 0 && len(pollOptions) == 0 {
		http.Error(w, "Status is empty", http.StatusUnprocessableEntity)
		return
	}

	if visibility == "" {
		visibility = "public"
	}

	vis := models.VisibilityPublic
	switch visibility {
	case "unlisted":
		vis = models.VisibilityUnlisted
	case "private":
		vis = models.VisibilityPrivate
	case "direct":
		vis = models.VisibilityDirect
	case "local":
		vis = models.VisibilityLocal
	case "local_unlisted":
		vis = models.VisibilityLocalUnlisted
	}

	var inReplyTo *string
	if inReplyToID != "" {
		inReplyTo = &inReplyToID
	}
	var inReplyToURI *string
	if redraftOriginal != nil {
		inReplyTo = redraftOriginal.InReplyToID
		inReplyToURI = redraftOriginal.InReplyToURI
		if inReplyTo != nil {
			parent, parentErr := h.store.GetStatus(r.Context(), *inReplyTo)
			if parentErr != nil || !h.app.CanViewStatus(r.Context(), parent, account) {
				inReplyTo = nil
			} else if inReplyToURI == nil {
				parentURI := parent.URI
				inReplyToURI = &parentURI
			}
		}
		quotedStatusID = ""
	}
	isBlogPost := h.blogAllowed(account) && r.FormValue("is_blog_post") == "on"
	if redraftOriginal != nil {
		if _, present := r.Form["is_blog_post"]; !present {
			isBlogPost = redraftOriginal.IsBlogPost
		}
	}

	input := app.StatusInput{
		Content:        statusSource,
		ContentType:    composerContentType(r),
		Summary:        spoilerText,
		Sensitive:      r.FormValue("sensitive") == "on",
		Visibility:     vis,
		Language:       r.FormValue("language"),
		InReplyToID:    inReplyTo,
		InReplyToURI:   inReplyToURI,
		QuotedStatusID: quotedStatusID,
		MediaIDs:       mediaIDs,
		PollOptions:    pollOptions,
		PollExpiresIn:  time.Duration(parseWebInt(r.FormValue("poll_expires_in"), 86400)) * time.Second,
		PollMultiple:   r.FormValue("poll_multiple") == "on",
		// Gated server-side, not just by hiding the composer checkbox, so a
		// crafted POST can't set the flag on a build/config that can't serve it.
		IsBlogPost: isBlogPost,
		// Sanitized in the app layer, which also resolves collisions; an
		// unusable value falls back to the configured slug template.
		BlogSlug: r.FormValue("blog_slug"),
	}

	// A free date/time (expires_at) takes precedence over the duration preset
	// select (expires_in) when both are present.
	if at, ok := parseComposerExpiresAt(r); ok {
		if !at.After(time.Now()) {
			http.Error(w, "expires_at must be in the future", http.StatusUnprocessableEntity)
			return
		}
		input.ExpiresIn = time.Until(at)
	} else {
		input.ExpiresIn = time.Duration(parseWebInt(r.FormValue("expires_in"), 0)) * time.Second
	}

	if scheduledAt, ok := parseComposerScheduledAt(r); ok && redraftOriginal == nil {
		if _, err := h.app.ScheduleStatus(r.Context(), account, input, scheduledAt); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Redirect(w, r, "/scheduled", http.StatusFound)
		return
	}

	created, err := h.app.CreateStatus(r.Context(), account, input)
	if err != nil {
		slog.Error("webui: create status error", "error", err)
		http.Error(w, "Error creating status", http.StatusInternalServerError)
		return
	}
	if redraftOriginal != nil {
		if err := h.app.DeleteStatus(r.Context(), account, redraftOriginal.ID); err != nil {
			slog.Error("webui: redraft delete error", "old_status", redraftOriginal.ID, "new_status", created.ID, "error", err)
			http.Redirect(w, r, "/statuses/"+created.ID+"/redraft-error?original="+url.QueryEscape(redraftOriginal.ID), http.StatusSeeOther)
			return
		}
		if redraftOriginal.BlogSlug != "" {
			if err := h.store.SetStatusBlog(r.Context(), created.ID, created.IsBlogPost, redraftOriginal.BlogSlug); err != nil {
				slog.Error("webui: redraft blog slug restore error", "status", created.ID, "error", err)
			}
		}
		http.Redirect(w, r, "/@"+account.Username+"/"+created.ID, http.StatusSeeOther)
		return
	}

	// Redirect back to the referrer (home, thread, or profile)
	// hx-boost on body will make this feel smooth via AJAX
	redirectTo := safeReturnPath(r, r.Referer(), "/")
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// parseComposerLocalTime reads a composer datetime-local field plus an
// optional JS-supplied "*_offset_minutes" field (from Date.getTimezoneOffset())
// and returns the equivalent UTC time. Without JS the offset field is absent
// and the local value is treated as server-local time, which still works,
// just assumes the server's timezone.
func parseComposerLocalTime(r *http.Request, field, offsetField string) (time.Time, bool) {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", raw, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	if offsetStr := r.FormValue(offsetField); offsetStr != "" {
		if offsetMinutes, err := strconv.Atoi(offsetStr); err == nil {
			// getTimezoneOffset() returns minutes *behind* UTC (e.g. UTC+2 -> -120),
			// so UTC = local wall time + offset.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC).Add(time.Duration(offsetMinutes) * time.Minute)
		}
	}
	return t, true
}

func parseComposerScheduledAt(r *http.Request) (time.Time, bool) {
	return parseComposerLocalTime(r, "scheduled_at", "scheduled_at_offset_minutes")
}

// parseComposerExpiresAt reads the composer's free-choice expiry datetime, an
// alternative to the expires_in duration preset select.
func parseComposerExpiresAt(r *http.Request) (time.Time, bool) {
	return parseComposerLocalTime(r, "expires_at", "expires_at_offset_minutes")
}

func parseWebInt(v string, d int) int {
	n, e := strconv.Atoi(v)
	if e != nil {
		return d
	}
	return n
}

func (h *Handler) VotePoll(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	if a == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	var choices []int
	for _, v := range r.Form["choices"] {
		n, e := strconv.Atoi(v)
		if e == nil {
			choices = append(choices, n)
		}
	}
	pollID := chi.URLParam(r, "id")
	poll, e := h.store.GetPoll(r.Context(), pollID, a.ID)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	status, e := h.store.GetStatus(r.Context(), poll.StatusID)
	if e != nil || !h.app.CanViewStatus(r.Context(), status, a) {
		http.NotFound(w, r)
		return
	}
	p, e := h.app.VotePoll(r.Context(), a, pollID, choices)
	if e != nil {
		http.Error(w, e.Error(), http.StatusUnprocessableEntity)
		return
	}
	status, e = h.store.GetStatus(r.Context(), p.StatusID)
	if e != nil {
		http.Redirect(w, r, safeReturnPath(r, r.Referer(), "/"), http.StatusFound)
		return
	}
	h.returnStatusComponent(w, r, status, a)
}

// Interaction Actions

func (h *Handler) FavouriteStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	status, err := h.app.ToggleFavourite(r.Context(), account, statusID)
	if err != nil {
		slog.Error("webui: favourite error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", "status-interactions-"+statusID)
	}

	h.returnStatusComponent(w, r, status, account)
}

func (h *Handler) EmojiReactionStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	statusID := chi.URLParam(r, "id")
	status, err := h.app.ToggleEmojiReaction(r.Context(), account, statusID, r.FormValue("emoji"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	h.returnStatusComponent(w, r, status, account)
}

func (h *Handler) ReblogStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	status, err := h.app.ToggleReblog(r.Context(), account, statusID)
	if err != nil {
		slog.Error("webui: reblog error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.returnStatusComponent(w, r, status, account)
}

func (h *Handler) DeleteStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	if err := h.app.DeleteStatus(r.Context(), account, statusID); err != nil {
		slog.Error("webui: delete status error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", "status-deleted")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

// EditStatusPage renders the composer pre-filled with one of the caller's own
// posts so it can be edited (Mastodon-style edit).
func (h *Handler) EditStatusPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || status.AccountID != account.ID {
		h.renderError(w, r, "Post not found")
		return
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		h.renderError(w, r, "Boosts cannot be edited")
		return
	}

	media, _ := h.store.GetMediaForStatus(r.Context(), status.ID)

	var draftExpiresAt string
	if status.ExpiresAt != nil {
		draftExpiresAt = formatTime(*status.ExpiresAt)
	}

	data := &PageData{
		Title:            "Edit post",
		Account:          account,
		CSRFToken:        getCSRFToken(r.Context()),
		EditingID:        status.ID,
		DraftContent:     status.ContentSource,
		DraftHTML:        status.ContentHTML,
		DraftMarkdown:    status.ContentType == content.ContentTypeMarkdown,
		DraftMarkdownSet: true,
		DraftSummary:     html.UnescapeString(status.Summary),
		DraftSensitive:   status.Sensitive,
		DraftVisibility:  status.Visibility,
		DraftLanguage:    status.Language,
		DraftIsBlogPost:  status.IsBlogPost,
		DraftBlogSlug:    status.BlogSlug,
		CanEditBlogPost:  status.Visibility == models.VisibilityPublic && (status.InReplyToID == nil || *status.InReplyToID == ""),
		DraftExpiresAt:   draftExpiresAt,
		ExistingMedia:    media,
	}
	h.renderPage(w, r, h.pageTitle(r, "page.edit_post"), data, "edit_status.html")
}

// EditStatusSubmit applies an edit to one of the caller's own posts.
func (h *Handler) EditStatusSubmit(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.parseComposeForm(w, r); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	statusSource, ok := composerStatusSource(w, r)
	if !ok {
		return
	}

	mediaIDs := r.Form["media_ids"]
	if mediaIDs == nil {
		mediaIDs = []string{}
	}
	if len(mediaIDs)+h.composerUploadCount(r) > h.maxMedia() {
		http.Error(w, "Too many media attachments", http.StatusUnprocessableEntity)
		return
	}
	// Kept attachments carry an editable alt text field next to their
	// checkbox, named after the media ID.
	mediaDescriptions := make(map[string]string, len(mediaIDs))
	for _, mediaID := range mediaIDs {
		values, ok := r.Form["media_description_existing_"+mediaID]
		if !ok || len(values) == 0 {
			continue
		}
		if utf8.RuneCountInString(strings.TrimSpace(values[0])) > models.MaxMediaDescriptionLength {
			http.Error(w, fmt.Sprintf("description is too long (maximum %d characters)", models.MaxMediaDescriptionLength), http.StatusUnprocessableEntity)
			return
		}
		mediaDescriptions[mediaID] = values[0]
	}
	uploadedMediaIDs, err := h.createComposerMedia(r, account)
	if err != nil {
		slog.Error("webui: edit media upload error", "error", err)
		http.Error(w, composerUploadErrorMessage(err), http.StatusUnprocessableEntity)
		return
	}
	mediaIDs = append(mediaIDs, uploadedMediaIDs...)

	// The expiry controls always submit an explicit choice: a free expires_at
	// datetime wins if present; otherwise expires_in decides ("" means "no
	// change"/untouched, "0" cancels an existing expiry, positive reschedules
	// it that far from now).
	var expiresIn *time.Duration
	if at, ok := parseComposerExpiresAt(r); ok {
		if !at.After(time.Now()) {
			http.Error(w, "expires_at must be in the future", http.StatusUnprocessableEntity)
			return
		}
		d := time.Until(at)
		expiresIn = &d
	} else if raw := r.FormValue("expires_in"); raw != "" {
		d := time.Duration(parseWebInt(raw, 0)) * time.Second
		expiresIn = &d
	}

	var isBlogPost *bool
	if h.blogAllowed(account) {
		if _, present := r.Form["is_blog_post"]; present {
			value := r.FormValue("is_blog_post") == "on"
			isBlogPost = &value
		}
	}

	_, err = h.app.EditStatus(r.Context(), account, id, app.EditStatusInput{
		Content:           statusSource,
		ContentType:       composerContentType(r),
		Summary:           r.FormValue("spoiler_text"),
		Sensitive:         r.FormValue("sensitive") == "on",
		Language:          r.FormValue("language"),
		MediaIDs:          mediaIDs,
		MediaDescriptions: mediaDescriptions,
		ExpiresIn:         expiresIn,
		IsBlogPost:        isBlogPost,
		BlogSlug:          r.FormValue("blog_slug"),
	})
	if err != nil {
		switch err.Error() {
		case storage.ErrStatusConflict.Error():
			http.Error(w, "Status changed; reload before editing", http.StatusConflict)
		case storage.ErrInvalidMedia.Error():
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		case "not your post":
			http.Error(w, "Forbidden", http.StatusForbidden)
		case "status is empty", "cannot edit a boost", "blog posts must be public", "blog posts cannot be replies":
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		case "blog posting not allowed":
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			slog.Error("webui: edit status error", "error", err)
			http.Error(w, "Error editing status", http.StatusInternalServerError)
		}
		return
	}

	http.Redirect(w, r, "/@"+account.Username+"/"+id, http.StatusFound)
}

// ComposerPreview renders the current source through the same pipeline used
// for published statuses. Visual HTML is first reduced to canonical Markdown.
func (h *Handler) ComposerPreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxComposerVisualHTML+(64<<10))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Preview is too large", http.StatusRequestEntityTooLarge)
		return
	}

	format := r.FormValue("format")
	source := r.FormValue("source")
	if len(source) > maxComposerVisualHTML || len(r.FormValue("wysiwyg_html")) > maxComposerVisualHTML {
		http.Error(w, "Preview is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var rendered string
	switch format {
	case "plain":
		rendered = string(content.SanitizeHTML(content.PlainTextToHTML(source)))
	case "markdown":
		rendered = content.MarkdownToHTML(source)
	case "wysiwyg":
		source = content.HTMLToMarkdown(r.FormValue("wysiwyg_html"))
		rendered = content.MarkdownToHTML(source)
	default:
		http.Error(w, "Invalid preview format", http.StatusUnprocessableEntity)
		return
	}

	rendered = content.LocalizeHashtagLinks(content.SanitizeHTMLStr(rendered))
	rendered = content.LocalizeInstanceLinks(rendered, h.domain)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Source string `json:"source"`
		HTML   string `json:"html"`
	}{Source: source, HTML: rendered})
}

// RedraftStatusPage opens a fresh composer without touching the original.
func (h *Handler) RedraftStatusPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || status.AccountID != account.ID {
		h.renderError(w, r, "Post not found")
		return
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		h.renderError(w, r, "Boosts cannot be redrafted")
		return
	}
	var parent *models.Status
	if status.InReplyToID != nil && *status.InReplyToID != "" {
		parent, _ = h.store.GetStatus(r.Context(), *status.InReplyToID)
		if parent != nil && h.app.CanViewStatus(r.Context(), parent, account) {
			h.app.EnrichStatus(r.Context(), parent, account)
		} else {
			parent = nil
		}
	}
	replyURI := ""
	if status.InReplyToURI != nil {
		replyURI = *status.InReplyToURI
	}

	data := &PageData{
		Account:          account,
		CSRFToken:        getCSRFToken(r.Context()),
		RedraftStatusID:  status.ID,
		RedraftReplyURI:  replyURI,
		ReplyTo:          parent,
		DraftContent:     status.ContentSource,
		DraftHTML:        status.ContentHTML,
		DraftMarkdown:    status.ContentType == content.ContentTypeMarkdown,
		DraftMarkdownSet: true,
		DraftSummary:     html.UnescapeString(status.Summary),
		DraftSensitive:   status.Sensitive,
		DraftVisibility:  status.Visibility,
		DraftLanguage:    status.Language,
		DraftIsBlogPost:  status.IsBlogPost,
		DraftBlogSlug:    status.BlogSlug,
	}
	h.renderPage(w, r, h.pageTitle(r, "status.action.redraft"), data, "redraft_status.html")
}

// RedraftStatusRedirect keeps old pages safe: their POST no longer deletes.
func (h *Handler) RedraftStatusRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/statuses/"+chi.URLParam(r, "id")+"/redraft", http.StatusSeeOther)
}

func (h *Handler) RedraftErrorPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	newStatus, newErr := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	original, oldErr := h.store.GetStatus(r.Context(), r.URL.Query().Get("original"))
	if account == nil || newErr != nil || oldErr != nil || newStatus.AccountID != account.ID || original.AccountID != account.ID {
		h.renderError(w, r, "Post not found")
		return
	}
	h.renderPage(w, r, h.pageTitle(r, "page.error_title"), map[string]interface{}{
		"Message":     h.pageTitle(r, "redraft.partial_error"),
		"OriginalURL": "/@" + account.Username + "/" + original.ID,
		"NewURL":      "/@" + account.Username + "/" + newStatus.ID,
	}, "error.html")
}

func (h *Handler) BookmarkStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	status, err := h.app.ToggleBookmark(r.Context(), account, statusID)
	if err != nil {
		slog.Error("webui: bookmark error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.returnStatusComponent(w, r, status, account)
}

// PinStatus toggles pinning a status (pin if unpinned, unpin if pinned).
func (h *Handler) PinStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	status, err := h.app.TogglePin(r.Context(), account, statusID)
	if err != nil {
		slog.Error("webui: pin error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.returnStatusComponent(w, r, status, account)
}

// UnpinStatus is called directly to unpin a status.
func (h *Handler) UnpinStatus(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	statusID := chi.URLParam(r, "id")
	status, err := h.app.UnpinStatus(r.Context(), account, statusID)
	if err != nil {
		slog.Error("webui: unpin error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.returnStatusComponent(w, r, status, account)
}

// composerContentType maps the composer's "markdown" checkbox to a status
// source format. Anything else is treated as plain text.
func composerContentType(r *http.Request) string {
	if r.FormValue("editor_mode") == "wysiwyg" || r.FormValue("markdown") == "on" {
		return content.ContentTypeMarkdown
	}
	return content.ContentTypePlain
}

func composerStatusSource(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.FormValue("editor_mode") != "wysiwyg" {
		return r.FormValue("status"), true
	}
	raw := r.FormValue("wysiwyg_html")
	if len(raw) > maxComposerVisualHTML {
		http.Error(w, "Visual post is too large", http.StatusRequestEntityTooLarge)
		return "", false
	}
	return content.HTMLToMarkdown(raw), true
}

func (h *Handler) parseComposeForm(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		maxSize := h.maxMediaSize
		if maxSize <= 0 {
			maxSize = 40 << 20
		}
		// MaxFileSize is per attachment, so allow up to MaxMedia files plus a
		// small allowance for the remaining multipart fields and boundaries.
		r.Body = http.MaxBytesReader(w, r.Body, (maxSize+medialib.MaxPreviewSize)*int64(h.maxMedia())+(1<<20))
		return r.ParseMultipartForm(maxSize)
	}
	return r.ParseForm()
}

func (h *Handler) maxMedia() int {
	if h.cfg != nil && h.cfg.Media.MaxMedia > 0 {
		return h.cfg.Media.MaxMedia
	}
	return 4
}

// maxStatusChars returns the configured maximum status length, falling back to
// the Mastodon default of 5000 when unset. Mirrors the API instance helper so
// the composer's character counter matches what the API advertises.
func (h *Handler) maxStatusChars() int {
	if h.cfg != nil && h.cfg.Instance.MaxStatusChars > 0 {
		return h.cfg.Instance.MaxStatusChars
	}
	return 5000
}

func (h *Handler) composerUploadCount(r *http.Request) int {
	if r.MultipartForm == nil {
		return 0
	}
	count := 0
	for field, headers := range r.MultipartForm.File {
		if field == "media_thumbnail" || strings.HasPrefix(field, "media_thumbnail_") {
			continue
		}
		if field != "media" && !strings.HasPrefix(field, "media_") {
			continue
		}
		for _, header := range headers {
			if header.Size > 0 {
				count++
			}
		}
	}
	return count
}

type composerUpload struct {
	header      *multipart.FileHeader
	thumbnail   *multipart.FileHeader
	description string
}

func (h *Handler) composerUploads(r *http.Request) []composerUpload {
	if r.MultipartForm == nil {
		return nil
	}

	// The web composer uses indexed field names so each file is unambiguously
	// paired with its own alt text, including when an earlier row is cleared.
	uploads := make([]composerUpload, 0, h.maxMedia())
	for i := 0; i < h.maxMedia(); i++ {
		mediaField := fmt.Sprintf("media_%d", i)
		thumbnailField := fmt.Sprintf("media_thumbnail_%d", i)
		descriptionField := fmt.Sprintf("media_description_%d", i)
		description := ""
		if values := r.MultipartForm.Value[descriptionField]; len(values) > 0 {
			description = values[0]
		}
		for _, header := range r.MultipartForm.File[mediaField] {
			if header.Size > 0 {
				var thumbnail *multipart.FileHeader
				if candidates := r.MultipartForm.File[thumbnailField]; len(candidates) > 0 && candidates[0].Size > 0 {
					thumbnail = candidates[0]
				}
				uploads = append(uploads, composerUpload{header: header, thumbnail: thumbnail, description: description})
			}
		}
	}
	if len(uploads) > 0 {
		return uploads
	}

	// Keep accepting the previous repeated-field format for compatibility with
	// existing forms and direct integrations.
	headers := r.MultipartForm.File["media"]
	thumbnails := r.MultipartForm.File["media_thumbnail"]
	descriptions := r.MultipartForm.Value["media_description"]
	for i, header := range headers {
		if header.Size <= 0 {
			continue
		}
		description := ""
		if len(descriptions) == 1 {
			description = descriptions[0]
		} else if i < len(descriptions) {
			description = descriptions[i]
		}
		var thumbnail *multipart.FileHeader
		if i < len(thumbnails) && thumbnails[i].Size > 0 {
			thumbnail = thumbnails[i]
		}
		uploads = append(uploads, composerUpload{header: header, thumbnail: thumbnail, description: description})
	}
	return uploads
}

func (h *Handler) createComposerMedia(r *http.Request, account *models.Account) ([]string, error) {
	uploads := h.composerUploads(r)
	mediaIDs := make([]string, 0, len(uploads))
	for _, upload := range uploads {
		mediaID, err := h.createComposerMediaFile(r, account, upload.header, upload.thumbnail, upload.description)
		if err != nil {
			return nil, err
		}
		mediaIDs = append(mediaIDs, mediaID)
	}
	return mediaIDs, nil
}

func (h *Handler) createComposerMediaFile(r *http.Request, account *models.Account, header, thumbnail *multipart.FileHeader, description string) (string, error) {
	description = strings.TrimSpace(description)
	if utf8.RuneCountInString(description) > models.MaxMediaDescriptionLength {
		return "", fmt.Errorf("description is too long (maximum %d characters)", models.MaxMediaDescriptionLength)
	}
	file, err := header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	maxSize := h.maxMediaSize
	if maxSize <= 0 {
		maxSize = 40 << 20
	}
	if header.Size > maxSize {
		return "", fmt.Errorf("file too large")
	}

	mimeType, mediaReader, err := medialib.SniffUpload(file, header.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	media := &models.MediaAttachment{
		AccountID:   account.ID,
		Type:        mediaKind(mimeType),
		Description: description,
		FileSize:    header.Size,
		MIMEType:    mimeType,
	}
	if err := h.store.CreateMediaAttachment(r.Context(), media); err != nil {
		return "", err
	}
	// Use the renamed filename if configured
	safeFilename := medialib.MediaFilename(header.Filename, mimeType, h.renameMedia)
	err = medialib.StoreUpload(r.Context(), h.mediaStore, mediaReader, header.Size, media, safeFilename, h.stripEXIF)
	if err != nil {
		_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
		return "", err
	}
	if thumbnail != nil {
		poster, openErr := thumbnail.Open()
		if openErr != nil {
			_ = h.mediaStore.Delete(r.Context(), media.StorageKey)
			_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
			return "", openErr
		}
		err = medialib.StorePreview(r.Context(), h.mediaStore, poster, thumbnail.Size, media, h.stripEXIF)
		poster.Close()
		if err != nil {
			_ = h.mediaStore.Delete(r.Context(), media.StorageKey)
			_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
			return "", err
		}
	}
	if err := h.store.UpdateMediaAttachment(r.Context(), media); err != nil {
		return "", err
	}

	return media.ID, nil
}

// composerUploadErrorMessage decides how much of a media upload error to show
// the user. The known validation failures below are generated with fixed,
// path-free text (no wrapped OS/filesystem errors), so it's safe to surface
// them as-is; anything else falls back to a generic message so we don't leak
// server-side paths from wrapped errors deeper in the storage pipeline.
func composerUploadErrorMessage(err error) string {
	msg := err.Error()
	for _, safePrefix := range []string{
		"image dimensions ",
		"file too large",
		"empty file",
		"SVG uploads are not supported",
		"unsupported media type",
		"thumbnail ",
		"description is too long",
	} {
		if strings.HasPrefix(msg, safePrefix) {
			return msg
		}
	}
	return "Error uploading media"
}

func mediaKind(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "unknown"
	}
}
