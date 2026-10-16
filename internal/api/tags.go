package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// maxFeaturedTags matches the max_featured_tags advertised in instance
// configuration (instanceConfiguration in instance.go).
const maxFeaturedTags = 10

// toMastodonTag builds the Mastodon Tag entity. When withFollowing is true the
// `following` field is populated from the given value; otherwise it is omitted.
func (h *Handler) toMastodonTag(t *models.Tag, withFollowing, following bool) MastodonTag {
	mt := MastodonTag{
		Name:    t.Name,
		URL:     h.absoluteURL(t.URL),
		History: []MastodonTagHistory{},
	}
	if withFollowing {
		f := following
		mt.Following = &f
	}
	return mt
}

// tagNameParam extracts and normalises the {id} path param, which for tags is
// the hashtag name (Mastodon uses the name as the id).
func tagNameParam(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(chi.URLParam(r, "id"), "#")))
}

// TagGet returns a single hashtag with the viewer's follow state.
// GET /api/v1/tags/{id}
func (h *Handler) TagGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	name := tagNameParam(r)
	if name == "" {
		writeAPIError(w, http.StatusBadRequest, "missing tag")
		return
	}

	tag, err := h.store.GetTagByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "tag not found")
			return
		}
		slog.Error("api: get tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching tag")
		return
	}

	following, err := h.store.IsFollowingTag(r.Context(), account.ID, tag.ID)
	if err != nil {
		slog.Error("api: is following tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching tag")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.toMastodonTag(tag, true, following))
}

// TagFollow subscribes the authenticated account to a hashtag.
// POST /api/v1/tags/{id}/follow
func (h *Handler) TagFollow(w http.ResponseWriter, r *http.Request) {
	h.setTagFollow(w, r, true)
}

// TagUnfollow unsubscribes the authenticated account from a hashtag.
// POST /api/v1/tags/{id}/unfollow
func (h *Handler) TagUnfollow(w http.ResponseWriter, r *http.Request) {
	h.setTagFollow(w, r, false)
}

func (h *Handler) setTagFollow(w http.ResponseWriter, r *http.Request, follow bool) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	name := tagNameParam(r)
	if name == "" {
		writeAPIError(w, http.StatusBadRequest, "missing tag")
		return
	}

	tag, err := h.store.GetOrCreateTag(r.Context(), name)
	if err != nil {
		slog.Error("api: get or create tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching tag")
		return
	}

	if follow {
		err = h.store.FollowTag(r.Context(), account.ID, tag.ID)
	} else {
		err = h.store.UnfollowTag(r.Context(), account.ID, tag.ID)
	}
	if err != nil {
		slog.Error("api: set tag follow error", "follow", follow, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error updating tag follow")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.toMastodonTag(tag, true, follow))
}

// FollowedTagsList returns the hashtags the authenticated account follows.
// GET /api/v1/followed_tags
func (h *Handler) FollowedTagsList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	tags, err := h.store.ListFollowedTags(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: list followed tags error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching followed tags")
		return
	}

	result := make([]MastodonTag, 0, len(tags))
	for _, t := range tags {
		result = append(result, h.toMastodonTag(t, true, true))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

func (h *Handler) toMastodonFeaturedTag(v *models.FeaturedTagView) MastodonFeaturedTag {
	var lastStatusAt *string
	if v.LastStatusAt != nil {
		s := v.LastStatusAt.UTC().Format("2006-01-02")
		lastStatusAt = &s
	}
	return MastodonFeaturedTag{
		ID:            v.ID,
		Name:          v.Name,
		URL:           h.absoluteURL(v.URL),
		StatusesCount: strconv.Itoa(v.StatusesCount),
		LastStatusAt:  lastStatusAt,
	}
}

// FeaturedTags returns the authenticated account's featured tags.
// GET /api/v1/featured_tags
func (h *Handler) FeaturedTags(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	views, err := h.store.ListFeaturedTags(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: list featured tags error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching featured tags")
		return
	}

	result := make([]MastodonFeaturedTag, 0, len(views))
	for _, v := range views {
		result = append(result, h.toMastodonFeaturedTag(v))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// FeaturedTagCreate pins a hashtag to the authenticated account's profile.
// POST /api/v1/featured_tags
func (h *Handler) FeaturedTagCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, _, err := decodeMastodonParams(r, 0)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(params.First("name"), "#")))
	if name == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "name is required")
		return
	}

	existing, err := h.store.ListFeaturedTags(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: list featured tags error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching featured tags")
		return
	}
	if len(existing) >= maxFeaturedTags {
		writeAPIError(w, http.StatusUnprocessableEntity, "too many featured tags")
		return
	}

	tag, err := h.store.GetOrCreateTag(r.Context(), name)
	if err != nil {
		slog.Error("api: get or create tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching tag")
		return
	}

	if _, err := h.store.CreateFeaturedTag(r.Context(), account.ID, tag.ID); err != nil {
		if errors.Is(err, storage.ErrFeaturedTagExists) {
			writeAPIError(w, http.StatusUnprocessableEntity, "tag is already featured")
			return
		}
		slog.Error("api: create featured tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error featuring tag")
		return
	}

	views, err := h.store.ListFeaturedTags(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: list featured tags error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching featured tags")
		return
	}
	for _, v := range views {
		if v.Name == name {
			writeAPIJSON(w, http.StatusOK, h.toMastodonFeaturedTag(v))
			return
		}
	}
	writeAPIError(w, http.StatusInternalServerError, "error fetching featured tag")
}

// FeaturedTagDelete unpins a featured tag from the authenticated account's profile.
// DELETE /api/v1/featured_tags/{id}
func (h *Handler) FeaturedTagDelete(w http.ResponseWriter, r *http.Request) {
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

	found, err := h.store.DeleteFeaturedTag(r.Context(), account.ID, id)
	if err != nil {
		slog.Error("api: delete featured tag error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error removing featured tag")
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "featured tag not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// AccountFeaturedTags returns another account's featured tags (public profile view).
// GET /api/v1/accounts/{id}/featured_tags
func (h *Handler) AccountFeaturedTags(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	views, err := h.store.ListFeaturedTags(r.Context(), id)
	if err != nil {
		slog.Error("api: list account featured tags error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching featured tags")
		return
	}

	result := make([]MastodonFeaturedTag, 0, len(views))
	for _, v := range views {
		result = append(result, h.toMastodonFeaturedTag(v))
	}
	writeAPIJSON(w, http.StatusOK, result)
}
