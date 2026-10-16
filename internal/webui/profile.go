package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Profile Page

// ProfilePage renders a user's profile page.
func (h *Handler) ProfilePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Accept")
	username := chi.URLParam(r, "username")
	viewer := getAuthAccount(r.Context())

	// ActivityPub clients dereferencing the web profile permalink get the
	// canonical actor instead of HTML. Local actors live at /users/{username}
	// (served through the federation response cache; ActorGet 404s unknown
	// names); remote handles redirect to their origin actor when cached and
	// are never resolved over the network for an AP fetch.
	if acceptsActivityPub(r) {
		if i := strings.IndexByte(username, '@'); i >= 0 {
			remoteDomain := username[i+1:]
			account, err := h.store.GetAccountByUsernameDomain(r.Context(), username[:i], &remoteDomain)
			if err == nil && account.ActorURI != "" {
				http.Redirect(w, r, safeHTTPRedirectTarget(account.ActorURI, "/"), http.StatusFound)
				return
			}
			http.Error(w, "Account not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/users/"+username, http.StatusFound)
		return
	}

	var account *models.Account
	var err error

	// Handle remote accounts: user@domain format (e.g. /@admin@example.org)
	if strings.Contains(username, "@") {
		parts := strings.SplitN(username, "@", 2)
		remoteUser := parts[0]
		remoteDomain := parts[1]

		// Try cached first
		account, err = h.store.GetAccountByUsernameDomain(r.Context(), remoteUser, &remoteDomain)

		// Unauthenticated visitors shouldn't browse other instances' users'
		// timelines from here - bounce them to the user's home instance. We
		// resolve the canonical actor URI (via WebFinger if not cached) so the
		// redirect lands on the real profile, but we never render the remote
		// timeline locally or fetch its outbox for anonymous requests.
		if viewer == nil {
			http.Redirect(w, r, safeHTTPRedirectTarget(h.remoteProfileURL(r, remoteUser, remoteDomain, account, err), "/"), http.StatusFound)
			return
		}

		if err != nil {
			// Resolve via WebFinger
			if h.federator != nil {
				resolved, resolveErr := h.federator.ResolveAccount(r.Context(), remoteUser+"@"+remoteDomain)
				if resolveErr != nil {
					slog.Warn("webui: remote account not found", "username", username, "error", resolveErr)
					http.Error(w, "Account not found", http.StatusNotFound)
					return
				}
				account = resolved
			} else {
				slog.Warn("webui: remote account not found", "username", username, "error", err)
				http.Error(w, "Account not found", http.StatusNotFound)
				return
			}
		}
	} else {
		account, err = h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
		if err != nil {
			slog.Warn("webui: profile lookup failed", "username", username, "error", err)
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Account not found", http.StatusNotFound)
			} else {
				http.Error(w, "Error fetching account", http.StatusInternalServerError)
			}
			return
		}
	}

	data := h.buildProfileData(r, account, viewer, 0)
	if data == nil {
		h.renderError(w, r, "Error fetching profile")
		return
	}
	data.Title = account.DisplayName + " (" + account.Username + ")"
	h.renderPage(w, r, account.DisplayName+" (@"+account.Username+")", data, "profile.html")

	if account.Domain != nil {
		content := queryMaxID(r) == nil && r.URL.Query().Get("media") != "1" && r.URL.Query().Get("blog") != "1"
		h.scheduleProfileRefresh(r.Context(), account, content && len(data.Statuses) < 3, content)
	}
}

const profileRefreshDedupTTL = 10 * time.Minute

func (h *Handler) scheduleProfileRefresh(ctx context.Context, account *models.Account, fetchOutbox, syncFeatured bool) {
	key := account.ID + ":actor"
	if fetchOutbox || syncFeatured {
		key = fmt.Sprintf("%s:%t:%t", account.ID, fetchOutbox, syncFeatured)
	}

	now := time.Now()
	expires := now.Add(profileRefreshDedupTTL)
	h.profileRefreshMu.Lock()
	if h.profileRefresh == nil {
		h.profileRefresh = make(map[string]time.Time)
	}
	if until, ok := h.profileRefresh[key]; ok && now.Before(until) {
		h.profileRefreshMu.Unlock()
		return
	}
	for k, until := range h.profileRefresh {
		if !now.Before(until) {
			delete(h.profileRefresh, k)
		}
	}
	h.profileRefresh[key] = expires
	h.profileRefreshMu.Unlock()

	go func() {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()

		if fetchOutbox || (syncFeatured && h.inboxProcessor != nil) {
			maxItems := 0
			if fetchOutbox {
				maxItems = 20
			}
			payload, _ := json.Marshal(outboxFetchPayload{
				AccountID: account.ID, MaxItems: maxItems,
				SyncFeatured: syncFeatured && h.inboxProcessor != nil,
			})
			job := &models.Job{Kind: OutboxFetchJobKind, Payload: string(payload), RunAt: time.Now(), MaxAttempts: 1, State: "queued"}
			if err := h.store.EnqueueJob(refreshCtx, job); err != nil {
				slog.Warn("webui: failed to queue profile refresh", "account", account.Username, "error", err)
				h.profileRefreshMu.Lock()
				if h.profileRefresh[key] == expires {
					delete(h.profileRefresh, key)
				}
				h.profileRefreshMu.Unlock()
			}
		}
		if h.federator != nil {
			h.federator.RevalidateActorAsync(refreshCtx, account)
		}
	}()
}

// buildProfileData loads a profile's stats, pinned/timeline statuses, and
// relationship info relative to viewer. Shared by ProfilePage (full page,
// tries=0) and ProfileLivePartial (the self-polling fragment that refreshes
// a remote profile while its background outbox/actor-refresh jobs are still
// filling it in).
func (h *Handler) buildProfileData(r *http.Request, account *models.Account, viewer *models.Account, tries int) *ProfileData {
	maxID := queryMaxID(r)
	onlyMedia := r.URL.Query().Get("media") == "1"
	showReplies, showBoosts := profilePostFilters(r)
	// The blog tab only exists for a local account allowed to publish one on a
	// blog-capable server; anywhere else the query parameter is ignored.
	onlyBlog := r.URL.Query().Get("blog") == "1" && h.profileBlogEnabled(account)
	if onlyBlog {
		onlyMedia = false
	}

	// Load pinned statuses first (shown at top of profile, first page only so
	// paginated pages don't repeat them). The two enriched lists are page-cached
	// per viewer+account; relationship info and counts below stay live so they
	// are always correct.
	var pinnedStatuses []*models.Status
	if maxID == nil && !onlyMedia && !onlyBlog {
		pinnedRes, filterErr := h.app.PageCached(
			r.Context(),
			h.app.PageKey(viewerIDOf(viewer), "profile-pinned:"+account.ID, boolKey(showReplies), boolKey(showBoosts)),
			func(ctx context.Context) (app.PageResult, error) {
				pinned, err := h.store.GetPinnedStatuses(ctx, account.ID, 20)
				if err != nil {
					pinned = []*models.Status{}
				}
				pinned = h.app.VisibleStatuses(ctx, pinned, viewer)
				pinned = filterProfileStatuses(pinned, showReplies, showBoosts)
				h.app.EnrichStatuses(ctx, pinned, viewer)
				pinned, err = h.app.FilterStatuses(ctx, "account", pinned, viewer)
				return app.PageResult{Statuses: pinned}, err
			})
		if filterErr != nil {
			slog.Error("webui: profile filter error", "error", filterErr)
			return nil
		}
		pinnedStatuses = pinnedRes.Statuses
	}

	statusesRes, filterErr := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(viewer), "profile-statuses:"+account.ID, cursorKey(maxID), boolKey(onlyMedia), boolKey(onlyBlog), boolKey(showReplies), boolKey(showBoosts)),
		func(ctx context.Context) (app.PageResult, error) {
			if onlyBlog {
				st, err := h.store.GetBlogStatuses(ctx, account.ID, maxID, 20)
				if err != nil {
					slog.Error("webui: profile blog statuses error", "error", err)
					st = []*models.Status{}
				}
				h.app.EnrichStatuses(ctx, st, viewer)
				cursor := lastStatusID(st)
				st, err = h.app.FilterStatuses(ctx, "account", st, viewer)
				return app.PageResult{Statuses: st, Cursor: cursor}, err
			}
			st, err := h.store.GetAccountStatuses(ctx, viewerIDOf(viewer), account.ID, maxID, nil, nil, 20, !showReplies, !showBoosts, onlyMedia, false)
			if err != nil {
				slog.Error("webui: profile statuses error", "error", err)
				st = []*models.Status{}
			}
			st = h.app.VisibleStatuses(ctx, st, viewer)
			h.app.EnrichStatuses(ctx, st, viewer)
			cursor := lastStatusID(st)
			st, err = h.app.FilterStatuses(ctx, "account", st, viewer)
			return app.PageResult{Statuses: st, Cursor: cursor}, err
		})
	if filterErr != nil {
		slog.Error("webui: profile filter error", "error", filterErr)
		return nil
	}
	statuses := statusesRes.Statuses

	// Get relationship info
	var isFollowing, isFollowedBy bool
	var followRequested, requestsFollow, isBlocking, isBlockedBy, isMuting, isNotifying bool
	var lists []*models.List
	if viewer != nil && viewer.ID != account.ID {
		isFollowing, _ = h.store.IsFollowing(r.Context(), viewer.ID, account.ID)
		if isFollowing {
			lists, _ = h.store.ListLists(r.Context(), viewer.ID)
		}
		// The inbound follow row answers both "do they follow me" and "are they
		// still waiting for my approval", so it replaces the IsFollowing call.
		if inbound, err := h.store.GetFollowByPair(r.Context(), account.ID, viewer.ID); err == nil {
			isFollowedBy = inbound.State == models.FollowAccepted
			requestsFollow = inbound.State == models.FollowPending
		}
		// Check follow state and the notify "bell" preference.
		follow, err := h.store.GetFollowByPair(r.Context(), viewer.ID, account.ID)
		if err == nil {
			if follow.State == "pending" {
				followRequested = true
			}
			isNotifying = follow.Notify
		}
		isBlocking, _ = h.store.BlockExists(r.Context(), viewer.ID, account.ID)
		isBlockedBy, _ = h.store.BlockExists(r.Context(), account.ID, viewer.ID)
		isMuting, _ = h.store.IsMuted(r.Context(), viewer.ID, account.ID)
	}

	// Both remote and local accounts serve the denormalized counts: remote
	// ones are published by the origin server, local ones are maintained
	// incrementally by the status/follow write paths and repaired daily by
	// housekeeping (RepairLocalAccountCounts), so live COUNT(*) queries per
	// profile render bought nothing.
	followersCount := account.FollowersCount
	followingCount := account.FollowingCount
	statusesCount := account.StatusesCount
	relationship := map[string]interface{}{
		"following":       isFollowing,
		"followedBy":      isFollowedBy,
		"followRequested": followRequested,
		"requestsFollow":  requestsFollow,
		"blocking":        isBlocking,
		"blockedBy":       isBlockedBy,
		"muting":          isMuting,
		"notifying":       isNotifying,
	}

	// Check if this profile's owner wants to hide follower/following info.
	// Only hide from other viewers, not from the profile owner themselves.
	hideFollowers := app.FollowListHidden(account, viewer, true)
	hideFollowing := app.FollowListHidden(account, viewer, false)

	// Zero out counts if hidden from this viewer
	displayFollowersCount := followersCount
	displayFollowingCount := followingCount
	if hideFollowers {
		displayFollowersCount = -1 // sentinel: don't show
	}
	if hideFollowing {
		displayFollowingCount = -1
	}

	// Custom emojis used in the profile's display name / bio.
	h.app.LoadAccountEmojis(r.Context(), account)

	// Offer the next page only when this one came back full; a short page
	// means the timeline is exhausted.
	var timelineCursor string
	if len(statuses) == 20 {
		timelineCursor = statusesRes.Cursor
	}

	// Two independent things can still be "landing": posts (outbox_fetch)
	// and follower/following/status counts (actor_refresh). Neither implies
	// the other, so this ORs them instead of requiring both:
	//   - a brand-new account resolves its counts synchronously
	//     (ResolveAccount -> FetchActor) but its posts are still an async
	//     outbox_fetch away - posts empty, counts already fine.
	//   - an account already known locally (e.g. from an earlier boost) can
	//     already have a post or two cached, but was only ever light-fetched
	//     (no counts) until this view's RevalidateActorAsync queued a real
	//     refresh - posts non-empty, counts still all zero.
	// Requiring both empty (the previous version of this check) missed the
	// second case entirely: with posts already present, Fetching never
	// went true, so the fragment never polled and the stats row stayed
	// stuck at zero until a manual reload.
	// The tries cap is a naive heuristic (not tied to actual job
	// completion), raise profileLivePollMaxTries or wire a real completion
	// signal if jobs still routinely take longer than this to land.
	postsEmpty := len(pinnedStatuses) == 0 && len(statuses) == 0
	countsEmpty := statusesCount == 0 && followersCount == 0 && followingCount == 0
	// The media tab is legitimately empty for text-only accounts, so it never
	// counts as "still landing".
	fetching := maxID == nil && !onlyMedia && account.Domain != nil && tries < profileLivePollMaxTries &&
		(tries == 0 || postsEmpty || countsEmpty)

	handle := account.Username
	if account.Domain != nil && *account.Domain != "" {
		handle += "@" + *account.Domain
	}
	movedToURL, movedToLabel := h.movedProfile(r.Context(), account, viewer)

	return &ProfileData{
		Account:        viewer,
		Profile:        account,
		MovedToURL:     movedToURL,
		MovedToLabel:   movedToLabel,
		Lists:          lists,
		ListAdded:      r.URL.Query().Get("list_added") == "1",
		Handle:         handle,
		PinnedStatuses: pinnedStatuses,
		Statuses:       statuses,
		StatusesCount:  statusesCount,
		FollowersCount: displayFollowersCount,
		FollowingCount: displayFollowingCount,
		HideFollowers:  hideFollowers,
		HideFollowing:  hideFollowing,
		Relationship:   relationship,
		TimelineCursor: timelineCursor,
		CSRFToken:      getCSRFToken(r.Context()),
		Fetching:       fetching,
		PollTries:      tries + 1,
		OnlyMedia:      onlyMedia,
		ShowReplies:    showReplies,
		ShowBoosts:     showBoosts,
		OnlyBlog:       onlyBlog,
		HasBlog:        h.profileBlogEnabled(account),
		BlogURL:        h.profileBlogURL(account),
		RSSFeedURL:     profileRSSFeedURL(account),
	}
}

func (h *Handler) movedProfile(ctx context.Context, account, viewer *models.Account) (string, string) {
	destination := string(safeTemplateURL(account.MovedToURI))
	if destination == "" {
		return "", ""
	}
	target, err := h.store.GetAccountByURI(ctx, destination)
	if err != nil {
		return destination, destination
	}
	handle := "@" + target.Username
	if target.Domain != nil && *target.Domain != "" {
		handle += "@" + *target.Domain
	}
	return profilePath(target, viewer), handle
}

// profilePostFilters defaults to showing both kinds of post. filters=1 marks
// a submitted filter form, where an absent checkbox means it was switched off.
func profilePostFilters(r *http.Request) (showReplies, showBoosts bool) {
	q := r.URL.Query()
	if q.Get("filters") == "1" {
		return q.Get("replies") == "1", q.Get("boosts") == "1"
	}
	return true, true
}

func filterProfileStatuses(statuses []*models.Status, showReplies, showBoosts bool) []*models.Status {
	filtered := statuses[:0]
	for _, status := range statuses {
		if (!showReplies && (status.InReplyToID != nil || status.InReplyToURI != nil && *status.InReplyToURI != "")) || (!showBoosts && status.BoostOfID != nil) {
			continue
		}
		filtered = append(filtered, status)
	}
	return filtered
}

func profileRSSFeedURL(account *models.Account) string {
	if account != nil && account.Domain == nil && account.RSSFeedEnabled {
		return "/@" + account.Username + ".rss"
	}
	return ""
}

// profileBlogEnabled reports whether this profile has a static blog worth a
// tab: a local account allowed to blog on a blog-capable server.
func (h *Handler) profileBlogEnabled(account *models.Account) bool {
	return account != nil && account.Domain == nil && h.blogAllowed(account)
}

// profileBlogURL returns the real static site only after it has been generated.
func (h *Handler) profileBlogURL(account *models.Account) string {
	if h.profileBlogEnabled(account) && h.blogSiteGenerated(account) {
		return "/@" + account.Username + "/blog"
	}
	return ""
}

// blogSiteGenerated reports whether the static blog site has actually been
// built for this account yet (see internal/blog.Generate); until then its
// URL would just 404.
func (h *Handler) blogSiteGenerated(account *models.Account) bool {
	if accounts.ValidateUsername(account.Username) != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(h.cfg.Blog.ResolvedOutputDir(), account.Username, "index.html"))
	return err == nil
}

// profileLivePollMaxTries bounds how many times the profile-live fragment
// re-polls an empty remote profile (at ~2s apart) before giving up and
// leaving a manual reload as the fallback.
const profileLivePollMaxTries = 10

// ProfileLivePartial re-renders the profile-live fragment (stats + pinned +
// timeline) for a remote profile that was still empty on first render. GET
// /partial/profile/{username}/live
func (h *Handler) ProfileLivePartial(w http.ResponseWriter, r *http.Request) {
	viewer := getAuthAccount(r.Context())
	username := chi.URLParam(r, "username")

	var domain *string
	if i := strings.IndexByte(username, '@'); i >= 0 {
		d := username[i+1:]
		domain = &d
		username = username[:i]
		// The full profile page bounces anonymous visitors to the remote
		// instance; don't serve remote timelines to them here either.
		if viewer == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
	}
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, domain)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Error fetching account", http.StatusInternalServerError)
			return
		}
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	tries, _ := strconv.Atoi(r.URL.Query().Get("tries"))
	data := h.buildProfileData(r, account, viewer, tries)
	if data == nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if err := h.localeTemplates(r).ExecuteTemplate(w, "components/profile-poll.html", data); err != nil {
		slog.Error("webui: render profile live partial error", "error", err)
	}
	if domain != nil {
		h.scheduleProfileRefresh(r.Context(), account, len(data.Statuses) < 3, true)
	}
}

// PartialProfileTimeline returns an HTML fragment of a profile's older
// statuses, used for infinite scroll on the profile page. The username param
// may be a remote user@domain handle; only cached accounts are served - the
// partial never resolves or fetches over the network.
func (h *Handler) PartialProfileTimeline(w http.ResponseWriter, r *http.Request) {
	viewer := getAuthAccount(r.Context())
	username := chi.URLParam(r, "username")

	var domain *string
	if i := strings.IndexByte(username, '@'); i >= 0 {
		d := username[i+1:]
		domain = &d
		username = username[:i]
		// The full profile page bounces anonymous visitors to the remote
		// instance; don't serve remote timelines to them here either.
		if viewer == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
	}
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, domain)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	maxID := queryMaxID(r)
	limit := 10
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 40 {
		limit = l
	}

	onlyMedia := r.URL.Query().Get("media") == "1"
	showReplies, showBoosts := profilePostFilters(r)
	onlyBlog := r.URL.Query().Get("blog") == "1" && h.profileBlogEnabled(account)
	if onlyBlog {
		onlyMedia = false
	}

	var statuses []*models.Status
	if onlyBlog {
		statuses, err = h.store.GetBlogStatuses(r.Context(), account.ID, maxID, limit)
	} else {
		statuses, err = h.store.GetAccountStatuses(r.Context(), viewerIDOf(viewer), account.ID, maxID, nil, nil, limit, !showReplies, !showBoosts, onlyMedia, false)
	}
	if err != nil {
		slog.Error("webui: partial profile timeline error", "account", account.ID, "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	statuses = h.app.VisibleStatuses(r.Context(), statuses, viewer)

	h.app.EnrichStatuses(r.Context(), statuses, viewer)
	cursor := lastStatusID(statuses)
	statuses, err = h.app.FilterStatuses(r.Context(), "account", statuses, viewer)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if len(statuses) == 0 && cursor == "" {
		w.Write([]byte(`<div class="empty-state">No more posts</div>`))
		return
	}

	handle := account.Username
	if account.Domain != nil {
		handle += "@" + *account.Domain
	}
	nextURL := fmt.Sprintf("/partial/profile/%s?max_id=%s&limit=%d", url.PathEscape(handle), cursor, limit)
	if onlyMedia {
		nextURL += "&media=1"
	}
	if onlyBlog {
		nextURL += "&blog=1"
	}
	if !onlyMedia && !onlyBlog {
		nextURL += "&filters=1"
		if showReplies {
			nextURL += "&replies=1"
		}
		if showBoosts {
			nextURL += "&boosts=1"
		}
	}

	csrf := getCSRFToken(r.Context())
	locale := h.resolveLocale(r, viewer)
	for _, s := range statuses {
		_, _ = io.WriteString(w, string(h.renderStatusCard(s, viewer, csrf, locale)))
	}
	_, _ = io.WriteString(w, fmt.Sprintf(`<div hx-trigger="intersect once" hx-get="%s" hx-swap="afterend"><div class="scroll-trigger"></div></div>`, escapeHTMLAttribute(nextURL)))
}

// remoteProfileURL returns the best link to a remote user's profile on their
// home instance. It prefers the canonical actor URI from a cached record;
// otherwise it resolves the handle via WebFinger to obtain the real actor URI,
// and only falls back to the conventional https://domain/@user permalink if
// resolution fails.
func (h *Handler) remoteProfileURL(r *http.Request, user, domain string, cached *models.Account, lookupErr error) string {
	if lookupErr == nil && cached != nil && cached.ActorURI != "" {
		return cached.ActorURI
	}
	if h.federator != nil {
		if resolved, err := h.federator.ResolveAccount(r.Context(), user+"@"+domain); err == nil && resolved != nil && resolved.ActorURI != "" {
			return resolved.ActorURI
		} else if err != nil {
			slog.Warn("webui: failed to resolve remote profile for redirect", "handle", user+"@"+domain, "error", err)
		}
	}
	return "https://" + domain + "/@" + user
}

// resolveProfileByHandle looks up an account by a "username" or
// "username@domain" route param, as used by the followers/following pages.
// Remote accounts are only resolved from the local cache (no WebFinger) since
// these pages show follow lists we already store, not a freshly fetched actor.
func (h *Handler) resolveProfileByHandle(ctx context.Context, username string) (*models.Account, error) {
	if i := strings.IndexByte(username, '@'); i >= 0 {
		domain := username[i+1:]
		return h.store.GetAccountByUsernameDomain(ctx, username[:i], &domain)
	}
	return h.store.GetAccountByUsernameDomain(ctx, username, nil)
}

const followListPageSize = 40

// followListCursor resolves the ?max_id account-ID cursor of a follow-list
// page to the follows-row cursor ListFollowers/ListFollowing paginate on. The
// URL carries the last account shown because the list templates only see
// accounts; the pair lookup recovers the follow row. If the follow disappeared
// meanwhile the lookup fails and we restart from the first page.
func (h *Handler) followListCursor(ctx context.Context, r *http.Request, profileID string, followers bool) (*string, error) {
	maxID := queryMaxID(r)
	if maxID == nil {
		return nil, nil
	}
	var follow *models.Follow
	var err error
	if followers {
		follow, err = h.store.GetFollowByPair(ctx, *maxID, profileID)
	} else {
		follow, err = h.store.GetFollowByPair(ctx, profileID, *maxID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &follow.ID, nil
}

// followListNextCursor returns the ?max_id value for the next page link: the
// last account's ID when the page came back full, "" when exhausted.
func followListNextCursor(accounts []*models.Account) string {
	if len(accounts) < followListPageSize {
		return ""
	}
	return accounts[len(accounts)-1].ID
}

func (h *Handler) followListStates(ctx context.Context, viewer *models.Account, accounts []*models.Account) map[string]string {
	states := make(map[string]string, len(accounts))
	if viewer == nil {
		return states
	}
	// Pages cap this at 40 queries; batch only if list latency becomes measurable.
	for _, account := range accounts {
		if follow, err := h.store.GetFollowByPair(ctx, viewer.ID, account.ID); err == nil {
			states[account.ID] = string(follow.State)
		}
	}
	return states
}

// FollowersPage renders a list of accounts that follow the given profile.
func (h *Handler) FollowersPage(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	viewer := getAuthAccount(r.Context())

	account, err := h.resolveProfileByHandle(r.Context(), username)
	if err != nil {
		slog.Warn("webui: followers page account not found", "username", username, "error", err)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Account not found", http.StatusNotFound)
		} else {
			http.Error(w, "Error fetching account", http.StatusInternalServerError)
		}
		return
	}

	// If the profile owner hides their followers, block access for non-owners
	if app.FollowListHidden(account, viewer, true) {
		http.Error(w, "This account hides their followers", http.StatusForbidden)
		return
	}

	// ListFollowers orders by created_at but cursors on follows.id - valid
	// because follow IDs are time-sortable (same assumption as the API path).
	cursor, err := h.followListCursor(r.Context(), r, account.ID, true)
	if err != nil {
		http.Error(w, "Error fetching list cursor", http.StatusInternalServerError)
		return
	}
	followers, err := h.store.ListFollowers(r.Context(), account.ID, cursor, followListPageSize)
	if err != nil {
		slog.Error("webui: followers list error", "error", err)
		http.Error(w, "Error fetching account list", http.StatusInternalServerError)
		return
	}

	count, err := h.store.CountFollowers(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Error counting account list", http.StatusInternalServerError)
		return
	}

	h.app.LoadAccountEmojis(r.Context(), account)
	h.app.LoadAccountsEmojis(r.Context(), followers)

	data := &FollowListData{
		Title:       "Followers of " + account.Username,
		Account:     viewer,
		Profile:     account,
		Accounts:    followers,
		FollowState: h.followListStates(r.Context(), viewer, followers),
		Count:       count,
		ListKind:    "followers",
		NextCursor:  followListNextCursor(followers),
		CSRFToken:   getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "follow_list.followers_of")+" @"+account.Username, data, "follow_list.html")
}

// FollowingPage renders a list of accounts that the given profile follows.
func (h *Handler) FollowingPage(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	viewer := getAuthAccount(r.Context())

	account, err := h.resolveProfileByHandle(r.Context(), username)
	if err != nil {
		slog.Warn("webui: following page account not found", "username", username, "error", err)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Account not found", http.StatusNotFound)
		} else {
			http.Error(w, "Error fetching account", http.StatusInternalServerError)
		}
		return
	}

	// If the profile owner hides their following, block access for non-owners
	if app.FollowListHidden(account, viewer, false) {
		http.Error(w, "This account hides their following", http.StatusForbidden)
		return
	}

	// See FollowersPage for the cursor scheme.
	cursor, err := h.followListCursor(r.Context(), r, account.ID, false)
	if err != nil {
		http.Error(w, "Error fetching list cursor", http.StatusInternalServerError)
		return
	}
	following, err := h.store.ListFollowing(r.Context(), account.ID, cursor, followListPageSize)
	if err != nil {
		slog.Error("webui: following list error", "error", err)
		http.Error(w, "Error fetching account list", http.StatusInternalServerError)
		return
	}

	count, err := h.store.CountFollowing(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Error counting account list", http.StatusInternalServerError)
		return
	}

	h.app.LoadAccountEmojis(r.Context(), account)
	h.app.LoadAccountsEmojis(r.Context(), following)

	data := &FollowListData{
		Title:       "Following of " + account.Username,
		Account:     viewer,
		Profile:     account,
		Accounts:    following,
		FollowState: h.followListStates(r.Context(), viewer, following),
		Count:       count,
		ListKind:    "following",
		NextCursor:  followListNextCursor(following),
		CSRFToken:   getCSRFToken(r.Context()),
	}

	h.renderPage(w, r, h.pageTitle(r, "page.following_of")+" @"+account.Username, data, "follow_list.html")
}

// ResolveAccount resolves a remote account by handle (e.g. @user@domain) and
// redirects to its profile page. Falls back to local search.
func (h *Handler) ResolveAccount(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "Missing query", http.StatusBadRequest)
		return
	}

	// Strip leading @ if present
	q = strings.TrimPrefix(q, "@")

	// Handle full URLs (e.g. https://mastodon.social/@Gargron or
	// https://mastodon.social/users/Gargron). Resolve via the federator
	// which will fetch the actor document and extract the canonical URI.
	if strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://") {
		if h.federator != nil {
			account, err := h.federator.ResolveAccount(r.Context(), q)
			if err == nil && account != nil {
				if account.Domain != nil {
					http.Redirect(w, r, "/@"+account.Username+"@"+*account.Domain, http.StatusFound)
				} else {
					http.Redirect(w, r, "/@"+account.Username, http.StatusFound)
				}
				return
			}
			slog.Warn("webui: failed to resolve URL", "url", q, "error", err)
		}
	}

	// If it's a remote handle (contains @), resolve via WebFinger
	if strings.Contains(q, "@") {
		parts := strings.SplitN(q, "@", 2)
		remoteUser := parts[0]
		remoteDomain := parts[1]

		// Try cached first
		account, err := h.store.GetAccountByUsernameDomain(r.Context(), remoteUser, &remoteDomain)
		if err != nil && h.federator != nil {
			resolved, resolveErr := h.federator.ResolveAccount(r.Context(), remoteUser+"@"+remoteDomain)
			if resolveErr == nil {
				account = resolved
			} else {
				slog.Warn("webui: failed to resolve remote account", "handle", remoteUser+"@"+remoteDomain, "error", resolveErr)
			}
		}
		if account != nil {
			http.Redirect(w, r, "/@"+remoteUser+"@"+remoteDomain, http.StatusFound)
			return
		}
	}

	// Try local account
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), q, nil)
	if err == nil && account != nil {
		http.Redirect(w, r, "/@"+account.Username, http.StatusFound)
		return
	}

	http.Error(w, "Account not found", http.StatusNotFound)
}

// Follow Actions

func (h *Handler) FollowAccount(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	targetID := chi.URLParam(r, "id")
	if _, err := h.app.Follow(r.Context(), account, targetID); err != nil {
		slog.Error("webui: follow error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.redirectBack(w, r)
}

func (h *Handler) UnfollowAccount(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	targetID := chi.URLParam(r, "id")

	if err := h.app.Unfollow(r.Context(), account, targetID); err != nil {
		slog.Error("webui: unfollow error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.redirectBack(w, r)
}

// FollowRequestsData lists the accounts waiting for the viewer's approval.
type FollowRequestsData struct {
	Title     string
	Account   *models.Account
	Accounts  []*models.Account
	CSRFToken string
}

// followRequestsPageSize bounds the list; the oldest requests beyond it show up
// as the newer ones get answered.
const followRequestsPageSize = 40

// FollowRequestsPage lists every pending follow request in one place, for
// people who answer them in a batch rather than one notification at a time.
func (h *Handler) FollowRequestsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	requests, err := h.store.ListFollowRequests(r.Context(), account.ID, followRequestsPageSize)
	if err != nil {
		slog.Error("webui: list follow requests error", "error", err)
		h.renderError(w, r, "Error fetching follow requests")
		return
	}

	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		ids = append(ids, request.AccountID)
	}
	byID, _ := h.store.GetAccountsByIDs(r.Context(), ids)
	// Keep the newest-first order of the follow rows; the map loses it.
	accounts := make([]*models.Account, 0, len(ids))
	for _, id := range ids {
		if requester := byID[id]; requester != nil {
			accounts = append(accounts, requester)
		}
	}
	h.app.LoadAccountsEmojis(r.Context(), accounts)

	data := &FollowRequestsData{
		Title:     "Follow requests",
		Account:   account,
		Accounts:  accounts,
		CSRFToken: getCSRFToken(r.Context()),
	}
	h.renderPage(w, r, h.pageTitle(r, "follow_requests.heading"), data, "follow_requests.html")
}

// AuthorizeFollowRequest approves a pending follow request; {id} is the
// requesting account's ID, as shown on the notification row.
func (h *Handler) AuthorizeFollowRequest(w http.ResponseWriter, r *http.Request) {
	h.answerFollowRequest(w, r, true)
}

// RejectFollowRequest denies a pending follow request.
func (h *Handler) RejectFollowRequest(w http.ResponseWriter, r *http.Request) {
	h.answerFollowRequest(w, r, false)
}

func (h *Handler) answerFollowRequest(w http.ResponseWriter, r *http.Request, accept bool) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := h.app.AnswerFollowRequest(r.Context(), account, chi.URLParam(r, "id"), accept); err != nil {
		slog.Warn("webui: answer follow request", "accept", accept, "error", err)
		http.Error(w, "Follow request not found", http.StatusNotFound)
		return
	}

	h.redirectBack(w, r)
}

// NotifyAccount toggles the notify "bell" on a follow so the viewer gets a
// notification each time the target posts. The desired state is sent as a
// "notify" form value ("true"/"false") so the control works without JavaScript.
func (h *Handler) NotifyAccount(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	targetID := chi.URLParam(r, "id")
	notify := r.FormValue("notify") == "true"
	if err := h.app.SetFollowNotify(r.Context(), account, targetID, notify); err != nil {
		slog.Error("webui: set follow notify error", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	h.redirectBack(w, r)
}

// redirectBack returns the user to the page they acted from (falling back to the
// home timeline), which keeps form-based actions working without JavaScript.
func (h *Handler) redirectBack(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, safeReturnPath(r, r.Referer(), "/"), http.StatusFound)
}

// moderationRedirect runs a moderation action and redirects back to the page the
// user came from, so the buttons work without JavaScript.
func (h *Handler) moderationRedirect(w http.ResponseWriter, r *http.Request, action string, fn func(account *models.Account, targetID string) error) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if err := fn(account, targetID); err != nil {
		slog.Error("webui: moderation action error", "action", action, "error", err)
		h.renderError(w, r, "Error performing "+action)
		return
	}
	http.Redirect(w, r, safeReturnPath(r, r.Referer(), "/"), http.StatusFound)
}

// ReportFormPage renders a form to report an account.
// GET /report?account={id}&status={id}
func (h *Handler) ReportFormPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	targetID := r.URL.Query().Get("account")
	target, err := h.store.GetAccount(r.Context(), targetID)
	if err != nil {
		h.renderError(w, r, "Account not found")
		return
	}
	// Dereferenced for the template: a *string reaching fmt.Sprintf via T
	// renders as %!s(*string=0x...) instead of the domain.
	targetDomain := ""
	if target.Domain != nil {
		targetDomain = *target.Domain
	}
	h.renderPage(w, r, h.pageTitle(r, "report.heading"), map[string]interface{}{
		"Account":      account,
		"Target":       target,
		"TargetDomain": targetDomain,
		"StatusID":     r.URL.Query().Get("status"),
		"CSRFToken":    getCSRFToken(r.Context()),
	}, "report.html")
}

// ReportSubmit files a report from the web UI.
// POST /report
func (h *Handler) ReportSubmit(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	targetID := r.FormValue("account")
	if targetID == "" {
		h.renderError(w, r, "Missing account")
		return
	}
	var statusIDs []string
	if s := r.FormValue("status"); s != "" {
		statusIDs = append(statusIDs, s)
	}
	forward := r.FormValue("forward") == "on" || r.FormValue("forward") == "true"
	if _, err := h.app.CreateReport(r.Context(), account, targetID, statusIDs, r.FormValue("comment"), r.FormValue("category"), forward); err != nil {
		slog.Error("webui: create report error", "error", err)
		http.Error(w, "Could not submit report: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	target, err := h.store.GetAccount(r.Context(), targetID)
	if err == nil {
		http.Redirect(w, r, "/@"+target.Username+acctDomainSuffix(target), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// acctDomainSuffix returns "@domain" for a remote account or "" for a local one.
func acctDomainSuffix(a *models.Account) string {
	if a != nil && a.Domain != nil {
		return "@" + *a.Domain
	}
	return ""
}

// BlockAccount blocks the target account.
func (h *Handler) BlockAccount(w http.ResponseWriter, r *http.Request) {
	h.moderationRedirect(w, r, "block", func(a *models.Account, id string) error {
		return h.app.Block(r.Context(), a, id)
	})
}

// UnblockAccount removes a block.
func (h *Handler) UnblockAccount(w http.ResponseWriter, r *http.Request) {
	h.moderationRedirect(w, r, "unblock", func(a *models.Account, id string) error {
		return h.app.Unblock(r.Context(), a, id)
	})
}

// MuteAccount mutes the target account (notifications hidden too).
func (h *Handler) MuteAccount(w http.ResponseWriter, r *http.Request) {
	h.moderationRedirect(w, r, "mute", func(a *models.Account, id string) error {
		return h.app.Mute(r.Context(), a, id, true, nil)
	})
}

// UnmuteAccount removes a mute.
func (h *Handler) UnmuteAccount(w http.ResponseWriter, r *http.Request) {
	h.moderationRedirect(w, r, "unmute", func(a *models.Account, id string) error {
		return h.app.Unmute(r.Context(), a, id)
	})
}
