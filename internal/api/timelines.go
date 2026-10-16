package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// pageKeyPtr renders an optional pagination cursor for a page cache key.
func pageKeyPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pageKeyBool renders a bool as a compact page cache key segment.
func pageKeyBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// viewerIDOf returns the account ID for moderation filtering, or "" for an
// anonymous viewer (who only has global suspension/domain filtering applied).
func viewerIDOf(a *models.Account) string {
	if a == nil {
		return ""
	}
	return a.ID
}

// Home Timeline

// TimelineHome returns statuses from accounts the user follows.
// GET /api/v1/timelines/home
func (h *Handler) TimelineHome(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)

	// Two cache layers share the key parts: the inner PageCached holds the
	// enriched status slice; the outer SerializedPageCached holds the fully
	// encoded response body (trailing "json" part), so a poll loop hitting the
	// same page never re-runs the Mastodon conversion or json.Marshal.
	keyParts := []string{pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit), pageKeyBool(account.HideReplies), pageKeyBool(account.HideBoosts)}
	sp, err := h.app.SerializedPageCached(
		r.Context(),
		h.app.PageKey(account.ID, "api:home", append(append([]string{}, keyParts...), "json")...),
		func(ctx context.Context) (app.SerializedPage, []*models.Status, error) {
			res, err := h.app.PageCached(
				ctx,
				h.app.PageKey(account.ID, "api:home", keyParts...),
				func(ctx context.Context) (app.PageResult, error) {
					statuses, err := h.store.GetHomeTimeline(ctx, account.ID, params.MaxID, params.MinID, params.SinceID, params.Limit, account.HideReplies, account.HideBoosts)
					if err != nil {
						return app.PageResult{}, err
					}
					h.app.EnrichStatuses(ctx, statuses, account)
					cursor, count := pageCursor(statuses)
					statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
					return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
				})
			if err != nil {
				return app.SerializedPage{}, nil, err
			}
			return h.serializeStatusPageCursor(res.Statuses, account, res.Cursor, res.RawCount)
		})
	if err != nil {
		slog.Error("api: home timeline error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching timeline")
		return
	}

	setLinkHeaderIDs(w, "/api/v1/timelines/home", params, sp.FirstID, sp.LastID, sp.Count)
	writeAPIJSONBytes(w, http.StatusOK, sp.Body)
}

// serializeStatusPage converts an enriched status page to its final Mastodon
// JSON bytes plus the pagination anchors, for the serialized page cache.
func (h *Handler) serializeStatusPage(statuses []*models.Status, viewer *models.Account) (app.SerializedPage, []*models.Status, error) {
	return h.serializeStatusPageCursor(statuses, viewer, "", len(statuses))
}

func (h *Handler) serializeStatusPageCursor(statuses []*models.Status, viewer *models.Account, cursor string, count int) (app.SerializedPage, []*models.Status, error) {
	result := make([]*MastodonStatus, 0, len(statuses))
	for _, s := range statuses {
		result = append(result, h.statusToMastodon(s, viewer))
	}
	body, err := json.Marshal(result)
	if err != nil {
		return app.SerializedPage{}, nil, err
	}
	sp := app.SerializedPage{Body: body, Count: count, LastID: cursor}
	if len(statuses) > 0 {
		sp.FirstID = statuses[0].ID
		if sp.LastID == "" {
			sp.LastID = statuses[len(statuses)-1].ID
		}
	}
	return sp, statuses, nil
}

// Public Timeline

// TimelinePublic returns public statuses from the local or federated timeline.
// GET /api/v1/timelines/public
func (h *Handler) TimelinePublic(w http.ResponseWriter, r *http.Request) {
	localStr := r.URL.Query().Get("local")
	local := localStr == "true" || localStr == "1"
	remoteStr := r.URL.Query().Get("remote")
	onlyRemote := remoteStr == "true" || remoteStr == "1"

	params := parsePagination(r)
	authAccount := getAuthAccount(r.Context())
	hideReplies := authAccount != nil && authAccount.HideReplies
	hideBoosts := authAccount != nil && authAccount.HideBoosts
	viewerID := viewerIDOf(authAccount)

	// If only_remote is set, we want non-local posts only
	// If local is set, we want local posts only
	// If neither, we want all public posts
	keyParts := []string{pageKeyBool(local), pageKeyBool(onlyRemote), pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit), pageKeyBool(hideReplies), pageKeyBool(hideBoosts)}
	sp, err := h.app.SerializedPageCached(
		r.Context(),
		h.app.PageKey(viewerID, "api:public", append(append([]string{}, keyParts...), "json")...),
		func(ctx context.Context) (app.SerializedPage, []*models.Status, error) {
			res, err := h.app.PageCached(
				ctx,
				h.app.PageKey(viewerID, "api:public", keyParts...),
				func(ctx context.Context) (app.PageResult, error) {
					var statuses []*models.Status
					var ferr error
					if local {
						statuses, ferr = h.store.GetPublicTimeline(ctx, viewerID, true, params.MaxID, params.MinID, params.SinceID, params.Limit, hideReplies, hideBoosts)
					} else {
						statuses, ferr = h.store.GetPublicTimeline(ctx, viewerID, false, params.MaxID, params.MinID, params.SinceID, params.Limit, hideReplies, hideBoosts)
					}
					if ferr != nil {
						return app.PageResult{}, ferr
					}
					// Check auth to show relationship info, but public timeline is accessible without auth
					h.app.EnrichStatuses(ctx, statuses, authAccount)
					cursor, count := pageCursor(statuses)
					statuses, ferr = h.app.FilterStatuses(ctx, "public", statuses, authAccount)
					return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, ferr
				})
			if err != nil {
				return app.SerializedPage{}, nil, err
			}
			return h.serializeStatusPageCursor(res.Statuses, authAccount, res.Cursor, res.RawCount)
		})
	if err != nil {
		slog.Error("api: public timeline error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching timeline")
		return
	}

	setLinkHeaderIDs(w, "/api/v1/timelines/public", params, sp.FirstID, sp.LastID, sp.Count)
	writeAPIJSONBytes(w, http.StatusOK, sp.Body)
}

// Bubble Timeline

// TimelineBubble returns statuses from local accounts and bubble instances.
// GET /api/v1/timelines/bubble
func (h *Handler) TimelineBubble(w http.ResponseWriter, r *http.Request) {
	params := parsePagination(r)
	authAccount := getAuthAccount(r.Context())
	hideReplies := authAccount != nil && authAccount.HideReplies
	hideBoosts := authAccount != nil && authAccount.HideBoosts

	viewerID := viewerIDOf(authAccount)

	keyParts := []string{pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit), pageKeyBool(hideReplies), pageKeyBool(hideBoosts)}
	sp, err := h.app.SerializedPageCached(
		r.Context(),
		h.app.PageKey(viewerID, "api:bubble", append(append([]string{}, keyParts...), "json")...),
		func(ctx context.Context) (app.SerializedPage, []*models.Status, error) {
			res, err := h.app.PageCached(
				ctx,
				h.app.PageKey(viewerID, "api:bubble", keyParts...),
				func(ctx context.Context) (app.PageResult, error) {
					statuses, err := h.store.GetBubbleTimeline(ctx, viewerID, params.MaxID, params.MinID, params.SinceID, params.Limit, hideReplies, hideBoosts)
					if err != nil {
						return app.PageResult{}, err
					}
					h.app.EnrichStatuses(ctx, statuses, authAccount)
					cursor, count := pageCursor(statuses)
					statuses, err = h.app.FilterStatuses(ctx, "public", statuses, authAccount)
					return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
				})
			if err != nil {
				return app.SerializedPage{}, nil, err
			}
			return h.serializeStatusPageCursor(res.Statuses, authAccount, res.Cursor, res.RawCount)
		})
	if err != nil {
		slog.Error("api: bubble timeline error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching timeline")
		return
	}

	setLinkHeaderIDs(w, "/api/v1/timelines/bubble", params, sp.FirstID, sp.LastID, sp.Count)
	writeAPIJSONBytes(w, http.StatusOK, sp.Body)
}

// Hashtag Timeline

// TimelineTag returns public statuses with a specific hashtag.
// GET /api/v1/timelines/tag/{tag}
func (h *Handler) TimelineTag(w http.ResponseWriter, r *http.Request) {
	tag := chi.URLParam(r, "tag")
	if tag == "" {
		writeAPIError(w, http.StatusBadRequest, "missing tag")
		return
	}

	params := parsePagination(r)
	authAccount := getAuthAccount(r.Context())
	hideReplies := authAccount != nil && authAccount.HideReplies
	hideBoosts := authAccount != nil && authAccount.HideBoosts
	// Unauthenticated requests get a local-only view (matching the public web UI);
	// authenticated clients may still opt in with the Mastodon ?local=true param.
	localOnly := authAccount == nil || r.URL.Query().Get("local") == "true"

	viewerID := viewerIDOf(authAccount)

	keyParts := []string{pageKeyBool(localOnly), pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit), pageKeyBool(hideReplies), pageKeyBool(hideBoosts)}
	sp, err := h.app.SerializedPageCached(
		r.Context(),
		h.app.PageKey(viewerID, "api:tag:"+strings.ToLower(tag), append(append([]string{}, keyParts...), "json")...),
		func(ctx context.Context) (app.SerializedPage, []*models.Status, error) {
			res, err := h.app.PageCached(
				ctx,
				h.app.PageKey(viewerID, "api:tag:"+strings.ToLower(tag), keyParts...),
				func(ctx context.Context) (app.PageResult, error) {
					statuses, err := h.store.GetTagTimeline(ctx, viewerID, tag, params.MaxID, params.MinID, params.SinceID, params.Limit, hideReplies, hideBoosts, localOnly)
					if err != nil {
						return app.PageResult{}, err
					}
					h.app.EnrichStatuses(ctx, statuses, authAccount)
					cursor, count := pageCursor(statuses)
					statuses, err = h.app.FilterStatuses(ctx, "public", statuses, authAccount)
					return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
				})
			if err != nil {
				return app.SerializedPage{}, nil, err
			}
			return h.serializeStatusPageCursor(res.Statuses, authAccount, res.Cursor, res.RawCount)
		})
	if err != nil {
		slog.Error("api: tag timeline error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching tag timeline")
		return
	}

	setLinkHeaderIDs(w, "/api/v1/timelines/tag/"+tag, params, sp.FirstID, sp.LastID, sp.Count)
	writeAPIJSONBytes(w, http.StatusOK, sp.Body)
}

func pageCursor(statuses []*models.Status) (string, int) {
	if len(statuses) == 0 {
		return "", 0
	}
	return statuses[len(statuses)-1].ID, len(statuses)
}

func firstStatusID(statuses []*models.Status) string {
	if len(statuses) == 0 {
		return ""
	}
	return statuses[0].ID
}
