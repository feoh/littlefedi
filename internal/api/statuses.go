package api

import (
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"html"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func parseIntDefault(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// Status Endpoints

// StatusCreate creates a new status.
// POST /api/v1/statuses
func (h *Handler) StatusCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 32<<20)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	status := params.First("status")
	inReplyToID := params.First("in_reply_to_id")
	quotedStatusID := params.First("quoted_status_id")
	mediaIDs := params.All("media_ids[]", "media_ids")
	pollOptions := params.All("poll[options][]", "poll[options]", "poll.options")
	if len(mediaIDs) > h.maxMedia() {
		writeAPIError(w, http.StatusUnprocessableEntity, "too many media attachments")
		return
	}
	sensitiveStr := params.First("sensitive")
	spoilerText := params.First("spoiler_text")
	visibility := params.First("visibility")
	language := params.First("language")

	if status == "" && len(mediaIDs) == 0 && len(pollOptions) == 0 {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "status is empty", "parse_mode", parseMode)
		writeAPIError(w, http.StatusUnprocessableEntity, "status is empty")
		return
	}

	if visibility == "" {
		visibility = "public"
	}
	if language == "" {
		language = "en"
	}
	if inReplyToID != "" {
		if parent, err := h.store.GetStatus(r.Context(), inReplyToID); err == nil &&
			(parent.Visibility == models.VisibilityLocal || parent.Visibility == models.VisibilityLocalUnlisted) {
			visibility = string(parent.Visibility)
		}
	}

	// Parse visibility
	vis := models.VisibilityPublic
	switch visibility {
	case "public":
		vis = models.VisibilityPublic
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
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid visibility")
		return
	}

	// Resolve inReplyTo to get the parent status
	var inReplyTo *string
	if inReplyToID != "" {
		inReplyTo = &inReplyToID
	}
	pollDuration := 86400
	if raw := params.First("poll[expires_in]", "poll.expires_in"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 300 || n > 30*24*3600 {
			writeAPIError(w, http.StatusUnprocessableEntity, "poll expiration out of range")
			return
		}
		pollDuration = n
	}
	for _, field := range []string{"multiple", "hide_totals"} {
		if raw := params.First("poll["+field+"]", "poll."+field); raw != "" {
			switch raw {
			case "true", "false", "1", "0", "yes", "no", "on", "off":
			default:
				writeAPIError(w, http.StatusUnprocessableEntity, "invalid poll boolean")
				return
			}
		}
	}
	input := app.StatusInput{
		Content:        status,
		Summary:        spoilerText,
		Sensitive:      parseAPIBool(sensitiveStr),
		Visibility:     vis,
		Language:       language,
		InReplyToID:    inReplyTo,
		QuotedStatusID: quotedStatusID,
		MediaIDs:       mediaIDs,
		PollOptions:    pollOptions,
		PollExpiresIn:  time.Duration(pollDuration) * time.Second,
		PollMultiple:   parseAPIBool(params.First("poll[multiple]", "poll.multiple")),
		PollHideTotals: parseAPIBool(params.First("poll[hide_totals]", "poll.hide_totals")),
		ExpiresIn:      time.Duration(parseIntDefault(params.First("expires_in"), 0)) * time.Second,
	}
	if err := app.ValidateStatusInput(input); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if len(pollOptions) == 0 {
		for key := range params {
			if strings.HasPrefix(key, "poll.") || strings.HasPrefix(key, "poll[") {
				writeAPIError(w, http.StatusUnprocessableEntity, "poll must have 2 to 4 options")
				return
			}
		}
	}
	if raw := params.First("scheduled_at"); raw != "" {
		scheduledAt, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "scheduled_at must be a valid RFC3339 timestamp")
			return
		}
		scheduled, err := h.app.ScheduleStatus(r.Context(), account, input, scheduledAt)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAPIJSON(w, http.StatusOK, h.scheduledStatusEntity(scheduled))
		return
	}
	created, err := h.app.CreateStatus(r.Context(), account, input)
	if err != nil {
		slog.Error("api: create status error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating status")
		return
	}
	h.app.EnrichStatus(r.Context(), created, account)

	slog.Info("api: status created", "id", created.ID)

	result := h.statusToMastodon(created, account)
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusesGet returns the requested statuses the viewer can read.
// GET /api/v1/statuses
func (h *Handler) StatusesGet(w http.ResponseWriter, r *http.Request) {
	requested := r.URL.Query()["id[]"]
	if len(requested) == 0 {
		requested = r.URL.Query()["id"]
	}
	ids := make([]string, 0, 20)
	for _, id := range requested {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
			if len(ids) > 20 {
				writeAPIError(w, http.StatusUnprocessableEntity, "too many status ids (maximum 20)")
				return
			}
		}
	}
	byID, err := h.store.GetStatusesByIDs(r.Context(), ids)
	if err != nil {
		slog.Error("api: batch status error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching statuses")
		return
	}
	account := getAuthAccount(r.Context())
	statuses := make([]*models.Status, 0, len(ids))
	for _, id := range ids {
		if status := byID[id]; h.app.CanViewStatus(r.Context(), status, account) {
			statuses = append(statuses, status)
		}
	}
	h.app.EnrichStatuses(r.Context(), statuses, account)
	statuses, err = h.app.FilterStatuses(r.Context(), "thread", statuses, account)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering statuses")
		return
	}
	result := make([]*MastodonStatus, 0, len(statuses))
	for _, status := range statuses {
		result = append(result, h.statusToMastodon(status, account))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusGet returns a single status.
// GET /api/v1/statuses/{id}
func (h *Handler) StatusGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		slog.Warn("api: status not found", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}

	authAccount := getAuthAccount(r.Context())
	if !h.app.CanViewStatus(r.Context(), status, authAccount) {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}

	// Use the same cache-aware batched enrichment path as timelines. Even for a
	// single status this avoids the many one-relation-at-a-time queries on a
	// cold cache and makes subsequent opens a component-cache hit.
	statuses := []*models.Status{status}
	h.app.EnrichStatuses(r.Context(), statuses, authAccount)
	status = statuses[0]
	statuses, err = h.app.FilterStatuses(r.Context(), "thread", statuses, authAccount)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering status")
		return
	}
	if len(statuses) == 0 {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	status = statuses[0]

	// Rendering a status must never wait on its remote origin. Resolve() may do
	// several federation requests with an 8-second deadline; those belong on
	// explicit interaction/federation paths, not GET /api/v1/statuses/:id.
	interactions := h.interactions.ResolveLocal(r.Context(), status)
	status = app.WithInteractionCounts(status, interactions.FavouritesCount, interactions.ReblogsCount)

	result := h.statusToMastodon(status, authAccount)
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusEdit edits an existing status (Mastodon-compatible).
// PUT /api/v1/statuses/{id}
func (h *Handler) StatusEdit(w http.ResponseWriter, r *http.Request) {
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

	params, parseMode, err := decodeMastodonParams(r, 32<<20)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	mediaIDs := params.All("media_ids[]", "media_ids")
	if mediaIDs == nil {
		if _, present := params["media_ids[]"]; present {
			mediaIDs = []string{}
		} else if _, present := params["media_ids"]; present {
			mediaIDs = []string{}
		}
	}
	if len(mediaIDs) > h.maxMedia() {
		writeAPIError(w, http.StatusUnprocessableEntity, "too many media attachments")
		return
	}

	// Clients change alt text on a published post through media_attributes,
	// one entry per attachment. Form bodies send media_attributes[][id] and
	// media_attributes[][description]; JSON arrays of objects arrive flattened
	// to media_attributes.id and media_attributes.description.
	attrIDs := params.All("media_attributes[][id]", "media_attributes.id")
	attrDescriptions := params.All("media_attributes[][description]", "media_attributes.description")
	var mediaDescriptions map[string]string
	if len(attrIDs) > 0 || len(attrDescriptions) > 0 {
		if len(attrIDs) != len(attrDescriptions) {
			writeAPIError(w, http.StatusUnprocessableEntity, "each media attribute needs an id and a description")
			return
		}
		mediaDescriptions = make(map[string]string, len(attrIDs))
		for i, mediaID := range attrIDs {
			if utf8.RuneCountInString(strings.TrimSpace(attrDescriptions[i])) > models.MaxMediaDescriptionLength {
				writeAPIError(w, http.StatusUnprocessableEntity, "description is too long")
				return
			}
			mediaDescriptions[mediaID] = attrDescriptions[i]
		}
	}

	editInput := app.EditStatusInput{
		Content:           params.First("status"),
		Summary:           params.First("spoiler_text"),
		Sensitive:         parseAPIBool(params.First("sensitive")),
		Language:          params.First("language"),
		MediaIDs:          mediaIDs,
		MediaDescriptions: mediaDescriptions,
	}
	if _, present := params["expires_in"]; present {
		d := time.Duration(parseIntDefault(params.First("expires_in"), 0)) * time.Second
		editInput.ExpiresIn = &d
	}

	updated, err := h.app.EditStatus(r.Context(), account, id, editInput)
	if err != nil {
		switch err.Error() {
		case storage.ErrStatusConflict.Error():
			writeAPIError(w, http.StatusConflict, "status changed; reload before editing")
		case storage.ErrInvalidMedia.Error():
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		case "not your post":
			writeAPIError(w, http.StatusForbidden, "this status is not yours")
		case "cannot edit a boost", "status is empty":
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			slog.Error("api: edit status error", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error editing status")
		}
		return
	}
	h.app.EnrichStatus(r.Context(), updated, account)

	slog.Info("api: status edited", "id", updated.ID)
	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(updated, account))
}

// StatusSource returns the editable source of a status (Mastodon-compatible).
// GET /api/v1/statuses/{id}/source
func (h *Handler) StatusSource(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil || status.AccountID != account.ID {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"id":           status.ID,
		"text":         status.ContentSource,
		"spoiler_text": html.UnescapeString(status.Summary),
	})
}

// StatusDelete deletes a status.
// DELETE /api/v1/statuses/{id}
func (h *Handler) StatusDelete(w http.ResponseWriter, r *http.Request) {
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

	if err := h.app.DeleteStatus(r.Context(), account, id); err != nil {
		slog.Error("api: delete status error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error deleting status")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// StatusContext returns the ancestor and descendant statuses for threading.
// GET /api/v1/statuses/{id}/context
func (h *Handler) StatusContext(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	authAccount := getAuthAccount(r.Context())
	if !h.app.CanViewStatus(r.Context(), status, authAccount) {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	// Complete the remote thread in the background and serve the locally known
	// context right away, as the web UI does. Blocking here would delay every
	// app's thread view by up to thread_fetch_timeout. Mastodon apps re-poll the
	// context on their own, so fetched replies show up on the next open.
	if h.threadResolver != nil {
		h.threadResolver.RefreshInBackground(status)
	}

	ancestors, descendants, err := h.store.GetStatusContext(r.Context(), id)
	if err != nil {
		slog.Warn("api: context error", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}

	// One batched enrichment over the whole thread instead of per-status
	// queries - an N+1 that hurts on low-power devices. Enrichment may
	// replace slice elements (component-cache hits are copies), so convert
	// from the combined slice, not the original ones.
	ancestors = h.app.VisibleStatuses(r.Context(), ancestors, authAccount)
	descendants = h.app.VisibleStatuses(r.Context(), descendants, authAccount)
	all := make([]*models.Status, 0, len(ancestors)+len(descendants))
	all = append(all, ancestors...)
	all = append(all, descendants...)
	h.app.EnrichStatuses(r.Context(), all, authAccount)
	all, err = h.app.FilterStatuses(r.Context(), "thread", all, authAccount)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error filtering context")
		return
	}
	ancestors = filteredStatusesInOrder(all, ancestors)
	descendants = filteredStatusesInOrder(all, descendants)

	ancestorResults := make([]*MastodonStatus, 0, len(ancestors))
	for _, s := range ancestors {
		ancestorResults = append(ancestorResults, h.statusToMastodon(s, authAccount))
	}

	descendantResults := make([]*MastodonStatus, 0, len(descendants))
	for _, s := range descendants {
		descendantResults = append(descendantResults, h.statusToMastodon(s, authAccount))
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"ancestors":   ancestorResults,
		"descendants": descendantResults,
	})
}

func filteredStatusesInOrder(filtered, original []*models.Status) []*models.Status {
	byID := make(map[string]*models.Status, len(filtered))
	for _, status := range filtered {
		byID[status.ID] = status
	}
	out := make([]*models.Status, 0, len(original))
	for _, status := range original {
		if kept := byID[status.ID]; kept != nil {
			out = append(out, kept)
		}
	}
	return out
}

// StatusRebloggedBy returns accounts that reblogged a status.
// GET /api/v1/statuses/{id}/reblogged_by
func (h *Handler) StatusRebloggedBy(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, getAuthAccount(r.Context())) {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	interactions := h.interactions.Resolve(r.Context(), status)
	result := make([]*MastodonAccount, 0, len(interactions.Reblogs))
	for _, account := range interactions.Reblogs {
		result = append(result, h.accountToMastodon(account, getAuthAccount(r.Context()), false))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusFavouritedBy returns accounts that favourited a status.
// GET /api/v1/statuses/{id}/favourited_by
func (h *Handler) StatusFavouritedBy(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, getAuthAccount(r.Context())) {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	interactions := h.interactions.Resolve(r.Context(), status)
	result := make([]*MastodonAccount, 0, len(interactions.Favourites))
	for _, account := range interactions.Favourites {
		result = append(result, h.accountToMastodon(account, getAuthAccount(r.Context()), false))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusFavourite favourites a status.
// POST /api/v1/statuses/{id}/favourite
func (h *Handler) StatusFavourite(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Favourite(r.Context(), account, id)
	if err != nil {
		if writeFederationPolicyError(w, err) {
			return
		}
		slog.Error("api: favourite error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error favouriting")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusUnfavourite removes a favourite.
// POST /api/v1/statuses/{id}/unfavourite
func (h *Handler) StatusUnfavourite(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Unfavourite(r.Context(), account, id)
	if err != nil {
		slog.Error("api: unfavourite error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unfavouriting")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

func (h *Handler) StatusEmojiReactionAdd(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	status, err := h.app.AddEmojiReaction(r.Context(), account, chi.URLParam(r, "id"), chi.URLParam(r, "emoji"))
	if err != nil {
		if writeFederationPolicyError(w, err) {
			return
		}
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)
	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

func (h *Handler) StatusEmojiReactionRemove(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	status, err := h.app.RemoveEmojiReaction(r.Context(), account, chi.URLParam(r, "id"), chi.URLParam(r, "emoji"))
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)
	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

func (h *Handler) StatusEmojiReactions(w http.ResponseWriter, r *http.Request) {
	viewer := getAuthAccount(r.Context())
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || !h.app.CanViewStatus(r.Context(), status, viewer) {
		writeAPIError(w, http.StatusNotFound, "status not found")
		return
	}
	h.app.EnrichStatus(r.Context(), status, viewer)
	wanted := chi.URLParam(r, "emoji")
	type reactionAccounts struct {
		Name      string             `json:"name"`
		Count     int                `json:"count"`
		Me        bool               `json:"me"`
		Accounts  []*MastodonAccount `json:"accounts"`
		URL       string             `json:"url,omitempty"`
		StaticURL string             `json:"static_url,omitempty"`
	}
	result := make([]reactionAccounts, 0, len(status.EmojiReactions))
	for _, reaction := range status.EmojiReactions {
		if wanted != "" && reaction.Name != wanted {
			continue
		}
		accounts, _ := h.store.ListEmojiReactionAccounts(r.Context(), status.ID, reaction.Key, 80)
		item := reactionAccounts{Name: reaction.Name, Count: reaction.Count, Me: reaction.Me, Accounts: make([]*MastodonAccount, 0, len(accounts))}
		for _, account := range accounts {
			item.Accounts = append(item.Accounts, h.accountToMastodon(account, viewer, false))
		}
		if reaction.Emoji != nil {
			item.URL = h.displayMediaURL(reaction.Emoji.URL)
			item.StaticURL = h.displayMediaURL(reaction.Emoji.StaticURL)
		}
		result = append(result, item)
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// StatusReblog reblogs (boosts) a status.
// POST /api/v1/statuses/{id}/reblog
func (h *Handler) StatusReblog(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Reblog(r.Context(), account, id)
	if err != nil {
		if writeFederationPolicyError(w, err) {
			return
		}
		slog.Error("api: reblog error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error reblogging")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusUnreblog removes a reblog.
// POST /api/v1/statuses/{id}/unreblog
func (h *Handler) StatusUnreblog(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Unreblog(r.Context(), account, id)
	if err != nil {
		slog.Error("api: unreblog error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unreblogging")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusBookmark bookmarks a status.
// POST /api/v1/statuses/{id}/bookmark
func (h *Handler) StatusBookmark(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Bookmark(r.Context(), account, id)
	if err != nil {
		slog.Error("api: bookmark error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error bookmarking")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusUnbookmark removes a bookmark.
// POST /api/v1/statuses/{id}/unbookmark
func (h *Handler) StatusUnbookmark(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.Unbookmark(r.Context(), account, id)
	if err != nil {
		slog.Error("api: unbookmark error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unbookmarking")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusPin pins a status.
// POST /api/v1/statuses/{id}/pin
func (h *Handler) StatusPin(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.PinStatus(r.Context(), account, id)
	if err != nil {
		slog.Error("api: pin error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error pinning")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}

// StatusUnpin unpins a status.
// POST /api/v1/statuses/{id}/unpin
func (h *Handler) StatusUnpin(w http.ResponseWriter, r *http.Request) {
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

	status, err := h.app.UnpinStatus(r.Context(), account, id)
	if err != nil {
		slog.Error("api: unpin error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unpinning")
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)

	writeAPIJSON(w, http.StatusOK, h.statusToMastodon(status, account))
}
