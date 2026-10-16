package api

// Mastodon-compatible Lists endpoints. CRUD and membership go through
// app.Service so validation, the members-must-be-followed invariant, and page
// cache invalidation are shared with the web UI.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// listToMastodon converts a models.List to a MastodonList.
func (h *Handler) listToMastodon(list *models.List) MastodonList {
	return MastodonList{
		ID:            list.ID,
		Title:         list.Title,
		RepliesPolicy: list.RepliesPolicy,
		Exclusive:     list.Exclusive,
	}
}

// writeListError maps app-layer list errors to Mastodon-shaped responses.
func writeListError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrListNotFound), errors.Is(err, app.ErrListMemberNotFollowed):
		// Mastodon reports both a missing/foreign list and a not-followed
		// member as "Record not found".
		writeAPIError(w, http.StatusNotFound, "Record not found")
	case errors.Is(err, app.ErrListInvalid):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		slog.Error("api: list operation error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error processing list")
	}
}

// ListsList returns the user's lists.
// GET /api/v1/lists
func (h *Handler) ListsList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	lists, err := h.store.ListLists(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: lists list error", "error", err)
		writeAPIJSON(w, http.StatusOK, []interface{}{})
		return
	}

	result := make([]MastodonList, 0, len(lists))
	for _, l := range lists {
		result = append(result, h.listToMastodon(l))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// ListCreate creates a new list.
// POST /api/v1/lists
func (h *Handler) ListCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusBadRequest, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	list, err := h.app.CreateList(r.Context(), account,
		params.First("title"), params.First("replies_policy"), parseAPIBool(params.First("exclusive")))
	if err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, h.listToMastodon(list))
}

// ListGet returns a single list.
// GET /api/v1/lists/{id}
func (h *Handler) ListGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	list, err := h.app.GetOwnedList(r.Context(), account, chi.URLParam(r, "id"))
	if err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, h.listToMastodon(list))
}

// ListUpdate updates a list.
// PUT /api/v1/lists/{id}
func (h *Handler) ListUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusBadRequest, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var title, repliesPolicy *string
	var exclusive *bool
	if t := params.First("title"); t != "" {
		title = &t
	}
	if rp := params.First("replies_policy"); rp != "" {
		repliesPolicy = &rp
	}
	if ex := params.First("exclusive"); ex != "" {
		v := parseAPIBool(ex)
		exclusive = &v
	}

	list, err := h.app.UpdateList(r.Context(), account, chi.URLParam(r, "id"), title, repliesPolicy, exclusive)
	if err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, h.listToMastodon(list))
}

// ListDelete deletes a list.
// DELETE /api/v1/lists/{id}
func (h *Handler) ListDelete(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if err := h.app.DeleteList(r.Context(), account, chi.URLParam(r, "id")); err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// ListAccounts returns accounts in a list, paginated by account ID. Mastodon
// quirk: the default page is 40 and an explicit limit=0 returns every member.
// GET /api/v1/lists/{id}/accounts
func (h *Handler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	list, err := h.app.GetOwnedList(r.Context(), account, chi.URLParam(r, "id"))
	if err != nil {
		writeListError(w, err)
		return
	}

	params := parsePagination(r)
	limit := 40
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			if n == 0 {
				limit = 0 // all members
			} else if n > 0 && n <= 80 {
				limit = n
			}
		}
	}

	members, err := h.store.ListAccountsForList(r.Context(), list.ID, params.MaxID, params.SinceID, limit)
	if err != nil {
		slog.Error("api: list accounts error", "error", err)
		writeAPIJSON(w, http.StatusOK, []*MastodonAccount{})
		return
	}

	result := make([]*MastodonAccount, 0, len(members))
	for _, a := range members {
		result = append(result, h.accountToMastodon(a, account, false))
	}

	// Cursor Link header (account IDs), only when the page could be full.
	if limit > 0 && len(members) == limit {
		base := "/api/v1/lists/" + list.ID + "/accounts"
		links := []string{
			fmt.Sprintf(`<%s?max_id=%s&limit=%d>; rel="next"`, base, members[len(members)-1].ID, limit),
			fmt.Sprintf(`<%s?since_id=%s&limit=%d>; rel="prev"`, base, members[0].ID, limit),
		}
		w.Header().Set("Link", strings.Join(links, ", "))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// ListAccountsAdd adds accounts to a list. All-or-nothing: any target that is
// missing or not followed rejects the whole request with 404 (Mastodon parity).
// POST /api/v1/lists/{id}/accounts
func (h *Handler) ListAccountsAdd(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusBadRequest, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	accountIDs := params.All("account_ids[]", "account_ids")
	if len(accountIDs) == 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, "account_ids is required")
		return
	}

	if err := h.app.AddListMembers(r.Context(), account, chi.URLParam(r, "id"), accountIDs); err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// ListAccountsRemove removes accounts from a list.
// DELETE /api/v1/lists/{id}/accounts
func (h *Handler) ListAccountsRemove(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params, _, _ := decodeMastodonParams(r, 0)
	accountIDs := params.All("account_ids[]", "account_ids")
	if len(accountIDs) == 0 {
		// If no account_ids specified, try to read from URL query params
		accountIDs = r.URL.Query()["account_ids[]"]
		if len(accountIDs) == 0 {
			accountIDs = r.URL.Query()["account_ids"]
		}
	}

	if err := h.app.RemoveListMembers(r.Context(), account, chi.URLParam(r, "id"), accountIDs); err != nil {
		writeListError(w, err)
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// AccountLists returns the viewer's lists that contain the given account.
// GET /api/v1/accounts/{id}/lists
func (h *Handler) AccountLists(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if _, err := h.store.GetAccount(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusNotFound, "Record not found")
		return
	}

	lists, err := h.store.ListListsContainingAccount(r.Context(), account.ID, id)
	if err != nil {
		slog.Error("api: account lists error", "error", err)
		writeAPIJSON(w, http.StatusOK, []MastodonList{})
		return
	}

	result := make([]MastodonList, 0, len(lists))
	for _, l := range lists {
		result = append(result, h.listToMastodon(l))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// TimelineList returns statuses from a list's members, served through the same
// page cache + Link header pipeline as the home timeline. The list's page key
// kind is "list:<id>" so pageWindowKind validates it against the owner's home
// generation.
// GET /api/v1/timelines/list/{id}
func (h *Handler) TimelineList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	list, err := h.app.GetOwnedList(r.Context(), account, chi.URLParam(r, "id"))
	if err != nil {
		writeListError(w, err)
		return
	}

	params := parsePagination(r)

	keyParts := []string{pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit), list.RepliesPolicy}
	sp, err := h.app.SerializedPageCached(
		r.Context(),
		h.app.PageKey(account.ID, "api:list:"+list.ID, append(append([]string{}, keyParts...), "json")...),
		func(ctx context.Context) (app.SerializedPage, []*models.Status, error) {
			res, err := h.app.PageCached(
				ctx,
				h.app.PageKey(account.ID, "api:list:"+list.ID, keyParts...),
				func(ctx context.Context) (app.PageResult, error) {
					statuses, err := h.store.GetListTimeline(ctx, list, params.MaxID, params.MinID, params.SinceID, params.Limit)
					if err != nil {
						return app.PageResult{}, err
					}
					h.app.EnrichStatuses(ctx, statuses, account)
					// Mastodon applies home-context filters to list timelines.
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
		slog.Error("api: list timeline error", "list_id", list.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching timeline")
		return
	}

	setLinkHeaderIDs(w, "/api/v1/timelines/list/"+list.ID, params, sp.FirstID, sp.LastID, sp.Count)
	writeAPIJSONBytes(w, http.StatusOK, sp.Body)
}
