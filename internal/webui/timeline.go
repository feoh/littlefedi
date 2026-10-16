package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/emoji"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
)

// Home / Timeline Pages

// collapseTimelineBoosts keeps the newest timeline entry for each original
// status and collects the accounts from duplicate boost wrappers for display.
// The input must be in timeline order (newest first).
func collapseTimelineBoosts(statuses []*models.Status) []*models.Status {
	result := make([]*models.Status, 0, len(statuses))
	byOriginal := make(map[string]*models.Status, len(statuses))
	boostersSeen := make(map[string]map[string]bool)

	for _, status := range statuses {
		if status == nil {
			continue
		}

		originalID := status.ID
		if status.BoostOfID != nil && *status.BoostOfID != "" {
			originalID = *status.BoostOfID
		}

		representative, exists := byOriginal[originalID]
		if !exists {
			representative = status
			byOriginal[originalID] = representative
			result = append(result, representative)
		}

		if status.BoostOfID == nil || status.Account == nil {
			continue
		}
		if boostersSeen[originalID] == nil {
			boostersSeen[originalID] = make(map[string]bool)
		}
		accountKey := status.Account.ID
		if accountKey == "" {
			accountKey = status.Account.Username
			if status.Account.Domain != nil {
				accountKey += "@" + *status.Account.Domain
			}
		}
		if !boostersSeen[originalID][accountKey] {
			representative.BoostedBy = append(representative.BoostedBy, status.Account)
			boostersSeen[originalID][accountKey] = true
		}
	}

	return result
}

// cursorKey renders an optional pagination cursor for use in a page cache key.
func cursorKey(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// boolKey renders a bool as a compact page-cache key segment.
func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func lastStatusID(statuses []*models.Status) string {
	for i := len(statuses) - 1; i >= 0; i-- {
		if statuses[i] != nil {
			return statuses[i].ID
		}
	}
	return ""
}

// viewerIDOf returns the account ID used for moderation filtering of shared
// timelines, or "" for an anonymous visitor.
func viewerIDOf(a *models.Account) string {
	if a == nil {
		return ""
	}
	return a.ID
}

// normalizeDomainParam lowercases and trims a domain, stripping any scheme,
// leading "@" or path a form might submit.
func normalizeDomainParam(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	d = strings.TrimPrefix(d, "@")
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexByte(d, '/'); i >= 0 {
		d = d[:i]
	}
	return d
}

// queryMaxID extracts the ?max_id= pagination cursor from a request, returning
// nil when absent. This lets full-page timeline routes paginate for clients
// without JavaScript, where the htmx infinite-scroll loader cannot run.
func queryMaxID(r *http.Request) *string {
	if v := r.URL.Query().Get("max_id"); v != "" {
		return &v
	}
	return nil
}

// HomePage renders the home timeline (or public timeline if not logged in).
func (h *Handler) HomePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		if h.cfg != nil && h.cfg.Web.RedirectGuestsToAbout {
			redirectGuest(w, r, "/about")
			return
		}
		h.LocalTimelinePage(w, r)
		return
	}

	hideReplies := account.HideReplies
	hideBoosts := account.HideBoosts

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "home", cursorKey(maxID), boolKey(hideReplies), boolKey(hideBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetHomeTimeline(ctx, account.ID, maxID, nil, nil, 20, hideReplies, hideBoosts)
			if err != nil {
				return app.PageResult{}, err
			}
			statuses = h.app.VisibleStatuses(ctx, statuses, account)
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: home timeline error", "error", err)
		h.renderError(w, r, "Error fetching timeline")
		return
	}
	statuses := res.Statuses
	timelineCursor := res.Cursor

	data := &PageData{
		Title:          "Home",
		Account:        account,
		Statuses:       statuses,
		Timeline:       "home",
		HideReplies:    hideReplies,
		HideBoosts:     hideBoosts,
		TimelineCursor: timelineCursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	// Quote composer: /?quote={id} pre-loads the home composer to quote a post.
	if quoteID := r.URL.Query().Get("quote"); quoteID != "" {
		if quoted, err := h.store.GetStatus(r.Context(), quoteID); err == nil &&
			h.app.CanViewStatus(r.Context(), quoted, account) {
			h.app.EnrichStatus(r.Context(), quoted, account)
			filtered, err := h.app.FilterStatuses(r.Context(), "home", []*models.Status{quoted}, account)
			if err != nil {
				h.renderError(w, r, "Error fetching timeline")
				return
			}
			if len(filtered) == 1 {
				data.QuotedStatus = filtered[0]
			}
		}
	}

	h.renderPage(w, r, h.pageTitle(r, "nav.home"), data, "timeline.html")
}

// PublicTimelinePage renders the federated public timeline.
func (h *Handler) PublicTimelinePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		redirectGuest(w, r, "/auth/sign_in")
		return
	}

	hideReplies := account != nil && account.HideReplies
	hideBoosts := account != nil && account.HideBoosts

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(account), "public", cursorKey(maxID), boolKey(hideReplies), boolKey(hideBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetPublicTimeline(ctx, viewerIDOf(account), false, maxID, nil, nil, 20, hideReplies, hideBoosts)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "public", statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: public timeline error", "error", err)
		if account != nil {
			h.renderError(w, r, "Error fetching timeline")
		} else {
			http.Error(w, "Error", http.StatusInternalServerError)
		}
		return
	}
	statuses := res.Statuses
	timelineCursor := res.Cursor

	title := h.pageTitle(r, "timeline.federated")
	if r.URL.Path == "/public/local" {
		title = h.pageTitle(r, "timeline.local")
	}

	data := &PageData{
		Title:          title,
		Account:        account,
		Statuses:       statuses,
		Timeline:       "public",
		HideReplies:    hideReplies,
		HideBoosts:     hideBoosts,
		TimelineCursor: timelineCursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, title, data, "timeline.html")
}

// LocalTimelinePage renders the local timeline.
func (h *Handler) LocalTimelinePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil && !h.publicLocalTimelineEnabled() {
		redirectGuest(w, r, "/auth/sign_in")
		return
	}

	hideReplies := account != nil && account.HideReplies
	hideBoosts := account != nil && account.HideBoosts

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(account), "local", cursorKey(maxID), boolKey(hideReplies), boolKey(hideBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetPublicTimeline(ctx, viewerIDOf(account), true, maxID, nil, nil, 20, hideReplies, hideBoosts)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "public", statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: local timeline error", "error", err)
		if account != nil {
			h.renderError(w, r, "Error fetching timeline")
		} else {
			http.Error(w, "Error", http.StatusInternalServerError)
		}
		return
	}
	statuses := res.Statuses
	timelineCursor := res.Cursor

	data := &PageData{
		Title:          "Local Timeline",
		Account:        account,
		Statuses:       statuses,
		Timeline:       "local",
		HideReplies:    hideReplies,
		HideBoosts:     hideBoosts,
		TimelineCursor: timelineCursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "timeline.local"), data, "timeline.html")
}

// BubbleTimelinePage renders the bubble timeline (local + curated instances).
func (h *Handler) BubbleTimelinePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		redirectGuest(w, r, "/auth/sign_in")
		return
	}

	hideReplies := account != nil && account.HideReplies
	hideBoosts := account != nil && account.HideBoosts

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(account), "bubble", cursorKey(maxID), boolKey(hideReplies), boolKey(hideBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetBubbleTimeline(ctx, viewerIDOf(account), maxID, nil, nil, 20, hideReplies, hideBoosts)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "public", statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: bubble timeline error", "error", err)
		if account != nil {
			h.renderError(w, r, "Error fetching timeline")
		} else {
			http.Error(w, "Error", http.StatusInternalServerError)
		}
		return
	}
	statuses := res.Statuses
	timelineCursor := res.Cursor

	data := &PageData{
		Title:          "Bubble Timeline",
		Account:        account,
		Statuses:       statuses,
		Timeline:       "bubble",
		HideReplies:    hideReplies,
		HideBoosts:     hideBoosts,
		TimelineCursor: timelineCursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "timeline.bubble"), data, "timeline.html")
}

// BookmarksPage renders the signed-in user's bookmarked statuses.
func (h *Handler) BookmarksPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "bookmarks", cursorKey(maxID)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.ListBookmarks(ctx, account.ID, maxID, 20)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: statuses, Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: bookmarks error", "error", err)
		h.renderError(w, r, "Error fetching bookmarks")
		return
	}

	data := &PageData{
		Title:          "Bookmarks",
		Account:        account,
		Statuses:       res.Statuses,
		Timeline:       "bookmarks",
		TimelineCursor: res.Cursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "mobile.bookmarks"), data, "timeline.html")
}

// FavouritesPage renders the signed-in user's favourited statuses.
func (h *Handler) FavouritesPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "favourites", cursorKey(maxID)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.ListFavourites(ctx, account.ID, maxID, 20)
			if err != nil {
				return app.PageResult{}, err
			}
			cursor := lastStatusID(statuses)
			statuses = h.app.VisibleStatuses(ctx, statuses, account)
			h.app.EnrichStatuses(ctx, statuses, account)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: statuses, Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: favourites error", "error", err)
		h.renderError(w, r, "Error fetching favourites")
		return
	}

	data := &PageData{
		Title:          "Favourites",
		Account:        account,
		Statuses:       res.Statuses,
		Timeline:       "favourites",
		TimelineCursor: res.Cursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "mobile.favourites"), data, "timeline.html")
}

// DirectMessagesPage renders the signed-in user's direct (private) messages -
// statuses they authored or were mentioned in with direct visibility.
func (h *Handler) DirectMessagesPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "direct", cursorKey(maxID)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetDirectTimeline(ctx, account.ID, maxID, nil, nil, 20)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: statuses, Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: direct messages error", "error", err)
		h.renderError(w, r, "Error fetching direct messages")
		return
	}

	data := &PageData{
		Title:          "Direct messages",
		Account:        account,
		Statuses:       res.Statuses,
		Timeline:       "direct",
		TimelineCursor: res.Cursor,
		CSRFToken:      getCSRFToken(r.Context()),
	}
	if targetID := r.URL.Query().Get("account"); targetID != "" {
		if target, err := h.store.GetAccount(r.Context(), targetID); err == nil && target.ID != account.ID {
			data.DraftContent = accountMention(target) + " "
			data.DraftVisibility = models.VisibilityDirect
		}
	}

	h.renderPage(w, r, h.pageTitle(r, "mobile.direct"), data, "timeline.html")
}

// Search Page

// SearchData is the view model for the search results page.
type SearchData struct {
	Title     string
	Account   *models.Account // the viewer
	Query     string
	Accounts  []*models.Account
	Statuses  []*models.Status
	Hashtags  []*models.Tag
	CSRFToken string
}

// SearchPage renders combined search results for accounts, statuses, and
// hashtags. An explicit handle (@user@domain) or URL is resolved and redirects
// straight to the matching profile; anything else shows a results page.
func (h *Handler) SearchPage(w http.ResponseWriter, r *http.Request) {
	viewer := getAuthAccount(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	data := &SearchData{
		Title:     "Search",
		Account:   viewer,
		Query:     q,
		Accounts:  []*models.Account{},
		Statuses:  []*models.Status{},
		Hashtags:  []*models.Tag{},
		CSRFToken: getCSRFToken(r.Context()),
	}

	if q == "" {
		h.renderPage(w, r, h.pageTitle(r, "nav.search"), data, "search.html")
		return
	}

	handle := strings.TrimPrefix(q, "@")
	isURL := strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://")

	// A pasted URL may be a status permalink rather than a profile - check
	// that first (cheap local lookup, then an on-demand remote fetch) so it
	// jumps to the post instead of being misparsed as an account below.
	if isURL {
		if status, err := h.store.GetStatusByURI(r.Context(), q); err == nil && status != nil &&
			h.app.CanViewStatus(r.Context(), status, viewer) {
			http.Redirect(w, r, statusPath(status), http.StatusFound)
			return
		}
		if h.inboxProcessor != nil {
			if status, err := h.inboxProcessor.ResolveRemoteStatusByURL(r.Context(), q); err == nil && status != nil {
				if !h.app.CanViewStatus(r.Context(), status, viewer) {
					http.NotFound(w, r)
					return
				}
				if status.Account == nil {
					status.Account, _ = h.store.GetAccount(r.Context(), status.AccountID)
				}
				http.Redirect(w, r, statusPath(status), http.StatusFound)
				return
			} else if err != nil {
				slog.Debug("webui: search status URL resolve failed", "query", q, "error", err)
			}
		}
	}

	// An explicit remote handle or URL is a "jump to profile" - resolve it and
	// redirect. If resolution fails we fall through and show search results.
	if (isURL || strings.Contains(handle, "@")) && h.federator != nil {
		if account, err := h.federator.ResolveAccount(r.Context(), handle); err == nil && account != nil {
			if account.Domain != nil {
				http.Redirect(w, r, "/@"+account.Username+"@"+*account.Domain, http.StatusFound)
			} else {
				http.Redirect(w, r, "/@"+account.Username, http.StatusFound)
			}
			return
		} else if err != nil {
			slog.Debug("webui: search account resolve failed", "query", handle, "error", err)
		}
	}

	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}

	if accounts, err := h.store.SearchAccounts(r.Context(), q, 0, 20); err != nil {
		slog.Error("webui: search accounts error", "error", err)
	} else {
		h.app.LoadAccountsEmojis(r.Context(), accounts)
		data.Accounts = accounts
	}

	if statuses, err := h.store.SearchStatuses(r.Context(), q, nil, viewerID, 0, 20); err != nil {
		slog.Error("webui: search statuses error", "error", err)
	} else {
		h.app.EnrichStatuses(r.Context(), statuses, viewer)
		data.Statuses, err = h.app.FilterStatuses(r.Context(), "public", statuses, viewer)
		if err != nil {
			slog.Error("webui: search filter error", "error", err)
			h.renderError(w, r, "Error fetching search results")
			return
		}
	}

	if tags, err := h.store.SearchTags(r.Context(), q, 0, 20); err != nil {
		slog.Error("webui: search hashtags error", "error", err)
	} else {
		data.Hashtags = tags
	}

	h.renderPage(w, r, h.pageTitle(r, "page.search_query", q), data, "search.html")
}

// Hashtag Timeline Page

// TagTimelineData is the view model for a hashtag timeline page.
type TagTimelineData struct {
	Title          string
	Account        *models.Account
	Tag            string
	Statuses       []*models.Status
	TimelineCursor string
	LoggedIn       bool
	Following      bool
	StatusesCount  int
	CSRFToken      string
}

// TagTimelinePage renders public statuses carrying a given hashtag.
func (h *Handler) TagTimelinePage(w http.ResponseWriter, r *http.Request) {
	viewer := getAuthAccount(r.Context())
	tag := tagNameFromPath(r)
	if tag == "" {
		http.Redirect(w, r, "/search", http.StatusFound)
		return
	}

	// Unauthenticated visitors get a local-only view so we don't surface
	// uncurated remote posts under this instance's tag namespace.
	localOnly := viewer == nil
	maxID := queryMaxID(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(viewer), "tag:"+tag, boolKey(localOnly), cursorKey(maxID)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetTagTimeline(ctx, viewerIDOf(viewer), tag, maxID, nil, nil, 20, false, false, localOnly)
			if err != nil {
				return app.PageResult{}, err
			}
			h.app.EnrichStatuses(ctx, statuses, viewer)
			cursor := lastStatusID(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "public", statuses, viewer)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, err
		})
	if err != nil {
		slog.Error("webui: tag timeline error", "tag", tag, "error", err)
		h.renderError(w, r, "Error fetching hashtag timeline")
		return
	}
	statuses := res.Statuses
	timelineCursor := res.Cursor

	data := &TagTimelineData{
		Title:          "#" + tag,
		Account:        viewer,
		Tag:            tag,
		Statuses:       statuses,
		TimelineCursor: timelineCursor,
		LoggedIn:       viewer != nil,
		CSRFToken:      getCSRFToken(r.Context()),
	}

	// Follow state and post count come from the tag record, which may not yet
	// exist if nobody local has posted with it.
	if tagObj, err := h.store.GetTagByName(r.Context(), tag); err == nil {
		data.StatusesCount = tagObj.StatusesCount
		if viewer != nil {
			if following, err := h.store.IsFollowingTag(r.Context(), viewer.ID, tagObj.ID); err == nil {
				data.Following = following
			}
		}
	}

	h.renderPage(w, r, "#"+tag, data, "tag_timeline.html")
}

// normalizeTag trims surrounding whitespace and a leading '#' and lowercases
// the hashtag name so lookups match the stored (lowercase) tag names.
func normalizeTag(raw string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "#"))
}

func tagNameFromPath(r *http.Request) string {
	tag := chi.URLParam(r, "tag")
	// chi uses RawPath when available, preserving percent escapes.
	if r.URL.RawPath != "" {
		tag, _ = url.PathUnescape(tag)
	}
	return normalizeTag(tag)
}

func validTagQuery(tag string) bool {
	return tag != "" && strings.IndexFunc(tag, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	}) == -1
}

// FollowTagWeb subscribes the logged-in user to a hashtag (web UI POST).
func (h *Handler) FollowTagWeb(w http.ResponseWriter, r *http.Request) {
	h.setTagFollowWeb(w, r, true)
}

// UnfollowTagWeb unsubscribes the logged-in user from a hashtag (web UI POST).
func (h *Handler) UnfollowTagWeb(w http.ResponseWriter, r *http.Request) {
	h.setTagFollowWeb(w, r, false)
}

func (h *Handler) setTagFollowWeb(w http.ResponseWriter, r *http.Request, follow bool) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	tag := tagNameFromPath(r)
	if tag == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	tagObj, err := h.store.GetOrCreateTag(r.Context(), tag)
	if err != nil {
		slog.Error("webui: get or create tag error", "tag", tag, "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if follow {
		err = h.store.FollowTag(r.Context(), account.ID, tagObj.ID)
	} else {
		err = h.store.UnfollowTag(r.Context(), account.ID, tagObj.ID)
	}
	if err != nil {
		slog.Error("webui: set tag follow error", "tag", tag, "follow", follow, "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(tagFollowButtonHTML(tag, follow, getCSRFToken(r.Context()))))
		return
	}

	ref := safeReturnPath(r, r.Referer(), "/tags/"+url.PathEscape(tag))
	http.Redirect(w, r, ref, http.StatusFound)
}

// tagFollowButtonHTML renders the follow/unfollow toggle so htmx can swap it
// in place after a follow state change. following reflects the state after
// the toggle. The markup must match the forms in tag_timeline.html and
// followed_tags.html: a real POST form so the toggle keeps working without
// JavaScript after a swap.
func tagFollowButtonHTML(tag string, following bool, csrf string) string {
	action, class, label := "follow", "btn btn-primary", "Follow"
	if following {
		action, class, label = "unfollow", "btn btn-ghost", "Following"
	}
	u := "/tags/" + url.PathEscape(tag) + "/" + action
	return fmt.Sprintf(`<form method="post" action="%s" hx-post="%s" hx-swap="outerHTML" style="display:inline-flex;"><input type="hidden" name="csrf_token" value="%s"><button class="%s" type="submit">%s</button></form>`,
		u, u, html.EscapeString(csrf), class, label)
}

// FollowedTagsData is the view model for the followed-hashtags page.
type FollowedTagsData struct {
	Title     string
	Account   *models.Account
	Tags      []*models.Tag
	CSRFToken string
	Query     string
	Searched  bool
	Results   []*models.Tag
	Followed  map[string]bool
}

// FollowedTagsPage lists followed hashtags and lets the user search for or
// follow a tag by name, even before any posts use it.
func (h *Handler) FollowedTagsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	tags, err := h.store.ListFollowedTags(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: list followed tags error", "error", err)
		h.renderError(w, r, "Error fetching followed hashtags")
		return
	}

	followed := make(map[string]bool, len(tags))
	for _, t := range tags {
		followed[t.Name] = true
	}

	query := normalizeTag(r.URL.Query().Get("q"))
	searched := query != ""
	var results []*models.Tag
	// Two runes is the same floor the composer's autocomplete uses: shorter
	// queries match almost everything and are not worth a table scan.
	if utf8.RuneCountInString(query) >= 2 {
		results, err = h.store.SearchTags(r.Context(), query, 0, 20)
		if err != nil {
			slog.Error("webui: search hashtags error", "error", err)
			results = nil
		}
	}
	// Always offer the exact name on top, so a tag nobody has used yet can
	// still be followed.
	if validTagQuery(query) {
		exact := false
		for _, tag := range results {
			if strings.EqualFold(tag.Name, query) {
				exact = true
				break
			}
		}
		if !exact {
			results = append([]*models.Tag{{Name: query}}, results...)
		}
	}

	data := &FollowedTagsData{
		Title:     "Followed hashtags",
		Account:   account,
		Tags:      tags,
		CSRFToken: getCSRFToken(r.Context()),
		Query:     r.URL.Query().Get("q"),
		Searched:  searched,
		Results:   results,
		Followed:  followed,
	}
	h.renderPage(w, r, h.pageTitle(r, "followed_tags.heading"), data, "followed_tags.html")
}

// PartialTagTimeline returns an HTML fragment of statuses for a hashtag, used
// for infinite scroll on the tag page.
func (h *Handler) PartialTagTimeline(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	tag := tagNameFromPath(r)
	if tag == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
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

	statuses, err := h.store.GetTagTimeline(r.Context(), viewerIDOf(account), tag, maxIDPtr, nil, nil, limit, false, false, account == nil)
	if err != nil {
		slog.Error("webui: partial tag timeline error", "tag", tag, "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.app.EnrichStatuses(r.Context(), statuses, account)
	lastID := lastStatusID(statuses)
	statuses, err = h.app.FilterStatuses(r.Context(), "public", statuses, account)
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

	nextURL := fmt.Sprintf("/partial/tag/%s?max_id=%s&limit=%d", url.PathEscape(tag), lastID, limit)

	csrf := getCSRFToken(r.Context())
	locale := h.resolveLocale(r, account)
	for _, s := range statuses {
		_, _ = io.WriteString(w, string(h.renderStatusCard(s, account, csrf, locale)))
	}
	_, _ = io.WriteString(w, fmt.Sprintf(`<div hx-trigger="intersect once" hx-get="%s" hx-swap="afterend"><div class="scroll-trigger"></div></div>`, escapeHTMLAttribute(nextURL)))
}

// AutocompleteTags returns JSON hashtags matching the query prefix, used by the
// composer for #hashtag autocompletion.
// GET /api/tags/autocomplete?q=foo
func (h *Handler) AutocompleteTags(w http.ResponseWriter, r *http.Request) {
	query := normalizeTag(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if utf8.RuneCountInString(query) < 2 {
		w.Write([]byte("[]"))
		return
	}

	tags, err := h.store.SearchTagPrefixes(r.Context(), query, 0, 10)
	if err != nil {
		slog.Error("webui: tag autocomplete search error", "error", err)
		w.Write([]byte("[]"))
		return
	}

	type tagResult struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		StatusesCount int    `json:"statuses_count"`
	}
	results := make([]tagResult, 0, len(tags))
	for _, t := range tags {
		results = append(results, tagResult{Name: t.Name, URL: t.URL, StatusesCount: t.StatusesCount})
	}
	json.NewEncoder(w).Encode(results)
}

// AutocompleteEmojis returns JSON emoji entries whose shortcode starts with the
// query, used by the composer for :shortcode: autocompletion. Unicode emojis are
// searched first, then custom emojis from the database.
// GET /api/emojis/autocomplete?q=wink
func (h *Handler) AutocompleteEmojis(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimLeft(strings.TrimPrefix(r.URL.Query().Get("q"), ":"), " ")
	w.Header().Set("Content-Type", "application/json")
	if query == "" {
		w.Write([]byte("[]"))
		return
	}

	type emojiResult struct {
		Shortcode string `json:"shortcode"`
		Char      string `json:"char,omitempty"`
		URL       string `json:"url,omitempty"`
		StaticURL string `json:"static_url,omitempty"`
		Domain    string `json:"domain,omitempty"`
		Type      string `json:"type"` // "unicode" or "custom"
	}
	var results []emojiResult

	// Unicode emojis (prefix match, limited to 6)
	for shortcode, char := range emoji.UnicodeMap {
		if strings.HasPrefix(shortcode, query) {
			results = append(results, emojiResult{
				Shortcode: shortcode,
				Char:      char,
				Type:      "unicode",
			})
			if len(results) >= 6 {
				break
			}
		}
	}

	// Custom emojis from the database
	customEmojis, err := h.store.SearchEmojis(r.Context(), query, 0, 10)
	if err != nil {
		slog.Error("webui: emoji autocomplete search error", "error", err)
	}
	for _, e := range customEmojis {
		url := h.browserMediaURL(e.URL)
		staticURL := h.browserMediaURL(e.StaticURL)
		domain := ""
		if e.Domain != nil {
			domain = *e.Domain
		}
		results = append(results, emojiResult{
			Shortcode: e.Shortcode,
			URL:       url,
			StaticURL: staticURL,
			Domain:    domain,
			Type:      "custom",
		})
	}

	if results == nil {
		results = make([]emojiResult, 0)
	}
	json.NewEncoder(w).Encode(results)
}

// Thread Page

// ThreadPage renders a single status and its replies.
func (h *Handler) ThreadPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Accept")
	id := chi.URLParam(r, "id")
	viewer := getAuthAccount(r.Context())

	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		slog.Warn("webui: status not found", "id", id, "error", err)
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, viewer) {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}

	// ActivityPub clients dereferencing the web permalink get the canonical
	// AP object instead of HTML: local objects live at /objects/{id} (served
	// through the federation response cache), remote ones belong to their
	// origin server. This runs before any enrichment or page caching, so AP
	// fetches never populate or touch the HTML-side caches.
	if acceptsActivityPub(r) {
		if status.Account != nil && status.Account.Domain != nil {
			http.Redirect(w, r, safeHTTPRedirectTarget(status.URI, "/"), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/objects/"+status.ID, http.StatusFound)
		return
	}

	// Unauthenticated visitors shouldn't read other instances' posts from here -
	// send them to the post's home instance instead.
	if viewer == nil && status.Account != nil && status.Account.Domain != nil {
		dest := status.URL
		if dest == "" {
			dest = status.URI
		}
		http.Redirect(w, r, safeHTTPRedirectTarget(dest, "/"), http.StatusFound)
		return
	}
	// Start (or join) a background crawl that completes the full conversation -
	// ancestors plus cross-instance replies - off the request path. The page is
	// served immediately from cache; the template polls for the rest to arrive.
	fetching := false
	if h.threadResolver != nil {
		fetching = h.threadResolver.RefreshInBackground(status)
	}

	// Get context (ancestors and descendants)
	ancestors, descendants, err := h.store.GetStatusContext(r.Context(), status.ID)
	if err != nil {
		ancestors = []*models.Status{}
		descendants = []*models.Status{}
	}
	ancestors = h.app.VisibleStatuses(r.Context(), ancestors, viewer)
	descendants = h.app.VisibleStatuses(r.Context(), descendants, viewer)

	h.app.EnrichStatus(r.Context(), status, viewer)
	h.app.EnrichStatuses(r.Context(), ancestors, viewer)
	h.app.EnrichStatuses(r.Context(), descendants, viewer)
	filtered, err := h.app.FilterStatuses(r.Context(), "thread", append([]*models.Status{status}, append(ancestors, descendants...)...), viewer)
	if err != nil {
		h.renderError(w, r, "Error fetching thread")
		return
	}
	if len(filtered) == 0 || filtered[0].ID != status.ID {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	status = filtered[0]
	ancestors = filteredThreadPart(filtered[1:], ancestors)
	descendants = filteredThreadPart(filtered[1:], descendants)
	// Never make opening a post wait on its origin server. A reader can request
	// a live remote refresh from the local snapshot rendered below.
	resolveInteractions := h.interactions.ResolveLocal
	if r.URL.Query().Get("refresh_interactions") == "1" {
		resolveInteractions = h.interactions.Resolve
	}
	interactions := resolveInteractions(r.Context(), status)
	status = app.WithInteractionCounts(status, interactions.FavouritesCount, interactions.ReblogsCount)
	h.app.LoadAccountsEmojis(r.Context(), interactions.Favourites)
	h.app.LoadAccountsEmojis(r.Context(), interactions.Reblogs)
	for _, reaction := range interactions.EmojiReactions {
		h.app.LoadAccountsEmojis(r.Context(), reaction.Accounts)
	}
	quotes := h.ownStatusQuotes(r.Context(), status, viewer)

	data := &ThreadData{
		Title:        "Status by " + status.Account.Username,
		Account:      viewer,
		Status:       status,
		Ancestors:    ancestors,
		Descendants:  buildThreadReplies(status.ID, descendants),
		FavouritedBy: interactions.Favourites,
		RebloggedBy:  interactions.Reblogs,
		Quotes:       quotes,
		Reactions:    interactions.EmojiReactions,
		CSRFToken:    getCSRFToken(r.Context()),
		Fetching:     fetching,
		Replying:     r.URL.Query().Get("reply") == "1",
	}

	h.renderPage(w, r, h.pageTitle(r, "thread.post_by")+" "+status.Account.Username, data, "thread.html")
}

// InteractionPartial resolves the optional remote interaction enhancement.
// The main thread page remains fully usable without this request (including in
// text browsers); a slow origin can delay only this small fragment.
func (h *Handler) InteractionPartial(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, getAuthAccount(r.Context())) {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if status.Account == nil && status.AccountID != "" {
		status.Account, _ = h.store.GetAccount(r.Context(), status.AccountID)
	}
	resolveInteractions := h.interactions.ResolveLocal
	if r.URL.Query().Get("refresh") == "1" {
		resolveInteractions = h.interactions.Resolve
	}
	interactions := resolveInteractions(r.Context(), status)
	status.FavouritesCount = interactions.FavouritesCount
	status.ReblogsCount = interactions.ReblogsCount
	h.app.LoadAccountsEmojis(r.Context(), interactions.Favourites)
	h.app.LoadAccountsEmojis(r.Context(), interactions.Reblogs)
	for _, reaction := range interactions.EmojiReactions {
		h.app.LoadAccountsEmojis(r.Context(), reaction.Accounts)
	}
	quotes := h.ownStatusQuotes(r.Context(), status, getAuthAccount(r.Context()))

	w.Header().Set("Content-Type", "text/html")
	data := map[string]interface{}{
		"Status":       status,
		"FavouritedBy": interactions.Favourites,
		"RebloggedBy":  interactions.Reblogs,
		"Account":      getAuthAccount(r.Context()),
		"Quotes":       quotes,
		"Reactions":    interactions.EmojiReactions,
		// Only the partial carries the out-of-band count spans: the full page
		// renders those ids already, in the action bar.
		"OOBCounts": true,
	}
	if err := h.localeTemplates(r).ExecuteTemplate(w, "components/status-interactions.html", data); err != nil {
		slog.Error("webui: render status interactions partial error", "error", err)
	}
}

func (h *Handler) ownStatusQuotes(ctx context.Context, status *models.Status, viewer *models.Account) []*models.Status {
	if viewer == nil || status == nil || viewer.ID != status.AccountID {
		return nil
	}
	// Show the newest 80 quotes; paginate if a full history is needed.
	quotes, _ := h.store.ListStatusQuotes(ctx, status.ID, 80)
	quotes = h.app.VisibleStatuses(ctx, quotes, viewer)
	h.app.EnrichStatuses(ctx, quotes, viewer)
	return quotes
}

// threadPollMaxTries bounds how many times the thread-replies fragment
// re-polls (at ~2s apart) while a background crawl completes the conversation.
// The resolver's Fetching flag is the real signal, but an origin server that
// never answers can leave it true indefinitely, and an open tab would keep
// requesting forever.
const threadPollMaxTries = 15

// ThreadPartial returns the thread's replies fragment (and an out-of-band
// ancestors refresh) for HTMX polling while a background crawl completes the
// conversation. GET /partial/thread/{id}
func (h *Handler) ThreadPartial(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	viewer := getAuthAccount(r.Context())
	tries, _ := strconv.Atoi(r.URL.Query().Get("tries"))

	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, viewer) {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}

	ancestors, descendants, err := h.store.GetStatusContext(r.Context(), status.ID)
	if err != nil {
		ancestors = []*models.Status{}
		descendants = []*models.Status{}
	}
	ancestors = h.app.VisibleStatuses(r.Context(), ancestors, viewer)
	descendants = h.app.VisibleStatuses(r.Context(), descendants, viewer)
	h.app.EnrichStatuses(r.Context(), ancestors, viewer)
	h.app.EnrichStatuses(r.Context(), descendants, viewer)
	all, err := h.app.FilterStatuses(r.Context(), "thread", append(ancestors, descendants...), viewer)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	ancestors = filteredThreadPart(all, ancestors)
	descendants = filteredThreadPart(all, descendants)

	fetching := h.threadResolver != nil && tries < threadPollMaxTries && h.threadResolver.Fetching(status.URI)

	data := map[string]interface{}{
		"StatusID":    status.ID,
		"Ancestors":   ancestors,
		"Descendants": buildThreadReplies(status.ID, descendants),
		"Account":     viewer,
		"CSRFToken":   getCSRFToken(r.Context()),
		"Fetching":    fetching,
		"PollTries":   tries + 1,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := h.localeTemplates(r).ExecuteTemplate(w, "components/thread-poll.html", data); err != nil {
		slog.Error("webui: render thread poll partial error", "error", err)
	}
}

func filteredThreadPart(filtered, original []*models.Status) []*models.Status {
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

// Notifications Page

type notificationPreferenceOption struct {
	Name    string
	Label   string
	Types   []string
	Checked bool
}

var notificationPreferenceOptions = []notificationPreferenceOption{
	{Name: "mentions", Label: "Mentions", Types: []string{"mention"}},
	{Name: "likes", Label: "Likes", Types: []string{"favourite"}},
	{Name: "boosts", Label: "Boosts", Types: []string{"reblog"}},
	{Name: "quotes", Label: "Quotes", Types: []string{"quote"}},
	{Name: "follows", Label: "Follows and follow requests", Types: []string{"follow", "follow_request", "follow_accept", "follow_reject", "unfollow"}},
	{Name: "polls", Label: "Poll results", Types: []string{"poll"}},
	{Name: "updates", Label: "Post edits", Types: []string{"update"}},
	{Name: "account_activity", Label: "Account and moderation activity", Types: []string{"move", "block", "unblock", "admin.report", "admin.account_deleted", "admin.account_moved"}},
}

func notificationExcludedTypes(account *models.Account) []string {
	return app.NotificationExcludedTypes(account)
}

// allNotificationTypes flattens every known notification type. Used to turn
// the "Mentions" tab filter (an allowlist) into an exclude-list, since
// ListNotifications only knows how to exclude types.
func allNotificationTypes() []string {
	var types []string
	for _, opt := range notificationPreferenceOptions {
		types = append(types, opt.Types...)
	}
	return append(types, "emoji_reaction")
}

// notificationFilterExcludedTypes combines the viewer's notification
// preferences with the page's "filter" query param (currently just
// "mentions") into a single exclude-list for ListNotifications.
func notificationFilterExcludedTypes(filter string, account *models.Account) []string {
	excluded := notificationExcludedTypes(account)
	if filter == "mentions" {
		for _, t := range allNotificationTypes() {
			if t != "mention" {
				excluded = append(excluded, t)
			}
		}
		// "status" (the notify-bell "posted" notification for favourited
		// accounts) and "blog.rebuilt" have no settings toggle of their own,
		// so they're absent from allNotificationTypes() and must be excluded
		// explicitly here.
		excluded = append(excluded, "status", "blog.rebuilt", "follow_import.complete", "follow_import.failed")
	}
	return excluded
}

func notificationOptionsFor(account *models.Account, locale string) []notificationPreferenceOption {
	excluded := make(map[string]bool)
	for _, notificationType := range notificationExcludedTypes(account) {
		excluded[notificationType] = true
	}
	options := make([]notificationPreferenceOption, len(notificationPreferenceOptions))
	for i, option := range notificationPreferenceOptions {
		option.Label = i18n.T(locale, "settings.notif_option."+option.Name)
		option.Checked = true
		for _, notificationType := range option.Types {
			if excluded[notificationType] {
				option.Checked = false
				break
			}
		}
		options[i] = option
	}
	return options
}

type enrichedNotification struct {
	ID              string
	CreatedAt       time.Time
	Type            string
	TypeLabel       string
	Icon            string
	Account         *models.Account
	MovedToURL      string
	MovedToLabel    string
	ImportURL       string
	ImportResult    *models.AccountImportResult
	Status          *models.Status
	Read            bool
	Count           int
	AdditionalCount int
	GroupKey        string
	Emoji           string
	ReactionEmoji   *models.CustomEmoji
	// Pending marks a follow_request whose follow is still awaiting an answer,
	// so the row renders its accept/reject buttons. CSRFToken is carried per row
	// because the component is rendered with the notification as its dot.
	Pending   bool
	CSRFToken string
}

// notificationsPageSize bounds how many raw notifications are loaded per
// request. Keeping the batch small matters on low-power devices (e.g. a
// Raspberry Pi Zero W) where enriching every row means a DB lookup and
// content/emoji resolution per notification. The rest load on demand via
// infinite scroll (or a "Load older notifications" link without JavaScript).
const notificationsPageSize = 20

// NotificationsPage renders the notifications page.
func (h *Handler) NotificationsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	filter := r.URL.Query().Get("filter")

	notifications, err := h.store.ListNotifications(r.Context(), account.ID, queryMaxID(r), notificationsPageSize, notificationFilterExcludedTypes(filter, account))
	if err != nil {
		slog.Error("webui: notifications error", "error", err)
		notifications = []*models.Notification{}
	}

	// The cursor is the oldest raw notification in this batch; grouping never
	// changes it, so paging stays correct whether or not grouping is enabled.
	lastID := lastNotificationID(notifications)
	hasMore := len(notifications) == notificationsPageSize

	enriched, err := h.enrichNotificationsChecked(r.Context(), account, notifications)
	if err != nil {
		h.renderError(w, r, "Error fetching notifications")
		return
	}

	h.markLoadedNotificationsRead(r.Context(), account.ID, notifications)

	data := &NotificationsData{
		Title:          "Notifications",
		Account:        account,
		Notifications:  enriched,
		CSRFToken:      getCSRFToken(r.Context()),
		TimelineCursor: lastID,
		HasMore:        hasMore,
		Filter:         filter,
	}

	h.renderPage(w, r, h.pageTitle(r, "nav.notifications"), data, "notifications.html")
}

// enrichNotifications turns raw notifications into display rows: it resolves
// the actor and (for post-related notifications) the target status, resolves
// custom emojis in actor display names and, when the viewer has grouping
// enabled, collapses consecutive notifications of the same type/target into a
// single row with an "and N others" count. Shared by the full page and the
// infinite-scroll partial so both render identical markup.
func (h *Handler) enrichNotifications(ctx context.Context, account *models.Account, notifications []*models.Notification) []*enrichedNotification {
	enriched, _ := h.enrichNotificationsChecked(ctx, account, notifications)
	return enriched
}

func (h *Handler) enrichNotificationsChecked(ctx context.Context, account *models.Account, notifications []*models.Notification) ([]*enrichedNotification, error) {
	// Batch-load the shared relations to keep ordinary notification pages
	// cheap on low-power devices.
	accountIDs := make([]string, 0, len(notifications))
	statusIDs := make([]string, 0, len(notifications))
	for _, n := range notifications {
		accountIDs = append(accountIDs, n.FromAccountID)
		if n.StatusID != nil && *n.StatusID != "" {
			statusIDs = append(statusIDs, *n.StatusID)
		}
	}
	accounts, _ := h.store.GetAccountsByIDs(ctx, accountIDs)
	statuses, _ := h.store.GetStatusesByIDs(ctx, statusIDs)

	statusList := make([]*models.Status, 0, len(statuses))
	for id, st := range statuses {
		if !h.app.CanViewStatus(ctx, st, account) {
			delete(statuses, id)
			continue
		}
		statusList = append(statusList, st)
	}
	h.app.EnrichStatuses(ctx, statusList, account)
	statusList, err := h.app.FilterStatuses(ctx, "notifications", statusList, account)
	if err != nil {
		return nil, err
	}
	// Enrichment may replace slice elements (component-cache hits are
	// copies), so rebuild the lookup map from the enriched slice.
	clear(statuses)
	for _, st := range statusList {
		statuses[st.ID] = st
	}

	// Resolve custom emojis in the actors' display names so they render inline.
	fromAccounts := make([]*models.Account, 0, len(accounts))
	for _, a := range accounts {
		fromAccounts = append(fromAccounts, a)
	}
	h.app.LoadAccountsEmojis(ctx, fromAccounts)

	enriched := make([]*enrichedNotification, 0, len(notifications))
	for _, n := range notifications {
		fromAccount := accounts[n.FromAccountID]
		if fromAccount == nil {
			continue
		}

		groupKey := n.Type
		if n.Type == "move" || n.Type == "admin.account_moved" {
			groupKey += "\x00" + n.FromAccountID
		}
		if n.StatusID != nil {
			groupKey += "\x00" + *n.StatusID
		}
		en := &enrichedNotification{
			ID:        n.ID,
			CreatedAt: n.CreatedAt,
			Type:      n.Type,
			Read:      n.Read,
			Account:   fromAccount,
			Count:     1,
			GroupKey:  groupKey,
			Emoji:     n.Emoji,
		}
		if n.Type == "move" || n.Type == "admin.account_moved" {
			en.MovedToURL, en.MovedToLabel = h.movedProfile(ctx, fromAccount, account)
		}
		if kind, importID := models.ParseFollowImportNotificationType(n.Type); importID != "" {
			en.Type = kind
			en.ImportURL = "/settings/import?import_id=" + url.QueryEscape(importID)
			if imp, err := h.store.GetAccountImport(ctx, importID, account.ID); err == nil {
				var result models.AccountImportResult
				if err := json.Unmarshal([]byte(imp.ResultJSON), &result); err == nil && result.FollowImport != nil {
					en.ImportResult = &result
				}
			}
		}

		switch en.Type {
		case "follow":
			en.TypeLabel = "Followed you"
			en.Icon = "user-plus"
		case "favourite":
			en.TypeLabel = "Favourited your post"
			en.Icon = "heart"
		case "emoji_reaction":
			en.TypeLabel = "Reacted to your post"
			en.Icon = "heart"
		case "reblog":
			en.TypeLabel = "Boosted your post"
			en.Icon = "repeat"
		case "quote":
			en.TypeLabel = "Quoted your post"
			en.Icon = "quote"
		case "mention":
			en.TypeLabel = "Mentioned you"
			en.Icon = "at-sign"
		case "follow_request":
			en.TypeLabel = "Wants to follow you"
			en.Icon = "user-plus"
			// One lookup per follow_request row (at most the page size,
			// usually zero or one); batch it if pending requests ever get common.
			if follow, err := h.store.GetFollowByPair(ctx, n.FromAccountID, account.ID); err == nil && follow.State == models.FollowPending {
				en.Pending = true
				en.CSRFToken = getCSRFToken(ctx)
			}
		case "update":
			en.TypeLabel = "Edited a post"
			en.Icon = "edit"
		case "move":
			en.TypeLabel = "Moved to a new account"
			en.Icon = "repeat"
		case "unfollow":
			en.TypeLabel = "Unfollowed you"
			en.Icon = "user-minus"
		case "block":
			en.TypeLabel = "Blocked you"
			en.Icon = "slash"
		case "unblock":
			en.TypeLabel = "Unblocked you"
			en.Icon = "user-plus"
		case "follow_accept":
			en.TypeLabel = "Accepted your follow"
			en.Icon = "user-plus"
		case "follow_reject":
			en.TypeLabel = "Rejected your follow"
			en.Icon = "user-minus"
		case "blog.rebuilt":
			en.TypeLabel = "Regenerated your blog"
			en.Icon = "bell"
		case "follow_import.complete":
			en.TypeLabel = "Your follows import is complete"
			en.Icon = "bell"
		case "follow_import.failed":
			en.TypeLabel = "Your follows import stopped after an error"
			en.Icon = "bell"
		case "admin.account_deleted":
			en.TypeLabel = "A user deleted their account"
			en.Icon = "user-minus"
		case "admin.account_moved":
			en.TypeLabel = "Moved their account elsewhere"
			en.Icon = "repeat"
		default:
			en.TypeLabel = n.Type
			en.Icon = "bell"
		}

		if n.StatusID != nil && *n.StatusID != "" {
			if status := statuses[*n.StatusID]; status != nil {
				// Older reblog notifications point at the boost wrapper, which
				// carries no content of its own. Fall back to the boosted
				// original so the row still shows a readable preview and links
				// to the actual post rather than the empty wrapper.
				if status.BoostOfID != nil && status.BoostOf != nil {
					status = status.BoostOf
				}
				en.Status = status
				if n.Type == "emoji_reaction" {
					for _, reaction := range status.EmojiReactions {
						if reaction.Name == n.Emoji {
							en.ReactionEmoji = reaction.Emoji
							break
						}
					}
				}
			} else {
				continue
			}
		}

		enriched = append(enriched, en)
	}

	if account.GroupNotifications {
		grouped := make([]*enrichedNotification, 0, len(enriched))
		byKey := make(map[string]*enrichedNotification)
		for _, notification := range enriched {
			// Each follow request is answered individually, so it always keeps
			// its own row with its own accept/reject buttons.
			if notification.Type == "follow_request" {
				grouped = append(grouped, notification)
				continue
			}
			if group := byKey[notification.GroupKey]; group != nil {
				group.Count++
				group.AdditionalCount++
				group.Read = group.Read && notification.Read
				continue
			}
			byKey[notification.GroupKey] = notification
			grouped = append(grouped, notification)
		}
		enriched = grouped
	}

	return enriched, nil
}

// lastNotificationID returns the ID of the oldest notification in a batch (the
// slice is ordered newest-first), used as the max_id pagination cursor.
func lastNotificationID(ns []*models.Notification) string {
	if len(ns) == 0 {
		return ""
	}
	return ns[len(ns)-1].ID
}

func (h *Handler) markLoadedNotificationsRead(ctx context.Context, accountID string, notifications []*models.Notification) {
	var ids []string
	for _, n := range notifications {
		if !n.Read {
			ids = append(ids, n.ID)
		}
	}
	if err := h.store.MarkNotificationsRead(ctx, accountID, ids); err != nil {
		slog.Error("webui: mark notifications read", "error", err)
	}
}

// PartialNotifications returns a partial HTML fragment of notification rows for
// infinite scroll, appended to the list as the viewer scrolls (JavaScript on)
// - the no-JS path uses the "Load older notifications" link instead.
func (h *Handler) PartialNotifications(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	limit := notificationsPageSize
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 40 {
		limit = l
	}
	filter := r.URL.Query().Get("filter")

	notifications, err := h.store.ListNotifications(r.Context(), account.ID, queryMaxID(r), limit, notificationFilterExcludedTypes(filter, account))
	if err != nil {
		slog.Error("webui: partial notifications error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	lastID := lastNotificationID(notifications)
	hasMore := len(notifications) == limit
	enriched, err := h.enrichNotificationsChecked(r.Context(), account, notifications)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")

	if len(enriched) == 0 && !hasMore {
		w.Write([]byte(`<div class="empty-state">No more notifications</div>`))
		return
	}

	h.markLoadedNotificationsRead(r.Context(), account.ID, notifications)

	tmpl := h.localeTemplates(r)
	for _, en := range enriched {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "components/notification.html", en); err != nil {
			slog.Error("webui: render notification partial error", "error", err)
			continue
		}
		w.Write(buf.Bytes())
	}

	if hasMore {
		filterParam := ""
		if filter != "" {
			filterParam = "&filter=" + url.QueryEscape(filter)
		}
		nextURL := fmt.Sprintf("/partial/notifications?max_id=%s&limit=%d%s", lastID, limit, filterParam)
		w.Write([]byte(fmt.Sprintf(`<div hx-trigger="intersect once" hx-get="%s" hx-swap="afterend"><div class="scroll-trigger"></div></div>`, escapeHTMLAttribute(nextURL))))
	}
}

// PartialNotificationsNew returns notifications newer than since_id, newest
// first, for the streaming client to prepend at the top of an already-open
// notifications page - so new arrivals show up live instead of requiring a
// reload.
func (h *Handler) PartialNotificationsNew(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sinceID := r.URL.Query().Get("since_id")
	w.Header().Set("Content-Type", "text/html")
	if sinceID == "" {
		return
	}

	filter := r.URL.Query().Get("filter")
	notifications, err := h.store.ListNotificationsSince(r.Context(), account.ID, sinceID, notificationsPageSize, notificationFilterExcludedTypes(filter, account))
	if err != nil {
		slog.Error("webui: partial notifications new error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	if len(notifications) == 0 {
		return
	}

	enriched, err := h.enrichNotificationsChecked(r.Context(), account, notifications)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	h.markLoadedNotificationsRead(r.Context(), account.ID, notifications)
	tmpl := h.localeTemplates(r)
	for _, en := range enriched {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "components/notification.html", en); err != nil {
			slog.Error("webui: render notification partial error", "error", err)
			continue
		}
		w.Write(buf.Bytes())
	}
}

func (h *Handler) NotificationsRead(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	if err := h.store.MarkAllNotificationsRead(r.Context(), account.ID); err != nil {
		slog.Error("webui: mark all notifications read", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusFound)
}

// NotificationsClear deletes all notifications for the current user.
func (h *Handler) NotificationsClear(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	if err := h.store.DeleteAllNotifications(r.Context(), account.ID); err != nil {
		slog.Error("webui: clear notifications error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/notifications", http.StatusFound)
}

// Partial / htmx endpoints

// PartialTimeline returns a partial HTML fragment of statuses for infinite scroll.
func (h *Handler) PartialTimeline(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	account := getAuthAccount(r.Context())

	maxID := r.URL.Query().Get("max_id")
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 40 {
		limit = l
	}

	hideReplies := account != nil && account.HideReplies
	hideBoosts := account != nil && account.HideBoosts

	var maxIDPtr *string
	if maxID != "" {
		maxIDPtr = &maxID
	}

	// Validate the timeline kind and enforce auth before consulting the cache.
	switch kind {
	case "home", "bookmarks", "favourites", "direct":
		if account == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	case "public", "bubble":
		if account == nil {
			redirectGuest(w, r, "/auth/sign_in")
			return
		}
	case "local":
		if account == nil && !h.publicLocalTimelineEnabled() {
			redirectGuest(w, r, "/auth/sign_in")
			return
		}
	default:
		http.Error(w, "Unknown timeline", http.StatusBadRequest)
		return
	}

	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(account), "partial:"+kind, maxID, strconv.Itoa(limit), boolKey(hideReplies), boolKey(hideBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			var statuses []*models.Status
			var ferr error
			switch kind {
			case "home":
				statuses, ferr = h.store.GetHomeTimeline(ctx, account.ID, maxIDPtr, nil, nil, limit, hideReplies, hideBoosts)
			case "public":
				statuses, ferr = h.store.GetPublicTimeline(ctx, viewerIDOf(account), false, maxIDPtr, nil, nil, limit, hideReplies, hideBoosts)
			case "local":
				statuses, ferr = h.store.GetPublicTimeline(ctx, viewerIDOf(account), true, maxIDPtr, nil, nil, limit, hideReplies, hideBoosts)
			case "bubble":
				statuses, ferr = h.store.GetBubbleTimeline(ctx, viewerIDOf(account), maxIDPtr, nil, nil, limit, hideReplies, hideBoosts)
			case "bookmarks":
				statuses, ferr = h.store.ListBookmarks(ctx, account.ID, maxIDPtr, limit)
				statuses = h.app.VisibleStatuses(ctx, statuses, account)
			case "favourites":
				statuses, ferr = h.store.ListFavourites(ctx, account.ID, maxIDPtr, limit)
				statuses = h.app.VisibleStatuses(ctx, statuses, account)
			case "direct":
				statuses, ferr = h.store.GetDirectTimeline(ctx, account.ID, maxIDPtr, nil, nil, limit)
			}
			if ferr != nil {
				return app.PageResult{}, ferr
			}
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor := lastStatusID(statuses)
			scope := "home"
			if kind == "public" || kind == "local" || kind == "bubble" {
				scope = "public"
			}
			statuses, ferr = h.app.FilterStatuses(ctx, scope, statuses, account)
			return app.PageResult{Statuses: collapseTimelineBoosts(statuses), Cursor: cursor}, ferr
		})
	if err != nil {
		slog.Error("webui: partial timeline error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	statuses := res.Statuses
	lastID := res.Cursor

	if len(statuses) == 0 && lastID == "" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="empty-state">No more posts</div>`))
		return
	}

	nextURL := fmt.Sprintf("/partial/timeline/%s?max_id=%s&limit=%d", kind, lastID, limit)

	w.Header().Set("Content-Type", "text/html")

	csrf := getCSRFToken(r.Context())
	locale := h.resolveLocale(r, account)
	for _, s := range statuses {
		_, _ = io.WriteString(w, string(h.renderStatusCard(s, account, csrf, locale)))
	}
	_, _ = io.WriteString(w, fmt.Sprintf(`<div hx-trigger="intersect once, timeline-restore once" hx-get="%s" hx-swap="afterend"><div class="scroll-trigger"></div></div>`, escapeHTMLAttribute(nextURL)))
}

// PartialStatus returns a single status component (for htmx refresh).
func (h *Handler) PartialStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	account := getAuthAccount(r.Context())

	status, err := h.store.GetStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, account) {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}

	// Streaming fetches new cards through this endpoint. Apply timeline
	// preferences here as the authoritative check: a remote reply may only have
	// InReplyToURI, and a newly received boost may not yet have BoostOf hydrated
	// in the stream's Mastodon JSON payload.
	if r.URL.Query().Get("timeline") == "1" && account != nil {
		isReply := status.InReplyToID != nil || (status.InReplyToURI != nil && *status.InReplyToURI != "")
		if (account.HideReplies && isReply) || (account.HideBoosts && status.BoostOfID != nil) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	h.app.EnrichStatus(r.Context(), status, account)
	scope := r.URL.Query().Get("scope")
	switch scope {
	case "home", "notifications", "public", "thread", "account":
	default:
		scope = "thread"
	}
	filtered, err := h.app.FilterStatuses(r.Context(), scope, []*models.Status{status}, account)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	if len(filtered) == 0 {
		if r.URL.Query().Get("timeline") == "1" {
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Error(w, "Status not found", http.StatusNotFound)
		}
		return
	}
	status = filtered[0]

	data := map[string]interface{}{
		"Status":    status,
		"Account":   account,
		"CSRFToken": getCSRFToken(r.Context()),
	}

	w.Header().Set("Content-Type", "text/html")
	h.localeTemplates(r).ExecuteTemplate(w, "components/status.html", data)
}

// ReplyComposerPartial puts a reply composer next to the status it answers.
// The reply link itself remains a normal thread-page link without JavaScript.
func (h *Handler) ReplyComposerPartial(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Sign in required", http.StatusUnauthorized)
		return
	}
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || !h.app.CanViewStatus(r.Context(), status, account) {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	h.app.EnrichStatus(r.Context(), status, account)
	w.Header().Set("Content-Type", "text/html")
	_, _ = fmt.Fprintf(w, `<div class="inline-reply-composer" data-inline-reply-composer="composer-reply-%s"><a class="btn btn-ghost inline-reply-cancel" href="%s" hx-boost="false" data-inline-reply-cancel>%s</a>`, html.EscapeString(status.ID), html.EscapeString(statusPath(status)), html.EscapeString(h.pageTitle(r, "composer.cancel")))
	_ = h.localeTemplates(r).ExecuteTemplate(w, "components/composer.html", map[string]interface{}{
		"Account":          account,
		"CSRFToken":        getCSRFToken(r.Context()),
		"ComposerID":       "composer-reply-" + status.ID,
		"InReplyToID":      status.ID,
		"ReplyTo":          status,
		"ParentVisibility": status.Visibility,
	})
	_, _ = io.WriteString(w, "</div>")
}

// AutocompleteAccounts returns JSON with accounts matching the query prefix.
// GET /api/accounts/autocomplete?q=alice
func (h *Handler) AutocompleteAccounts(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(strings.TrimPrefix(r.URL.Query().Get("q"), "@"))
	if utf8.RuneCountInString(query) < 2 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	followingID := ""
	if r.URL.Query().Get("following") == "1" {
		account := getAuthAccount(r.Context())
		if account == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		followingID = account.ID
	}
	accounts, err := h.store.SearchAccountPrefixes(r.Context(), query, followingID, 0, 10)
	if err != nil {
		slog.Error("webui: autocomplete search error", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	type AcctResult struct {
		ID          string `json:"id"`
		Username    string `json:"username"`
		Acct        string `json:"acct"`
		DisplayName string `json:"display_name"`
		Avatar      string `json:"avatar"`
	}

	results := make([]AcctResult, 0, len(accounts))
	for _, a := range accounts {
		acct := a.Username
		if a.Domain != nil && *a.Domain != "" {
			acct = a.Username + "@" + *a.Domain
		}
		avatar := a.Avatar
		if avatar == "" {
			avatar = "/avatars/original/missing.png"
		}
		results = append(results, AcctResult{
			ID:          a.ID,
			Username:    a.Username,
			Acct:        acct,
			DisplayName: a.DisplayName,
			Avatar:      avatar,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (h *Handler) returnStatusComponent(w http.ResponseWriter, r *http.Request, status *models.Status, account *models.Account) {
	h.app.EnrichStatus(r.Context(), status, account)

	// For htmx requests, return just the status component (swapped in-place)
	// For regular POST (JS-off fallback), redirect back to the referring page
	if r.Header.Get("HX-Request") != "true" {
		referer := safeReturnPath(r, r.Referer(), "/")
		http.Redirect(w, r, referer, http.StatusFound)
		return
	}
	filtered, err := h.app.FilterStatuses(r.Context(), filterScopeForPath(r.Referer()), []*models.Status{status}, account)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	if len(filtered) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status = filtered[0]

	data := map[string]interface{}{
		"Status":    status,
		"Account":   account,
		"CSRFToken": getCSRFToken(r.Context()),
	}

	w.Header().Set("Content-Type", "text/html")
	h.localeTemplates(r).ExecuteTemplate(w, "components/status.html", data)
}

// StatusPreview renders the Open Graph card for the first ordinary link in a
// status. Fetching happens in a separate HTMX request so a slow website cannot
// hold up the timeline itself.
func (h *Handler) StatusPreview(w http.ResponseWriter, r *http.Request) {
	// Preview markup can contain media-proxy URLs signed with an ephemeral
	// per-process secret. Do not cache that markup: after a restart, a cached
	// card would keep the old signature and its image would fail with 403.
	w.Header().Set("Cache-Control", "private, no-store")
	account := getAuthAccount(r.Context())
	if account != nil && account.DataSaver {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.app.CanViewStatus(r.Context(), status, account) {
		http.NotFound(w, r)
		return
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		if original, originalErr := h.store.GetStatus(r.Context(), *status.BoostOfID); originalErr == nil {
			if !h.app.CanViewStatus(r.Context(), original, account) {
				http.NotFound(w, r)
				return
			}
			status = original
		}
	}
	if account != nil {
		h.app.EnrichStatus(r.Context(), status, account)
		filtered, err := h.app.FilterStatuses(r.Context(), filterScopeForPath(r.Referer()), []*models.Status{status}, account)
		if err != nil {
			http.Error(w, "Error", http.StatusInternalServerError)
			return
		}
		if len(filtered) == 0 || len(filtered[0].FilterMatches) > 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	previewURL := opengraph.URLFromHTML(status.ContentHTML)
	if previewURL == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	card, err := h.previewFetcher.PreviewForStatus(r.Context(), status.ID, previewURL)
	if err != nil || card == nil {
		// A missing preview is expected for non-HTML links and sites that block
		// crawlers. The post's normal hyperlink remains available, so this only
		// matters when someone is chasing a card that should have shown up.
		slog.Debug("webui: no preview card", "status", status.ID, "url", previewURL, "error", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.localeTemplates(r).ExecuteTemplate(w, "components/preview-card.html", card); err != nil {
		slog.Error("webui: preview card render error", "error", err)
		http.Error(w, "Render error", http.StatusInternalServerError)
	}
}

func filterScopeForPath(raw string) string {
	path := raw
	if parsed, err := url.Parse(raw); err == nil {
		path = parsed.Path
	}
	switch {
	case strings.HasPrefix(path, "/public"), strings.HasPrefix(path, "/tags/"), strings.HasPrefix(path, "/search"):
		return "public"
	case strings.HasPrefix(path, "/notifications"):
		return "notifications"
	case strings.HasPrefix(path, "/statuses/"):
		return "thread"
	case strings.HasPrefix(path, "/@"):
		return "account"
	default:
		return "home"
	}
}
