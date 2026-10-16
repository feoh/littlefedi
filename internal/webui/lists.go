package webui

// Server-rendered Lists UI: index (create + manage), per-list timeline, and
// member management. Every action is a real POST form with a csrf_token so the
// feature works without JavaScript; htmx is layered on top for infinite scroll.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// ListsIndexData is the view model for the lists index page.
type ListsIndexData struct {
	Title     string
	Account   *models.Account
	Lists     []*models.List
	CSRFToken string
	Error     string
}

// ListTimelineData is the view model for a single list's page.
type ListTimelineData struct {
	Title           string
	Account         *models.Account
	List            *models.List
	Statuses        []*models.Status
	Members         []*models.Account
	TimelineCursor  string
	CSRFToken       string
	MemberError     string // human-readable error from a failed member add
	RepliesPolicies []string
	CSPNonce        string
}

// ListsPage lists the user's lists and offers a create form.
func (h *Handler) ListsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	lists, err := h.store.ListLists(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: list lists error", "error", err)
		h.renderError(w, r, "Error fetching lists")
		return
	}

	data := &ListsIndexData{
		Title:     "Lists",
		Account:   account,
		Lists:     lists,
		CSRFToken: getCSRFToken(r.Context()),
		Error:     r.URL.Query().Get("error"),
	}
	h.renderPage(w, r, h.pageTitle(r, "lists.heading"), data, "lists.html")
}

// ListCreateWeb creates a list from the index form.
func (h *Handler) ListCreateWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	repliesPolicy := r.FormValue("replies_policy")
	exclusive := r.FormValue("exclusive") != ""

	list, err := h.app.CreateList(r.Context(), account, title, repliesPolicy, exclusive)
	if err != nil {
		if errors.Is(err, app.ErrListInvalid) {
			http.Redirect(w, r, "/lists?error="+url.QueryEscape(err.Error()), http.StatusFound)
			return
		}
		slog.Error("webui: list create error", "error", err)
		h.renderError(w, r, "Error creating list")
		return
	}
	http.Redirect(w, r, "/lists/"+list.ID, http.StatusFound)
}

// ListTimelinePage shows a list's timeline plus its settings and members.
func (h *Handler) ListTimelinePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	list, err := h.app.GetOwnedList(r.Context(), account, chi.URLParam(r, "id"))
	if err != nil {
		h.renderError(w, r, "List not found")
		return
	}
	h.renderListTimeline(w, r, account, list, queryMaxID(r), "")
}

// renderListTimeline builds and renders a list page, optionally with a member
// error message to surface an inline validation failure.
func (h *Handler) renderListTimeline(w http.ResponseWriter, r *http.Request, account *models.Account, list *models.List, maxID *string, memberErr string) {
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "list:"+list.ID, cursorKey(maxID), list.RepliesPolicy),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetListTimeline(ctx, list, maxID, nil, nil, 20)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: list timeline error", "list", list.ID, "error", err)
		h.renderError(w, r, "Error fetching list timeline")
		return
	}

	members, err := h.store.ListAccountsForList(r.Context(), list.ID, nil, nil, 0)
	if err != nil {
		slog.Error("webui: list members error", "list", list.ID, "error", err)
	}

	if memberErr == "" {
		memberErr = r.URL.Query().Get("error")
	}

	data := &ListTimelineData{
		Title:           list.Title,
		Account:         account,
		List:            list,
		Statuses:        res.Statuses,
		Members:         members,
		TimelineCursor:  res.Cursor,
		CSRFToken:       getCSRFToken(r.Context()),
		MemberError:     memberErr,
		RepliesPolicies: []string{models.ListRepliesList, models.ListRepliesFollowed, models.ListRepliesNone},
		CSPNonce:        r.Header.Get("X-littleFedi-CSP-Nonce"),
	}
	h.renderPage(w, r, list.Title, data, "list_timeline.html")
}

// ListUpdateWeb renames/reconfigures a list.
func (h *Handler) ListUpdateWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	title := r.FormValue("title")
	repliesPolicy := r.FormValue("replies_policy")
	exclusive := r.FormValue("exclusive") != ""

	var titlePtr, rpPtr *string
	if title != "" {
		titlePtr = &title
	}
	if repliesPolicy != "" {
		rpPtr = &repliesPolicy
	}
	if _, err := h.app.UpdateList(r.Context(), account, id, titlePtr, rpPtr, &exclusive); err != nil {
		if errors.Is(err, app.ErrListNotFound) {
			h.renderError(w, r, "List not found")
			return
		}
		http.Redirect(w, r, "/lists/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/lists/"+id, http.StatusFound)
}

// ListDeleteWeb deletes a list.
func (h *Handler) ListDeleteWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.app.DeleteList(r.Context(), account, chi.URLParam(r, "id")); err != nil && !errors.Is(err, app.ErrListNotFound) {
		slog.Error("webui: list delete error", "error", err)
		h.renderError(w, r, "Error deleting list")
		return
	}
	http.Redirect(w, r, "/lists", http.StatusFound)
}

// ListMemberAddWeb adds an account (by handle) to a list.
func (h *Handler) ListMemberAddWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	list, err := h.app.GetOwnedList(r.Context(), account, id)
	if err != nil {
		h.renderError(w, r, "List not found")
		return
	}

	acct := strings.TrimSpace(r.FormValue("acct"))
	target := h.resolveLocalOrKnownAccount(r, acct)
	if target == nil {
		h.renderListTimeline(w, r, account, list, nil, "Account “"+acct+"” not found. You can only add accounts you already follow.")
		return
	}

	if err := h.app.AddListMembers(r.Context(), account, list.ID, []string{target.ID}); err != nil {
		if errors.Is(err, app.ErrListMemberNotFollowed) {
			h.renderListTimeline(w, r, account, list, nil, "You must follow @"+target.Username+" before adding them to a list.")
			return
		}
		slog.Error("webui: list add member error", "error", err)
		h.renderError(w, r, "Error adding member")
		return
	}
	if r.FormValue("from_profile") == "1" {
		http.Redirect(w, r, accountPath(target)+"?list_added=1", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/lists/"+list.ID, http.StatusFound)
}

// ListMemberRemoveWeb removes an account from a list.
func (h *Handler) ListMemberRemoveWeb(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	accountID := chi.URLParam(r, "accountID")
	if err := h.app.RemoveListMembers(r.Context(), account, id, []string{accountID}); err != nil {
		if errors.Is(err, app.ErrListNotFound) {
			h.renderError(w, r, "List not found")
			return
		}
		slog.Error("webui: list remove member error", "error", err)
		h.renderError(w, r, "Error removing member")
		return
	}
	http.Redirect(w, r, "/lists/"+id, http.StatusFound)
}

// PartialListTimeline returns an HTML fragment for infinite scroll.
func (h *Handler) PartialListTimeline(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	list, err := h.app.GetOwnedList(r.Context(), account, chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	maxID := r.URL.Query().Get("max_id")
	limit := 10
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 40 {
		limit = l
	}
	var maxIDPtr *string
	if maxID != "" {
		maxIDPtr = &maxID
	}

	statuses, err := h.store.GetListTimeline(r.Context(), list, maxIDPtr, nil, nil, limit)
	if err != nil {
		slog.Error("webui: partial list timeline error", "list", list.ID, "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	h.app.EnrichStatuses(r.Context(), statuses, account)
	lastID := lastStatusID(statuses)
	statuses, err = h.app.FilterStatuses(r.Context(), "home", statuses, account)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	statuses = collapseTimelineBoosts(statuses)

	w.Header().Set("Content-Type", "text/html")
	if len(statuses) == 0 && lastID == "" {
		w.Write([]byte(`<div class="empty-state">No more posts</div>`))
		return
	}
	nextURL := fmt.Sprintf("/partial/list/%s?max_id=%s&limit=%d", url.PathEscape(list.ID), lastID, limit)
	csrf := getCSRFToken(r.Context())
	locale := h.resolveLocale(r, account)
	for _, s := range statuses {
		_, _ = io.WriteString(w, string(h.renderStatusCard(s, account, csrf, locale)))
	}
	_, _ = io.WriteString(w, fmt.Sprintf(`<div hx-trigger="intersect once" hx-get="%s" hx-swap="afterend"><div class="scroll-trigger"></div></div>`, escapeHTMLAttribute(nextURL)))
}

// resolveLocalOrKnownAccount resolves a handle ("user" or "user@domain") to a
// stored account. It only looks up already-known accounts - list membership
// requires a follow, so an unknown handle can never be a valid member anyway.
func (h *Handler) resolveLocalOrKnownAccount(r *http.Request, acct string) *models.Account {
	acct = strings.TrimPrefix(strings.TrimSpace(acct), "@")
	if acct == "" {
		return nil
	}
	if strings.Contains(acct, "@") {
		parts := strings.SplitN(acct, "@", 2)
		if a, err := h.store.GetAccountByUsernameDomain(r.Context(), parts[0], &parts[1]); err == nil {
			return a
		}
		return nil
	}
	if a, err := h.store.GetAccountByUsernameDomain(r.Context(), acct, nil); err == nil {
		return a
	}
	return nil
}
