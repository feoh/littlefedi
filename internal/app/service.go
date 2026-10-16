// Package app provides shared application behavior for API and web handlers.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogslug"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/emoji"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
)

// extractInlineMentions parses @mentions from plain text content and resolves them.
// Returns Mention records for all resolved mentions (excluding self-mentions).
// skipIDs are account IDs to omit when a caller has already resolved them.
func (s *Service) extractInlineMentions(ctx context.Context, contentText string, statusID string, author *models.Account, skipIDs ...string) []*models.Mention {
	var mentions []*models.Mention
	seen := map[string]bool{}

	words := strings.Fields(contentText)
	// Build skip set for O(1) lookup
	skipSet := make(map[string]bool, len(skipIDs))
	for _, id := range skipIDs {
		skipSet[id] = true
	}
	for _, word := range words {
		core := word
		// Strip leading brackets
		for strings.HasPrefix(core, "(") || strings.HasPrefix(core, "[") {
			core = core[1:]
		}
		// Strip trailing punctuation
		for len(core) > 0 && strings.ContainsRune(".,!?:;)]", rune(core[len(core)-1])) {
			core = core[:len(core)-1]
		}

		if !strings.HasPrefix(core, "@") || len(core) < 2 {
			continue
		}

		acct := core[1:] // everything after @
		if acct == "" {
			continue
		}

		var mentionedAccount *models.Account

		if strings.Contains(acct, "@") {
			// user@domain format
			parts := strings.SplitN(acct, "@", 2)
			username := parts[0]
			domain := parts[1]
			if strings.EqualFold(domain, s.localDomain) {
				mentionedAccount, _ = s.store.GetAccountByUsernameDomain(ctx, username, nil)
			} else {
				// Known account first: every reply mentions the parent author,
				// who is already stored, and the network path (WebFinger +
				// count-enriching actor fetch) runs synchronously inside the
				// POST - it would block the composer on the remote server's
				// latency for accounts we already have.
				mentionedAccount, _ = s.store.GetAccountByUsernameDomain(ctx, username, &domain)
				if mentionedAccount == nil && s.federator != nil {
					resolved, err := s.federator.ResolveAccount(ctx, acct)
					if err == nil {
						mentionedAccount = resolved
					}
				}
			}
		} else {
			// Just @username - look up local
			mentionedAccount, _ = s.store.GetAccountByUsernameDomain(ctx, acct, nil)
		}

		if mentionedAccount == nil || mentionedAccount.ID == author.ID || seen[mentionedAccount.ID] || skipSet[mentionedAccount.ID] {
			continue
		}
		seen[mentionedAccount.ID] = true

		mentions = append(mentions, &models.Mention{
			StatusID:  statusID,
			AccountID: mentionedAccount.ID,
			Username:  mentionedAccount.Username,
			URL:       mentionedAccount.ActorURI,
			Acct:      acct,
		})
	}

	return mentions
}

type Service struct {
	blockSensitiveRelationshipNotifications bool

	store               storage.Storage
	federator           *federation.Federator
	domain              string // web domain (legacy field name)
	localDomain         string
	mediaPath           string
	mediaStore          objectstore.Store
	deliveries          sync.WaitGroup
	immediateDeliveries bool
	stream              streaming.Publisher // live streaming fan-out; nil until wired

	// pageCache caches enriched timeline/profile results keyed per viewer; nil
	// disables page caching (the read helpers fall back to a live build). It is
	// invalidated for a viewer whenever that viewer performs a write, so actors
	// always see their own actions immediately; other users' updates surface
	// within pageTTL.
	pageCache    *cache.MemoryCache
	pageTTL      time.Duration
	componentTTL time.Duration
	serveStale   bool
	// blogAllAccounts mirrors [blog] all_accounts: with it on, an account
	// needs no individual grant to mark a post as a blog post.
	blogAllAccounts bool
	pageMu          sync.Mutex
	pageCalls       map[string]*pageCacheCall
	serializedCalls map[string]*serializedPageCall
}

var ErrModerationRejected = errors.New("moderation action rejected")

type pageCacheCall struct {
	done   chan struct{}
	result PageResult
	err    error
}

// SetPageCache wires the optional per-viewer page cache. Safe to leave unset.
// pageTTL bounds rendered page snapshots; componentTTL bounds per-viewer
// enriched components, which are dependency-invalidated and so tolerate a much
// longer TTL (it only bounds embedded interaction-count staleness).
func (s *Service) SetPageCache(c *cache.MemoryCache, pageTTL, componentTTL time.Duration) {
	s.pageCache = c
	s.pageTTL = pageTTL
	s.componentTTL = componentTTL
	s.pageMu.Lock()
	if s.pageCalls == nil {
		s.pageCalls = make(map[string]*pageCacheCall)
	}
	s.pageMu.Unlock()
}

// SetServeStale enables stale-while-revalidate for cached pages: a
// generation-stale snapshot is served immediately while a background rebuild
// refreshes it for the next request.
func (s *Service) SetServeStale(v bool) { s.serveStale = v }

// SetBlogAllAccounts mirrors [blog] all_accounts, which grants blog publishing
// to every local account. Callers that build a Service from a config set it;
// the zero value keeps the per-account permission as the only way in.
func (s *Service) SetBlogAllAccounts(v bool) { s.blogAllAccounts = v }

func (s *Service) SetBlockSensitiveRelationshipNotifications(blocked bool) {
	s.blockSensitiveRelationshipNotifications = blocked
}

// PageResult is a cached rendered-timeline result: the post-processed status
// slice plus the pagination cursor derived from it. Treat the slice as
// read-only - it may be shared across concurrent requests on a cache hit.
type PageResult struct {
	Statuses []*models.Status
	Cursor   string
	RawCount int
}

// PageCacheDependency groups non-home page snapshots separately from reusable
// per-status components. Home snapshots have their own dependency because most
// shared-inbox deliveries cannot appear in any local user's home timeline.
const PageCacheDependency = "page-snapshot"

const HomePageCacheDependency = "page-snapshot:home"

func viewerPageDependency(viewerID string) string { return "page-viewer:" + viewerID }

// viewerPageKindDependency groups a viewer's page snapshots by window kind so a
// viewer's own status write can drop only the kinds where they must
// synchronously see their new post (home and their own profile), leaving every
// other snapshot generation-stale for the serve-stale fast path.
func viewerPageKindDependency(viewerID, kind string) string {
	return "page-viewer-kind:" + viewerID + ":" + kind
}

func viewerIDFromPageKey(key string) string {
	if !strings.HasPrefix(key, "v=") {
		return ""
	}
	rest := strings.TrimPrefix(key, "v=")
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return rest[:i]
	}
	return ""
}

// ViewerCacheKeyPrefix is the shared namespace for everything cached on behalf
// of one viewer - rendered pages (app layer) and rendered status cards (web UI).
// Dropping this prefix invalidates all of a viewer's cached artifacts at once, so
// a single `invalidatePages(viewerID)` on any of that viewer's writes clears both.
func ViewerCacheKeyPrefix(viewerID string) string { return "v=" + viewerID + ":" }

func pageKeyPrefix(viewerID string) string { return ViewerCacheKeyPrefix(viewerID) }

// PageKey builds a stable cache key for a rendered page. kind identifies the
// timeline ("home", "public", "local", "profile:<id>", ...); the remaining parts
// (cursor, limit, boolean flags) distinguish variants. All keys for one viewer
// share a prefix so they can be dropped together on that viewer's next write.
func (s *Service) PageKey(viewerID, kind string, parts ...string) string {
	var b strings.Builder
	b.WriteString(pageKeyPrefix(viewerID))
	b.WriteString(kind)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

// timelineGenerationProvider is implemented by the timeline caching decorator.
// The page cache uses it to validate snapshots on read (a generation mismatch
// means a status arrived or membership changed since the page was built)
// instead of being eagerly wiped on every status event. Generations are scoped:
// home is per-viewer (another user's new post does not stale this viewer's home
// page) and the other kinds are per window kind (an unlisted shared-inbox
// delivery never stales a public page snapshot).
type timelineGenerationProvider interface {
	TimelineGenerationFor(viewerID, kind string) uint64
}

// pageEntry stamps a cached page with the timeline generation (of its window
// kind) it was built from.
type pageEntry struct {
	res        PageResult
	generation uint64
}

// pageGeneration returns the current generation of the page's window kind for
// the viewer, and whether the underlying store tracks generations at all.
func (s *Service) pageGeneration(viewerID, kind string) (uint64, bool) {
	if p, ok := s.store.(timelineGenerationProvider); ok {
		return p.TimelineGenerationFor(viewerID, kind), true
	}
	return 0, false
}

// pageWindowKind maps a page cache key to the timeline window kind whose
// generation counter validates it. Unknown kinds map to "" (global generation
// only, advanced by moderation-class events).
func pageWindowKind(key string) string {
	rest := key
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[i+1:] // strip the "v=<viewer>" prefix
	}
	rest = strings.TrimPrefix(rest, "partial:")
	rest = strings.TrimPrefix(rest, "api:")
	segment := func(s string) string {
		if i := strings.IndexByte(s, ':'); i >= 0 {
			return s[:i]
		}
		return s
	}
	seg := segment(rest)
	switch seg {
	case "home", "public", "bubble", "tag", "direct", "bookmarks", "favourites":
		return seg
	case "list":
		// A list page's viewer is always the list's owner, and list content is
		// a subset of home content, so the viewer's home generation validates
		// list snapshots exactly (membership writes advance it via
		// invalidateViewerHome).
		return "home"
	case "local":
		return "public"
	case "profile-pinned", "profile-statuses", "acct":
		if len(rest) > len(seg)+1 {
			return "account:" + segment(rest[len(seg)+1:])
		}
	}
	return ""
}

// PageCached returns the cached page for key, or builds it via build and caches
// the result. When the page cache is disabled (or key is empty) it always
// builds. On a cache hit the returned slice is shared and must not be mutated.
//
// With serve-stale enabled, a snapshot invalidated only by the timeline
// generation (i.e. new posts arrived) is returned immediately while a single
// background rebuild refreshes it - on slow storage the visitor never waits on
// the rebuild, they see it one request later.
func (s *Service) PageCached(ctx context.Context, key string, build func(context.Context) (PageResult, error)) (PageResult, error) {
	if s.pageCache == nil || key == "" {
		return build(ctx)
	}
	generation, tracked := s.pageGeneration(viewerIDFromPageKey(key), pageWindowKind(key))
	v, ok, lookup := s.pageCache.GetWithResult(key)
	if ok && s.pageAudienceCurrent(ctx, v.(pageEntry).res.Statuses, viewerIDFromPageKey(key)) {
		entry := v.(pageEntry)
		if !tracked || entry.generation == generation {
			slog.Debug("cache: page hit", "page", pageKind(key))
			return entry.res, nil
		}
		if s.serveStale {
			// Detach the rebuild from the request: the response is written
			// before the rebuild finishes, which cancels the request context.
			rebuildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pageRebuildTimeout)
			go func() {
				defer cancel()
				_, _ = s.rebuildPage(rebuildCtx, key, generation, build)
			}()
			slog.Debug("cache: page served stale", "page", pageKind(key))
			return entry.res, nil
		}
		lookup = "stale_generation"
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		stats := s.pageCache.Stats()
		slog.Debug("cache: page rebuild", "page", pageKind(key), "reason", lookup, "ttl", s.pageTTL, "entries", stats.Entries, "bytes", stats.Bytes, "max_bytes", stats.MaxBytes, "misses", stats.Misses, "evictions", stats.Evictions, "expirations", stats.Expirations)
	}
	return s.rebuildPage(ctx, key, generation, build)
}

// Serialized pages must not bypass the content-version audience check.
func (s *Service) pageAudienceCurrent(ctx context.Context, statuses []*models.Status, viewerID string) bool {
	for _, st := range statuses {
		if st == nil {
			continue
		}
		if st.Visibility == models.VisibilityPrivate || st.Visibility == models.VisibilityDirect {
			allowed, err := s.store.CanReadStatusVersion(ctx, st, viewerID)
			if err != nil || !allowed {
				return false
			}
		}
		if !s.pageAudienceCurrent(ctx, []*models.Status{st.BoostOf, st.QuoteOf, st.InReplyTo}, viewerID) {
			return false
		}
	}
	return true
}

// pageRebuildTimeout bounds a detached stale-page refresh; generous because a
// cold rebuild on SD-card-class storage may take several seconds and paying it
// in the background is the point.
const pageRebuildTimeout = 60 * time.Second

// rebuildPage builds and stores a page snapshot with single-flight semantics:
// concurrent callers (foreground or detached stale refresh) coalesce onto one
// build per key.
func (s *Service) rebuildPage(ctx context.Context, key string, generation uint64, build func(context.Context) (PageResult, error)) (PageResult, error) {
	s.pageMu.Lock()
	if call := s.pageCalls[key]; call != nil {
		s.pageMu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err
		case <-ctx.Done():
			return PageResult{}, ctx.Err()
		}
	}
	call := &pageCacheCall{done: make(chan struct{})}
	s.pageCalls[key] = call
	s.pageMu.Unlock()
	version := s.pageCache.Version()
	started := time.Now()
	res, err := build(ctx)
	metrics.ObserveTimelineStage("page_build", time.Since(started))
	stored := false
	if err == nil {
		dependency := PageCacheDependency
		if pageWindowKind(key) == "home" {
			dependency = HomePageCacheDependency
		}
		viewerID := viewerIDFromPageKey(key)
		dependencies := []string{dependency, viewerPageDependency(viewerID)}
		if kind := pageWindowKind(key); kind != "" {
			dependencies = append(dependencies, viewerPageKindDependency(viewerID, kind))
		}
		for _, status := range res.Statuses {
			dependencies = append(dependencies, componentDependencies(status, viewerID)...)
		}
		// Stamp with the generation read before the build: a status arriving
		// mid-build makes the snapshot immediately stale, and the next read
		// rebuilds through the cheap incremental timeline head refresh.
		entry := pageEntry{res: res, generation: generation}
		stored = s.pageCache.SetIfVersionWithDependencies(version, key, entry, pageResultSize(res), s.pageTTL, dependencies)
	}
	slog.Debug("cache: page rebuilt", "page", pageKind(key), "statuses", len(res.Statuses), "duration", time.Since(started), "stored", stored, "error", err)
	call.result, call.err = res, err
	s.pageMu.Lock()
	delete(s.pageCalls, key)
	close(call.done)
	s.pageMu.Unlock()
	return res, err
}

func pageKind(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 && i+1 < len(key) {
		rest := key[i+1:]
		if j := strings.IndexByte(rest, ':'); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return key
}

// invalidatePages drops every cached page belonging to viewerID. Called after a
// viewer's own writes so they never see a stale snapshot of their own action.
func (s *Service) invalidatePages(viewerID string) {
	if s.pageCache != nil {
		slog.Debug("cache: viewer pages invalidated", "viewer", viewerID)
		s.pageCache.DeleteDependencies(viewerPageDependency(viewerID))
	}
	// PostgreSQL deployments use the streaming broker as a compact cache
	// invalidation bus. LocalBroker consumes this as a no-op.
	s.pub().CacheInvalidated(context.Background())
}

// InvalidateFilterPages drops every cached view whose membership or warning
// annotations can change after a filter write.
func (s *Service) InvalidateFilterPages(accountID string) { s.invalidatePages(accountID) }

// invalidatePagesAfterOwnStatus drops only the page snapshots where the poster
// must synchronously see their brand-new status: the composer redirects back
// to the Referer, so every timeline kind the new status can appear on for its
// visibility (mirroring statusCreated's kind bumps) plus home (webui, partial,
// API and list variants all share the "home" window kind) and the poster's own
// profile. Everything else (tag pages, bookmarks, other profiles, other
// viewers) stays cached generation-stale, so the next visit takes the
// serve-stale fast path instead of a cold foreground rebuild.
func (s *Service) invalidatePagesAfterOwnStatus(viewerID string, visibility models.Visibility) {
	if s.pageCache != nil {
		slog.Debug("cache: viewer own-status pages invalidated", "viewer", viewerID, "visibility", visibility)
		deps := []string{
			viewerPageKindDependency(viewerID, "home"),
			viewerPageKindDependency(viewerID, "account:"+viewerID),
		}
		switch visibility {
		case models.VisibilityPublic, models.VisibilityLocal:
			deps = append(deps,
				viewerPageKindDependency(viewerID, "public"),
				viewerPageKindDependency(viewerID, "bubble"))
		case models.VisibilityUnlisted, models.VisibilityLocalUnlisted:
			deps = append(deps, viewerPageKindDependency(viewerID, "bubble"))
		case models.VisibilityDirect:
			deps = append(deps, viewerPageKindDependency(viewerID, "direct"))
		}
		s.pageCache.DeleteDependencies(deps...)
	}
	// PostgreSQL deployments use the streaming broker as a compact cache
	// invalidation bus. LocalBroker consumes this as a no-op.
	s.pub().CacheInvalidated(context.Background())
}

func (s *Service) invalidateStatusAndViewer(viewerID, statusID string) {
	s.InvalidateStatusComponents(statusID)
	s.invalidatePages(viewerID)
}

func (s *Service) invalidateViewerStatusAndPages(viewerID, statusID string) {
	s.InvalidateViewerStatus(viewerID, statusID)
	if s.pageCache != nil {
		s.pageCache.DeletePrefix(pageKeyPrefix(viewerID) + "bookmarks:")
		s.pageCache.DeletePrefix(pageKeyPrefix(viewerID) + "api:bookmarks:")
	}
}

// pageResultSize estimates the memory footprint of a cached page. Statuses are
// shared with the entity cache's copies, but we bill their approximate content
// size here so the page budget reflects real retained bytes.
func pageResultSize(res PageResult) int {
	n := 128 + len(res.Cursor)
	for _, st := range res.Statuses {
		if st == nil {
			continue
		}
		n += 512 + len(st.ID) + len(st.ContentHTML) + len(st.Summary)
		if st.Account != nil {
			n += 256 + len(st.Account.DisplayName) + len(st.Account.Note)
		}
	}
	return n
}

// serialized (encoded-bytes) page cache
//
// Without this, the Mastodon JSON conversion and json.Marshal would run on every
// request even when PageCached hits. The web UI caches rendered card HTML; this
// is the API equivalent: the encoded response body cached per viewer and page,
// with the same key namespace, generation checks and dependency invalidation as
// the page snapshots it wraps.

// SerializedPage is a fully-encoded API response cached per viewer+page.
// FirstID/LastID/Count carry what the Link pagination header needs, so a hit
// writes headers + body without touching the status objects.
type SerializedPage struct {
	Body    []byte
	FirstID string
	LastID  string
	Count   int
}

type serializedPageEntry struct {
	audience   []*models.Status
	sp         SerializedPage
	generation uint64
}

type serializedPageCall struct {
	done   chan struct{}
	result SerializedPage
	err    error
}

// SerializedPageCached returns the cached encoded page for key, or builds it.
// build returns the encoded page plus the statuses it was derived from (used
// for dependency keys). key must be a PageKey with a trailing variant part
// (e.g. "json"): the "v=<viewer>:" prefix keeps viewer-write invalidation
// working and pageWindowKind still resolves the window for generation checks.
//
// No serve-stale here: on a generation mismatch the inner PageCached (called
// inside build) still serves stale and refreshes in the background, so the
// only in-line cost is the Mastodon conversion + marshal of an already-built
// page - milliseconds, not the cold query.
func (s *Service) SerializedPageCached(ctx context.Context, key string, build func(context.Context) (SerializedPage, []*models.Status, error)) (SerializedPage, error) {
	if s.pageCache == nil || key == "" {
		sp, _, err := build(ctx)
		return sp, err
	}
	generation, tracked := s.pageGeneration(viewerIDFromPageKey(key), pageWindowKind(key))
	if v, ok := s.pageCache.Get(key); ok {
		entry := v.(serializedPageEntry)
		if (!tracked || entry.generation == generation) && s.pageAudienceCurrent(ctx, entry.audience, viewerIDFromPageKey(key)) {
			return entry.sp, nil
		}
	}

	// Single-flight per key, sharing pageMu with the page-snapshot calls.
	s.pageMu.Lock()
	if s.serializedCalls == nil {
		s.serializedCalls = make(map[string]*serializedPageCall)
	}
	if call := s.serializedCalls[key]; call != nil {
		s.pageMu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err
		case <-ctx.Done():
			return SerializedPage{}, ctx.Err()
		}
	}
	call := &serializedPageCall{done: make(chan struct{})}
	s.serializedCalls[key] = call
	s.pageMu.Unlock()

	version := s.pageCache.Version()
	sp, statuses, err := build(ctx)
	if err == nil {
		dependency := PageCacheDependency
		if pageWindowKind(key) == "home" {
			dependency = HomePageCacheDependency
		}
		viewerID := viewerIDFromPageKey(key)
		dependencies := []string{dependency, viewerPageDependency(viewerID)}
		if kind := pageWindowKind(key); kind != "" {
			dependencies = append(dependencies, viewerPageKindDependency(viewerID, kind))
		}
		for _, status := range statuses {
			dependencies = append(dependencies, componentDependencies(status, viewerID)...)
		}
		var audience []*models.Status
		var collect func(*models.Status)
		collect = func(st *models.Status) {
			if st == nil {
				return
			}
			if st.Visibility == models.VisibilityPrivate || st.Visibility == models.VisibilityDirect {
				audience = append(audience, &models.Status{ID: st.ID, UpdatedAt: st.UpdatedAt, Visibility: st.Visibility, AccountID: st.AccountID})
			}
			collect(st.BoostOf)
			collect(st.QuoteOf)
			collect(st.InReplyTo)
		}
		for _, st := range statuses {
			collect(st)
		}
		entry := serializedPageEntry{sp: sp, generation: generation, audience: audience}
		s.pageCache.SetIfVersionWithDependencies(version, key, entry, len(sp.Body)+64+len(audience)*128, s.pageTTL, dependencies)
	}
	call.result, call.err = sp, err
	s.pageMu.Lock()
	delete(s.serializedCalls, key)
	close(call.done)
	s.pageMu.Unlock()
	return sp, err
}

// SetStreamPublisher wires the streaming fan-out. Safe to leave unset: all
// streaming calls go through pub(), which substitutes a no-op publisher.
func (s *Service) SetStreamPublisher(p streaming.Publisher) {
	s.stream = p
}

// pub returns the configured streaming publisher, or a no-op when none is wired.
func (s *Service) pub() streaming.Publisher {
	if s.stream == nil {
		return streaming.NopPublisher{}
	}
	return s.stream
}

// streamNotification serializes and fans a freshly created notification out to
// its recipient's live stream. Best-effort: streaming never blocks the action.
func (s *Service) streamNotification(ctx context.Context, notif *models.Notification) {
	if s.stream == nil || notif == nil {
		return
	}
	s.stream.NotificationCreated(ctx, notif)
}

type StatusInput struct {
	Content        string
	ContentType    string // text/plain (default) or text/markdown
	Summary        string
	Sensitive      bool
	Visibility     models.Visibility
	Language       string
	InReplyToID    *string
	InReplyToURI   *string // trusted internal use; keeps replies whose parent row is gone
	QuotedStatusID string
	MediaIDs       []string
	PollOptions    []string
	PollExpiresIn  time.Duration
	PollMultiple   bool
	PollHideTotals bool
	// ExpiresIn, if positive, self-destructs the status that long after
	// creation (approximate, enforced by the status_expire background job).
	ExpiresIn time.Duration
	// IsBlogPost flags a top-level post for the account's static blog site
	// (built only with -tags blog; rejected outside CreateStatus if combined
	// with a reply target).
	IsBlogPost bool
	// BlogSlug is an optional author-chosen permalink path for a blog post,
	// replacing the one the configured template would produce. Sanitized and
	// de-duplicated at creation; ignored on a non-blog post.
	BlogSlug string
}

// EditStatusInput carries the editable fields of an existing post. Visibility,
// reply target and quote are immutable on edit, matching Mastodon.
type EditStatusInput struct {
	Content     string
	ContentType string // text/plain (default) or text/markdown
	Summary     string
	Sensitive   bool
	Language    string
	MediaIDs    []string // nil preserves existing media; an empty slice removes it
	// MediaDescriptions replaces the alt text of the listed attachments,
	// keyed by media ID. Attachments not in the map keep their description.
	MediaDescriptions map[string]string
	// IsBlogPost, when non-nil, promotes or demotes the post.
	IsBlogPost *bool
	BlogSlug   string
	// ExpiresIn, when non-nil, updates the status's expiry: 0 cancels it,
	// positive reschedules it to that long from now. nil leaves it untouched.
	ExpiresIn *time.Duration
}

type Relationship struct {
	Following       bool
	FollowedBy      bool
	FollowRequested bool
	Blocking        bool
	BlockedBy       bool
	Muting          bool
}

func New(store storage.Storage, federator *federation.Federator, domain string) *Service {
	return NewWithDomains(store, federator, domain, domain)
}

func NewWithDomains(store storage.Storage, federator *federation.Federator, localDomain, webDomain string) *Service {
	return &Service{store: store, federator: federator, domain: webDomain, localDomain: localDomain}
}

func NewWithDomainsAndMediaPath(store storage.Storage, federator *federation.Federator, localDomain, webDomain, mediaPath string) *Service {
	return &Service{store: store, federator: federator, domain: webDomain, localDomain: localDomain, mediaPath: mediaPath, mediaStore: objectstore.NewLocal(mediaPath, "/media/")}
}

// SendWelcomeDirectMessage sends the configured instance welcome DM, if any.
func SendWelcomeDirectMessage(ctx context.Context, store storage.Storage, domain string, recipient *models.Account) error {
	message, err := store.GetWelcomeDirectMessage(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sender, err := store.GetAccount(ctx, message.SenderAccountID)
	if err != nil {
		return err
	}
	_, err = New(store, nil, domain).CreateLocalDirectMessage(ctx, sender, recipient, message.Content)
	return err
}

// CreateLocalDirectMessage addresses one local account through the shared status flow.
func (s *Service) CreateLocalDirectMessage(ctx context.Context, sender, recipient *models.Account, message string) (*models.Status, error) {
	return s.CreateStatus(ctx, sender, StatusInput{
		Content: "@" + recipient.Username + " " + strings.TrimSpace(message), ContentType: content.ContentTypeMarkdown, Visibility: models.VisibilityDirect,
	})
}

func (s *Service) SetMediaStore(store objectstore.Store) {
	if store != nil {
		s.mediaStore = store
	}
}

// WaitForDeliveries waits for immediate, account-targeted federation started by
// this service when SetImmediateDeliveries is enabled by a CLI caller.
func (s *Service) WaitForDeliveries() {
	s.deliveries.Wait()
}

// SetImmediateDeliveries keeps one-shot CLI commands from leaving work for a
// worker process that may not be running. Server requests use the durable queue.
func (s *Service) SetImmediateDeliveries(enabled bool) {
	s.immediateDeliveries = enabled
}

// customBlogSlug sanitizes an author-supplied permalink and makes it unique
// among the account's existing slugs, since the column carries a per-account
// unique index and a duplicate would fail the insert. An empty result (no
// custom slug given, nothing usable left after cleaning, or an unreadable
// slug list) simply leaves the slug for the site generator to assign from the
// configured template.
func (s *Service) customBlogSlug(ctx context.Context, accountID, statusID, requested string) string {
	cleaned := blogslug.Clean(requested)
	if cleaned == "" {
		return ""
	}
	taken, err := s.store.GetBlogSlugs(ctx, accountID)
	if err != nil {
		slog.Warn("app: cannot check blog slug uniqueness, deferring to the generator", "account_id", accountID, "error", err)
		return ""
	}
	return blogslug.Unique(cleaned, statusID, taken)
}

type preparedDelivery struct {
	target   *models.Account
	activity map[string]interface{}
}

type preparedStatus struct {
	status    *models.Status
	jobs      []*models.Job
	immediate []preparedDelivery
}

func (s *Service) CreateStatus(ctx context.Context, account *models.Account, input StatusInput) (*models.Status, error) {
	prepared, err := s.prepareStatus(ctx, account, input)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateStatusWithJobs(ctx, prepared.status, prepared.jobs); err != nil {
		return nil, err
	}
	s.finishStatus(ctx, prepared)
	return prepared.status, nil
}

func (s *Service) prepareStatus(ctx context.Context, account *models.Account, input StatusInput) (*preparedStatus, error) {
	input.Content = strings.TrimSpace(input.Content)
	input.ContentType = content.NormalizeContentType(input.ContentType)
	if err := ValidateStatusInput(input); err != nil {
		return nil, err
	}
	if input.Language == "" {
		input.Language = "en"
	}
	if input.IsBlogPost && (input.InReplyToID != nil && *input.InReplyToID != "" || input.InReplyToURI != nil && *input.InReplyToURI != "") {
		return nil, fmt.Errorf("blog posts cannot be replies")
	}

	var parent *models.Status
	if input.InReplyToID != nil && *input.InReplyToID != "" {
		if found, err := s.store.GetStatus(ctx, *input.InReplyToID); err == nil {
			if !s.CanViewStatus(ctx, found, account) {
				return nil, fmt.Errorf("parent status not found")
			}
			parent = found
			if parent.Account == nil && parent.AccountID != "" {
				parent.Account, _ = s.store.GetAccount(ctx, parent.AccountID)
			}
			if input.InReplyToURI == nil {
				input.Content = prefixedReplyContent(input.Content, account, parent.Account)
			}
		}
	}

	// Resolve the quoted status (it is always one we already store locally,
	// since quoting is only offered on rendered posts).
	var quoted *models.Status
	if input.QuotedStatusID != "" {
		if found, err := s.store.GetStatus(ctx, input.QuotedStatusID); err == nil {
			if !s.CanViewStatus(ctx, found, account) {
				return nil, fmt.Errorf("quoted status not found")
			}
			if found.Visibility == models.VisibilityPrivate || found.Visibility == models.VisibilityDirect {
				return nil, fmt.Errorf("cannot quote a non-public post")
			}
			quoted = found
			if quoted.Account == nil && quoted.AccountID != "" {
				quoted.Account, _ = s.store.GetAccount(ctx, quoted.AccountID)
			}
		}
	}

	// Default visibility: inherit from parent when replying, otherwise public
	if input.Visibility == "" {
		if parent != nil {
			input.Visibility = parent.Visibility
		} else {
			input.Visibility = models.VisibilityPublic
		}
	}
	if parent != nil && !replyVisibilityAllowed(input.Visibility, parent.Visibility) {
		return nil, fmt.Errorf("reply visibility cannot be wider than the parent")
	}

	now := time.Now()
	id := storage.NewStatusID(now)
	status := &models.Status{
		ID:            id,
		URI:           fmt.Sprintf("https://%s/objects/%s", s.domain, id),
		URL:           fmt.Sprintf("https://%s/@%s/%s", s.domain, account.Username, id),
		AccountID:     account.ID,
		Account:       account,
		ContentHTML:   renderInputHTML(input.Content, input.ContentType),
		ContentSource: input.Content,
		ContentType:   input.ContentType,
		Summary:       template.HTMLEscapeString(strings.TrimSpace(input.Summary)),
		Sensitive:     input.Sensitive || strings.TrimSpace(input.Summary) != "",
		Visibility:    input.Visibility,
		Language:      input.Language,
		IsBlogPost:    input.IsBlogPost,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if input.ExpiresIn > 0 {
		expiresAt := time.Now().Add(input.ExpiresIn)
		status.ExpiresAt = &expiresAt
	}
	if input.IsBlogPost {
		status.BlogSlug = s.customBlogSlug(ctx, account.ID, status.ID, input.BlogSlug)
	}
	if input.InReplyToID != nil && *input.InReplyToID != "" {
		status.InReplyToID = input.InReplyToID
		if input.InReplyToURI != nil && *input.InReplyToURI != "" {
			status.InReplyToURI = input.InReplyToURI
		} else if parent != nil {
			replyURI := parent.URI
			status.InReplyToURI = &replyURI
		}
	} else if input.InReplyToURI != nil && *input.InReplyToURI != "" {
		status.InReplyToURI = input.InReplyToURI
	}
	if quoted != nil {
		quotedID := quoted.ID
		quotedURI := quoted.URI
		status.QuoteOfID = &quotedID
		status.QuoteOfURI = &quotedURI
		status.QuoteState = "pending"
		status.QuoteOf = quoted
	}

	// Resolve every relationship before the authoritative write. CreateStatus
	// persists these rows and the materialized home feed atomically.
	resolvedMentions := s.extractInlineMentions(ctx, input.Content, status.ID, account)
	status.Mentions = make([]models.Mention, len(resolvedMentions))
	mentionURLs := make(map[string]string, len(resolvedMentions))
	for i, mention := range resolvedMentions {
		status.Mentions[i] = *mention
		mentionURLs[mention.Acct] = mention.URL
	}
	status.ContentHTML = content.ResolveSocialLinks(status.ContentHTML, "https://"+s.domain, mentionURLs)
	for _, name := range content.ExtractHashtags(input.Content) {
		tagObj, err := s.store.GetOrCreateTag(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve hashtag %q: %w", name, err)
		}
		status.Tags = append(status.Tags, *tagObj)
	}
	if status.Tags == nil {
		status.Tags = []models.Tag{}
	}
	for _, mediaID := range input.MediaIDs {
		media, err := s.store.GetMediaAttachment(ctx, mediaID)
		if err != nil || media.AccountID != account.ID || media.StatusID != nil {
			return nil, fmt.Errorf("invalid media attachment %q", mediaID)
		}
		status.MediaAttachments = append(status.MediaAttachments, *media)
	}
	if len(input.PollOptions) >= 2 {
		expires := time.Now().Add(input.PollExpiresIn)
		poll := &models.Poll{StatusID: status.ID, ExpiresAt: &expires, Multiple: input.PollMultiple, HideTotals: input.PollHideTotals}
		for _, option := range input.PollOptions {
			poll.Options = append(poll.Options, models.PollOption{Title: strings.TrimSpace(option)})
		}
		status.Poll = poll
	}
	// A quote of a local post is self-authorized. Persist the grant beside the
	// quoting status so neither can exist without the other.
	if quoted != nil && quoted.Account != nil && quoted.Account.Domain == nil {
		authID := uuid.New().String()
		authURI := fmt.Sprintf("https://%s/objects/%s/quote_authorizations/%s", s.domain, quoted.ID, authID)
		status.QuoteApprovalURI = &authURI
		status.QuoteState = "accepted"
		status.QuoteAuthorization = &models.QuoteAuthorization{
			ID: authID, URI: authURI, QuotedStatusID: quoted.ID,
			QuotingURI: status.URI, QuotingActorURI: account.ActorURI,
		}
	}

	var activity map[string]interface{}
	jobs := make([]*models.Job, 0, 3)
	if status.Poll != nil && status.Poll.ExpiresAt != nil {
		if status.Poll.ID == "" {
			status.Poll.ID = uuid.New().String()
		}
		payload, _ := json.Marshal(map[string]string{"poll_id": status.Poll.ID})
		jobs = append(jobs, &models.Job{Kind: "poll_close", Payload: string(payload), RunAt: *status.Poll.ExpiresAt, MaxAttempts: 3, State: "queued"})
	}
	if status.ExpiresAt != nil {
		payload, _ := json.Marshal(map[string]string{"status_id": status.ID})
		jobs = append(jobs, &models.Job{Kind: models.StatusExpireJobKind, Payload: string(payload), RunAt: *status.ExpiresAt, MaxAttempts: 10, State: "queued"})
	}
	if status.Visibility != models.VisibilityLocal && status.Visibility != models.VisibilityLocalUnlisted {
		activity = s.createActivity(status, account)
		switch status.Visibility {
		case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate:
			jobs = append(jobs, deliveryFanoutJob(account.ID, activity))
		}
	}

	prepared := &preparedStatus{status: status, jobs: jobs}
	if activity != nil {
		seen := make(map[string]bool)
		if status.Visibility != models.VisibilityDirect {
			inboxes, err := s.store.GetFollowerDestinations(ctx, account.ID)
			if err != nil {
				return nil, err
			}
			for _, inbox := range inboxes {
				seen[inbox] = true
			}
		}
		deliver := func(target *models.Account, body map[string]interface{}, deduplicate bool) error {
			if target == nil || target.Domain == nil {
				return nil
			}
			inbox := remoteInbox(target)
			if deduplicate && seen[inbox] {
				return nil
			}
			seen[inbox] = true
			if s.immediateDeliveries {
				prepared.immediate = append(prepared.immediate, preparedDelivery{target, body})
				return nil
			}
			job, err := federation.PrepareDelivery(ctx, s.store, inbox, body)
			if errors.Is(err, storage.ErrFederationBlocked) {
				return nil
			}
			if err != nil {
				return err
			}
			prepared.jobs = append(prepared.jobs, job)
			return nil
		}
		for _, mention := range status.Mentions {
			target, err := s.store.GetAccount(ctx, mention.AccountID)
			if err != nil {
				return nil, err
			}
			if err := deliver(target, activity, true); err != nil {
				return nil, err
			}
		}
		if parent != nil && parent.AccountID != account.ID {
			if err := deliver(parent.Account, activity, true); err != nil {
				return nil, err
			}
		}
		if quoted != nil && quoted.Account != nil && quoted.Account.Domain != nil {
			if err := deliver(quoted.Account, quoteRequestActivity(account, status, quoted), false); err != nil {
				return nil, err
			}
		}
	}
	if account.Domain == nil && !account.IsServiceActor && status.Visibility == models.VisibilityPublic {
		relayJobs, err := s.prepareRelayStatus(ctx, status)
		if err != nil {
			return nil, err
		}
		prepared.jobs = append(prepared.jobs, relayJobs...)
	}
	return prepared, nil
}

func (s *Service) finishStatus(ctx context.Context, prepared *preparedStatus) {
	status := prepared.status
	account, quoted := status.Account, status.QuoteOf
	for _, delivery := range prepared.immediate {
		s.deliverToAccount(ctx, account, delivery.target, delivery.activity)
	}

	// Quote approval (FEP-044f). Quoting a local post is self-granted right away
	// so the federated Create can carry the authorization; quoting a remote post
	// stays pending until the remote author's server replies with an Accept.
	if quoted != nil && quoted.Account != nil && quoted.Account.Domain == nil {
		if quoted.AccountID != account.ID {
			notif := &models.Notification{
				Type:          "quote",
				AccountID:     quoted.AccountID,
				FromAccountID: account.ID,
				StatusID:      &status.ID,
			}
			if err := s.store.UpsertNotification(ctx, notif); err != nil {
				slog.Error("app: failed to upsert quote notification", "error", err)
			} else {
				s.streamNotification(ctx, notif)
				push.EnqueuePushNotification(ctx, s.store, notif)
				mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
			}
		}
	}
	if len(status.Mentions) > 0 {
		// Notifications and external side effects start only after the content
		// transaction has committed successfully.
		for _, m := range status.Mentions {
			mentionedAccount, err := s.store.GetAccount(ctx, m.AccountID)
			if err == nil && mentionedAccount.Domain == nil {
				notif := &models.Notification{
					Type:          "mention",
					AccountID:     mentionedAccount.ID,
					FromAccountID: account.ID,
					StatusID:      &status.ID,
				}
				if err := s.store.UpsertNotification(ctx, notif); err == nil {
					s.streamNotification(ctx, notif)
					push.EnqueuePushNotification(ctx, s.store, notif)
					mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
				}
			}
		}
	}

	// Notify local followers who enabled the bell on this author.
	s.notifyBellFollowers(ctx, status)

	// Stream the new status to live timelines. Enrich first (viewer-neutral) so
	// the serialized payload carries mentions/tags/media; the caller re-enriches
	// with the real viewer for its own response.
	if s.stream != nil {
		s.EnrichStatus(ctx, status, nil)
		s.stream.StatusCreated(ctx, status)
	}

	// Invalidate only on success and only the pages where the poster must see
	// the new status immediately; visibility is resolved by now (it may have
	// been inherited from the parent).
	s.invalidatePagesAfterOwnStatus(account.ID, status.Visibility)

}

func quoteRequestActivity(account *models.Account, quoting, quoted *models.Status) map[string]interface{} {
	return map[string]interface{}{
		"@context":   federation.QuoteJSONLDContext(),
		"id":         quoting.URI + "/quote_request",
		"type":       "QuoteRequest",
		"actor":      account.ActorURI,
		"object":     quoted.URI,
		"instrument": quoting.URI,
		"to":         []string{quoted.Account.ActorURI},
	}
}

func validVisibility(v models.Visibility) bool {
	switch v {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate,
		models.VisibilityDirect, models.VisibilityLocal, models.VisibilityLocalUnlisted:
		return true
	}
	return false
}

func replyVisibilityAllowed(child, parent models.Visibility) bool {
	if !validVisibility(child) || !validVisibility(parent) {
		return false
	}
	if (parent == models.VisibilityLocal || parent == models.VisibilityLocalUnlisted) &&
		child != models.VisibilityLocal && child != models.VisibilityLocalUnlisted &&
		child != models.VisibilityPrivate && child != models.VisibilityDirect {
		return false
	}
	rank := func(v models.Visibility) int {
		switch v {
		case models.VisibilityPublic, models.VisibilityLocal:
			return 3
		case models.VisibilityUnlisted, models.VisibilityLocalUnlisted:
			return 2
		case models.VisibilityPrivate:
			return 1
		case models.VisibilityDirect:
			return 0
		}
		return 4
	}
	return rank(child) <= rank(parent)
}

// CanViewStatus reports whether viewer may read status. Unknown visibility and
// relationship lookup failures fail closed.
func (s *Service) CanViewStatus(ctx context.Context, status *models.Status, viewer *models.Account) bool {
	return s.canViewStatus(ctx, status, viewer, make(map[string]bool))
}

func (s *Service) canViewStatus(ctx context.Context, status *models.Status, viewer *models.Account, seen map[string]bool) bool {
	if status == nil {
		return false
	}
	if status.ID != "" {
		if seen[status.ID] {
			return false
		}
		seen[status.ID] = true
		defer delete(seen, status.ID)
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityLocal, models.VisibilityLocalUnlisted:
	case models.VisibilityPrivate, models.VisibilityDirect:
	default:
		return false
	}
	if status.Visibility == models.VisibilityPrivate || status.Visibility == models.VisibilityDirect {
		if viewer == nil {
			return false
		}
		allowed, err := s.store.CanReadStatusVersion(ctx, status, viewer.ID)
		if err != nil || !allowed {
			return false
		}
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		original := status.BoostOf
		if original == nil {
			original, _ = s.store.GetStatus(ctx, *status.BoostOfID)
		}
		if !s.canViewStatus(ctx, original, viewer, seen) {
			return false
		}
	}
	return true
}

// canEmbedStatus applies the same read rule to nested quote/reply cards. A
// public wrapper may remain visible, but it must never carry private content.
func (s *Service) canEmbedStatus(ctx context.Context, status *models.Status, viewer *models.Account) bool {
	if status == nil {
		return false
	}
	return s.CanViewStatus(ctx, status, viewer)
}

// VisibleStatuses removes statuses the viewer is not allowed to read.
func (s *Service) VisibleStatuses(ctx context.Context, statuses []*models.Status, viewer *models.Account) []*models.Status {
	visible := statuses[:0]
	for _, status := range statuses {
		if s.CanViewStatus(ctx, status, viewer) {
			visible = append(visible, status)
		}
	}
	return visible
}

// EditStatus updates the content, content warning, sensitivity, language and
// media of one of the caller's own posts, optionally promotes it to the blog,
// marks it edited, and federates an Update activity. Boosts cannot be edited.
func (s *Service) EditStatus(ctx context.Context, account *models.Account, statusID string, input EditStatusInput) (*models.Status, error) {
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	expected := status.UpdatedAt
	if status.AccountID != account.ID {
		return nil, fmt.Errorf("not your post")
	}
	if !validVisibility(status.Visibility) {
		return nil, fmt.Errorf("invalid visibility")
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		return nil, fmt.Errorf("cannot edit a boost")
	}
	polls, err := s.store.GetPollsForStatuses(ctx, []string{status.ID}, "")
	if err != nil {
		return nil, err
	}
	status.Poll = polls[status.ID]
	if input.MediaIDs == nil {
		media, err := s.store.GetMediaForStatus(ctx, status.ID)
		if err != nil {
			return nil, err
		}
		input.MediaIDs = make([]string, len(media))
		for i := range media {
			input.MediaIDs[i] = media[i].ID
		}
	}

	input.Content = strings.TrimSpace(input.Content)
	input.ContentType = content.NormalizeContentType(input.ContentType)
	if input.Content == "" && len(input.MediaIDs) == 0 {
		return nil, fmt.Errorf("status is empty")
	}
	if input.Language == "" {
		input.Language = status.Language
	}
	if status.Account == nil {
		status.Account = account
	}
	changeBlog := input.IsBlogPost != nil && *input.IsBlogPost != status.IsBlogPost
	if changeBlog {
		if *input.IsBlogPost && !s.blogAllAccounts && !account.BlogAllowed {
			return nil, fmt.Errorf("blog posting not allowed")
		}
		if *input.IsBlogPost && status.Visibility != models.VisibilityPublic {
			return nil, fmt.Errorf("blog posts must be public")
		}
		if *input.IsBlogPost && status.InReplyToID != nil && *status.InReplyToID != "" {
			return nil, fmt.Errorf("blog posts cannot be replies")
		}
		if *input.IsBlogPost && status.BlogSlug == "" {
			status.BlogSlug = s.customBlogSlug(ctx, account.ID, status.ID, input.BlogSlug)
		}
		status.IsBlogPost = *input.IsBlogPost
	}
	oldMentions, err := s.store.GetMentionsForStatus(ctx, status.ID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	status.ContentHTML = renderInputHTML(input.Content, input.ContentType)
	status.ContentSource = input.Content
	status.ContentType = input.ContentType
	status.Summary = template.HTMLEscapeString(strings.TrimSpace(input.Summary))
	status.Sensitive = input.Sensitive || strings.TrimSpace(input.Summary) != ""
	status.Language = input.Language
	status.EditedAt = &now
	status.UpdatedAt = now

	// Re-sync hashtags so tag pages and counts track the edited content.
	var tags []*models.Tag
	for _, name := range content.ExtractHashtags(input.Content) {
		tagObj, err := s.store.GetOrCreateTag(ctx, name)
		if err != nil {
			return nil, err
		}
		tags = append(tags, tagObj)
	}
	status.Tags = make([]models.Tag, 0, len(tags))
	for _, tag := range tags {
		status.Tags = append(status.Tags, *tag)
	}
	mentions := s.extractInlineMentions(ctx, input.Content, status.ID, account)
	status.Mentions = make([]models.Mention, 0, len(mentions))
	for _, mention := range mentions {
		status.Mentions = append(status.Mentions, *mention)
	}
	mentionURLs := make(map[string]string, len(mentions))
	for _, m := range mentions {
		mentionURLs[m.Acct] = m.URL
	}
	status.ContentHTML = content.ResolveSocialLinks(status.ContentHTML, "https://"+s.domain, mentionURLs)
	status.MediaAttachments = make([]models.MediaAttachment, 0, len(input.MediaIDs))
	seenMedia := make(map[string]bool, len(input.MediaIDs))
	for _, id := range input.MediaIDs {
		if seenMedia[id] {
			return nil, storage.ErrInvalidMedia
		}
		seenMedia[id] = true
		media, err := s.store.GetMediaAttachment(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.ErrInvalidMedia
		}
		if err != nil {
			return nil, err
		}
		if media.AccountID != account.ID || (media.StatusID != nil && *media.StatusID != status.ID) {
			return nil, storage.ErrInvalidMedia
		}
		if description, ok := input.MediaDescriptions[id]; ok {
			media.Description = strings.TrimSpace(description)
		}
		status.MediaAttachments = append(status.MediaAttachments, *media)
	}
	var jobs []*models.Job
	queueDelivery := func(inbox string, activity map[string]interface{}) error {
		job, err := federation.PrepareDelivery(ctx, s.store, inbox, activity)
		if err != nil {
			return err
		}
		jobs = append(jobs, job)
		return nil
	}

	if input.ExpiresIn != nil {
		if *input.ExpiresIn > 0 {
			expiresAt := time.Now().Add(*input.ExpiresIn)
			status.ExpiresAt = &expiresAt
		} else {
			status.ExpiresAt = nil
		}

		if status.ExpiresAt != nil {
			payload, _ := json.Marshal(map[string]string{"status_id": status.ID})
			jobs = append(jobs, &models.Job{Kind: models.StatusExpireJobKind, Payload: string(payload), RunAt: *status.ExpiresAt, MaxAttempts: 10, State: "queued"})
		}
	}

	// Federate the edit as an Update activity (non-local visibilities only).
	if status.Visibility != models.VisibilityLocal && status.Visibility != models.VisibilityLocalUnlisted {
		activity := s.updateActivity(status, account)
		if status.Visibility == models.VisibilityDirect {
			current := make(map[string]bool, len(mentions))
			for _, m := range mentions {
				current[m.AccountID] = true
				mentioned, err := s.store.GetAccount(ctx, m.AccountID)
				if err != nil {
					return nil, err
				}
				if mentioned.Domain != nil {
					if err := queueDelivery(remoteInbox(mentioned), activity); err != nil {
						return nil, err
					}
				}
			}
			retract := map[string]interface{}{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       status.URI + "/delete/" + uuid.NewString(),
				"type":     "Delete",
				"actor":    account.ActorURI,
				"object": map[string]interface{}{
					"type":       "Tombstone",
					"id":         status.URI,
					"formerType": "Note",
					"deleted":    time.Now().Format(time.RFC3339),
				},
			}
			for _, old := range oldMentions {
				if current[old.AccountID] {
					continue
				}
				removed, err := s.store.GetAccount(ctx, old.AccountID)
				if err != nil {
					return nil, err
				}
				if removed.Domain != nil {
					if err := queueDelivery(remoteInbox(removed), retract); err != nil {
						return nil, err
					}
				}
			}
		} else {
			rebloggers, err := s.store.ListReblogAccounts(ctx, status.ID, -1)
			if err != nil {
				return nil, err
			}
			var extraInboxes []string
			for _, reblogger := range rebloggers {
				if inbox := remoteInbox(reblogger); inbox != "" {
					extraInboxes = append(extraInboxes, inbox)
				}
			}
			for _, mention := range mentions {
				mentioned, err := s.store.GetAccount(ctx, mention.AccountID)
				if err != nil {
					return nil, err
				}
				if inbox := remoteInbox(mentioned); inbox != "" {
					extraInboxes = append(extraInboxes, inbox)
				}
			}
			body, err := json.Marshal(activity)
			if err != nil {
				return nil, err
			}
			deliveries, err := federation.PrepareFollowerDeliveries(ctx, s.store, account.ID, body, extraInboxes...)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, deliveries...)
		}
	}

	if err := s.store.EditStatusWithJobs(ctx, status, expected, jobs); err != nil {
		return nil, err
	}
	s.invalidateStatusAndViewer(account.ID, statusID)
	for _, m := range oldMentions {
		s.InvalidateViewerStatus(m.AccountID, statusID)
		s.invalidatePages(m.AccountID)
	}
	for _, m := range mentions {
		s.InvalidateViewerStatus(m.AccountID, statusID)
		s.invalidatePages(m.AccountID)
	}

	// Stream the edit to live timelines.
	if s.stream != nil {
		s.EnrichStatus(ctx, status, nil)
		s.stream.StatusUpdated(ctx, status)
	}

	return status, nil
}

func (s *Service) ToggleFavourite(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	if existing, _ := s.store.GetFavourite(ctx, account.ID, statusID); existing != nil {
		return s.Unfavourite(ctx, account, statusID)
	}
	return s.Favourite(ctx, account, statusID)
}

func (s *Service) VotePoll(ctx context.Context, account *models.Account, pollID string, choices []int) (*models.Poll, error) {
	poll, err := s.store.GetPoll(ctx, pollID, account.ID)
	if err != nil {
		return nil, err
	}
	status, err := s.store.GetStatus(ctx, poll.StatusID)
	if err != nil || !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("poll not found")
	}
	if err := s.requireFederationTarget(ctx, status.AccountID); err != nil {
		return nil, err
	}
	poll, err = s.store.VotePoll(ctx, pollID, account.ID, choices)
	if err != nil {
		return nil, err
	}
	s.InvalidateStatusComponents(status.ID)
	s.InvalidateViewerStatus(account.ID, status.ID)
	author, err := s.store.GetAccount(ctx, status.AccountID)
	if err == nil && author.Domain != nil {
		for _, choice := range choices {
			activityID := "https://" + s.domain + "/poll-votes/" + uuid.NewString()
			s.deliverToAccount(ctx, account, author, map[string]any{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       activityID,
				"type":     "Create",
				"actor":    account.ActorURI,
				"to":       []string{author.ActorURI},
				"object": map[string]any{
					"id":           activityID + "/note",
					"type":         "Note",
					"attributedTo": account.ActorURI,
					"inReplyTo":    status.URI,
					"name":         poll.Options[choice].Title,
					"to":           []string{author.ActorURI},
				},
			})
		}
	}
	return poll, nil
}

func (s *Service) Favourite(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.InvalidateStatusComponents(statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	if err := s.requireFederationTarget(ctx, status.AccountID); err != nil {
		return nil, err
	}
	fave := &models.Favourite{
		AccountID: account.ID,
		StatusID:  statusID,
		URI:       fmt.Sprintf("https://%s/users/%s/likes/%s", s.domain, account.Username, statusID),
	}
	notif, jobs, err := s.interactionNotification(ctx, "favourite", status, account)
	if err != nil {
		return nil, err
	}
	notified, err := s.store.CreateFavouriteWithNotification(ctx, fave, notif, jobs)
	if err != nil {
		return nil, err
	}
	if notified && notif != nil {
		s.streamNotification(ctx, notif)
	}

	// Only federate the like for non-local statuses
	if notified && status.Visibility != models.VisibilityLocal && status.Visibility != models.VisibilityLocalUnlisted {
		s.deliverToStatusAuthor(ctx, account, status, s.likeActivity(account, status, fave.URI))
	}
	return s.store.GetStatus(ctx, statusID)
}

func (s *Service) Unfavourite(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.InvalidateStatusComponents(statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	if existing, _ := s.store.GetFavourite(ctx, account.ID, statusID); existing == nil {
		return status, nil
	}
	if err := s.store.DeleteFavourite(ctx, account.ID, statusID); err != nil {
		return nil, err
	}
	// Only federate the unlike for non-local statuses
	if status.Visibility != models.VisibilityLocal && status.Visibility != models.VisibilityLocalUnlisted {
		s.deliverToStatusAuthor(ctx, account, status, s.undoLikeActivity(account, status))
	}
	return s.store.GetStatus(ctx, statusID)
}

func (s *Service) ToggleEmojiReaction(ctx context.Context, account *models.Account, statusID, value string) (*models.Status, error) {
	_, key, _, _, err := s.resolveEmojiReaction(ctx, value)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetEmojiReaction(ctx, account.ID, statusID, key); err == nil {
		return s.RemoveEmojiReaction(ctx, account, statusID, value)
	}
	return s.AddEmojiReaction(ctx, account, statusID, value)
}

func (s *Service) AddEmojiReaction(ctx context.Context, account *models.Account, statusID, value string) (*models.Status, error) {
	defer s.invalidateStatusAndViewer(account.ID, statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil || !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	content, key, custom, display, err := s.resolveEmojiReaction(ctx, value)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetEmojiReaction(ctx, account.ID, statusID, key); err == nil {
		return status, nil
	}
	if err := s.requireFederationTarget(ctx, status.AccountID); err != nil {
		return nil, err
	}
	reaction := &models.EmojiReaction{
		URI:       fmt.Sprintf("https://%s/users/%s/emoji_reactions/%s", s.domain, account.Username, uuid.NewString()),
		AccountID: account.ID,
		StatusID:  status.ID,
		Content:   content,
		Key:       key,
	}
	if custom != nil {
		reaction.EmojiID = &custom.ID
	}
	created, err := s.store.CreateEmojiReaction(ctx, reaction)
	if err != nil {
		return nil, err
	}
	if created {
		s.notifyStatusAuthor(ctx, "emoji_reaction", status, account, display)
		s.federateEmojiReaction(ctx, account, status, reaction, custom, false)
	}
	return s.store.GetStatus(ctx, statusID)
}

func (s *Service) RemoveEmojiReaction(ctx context.Context, account *models.Account, statusID, value string) (*models.Status, error) {
	defer s.invalidateStatusAndViewer(account.ID, statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil || !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	_, key, custom, _, err := s.resolveEmojiReaction(ctx, value)
	if err != nil {
		return nil, err
	}
	reaction, err := s.store.DeleteEmojiReaction(ctx, account.ID, statusID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return nil, err
	}
	s.federateEmojiReaction(ctx, account, status, reaction, custom, true)
	return s.store.GetStatus(ctx, statusID)
}

func (s *Service) resolveEmojiReaction(ctx context.Context, value string) (content, key string, custom *models.CustomEmoji, display string, err error) {
	value = strings.TrimSpace(value)
	if emoji.Known(value) {
		return value, "unicode:" + value, nil, value, nil
	}
	shortcode, domain, ok := emoji.ParseCustom(value)
	if !ok {
		return "", "", nil, "", fmt.Errorf("invalid emoji reaction")
	}
	emojis, err := s.store.GetEmojisByShortcodes(ctx, []string{shortcode}, &domain)
	if err != nil || len(emojis) != 1 {
		return "", "", nil, "", fmt.Errorf("unknown custom emoji")
	}
	custom = emojis[0]
	return ":" + shortcode + ":", "custom:" + custom.ID, custom, value, nil
}

func (s *Service) federateEmojiReaction(ctx context.Context, account *models.Account, status *models.Status, reaction *models.EmojiReaction, custom *models.CustomEmoji, undo bool) {
	if status.Visibility == models.VisibilityLocal || status.Visibility == models.VisibilityLocalUnlisted {
		return
	}
	author, err := s.store.GetAccount(ctx, status.AccountID)
	if err != nil {
		return
	}
	activity := s.emojiReactionActivity(account, author, status, reaction, custom)
	if undo {
		activity = map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id":       reaction.URI + "/undo",
			"type":     "Undo",
			"actor":    account.ActorURI,
			"object":   reaction.URI,
			"to":       []string{account.FollowersURI, author.ActorURI},
		}
	}
	extraInbox := ""
	if author.Domain != nil {
		extraInbox = author.InboxURI
		if author.SharedInboxURI != nil && *author.SharedInboxURI != "" {
			extraInbox = *author.SharedInboxURI
		}
	}
	s.deliverToFollowersAnd(ctx, account.ID, activity, extraInbox)
}

func (s *Service) emojiReactionActivity(account, author *models.Account, status *models.Status, reaction *models.EmojiReaction, custom *models.CustomEmoji) map[string]interface{} {
	context := []interface{}{"https://www.w3.org/ns/activitystreams", map[string]string{"litepub": "http://litepub.social/ns#", "EmojiReact": "litepub:EmojiReact"}}
	activity := map[string]interface{}{
		"@context": context,
		"id":       reaction.URI,
		"type":     "EmojiReact",
		"actor":    account.ActorURI,
		"object":   status.URI,
		"content":  reaction.Content,
		"to":       []string{account.FollowersURI, author.ActorURI},
	}
	if custom != nil {
		customURL := custom.RemoteURL
		if customURL == "" {
			customURL = custom.URL
		}
		context[1] = map[string]string{"litepub": "http://litepub.social/ns#", "EmojiReact": "litepub:EmojiReact", "toot": "http://joinmastodon.org/ns#", "Emoji": "toot:Emoji"}
		activity["tag"] = []map[string]interface{}{{
			"id": customURL, "type": "Emoji", "name": reaction.Content,
			"icon": map[string]interface{}{"type": "Image", "url": customURL},
		}}
	}
	return activity
}

func (s *Service) ToggleReblog(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	if existing, _ := s.store.GetReblogByAccountStatus(ctx, account.ID, statusID); existing != nil {
		return s.Unreblog(ctx, account, statusID)
	}
	return s.Reblog(ctx, account, statusID)
}

func (s *Service) Reblog(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.invalidateStatusAndViewer(account.ID, statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	if status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted {
		return nil, fmt.Errorf("cannot reblog a non-public post")
	}
	originalID := statusID
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		originalID = *status.BoostOfID
	}
	existing, err := s.store.GetReblogByAccountStatus(ctx, account.ID, originalID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := s.requireFederationTarget(ctx, status.AccountID); err != nil {
		return nil, err
	}
	now := time.Now()
	boostID := storage.NewStatusID(now)
	boost := &models.Status{
		ID:         boostID,
		URI:        fmt.Sprintf("https://%s/objects/%s", s.domain, boostID),
		URL:        fmt.Sprintf("https://%s/@%s/%s", s.domain, account.Username, boostID),
		AccountID:  account.ID,
		Account:    account,
		BoostOfID:  &originalID,
		Visibility: status.Visibility,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	original := status
	if originalID != status.ID {
		if original, err = s.store.GetStatus(ctx, originalID); err != nil {
			return nil, err
		}
	}
	notif, jobs, err := s.interactionNotification(ctx, "reblog", original, account)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		boost = existing
	} else {
		jobs = append(jobs, deliveryFanoutJob(account.ID, federation.BuildAnnounceAS2(s.domain, boost, account, original.URI)))
	}
	notified, err := s.store.CreateBoostWithNotification(ctx, boost, notif, jobs)
	if err != nil {
		return nil, err
	}
	if notified && notif != nil {
		s.streamNotification(ctx, notif)
	}

	return s.store.GetStatus(ctx, originalID)
}

func (s *Service) Unreblog(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	visible := s.CanViewStatus(ctx, status, account)
	originalID := statusID
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		originalID = *status.BoostOfID
	}
	existing, _ := s.store.GetReblogByAccountStatus(ctx, account.ID, originalID)
	if existing == nil {
		if !visible {
			return nil, fmt.Errorf("status not found")
		}
		return s.store.GetStatus(ctx, originalID)
	}
	if err := s.DeleteStatus(ctx, account, existing.ID); err != nil {
		return nil, err
	}
	if !visible {
		return nil, fmt.Errorf("status not found")
	}
	return s.store.GetStatus(ctx, originalID)
}

func (s *Service) ToggleBookmark(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	if existing, _ := s.store.GetBookmark(ctx, account.ID, statusID); existing != nil {
		return s.Unbookmark(ctx, account, statusID)
	}
	return s.Bookmark(ctx, account, statusID)
}

func (s *Service) Bookmark(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.invalidateViewerStatusAndPages(account.ID, statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	if existing, _ := s.store.GetBookmark(ctx, account.ID, statusID); existing == nil {
		if err := s.store.CreateBookmark(ctx, &models.Bookmark{AccountID: account.ID, StatusID: statusID}); err != nil {
			return nil, err
		}
	}
	return status, nil
}

func (s *Service) Unbookmark(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.invalidateViewerStatusAndPages(account.ID, statusID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteBookmark(ctx, account.ID, statusID); err != nil {
		return nil, err
	}
	if !s.CanViewStatus(ctx, status, account) {
		return nil, fmt.Errorf("status not found")
	}
	return status, nil
}

func (s *Service) TogglePin(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if status.AccountID != account.ID {
		return nil, fmt.Errorf("cannot pin another user's post")
	}
	if status.Pinned {
		return s.UnpinStatus(ctx, account, statusID)
	}
	return s.PinStatus(ctx, account, statusID)
}

func (s *Service) PinStatus(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.invalidatePages(account.ID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if status.AccountID != account.ID {
		return nil, fmt.Errorf("cannot pin another user's post")
	}
	if status.Visibility == models.VisibilityPrivate || status.Visibility == models.VisibilityDirect {
		return nil, fmt.Errorf("cannot pin a non-public post")
	}
	if status.BoostOfID != nil {
		return nil, fmt.Errorf("cannot pin a boost")
	}
	if err := s.store.PinStatus(ctx, statusID); err != nil {
		return nil, err
	}
	status.Pinned = true

	// Federate the pin via Add activity to the featured collection
	if account.Domain == nil && s.federator != nil {
		s.deliverToFollowers(ctx, account.ID, map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id":       fmt.Sprintf("%s#pins/%s", account.ActorURI, statusID),
			"type":     "Add",
			"actor":    account.ActorURI,
			"object":   status.URI,
			"target":   fmt.Sprintf("https://%s/users/%s/collections/featured", s.domain, account.Username),
		})
	}

	return status, nil
}

func (s *Service) UnpinStatus(ctx context.Context, account *models.Account, statusID string) (*models.Status, error) {
	defer s.invalidatePages(account.ID)
	status, err := s.store.GetStatus(ctx, statusID)
	if err != nil {
		return nil, err
	}
	if status.AccountID != account.ID {
		return nil, fmt.Errorf("cannot unpin another user's post")
	}
	if err := s.store.UnpinStatus(ctx, statusID); err != nil {
		return nil, err
	}
	status.Pinned = false

	// Federate the unpin via Remove activity from the featured collection
	if account.Domain == nil && s.federator != nil {
		s.deliverToFollowers(ctx, account.ID, map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id":       fmt.Sprintf("%s#pins/%s/undo", account.ActorURI, statusID),
			"type":     "Remove",
			"actor":    account.ActorURI,
			"object":   status.URI,
			"target":   fmt.Sprintf("https://%s/users/%s/collections/featured", s.domain, account.Username),
		})
	}

	return status, nil
}

func (s *Service) Follow(ctx context.Context, account *models.Account, targetID string) (*models.Follow, error) {
	defer s.invalidatePages(account.ID)
	target, err := s.store.GetAccount(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if blocked, _ := s.store.BlockExists(ctx, target.ID, account.ID); blocked {
		return nil, fmt.Errorf("cannot follow: blocked by target account")
	}
	if existing, _ := s.store.GetFollowByPair(ctx, account.ID, targetID); existing != nil {
		return existing, nil
	}
	if target.Domain != nil {
		allowed, reason, err := s.store.FederationDomainAllowed(ctx, *target.Domain)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, &federation.FederationPolicyError{Domain: *target.Domain, Reason: reason}
		}
	}
	follow := &models.Follow{
		AccountID:       account.ID,
		TargetAccountID: target.ID,
		URI:             fmt.Sprintf("https://%s/follows/%s", s.domain, uuid.New().String()),
		State:           models.FollowAccepted,
	}
	if target.Domain != nil || target.ManuallyApprovesFollowers {
		follow.State = models.FollowPending
	}
	if err := s.store.CreateFollow(ctx, follow); err != nil {
		return nil, err
	}
	notifType := "follow"
	if follow.State == models.FollowPending {
		notifType = "follow_request"
	}
	s.notifyRelationship(ctx, notifType, target, account)
	s.deliverToAccount(ctx, account, target, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       follow.URI,
		"type":     "Follow",
		"actor":    account.ActorURI,
		"object":   target.ActorURI,
	})
	return follow, nil
}

func (s *Service) requireFederationTarget(ctx context.Context, accountID string) error {
	target, err := s.store.GetAccount(ctx, accountID)
	if err != nil || target.Domain == nil {
		return err
	}
	allowed, reason, err := s.store.FederationDomainAllowed(ctx, *target.Domain)
	if err != nil {
		return err
	}
	if !allowed {
		return &federation.FederationPolicyError{Domain: *target.Domain, Reason: reason}
	}
	return nil
}

func (s *Service) Unfollow(ctx context.Context, account *models.Account, targetID string) error {
	defer s.invalidatePages(account.ID)
	target, err := s.store.GetAccount(ctx, targetID)
	if err != nil {
		return err
	}
	follow, err := s.store.GetFollowByPair(ctx, account.ID, targetID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteFollow(ctx, follow.ID); err != nil {
		return err
	}
	if follow.State == models.FollowPending {
		s.invalidatePages(target.ID)
		if err := s.store.DeleteNotifications(ctx, target.ID, account.ID, "follow_request"); err != nil {
			slog.Warn("app: failed to clear follow request notification", "error", err)
		}
	}
	s.notifyRelationship(ctx, "unfollow", target, account)
	s.deliverToAccount(ctx, account, target, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       follow.URI + "/undo",
		"type":     "Undo",
		"actor":    account.ActorURI,
		"object": map[string]interface{}{
			"id":     follow.URI,
			"type":   "Follow",
			"actor":  account.ActorURI,
			"object": target.ActorURI,
		},
	})
	return nil
}

// AnswerFollowRequest accepts or rejects a pending follow request addressed to
// account. id is either the follow's own ID or the requesting account's ID (the
// Mastodon API passes the latter). Every caller, API and web UI, goes through
// here so the answer is always federated back to a remote requester, which a
// bare store.AcceptFollow never was.
func (s *Service) AnswerFollowRequest(ctx context.Context, account *models.Account, id string, accept bool) error {
	follow, err := s.store.GetFollow(ctx, id)
	if err != nil {
		follow, err = s.store.GetFollowByPair(ctx, id, account.ID)
	}
	if err != nil || follow.TargetAccountID != account.ID || follow.State != models.FollowPending {
		return fmt.Errorf("follow request not found")
	}
	requester, err := s.store.GetAccount(ctx, follow.AccountID)
	if err != nil {
		return err
	}
	defer s.invalidatePages(account.ID)
	defer s.invalidatePages(requester.ID)

	activityType := "Reject"
	notifType := "follow_reject"
	if accept {
		activityType = "Accept"
		notifType = "follow_accept"
		err = s.store.AcceptFollow(ctx, follow.ID)
	} else {
		err = s.store.RejectFollow(ctx, follow.ID)
	}
	if err != nil {
		return err
	}

	// The request is settled: drop its notification so the row stops asking.
	if err := s.store.DeleteNotifications(ctx, account.ID, requester.ID, "follow_request"); err != nil {
		slog.Warn("app: failed to clear follow request notification", "error", err)
	}
	s.notifyRelationship(ctx, notifType, requester, account)

	// Queue rather than fire-and-forget: a lost Accept leaves the requester
	// pending forever on their side, with nothing to retry it.
	if inbox := remoteInbox(requester); inbox != "" {
		if err := federation.EnqueueDelivery(ctx, s.store, inbox, map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id":       account.ActorURI + "#" + strings.ToLower(activityType) + "s/" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(follow.URI)).String(),
			"type":     activityType,
			"actor":    account.ActorURI,
			"to":       []string{requester.ActorURI},
			"object": map[string]interface{}{
				"id":     follow.URI,
				"type":   "Follow",
				"actor":  requester.ActorURI,
				"object": account.ActorURI,
			},
		}); err != nil {
			slog.Error("app: failed to queue follow request answer", "type", activityType, "error", err)
		}
	}
	return nil
}

// remoteInbox returns the inbox to deliver an account-targeted activity to, or
// "" for local accounts (which need no delivery at all).
func remoteInbox(account *models.Account) string {
	if account.Domain == nil {
		return ""
	}
	if account.SharedInboxURI != nil && *account.SharedInboxURI != "" {
		return *account.SharedInboxURI
	}
	return account.InboxURI
}

func (s *Service) notifyRelationship(ctx context.Context, notifType string, account, from *models.Account) {
	if account == nil || from == nil || account.ID == from.ID || account.Domain != nil || account.IsServiceActor {
		return
	}
	if !account.AllowsRelationshipNotification(notifType, s.blockSensitiveRelationshipNotifications) {
		return
	}
	notif := &models.Notification{Type: notifType, AccountID: account.ID, FromAccountID: from.ID}
	if err := s.store.UpsertNotification(ctx, notif); err != nil {
		slog.Warn("app: failed to create relationship notification", "type", notifType, "error", err)
		return
	}
	s.streamNotification(ctx, notif)
	push.EnqueuePushNotification(ctx, s.store, notif)
	mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
}

// SetFollowNotify toggles the notify "bell" on an existing follow so the account
// receives a status notification whenever the target publishes a new post. It is
// a local-only preference and requires an existing follow.
func (s *Service) SetFollowNotify(ctx context.Context, account *models.Account, targetID string, notify bool) error {
	follow, err := s.store.GetFollowByPair(ctx, account.ID, targetID)
	if err != nil {
		return fmt.Errorf("not following")
	}
	if follow.Notify == notify {
		return nil
	}
	return s.store.SetFollowNotify(ctx, account.ID, targetID, notify)
}

// notifyBellFollowers creates a "status" notification for every local follower
// who enabled the notify bell on this author. Only top-level, non-direct posts
// trigger it, matching the "notify me when they post" semantics (replies and
// direct messages are excluded to avoid noise).
func (s *Service) notifyBellFollowers(ctx context.Context, status *models.Status) {
	if status == nil || status.InReplyToID != nil || status.Visibility == models.VisibilityDirect {
		return
	}
	followerIDs, err := s.store.ListNotifyFollowerIDs(ctx, status.AccountID)
	if err != nil {
		slog.Error("app: list notify followers failed", "error", err)
		return
	}
	for start := 0; start < len(followerIDs); start += 100 {
		end := min(start+100, len(followerIDs))
		batch := make([]*models.Notification, 0, end-start)
		for _, fid := range followerIDs[start:end] {
			batch = append(batch, &models.Notification{
				Type: "status", AccountID: fid, FromAccountID: status.AccountID, StatusID: &status.ID,
			})
		}
		if err := s.store.UpsertNotifications(ctx, batch); err != nil {
			slog.Warn("app: failed to create bell notification batch", "size", len(batch), "error", err)
			continue
		}
		for _, notif := range batch {
			s.streamNotification(ctx, notif)
			mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
		}
		push.EnqueuePushNotifications(ctx, s.store, batch)
	}
}

// Block creates a block from account to targetID, tears down any follow
// relationship in both directions and, for a remote target, federates a Block
// activity so the remote server enforces it too.
func (s *Service) Block(ctx context.Context, account *models.Account, targetID string) error {
	defer s.invalidatePages(account.ID)
	if targetID == account.ID {
		return fmt.Errorf("cannot block yourself")
	}
	target, err := s.store.GetAccount(ctx, targetID)
	if err != nil {
		return err
	}

	if err := s.store.CreateBlock(ctx, &models.Block{AccountID: account.ID, TargetAccountID: targetID}); err != nil {
		return err
	}

	// A block implicitly tears down both follow directions.
	if follow, err := s.store.GetFollowByPair(ctx, account.ID, targetID); err == nil && follow != nil {
		_ = s.store.DeleteFollow(ctx, follow.ID)
	}
	if follow, err := s.store.GetFollowByPair(ctx, targetID, account.ID); err == nil && follow != nil {
		_ = s.store.DeleteFollow(ctx, follow.ID)
	}
	s.notifyRelationship(ctx, "block", target, account)

	s.deliverToAccount(ctx, account, target, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       fmt.Sprintf("%s#blocks/%s", account.ActorURI, target.ID),
		"type":     "Block",
		"actor":    account.ActorURI,
		"object":   target.ActorURI,
	})
	return nil
}

// Unblock removes the block and, for a remote target, federates an Undo(Block).
func (s *Service) Unblock(ctx context.Context, account *models.Account, targetID string) error {
	defer s.invalidatePages(account.ID)
	target, err := s.store.GetAccount(ctx, targetID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteBlock(ctx, account.ID, targetID); err != nil {
		return err
	}
	s.notifyRelationship(ctx, "unblock", target, account)
	blockID := fmt.Sprintf("%s#blocks/%s", account.ActorURI, target.ID)
	s.deliverToAccount(ctx, account, target, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       blockID + "/undo",
		"type":     "Undo",
		"actor":    account.ActorURI,
		"object": map[string]interface{}{
			"id":     blockID,
			"type":   "Block",
			"actor":  account.ActorURI,
			"object": target.ActorURI,
		},
	})
	return nil
}

// Mute hides a target's content (and optionally notifications) from the account.
// Mutes are local-only and never federated. A non-nil durationMinutes makes the
// mute expire after that many minutes.
func (s *Service) Mute(ctx context.Context, account *models.Account, targetID string, hideNotifications bool, durationMinutes *int) error {
	defer s.invalidatePages(account.ID)
	if targetID == account.ID {
		return fmt.Errorf("cannot mute yourself")
	}
	if _, err := s.store.GetAccount(ctx, targetID); err != nil {
		return err
	}
	// Replace any existing mute so the new options/duration take effect.
	_ = s.store.DeleteMute(ctx, account.ID, targetID)
	return s.store.CreateMute(ctx, &models.Mute{
		AccountID:         account.ID,
		TargetAccountID:   targetID,
		HideNotifications: hideNotifications,
		Duration:          durationMinutes,
	})
}

// Unmute removes a mute. Local-only, never federated.
func (s *Service) Unmute(ctx context.Context, account *models.Account, targetID string) error {
	defer s.invalidatePages(account.ID)
	return s.store.DeleteMute(ctx, account.ID, targetID)
}

// ModerateAccount applies an admin moderation action to target and records it in
// the audit log. Actions:
//
//	suspend - hide all content everywhere and block login; for a local account
//	          also tombstone it and federate a Delete(Person) to followers.
//	silence - keep content visible to followers but out of public/federated
//	          timelines (local-only; federation is unchanged).
//	quiet   - like silence but also downgrades federation to followers-only
//	          so the account does not reach new servers.
//	disable - block local login only; content stays visible (local accounts only).
//	none    - clear any moderation state.
func (s *Service) ModerateAccount(ctx context.Context, admin, target *models.Account, action, note string) error {
	isLocal := target.Domain == nil
	switch action {
	case "suspend", "silence", "quiet", "disable", "none":
	default:
		return fmt.Errorf("%w: unknown action %q", ErrModerationRejected, action)
	}
	if action == "disable" && !isLocal {
		return fmt.Errorf("%w: cannot disable a remote account", ErrModerationRejected)
	}
	if target.IsAdmin && (action == "suspend" || action == "disable") {
		admins, err := s.store.GetAdmins(ctx)
		if err != nil {
			return err
		}
		anotherActive := false
		for _, candidate := range admins {
			if candidate.ID != target.ID && candidate.CanAuthenticate(false) {
				anotherActive = true
				break
			}
		}
		if !anotherActive {
			return fmt.Errorf("%w: the last active administrator cannot be suspended or disabled", ErrModerationRejected)
		}
	}

	if action == "none" {
		note = ""
	}
	target.ModerationNote = note

	switch action {
	case "suspend":
		now := time.Now()
		target.ModerationState = "suspended"
		target.SuspendedAt = &now
		if err := s.store.UpdateAccount(ctx, target); err != nil {
			return err
		}
		_ = s.store.CreateTombstone(ctx, &models.Tombstone{
			URI:        target.ActorURI,
			FormerType: "Person",
			DeletedAt:  now,
		})
		if isLocal && s.federator != nil {
			s.deliverToFollowers(ctx, target.ID, map[string]interface{}{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       target.ActorURI + "#delete",
				"type":     "Delete",
				"actor":    target.ActorURI,
				"object":   target.ActorURI,
				"to":       []string{"https://www.w3.org/ns/activitystreams#Public"},
			})
		}
	case "silence":
		target.ModerationState = "silenced"
		target.SuspendedAt = nil
		if err := s.store.UpdateAccount(ctx, target); err != nil {
			return err
		}
	case "quiet":
		target.ModerationState = "quiet"
		target.SuspendedAt = nil
		if err := s.store.UpdateAccount(ctx, target); err != nil {
			return err
		}
	case "disable":
		target.ModerationState = "disabled"
		target.SuspendedAt = nil
		if err := s.store.UpdateAccount(ctx, target); err != nil {
			return err
		}
	case "none":
		target.ModerationState = ""
		target.SuspendedAt = nil
		if err := s.store.UpdateAccount(ctx, target); err != nil {
			return err
		}
	}
	if action == "none" || action == "silence" || action == "quiet" {
		if err := s.store.DeleteTombstoneByURI(ctx, target.ActorURI); err != nil {
			return err
		}
	}
	if isLocal && (action == "suspend" || action == "disable") {
		if err := s.store.DeleteSessionsForAccount(ctx, target.ID); err != nil {
			slog.Warn("app: failed to revoke moderated account sessions", "account_id", target.ID, "error", err)
		}
		if err := s.store.RevokeOAuthTokensForAccount(ctx, target.ID); err != nil {
			slog.Warn("app: failed to revoke moderated account OAuth tokens", "account_id", target.ID, "error", err)
		}
	}

	adminID := ""
	if admin != nil {
		adminID = admin.ID
	}
	if err := s.store.CreateModerationLog(ctx, &models.ModerationLog{
		AdminAccountID: adminID,
		TargetType:     "account",
		TargetID:       target.ID,
		Action:         action,
		Note:           note,
	}); err != nil {
		slog.Warn("app: failed to write moderation log", "error", err)
	}
	return nil
}

// CreateReport files a moderation report against targetID, notifies admins, and
// - when forward is set and the target is remote - federates a Flag to the
// target's home instance. The Flag is sent from the instance actor (not the
// reporter) to match Mastodon and avoid leaking the reporter's identity.
func (s *Service) CreateReport(ctx context.Context, reporter *models.Account, targetID string, statusIDs []string, comment, category string, forward bool) (*models.Report, error) {
	target, err := s.store.GetAccount(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if reporter != nil && reporter.ID == targetID {
		return nil, fmt.Errorf("cannot report yourself")
	}
	if category == "" {
		category = "other"
	}
	switch category {
	case "spam", "violation", "legal", "other":
	default:
		return nil, fmt.Errorf("invalid report category %q", category)
	}
	for _, id := range statusIDs {
		status, err := s.store.GetStatus(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reported status %q does not exist", id)
		}
		if status.AccountID != targetID {
			return nil, fmt.Errorf("reported status %q belongs to another account", id)
		}
		if !s.CanViewStatus(ctx, status, reporter) {
			return nil, fmt.Errorf("reported status %q is not visible", id)
		}
	}

	report := &models.Report{
		TargetAccountID: targetID,
		StatusIDs:       strings.Join(statusIDs, ","),
		Comment:         comment,
		Category:        category,
		Forwarded:       forward && target.Domain != nil,
		State:           "open",
	}
	if reporter != nil {
		report.AccountID = &reporter.ID
	}
	if err := s.store.CreateReport(ctx, report); err != nil {
		return nil, err
	}

	s.notifyAdminsOfReport(ctx, reporter, target)

	if report.Forwarded && s.federator != nil {
		s.forwardReport(ctx, target, statusIDs, comment)
	}

	return report, nil
}

func (s *Service) ResolveReport(ctx context.Context, admin *models.Account, id, action string) error {
	if action == "" {
		action = "none"
	}
	switch action {
	case "none", "silence", "quiet", "suspend", "disable":
	default:
		return fmt.Errorf("%w: unknown report action %q", ErrModerationRejected, action)
	}
	report, err := s.store.GetReport(ctx, id)
	if err != nil {
		return err
	}
	if report.State != "open" {
		return fmt.Errorf("%w: report is already resolved", ErrModerationRejected)
	}
	if action != "none" {
		target, err := s.store.GetAccount(ctx, report.TargetAccountID)
		if err != nil {
			return err
		}
		if err := s.ModerateAccount(ctx, admin, target, action, "via report "+id); err != nil {
			return err
		}
	}
	adminID := ""
	if admin != nil {
		adminID = admin.ID
	}
	if err := s.store.ResolveReport(ctx, id, adminID, action); err != nil {
		return err
	}
	if err := s.store.CreateModerationLog(ctx, &models.ModerationLog{AdminAccountID: adminID, TargetType: "report", TargetID: id, Action: "resolve", Note: action}); err != nil {
		slog.Warn("app: failed to write report moderation log", "error", err)
	}
	return nil
}

// notifyAdminsOfReport sends an admin.report notification to every admin (except
// the reporter, if they happen to be an admin).
func (s *Service) notifyAdminsOfReport(ctx context.Context, reporter, target *models.Account) {
	admins, err := s.store.GetAdmins(ctx)
	if err != nil {
		slog.Warn("app: failed to list admins for report notification", "error", err)
		return
	}
	// FromAccountID must be non-null; the reported account is always known.
	from := target.ID
	if reporter != nil {
		from = reporter.ID
	}
	for _, admin := range admins {
		if reporter != nil && admin.ID == reporter.ID {
			continue
		}
		notif := &models.Notification{
			Type:          "admin.report",
			AccountID:     admin.ID,
			FromAccountID: from,
		}
		if err := s.store.UpsertNotification(ctx, notif); err != nil {
			slog.Warn("app: failed to create report notification", "admin", admin.ID, "error", err)
			continue
		}
		s.streamNotification(ctx, notif)
		push.EnqueuePushNotification(ctx, s.store, notif)
		mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
	}
}

// forwardReport delivers a Flag activity to the reported account's home instance.
func (s *Service) forwardReport(ctx context.Context, target *models.Account, statusIDs []string, comment string) {
	objects := []string{target.ActorURI}
	for _, id := range statusIDs {
		if st, err := s.store.GetStatus(ctx, id); err == nil && st != nil && st.URI != "" {
			objects = append(objects, st.URI)
		}
	}
	instanceActorURI := fmt.Sprintf("https://%s/users/%s", s.domain, s.domain)
	s.deliverToAccount(ctx, nil, target, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       fmt.Sprintf("https://%s/flags/%s", s.domain, uuid.New().String()),
		"type":     "Flag",
		"actor":    instanceActorURI,
		"content":  comment,
		"object":   objects,
	})
}

func (s *Service) UpdateProfile(ctx context.Context, account *models.Account, displayName, note string) error {
	account.DisplayName = strings.TrimSpace(displayName)
	account.NoteSource = strings.TrimSpace(note)
	// Render the bio like a Markdown post: this preserves line breaks (goldmark
	// hard-wraps single newlines into <br>) and adds links, @mentions, and
	// #hashtags. MarkdownToHTML already sanitizes, so no extra pass is needed.
	// The rendered HTML is what federates as the actor "summary".
	account.Note = s.RenderProfileNote(ctx, account, account.NoteSource)
	if err := s.store.UpdateAccount(ctx, account); err != nil {
		return err
	}
	s.InvalidateAccountComponents(account.ID)
	// Federate the profile update to followers
	if account.Domain == nil && s.federator != nil {
		s.deliverProfileUpdate(ctx, account)
	}
	return nil
}

// RenderProfileNote produces federation-safe bio HTML.
func (s *Service) RenderProfileNote(ctx context.Context, account *models.Account, source string) string {
	rendered := content.MarkdownToHTML(source)
	mentions := s.extractInlineMentions(ctx, source, "", account)
	mentionURLs := make(map[string]string, len(mentions))
	for _, mention := range mentions {
		mentionURLs[mention.Acct] = mention.URL
	}
	return content.ResolveSocialLinks(rendered, "https://"+s.domain, mentionURLs)
}

// InvalidateAccountComponents drops cached status presentations that embed an
// account, so profile edits are immediately visible on existing posts.
func (s *Service) InvalidateAccountComponents(accountID string) int {
	if s.pageCache == nil || accountID == "" {
		return 0
	}
	return s.pageCache.DeleteDependencies("account:" + accountID)
}

func (s *Service) interactionNotification(ctx context.Context, kind string, status *models.Status, from *models.Account) (*models.Notification, []*models.Job, error) {
	if status.AccountID == from.ID {
		return nil, nil, nil
	}
	target, err := s.store.GetAccount(ctx, status.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if target.Domain != nil || target.IsServiceActor {
		return nil, nil, nil
	}
	notif := &models.Notification{ID: uuid.NewString(), CreatedAt: time.Now(), Type: kind, AccountID: target.ID, FromAccountID: from.ID, StatusID: &status.ID}
	jobs, err := push.NotificationJobs(ctx, s.store, notif)
	if err != nil {
		return nil, nil, err
	}
	email, err := mail.NotificationJob(ctx, s.store, s.domain, notif)
	if err != nil {
		return nil, nil, err
	}
	if email != nil {
		jobs = append(jobs, email)
	}
	return notif, jobs, nil
}

func (s *Service) notifyStatusAuthor(ctx context.Context, notifType string, status *models.Status, from *models.Account, emoji ...string) {
	if status == nil || from == nil || status.AccountID == from.ID {
		return
	}
	target, err := s.store.GetAccount(ctx, status.AccountID)
	if err != nil || target.Domain != nil || target.IsServiceActor {
		return
	}
	notif := &models.Notification{Type: notifType, AccountID: target.ID, FromAccountID: from.ID, StatusID: &status.ID}
	if len(emoji) > 0 {
		notif.Emoji = emoji[0]
	}
	if err := s.store.UpsertNotification(ctx, notif); err != nil {
		slog.Warn("app: failed to create status notification", "type", notifType, "error", err)
		return
	}
	s.streamNotification(ctx, notif)
	push.EnqueuePushNotification(ctx, s.store, notif)
	mail.EnqueueEmailNotification(ctx, s.store, s.domain, notif)
}

func (s *Service) Relationship(ctx context.Context, accountID, targetID string) Relationship {
	following, _ := s.store.IsFollowing(ctx, accountID, targetID)
	followedBy, _ := s.store.IsFollowing(ctx, targetID, accountID)
	blocking, _ := s.store.BlockExists(ctx, accountID, targetID)
	blockedBy, _ := s.store.BlockExists(ctx, targetID, accountID)
	muting, _ := s.store.IsMuted(ctx, accountID, targetID)
	rel := Relationship{Following: following, FollowedBy: followedBy, Blocking: blocking, BlockedBy: blockedBy, Muting: muting}
	if follow, err := s.store.GetFollowByPair(ctx, accountID, targetID); err == nil && follow.State == models.FollowPending {
		rel.FollowRequested = true
	}
	return rel
}

// enrichPrefetch holds relations loaded in bulk for a whole timeline so that
// enrichStatus can stitch them in without a per-status query fan-out. A nil
// *enrichPrefetch is valid: every accessor reports "not prefetched" and
// enrichStatus falls back to its original one-query-per-relation path, so the
// single-status EnrichStatus entry point behaves exactly as before.
type enrichPrefetch struct {
	refStatuses map[string]*models.Status            // boost/quote/reply targets, Account preloaded
	accounts    map[string]*models.Account           // authors of top-level statuses
	mentions    map[string][]*models.Mention         // keyed by status id (full set)
	tags        map[string][]*models.Tag             // keyed by status id (full set)
	media       map[string][]*models.MediaAttachment // keyed by status id (full set ∪ quotes)
	faved       map[string]bool
	booked      map[string]bool
	reblogged   map[string]bool
	polls       map[string]*models.Poll
	previews    map[string]*models.StatusPreview
	reactions   map[string][]models.EmojiReactionSummary
	// emojiLookup resolves custom emojis without a per-status/per-account query:
	// domain key ("" = local) -> shortcode -> emoji, covering every shortcode the
	// batch's statuses and authors reference.
	emojiLookup map[string]map[string]models.CustomEmoji
}

// hasRelations reports whether mentions/tags/media/flags were prefetched for id
// (the "full set": top-level statuses and their boost targets).
func (pf *enrichPrefetch) hasRelations(id string) bool {
	if pf == nil {
		return false
	}
	_, ok := pf.mentions[id]
	return ok
}

// hasMedia reports whether media was prefetched for id (full set plus quoted posts).
func (pf *enrichPrefetch) hasMedia(id string) bool {
	if pf == nil {
		return false
	}
	_, ok := pf.media[id]
	return ok
}

func (pf *enrichPrefetch) refStatus(id string) *models.Status {
	if pf == nil {
		return nil
	}
	return pf.refStatuses[id]
}

func (pf *enrichPrefetch) account(id string) *models.Account {
	if pf == nil {
		return nil
	}
	return pf.accounts[id]
}

// EnrichStatuses fills in the related objects (author, parent, boost/quote
// targets, mentions, tags, media, custom emojis, and the viewer's
// favourite/bookmark/reblog flags) for a whole slice of statuses. It batch-loads
// every relation up front so a 40-item timeline costs a constant handful of
// queries instead of one-per-status-per-relation - the single biggest scaling
// win for timeline rendering on both SQLite (single writer) and PostgreSQL.
func (s *Service) EnrichStatuses(ctx context.Context, statuses []*models.Status, viewer *models.Account) {
	s.EnrichStatusesCached(ctx, statuses, viewer)
}

func (s *Service) enrichStatusesLive(ctx context.Context, statuses []*models.Status, viewer *models.Account) {
	if len(statuses) == 0 {
		return
	}
	started := time.Now()
	pf := s.buildPrefetch(ctx, statuses, viewer)
	for _, status := range statuses {
		s.enrichStatus(ctx, status, viewer, pf)
	}
	metrics.ObserveTimelineStage("enrich", time.Since(started))
}

// EnrichStatus enriches a single status. It runs the un-batched path (one query
// per relation); for timelines use EnrichStatuses, which batches.
func (s *Service) EnrichStatus(ctx context.Context, status *models.Status, viewer *models.Account) {
	s.enrichStatus(ctx, status, viewer, nil)
}

// buildPrefetch bulk-loads, in a constant number of queries, every relation the
// status slice will need. The "full set" (top-level statuses plus their boost
// targets) gets mentions, tags, media and viewer flags; quoted posts get media
// only (matching the un-batched path, which loads just media for a quote);
// boost/quote/reply targets are fetched once with their authors preloaded.
func (s *Service) buildPrefetch(ctx context.Context, statuses []*models.Status, viewer *models.Account) *enrichPrefetch {
	fullSet := make(map[string]struct{})
	mediaSet := make(map[string]struct{})
	refNeeded := make(map[string]struct{})
	authorNeeded := make(map[string]struct{})

	for _, st := range statuses {
		if st == nil {
			continue
		}
		fullSet[st.ID] = struct{}{}
		mediaSet[st.ID] = struct{}{}
		if st.Account == nil && st.AccountID != "" {
			authorNeeded[st.AccountID] = struct{}{}
		}
		if st.BoostOfID != nil && *st.BoostOfID != "" {
			fullSet[*st.BoostOfID] = struct{}{}
			mediaSet[*st.BoostOfID] = struct{}{}
			if st.BoostOf == nil {
				refNeeded[*st.BoostOfID] = struct{}{}
			}
		}
		if st.QuoteOfID != nil && *st.QuoteOfID != "" {
			mediaSet[*st.QuoteOfID] = struct{}{}
			if st.QuoteOf == nil {
				refNeeded[*st.QuoteOfID] = struct{}{}
			}
		}
		if st.InReplyToID != nil && *st.InReplyToID != "" && st.InReplyTo == nil {
			refNeeded[*st.InReplyToID] = struct{}{}
		}
	}

	pf := &enrichPrefetch{}
	pf.refStatuses, _ = s.store.GetStatusesByIDs(ctx, setKeys(refNeeded))
	for _, st := range pf.refStatuses {
		if st != nil && st.Account == nil && st.AccountID != "" {
			authorNeeded[st.AccountID] = struct{}{}
		}
	}
	pf.accounts, _ = s.store.GetAccountsByIDs(ctx, setKeys(authorNeeded))

	fullIDs := setKeys(fullSet)
	pf.mentions, _ = s.store.GetMentionsForStatuses(ctx, fullIDs)
	pf.tags, _ = s.store.GetTagsForStatuses(ctx, fullIDs)
	pf.media, _ = s.store.GetMediaForStatuses(ctx, setKeys(mediaSet))
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	pf.polls, _ = s.store.GetPollsForStatuses(ctx, fullIDs, viewerID)
	pf.previews, _ = s.store.GetStatusPreviewsForStatuses(ctx, fullIDs)
	pf.reactions, _ = s.store.GetEmojiReactionSummaries(ctx, fullIDs, viewerID, 80)

	if viewer != nil {
		pf.faved, pf.booked, pf.reblogged, _ = s.store.ViewerStatusInteractions(ctx, viewer.ID, fullIDs)
	}

	// Batch custom-emoji resolution: gather the shortcode candidates from every
	// status body and every author (display name / bio) that enrichment will
	// touch (the input statuses plus their boost/quote/reply targets) and
	// resolve them in one query per distinct instance domain instead of one per
	// status and per account.
	var scopes []emojiScope
	collect := func(st *models.Status) {
		if st == nil {
			return
		}
		author := st.Account
		if author == nil {
			author = pf.accounts[st.AccountID]
		}
		var domain *string
		if author != nil {
			domain = author.Domain
			scopes = append(scopes, emojiScope{domain: domain, texts: []string{author.DisplayName, author.Note}})
		}
		scopes = append(scopes, emojiScope{domain: domain, texts: []string{st.ContentHTML, st.Summary}})
	}
	for _, st := range statuses {
		collect(st)
	}
	for _, st := range pf.refStatuses {
		collect(st)
	}
	pf.emojiLookup = s.buildEmojiLookup(ctx, scopes)

	return pf
}

// enrichStatus is the shared enrichment core. When pf has the status's relations
// prefetched it stitches them from the maps; otherwise it falls back to the
// original per-relation queries, so behavior is identical with or without a
// prefetch.
func (s *Service) enrichStatus(ctx context.Context, status *models.Status, viewer *models.Account, pf *enrichPrefetch) {
	if status == nil {
		return
	}
	if status.InReplyTo != nil && !s.canEmbedStatus(ctx, status.InReplyTo, viewer) {
		status.InReplyTo = nil
	}
	if status.BoostOf != nil && !s.canEmbedStatus(ctx, status.BoostOf, viewer) {
		status.BoostOf = nil
	}
	if status.QuoteOf != nil && !s.canEmbedStatus(ctx, status.QuoteOf, viewer) {
		status.QuoteOf = nil
	}
	if status.Account == nil && status.AccountID != "" {
		if a := pf.account(status.AccountID); a != nil {
			status.Account = a
		} else {
			status.Account, _ = s.store.GetAccount(ctx, status.AccountID)
		}
	}
	if status.InReplyToID != nil && *status.InReplyToID != "" && status.InReplyTo == nil {
		parent := pf.refStatus(*status.InReplyToID)
		if parent == nil {
			parent, _ = s.store.GetStatus(ctx, *status.InReplyToID)
		}
		if parent != nil && s.canEmbedStatus(ctx, parent, viewer) {
			if parent.Account == nil && parent.AccountID != "" {
				parent.Account = pf.account(parent.AccountID)
				if parent.Account == nil {
					parent.Account, _ = s.store.GetAccount(ctx, parent.AccountID)
				}
			}
			status.InReplyTo = parent
		}
	}
	if pf.hasRelations(status.ID) {
		status.Mentions = mentionsToValues(pf.mentions[status.ID])
		status.Tags = tagsToValues(pf.tags[status.ID])
		status.MediaAttachments = mediaToValues(pf.media[status.ID])
	} else {
		status.Mentions = mentionsToValues(mustMentions(s.store.GetMentionsForStatus(ctx, status.ID)))
		status.Tags = tagsToValues(mustTags(s.store.GetTagsForStatus(ctx, status.ID)))
		status.MediaAttachments = mediaToValues(mustMedia(s.store.GetMediaForStatus(ctx, status.ID)))
	}
	s.loadStatusEmojis(ctx, status, pf)
	if pf != nil {
		status.Poll = pf.polls[status.ID]
		status.Preview = pf.previews[status.ID]
		status.EmojiReactions = pf.reactions[status.ID]
	} else {
		viewerID := ""
		if viewer != nil {
			viewerID = viewer.ID
		}
		status.Poll, _ = s.store.GetPollForStatus(ctx, status.ID, viewerID)
		if preview, err := s.store.GetStatusPreview(ctx, status.ID); err == nil && preview.SourceURL != "" {
			status.Preview = preview
		}
		reactions, _ := s.store.GetEmojiReactionSummaries(ctx, []string{status.ID}, viewerID, 80)
		status.EmojiReactions = reactions[status.ID]
	}
	s.loadAccountEmojisPF(ctx, status.Account, pf)
	if status.InReplyTo != nil {
		s.loadAccountEmojisPF(ctx, status.InReplyTo.Account, pf)
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" && status.BoostOf == nil {
		boosted := pf.refStatus(*status.BoostOfID)
		if boosted == nil {
			boosted, _ = s.store.GetStatus(ctx, *status.BoostOfID)
		}
		if boosted != nil && s.canEmbedStatus(ctx, boosted, viewer) {
			status.BoostOf = boosted
			s.enrichStatus(ctx, boosted, viewer, pf)
		}
	}
	// Load the quoted status one level deep (its own quote is not expanded, to
	// avoid unbounded recursion on quote chains).
	if status.QuoteOfID != nil && *status.QuoteOfID != "" && status.QuoteOf == nil {
		quoted := pf.refStatus(*status.QuoteOfID)
		if quoted == nil {
			quoted, _ = s.store.GetStatus(ctx, *status.QuoteOfID)
		}
		if quoted != nil && s.canEmbedStatus(ctx, quoted, viewer) {
			if quoted.Account == nil && quoted.AccountID != "" {
				quoted.Account = pf.account(quoted.AccountID)
				if quoted.Account == nil {
					quoted.Account, _ = s.store.GetAccount(ctx, quoted.AccountID)
				}
			}
			if pf.hasMedia(quoted.ID) {
				quoted.MediaAttachments = mediaToValues(pf.media[quoted.ID])
			} else {
				quoted.MediaAttachments = mediaToValues(mustMedia(s.store.GetMediaForStatus(ctx, quoted.ID)))
			}
			s.loadStatusEmojis(ctx, quoted, pf)
			s.loadAccountEmojisPF(ctx, quoted.Account, pf)
			status.QuoteOf = quoted
		}
	}
	// Viewer-specific interaction flags (never cached at the entity layer).
	if viewer != nil {
		targetID := status.ID
		if status.BoostOfID != nil && *status.BoostOfID != "" {
			targetID = *status.BoostOfID
		}
		if pf.hasRelations(status.ID) {
			status.Favourited = pf.faved[status.ID]
			status.Bookmarked = pf.booked[status.ID]
			status.Reblogged = pf.reblogged[targetID]
		} else {
			status.Favourited = hasFavourite(s.store, ctx, viewer.ID, status.ID)
			status.Bookmarked = hasBookmark(s.store, ctx, viewer.ID, status.ID)
			status.Reblogged = hasReblog(s.store, ctx, viewer.ID, targetID)
		}
	}
}

// setKeys returns the keys of a set as a slice (order unspecified).
func setKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// emojiScope pairs an instance domain (nil = local) with the text fragments to
// scan for `:shortcode:` references.
type emojiScope struct {
	domain *string
	texts  []string
}

// emojiDomainKey maps a nullable instance domain to a lookup key ("" = local).
func emojiDomainKey(domain *string) string {
	if domain == nil {
		return ""
	}
	return *domain
}

// uniqueShortcodes returns the distinct `:shortcode:` candidates across the
// given texts, preserving first-seen order.
func uniqueShortcodes(texts ...string) []string {
	seen := map[string]struct{}{}
	var codes []string
	for _, t := range texts {
		for _, code := range content.ExtractEmojiShortcodes(t) {
			if _, ok := seen[code]; ok {
				continue
			}
			seen[code] = struct{}{}
			codes = append(codes, code)
		}
	}
	return codes
}

// buildEmojiLookup resolves every shortcode referenced by the given scopes in a
// single query per distinct instance domain, returning domain key -> shortcode ->
// emoji. Scopes without shortcode candidates cost nothing; a fully empty input
// returns nil (and resolveFromEmojiLookup treats nil as "no emojis").
func (s *Service) buildEmojiLookup(ctx context.Context, scopes []emojiScope) map[string]map[string]models.CustomEmoji {
	type domReq struct {
		domain *string
		seen   map[string]struct{}
		codes  []string
	}
	byDomain := map[string]*domReq{}
	for _, sc := range scopes {
		codes := uniqueShortcodes(sc.texts...)
		if len(codes) == 0 {
			continue
		}
		key := emojiDomainKey(sc.domain)
		req := byDomain[key]
		if req == nil {
			req = &domReq{domain: sc.domain, seen: map[string]struct{}{}}
			byDomain[key] = req
		}
		for _, c := range codes {
			if _, ok := req.seen[c]; ok {
				continue
			}
			req.seen[c] = struct{}{}
			req.codes = append(req.codes, c)
		}
	}
	if len(byDomain) == 0 {
		return nil
	}
	lookup := make(map[string]map[string]models.CustomEmoji, len(byDomain))
	for key, req := range byDomain {
		emojis, err := s.store.GetEmojisByShortcodes(ctx, req.codes, req.domain)
		if err != nil {
			slog.Debug("app: failed to load custom emojis", "error", err)
			continue
		}
		m := make(map[string]models.CustomEmoji, len(emojis))
		for _, e := range emojis {
			m[e.Shortcode] = *e
		}
		lookup[key] = m
	}
	return lookup
}

// resolveFromEmojiLookup returns the emojis referenced across texts, filtered
// from a prebuilt lookup (no query). Order follows first appearance in the text.
func resolveFromEmojiLookup(lookup map[string]map[string]models.CustomEmoji, domain *string, texts ...string) []models.CustomEmoji {
	byCode := lookup[emojiDomainKey(domain)]
	if len(byCode) == 0 {
		return nil
	}
	var out []models.CustomEmoji
	for _, code := range uniqueShortcodes(texts...) {
		if e, ok := byCode[code]; ok {
			out = append(out, e)
		}
	}
	return out
}

// loadStatusEmojis populates status.Emojis with the custom emojis referenced by
// the status's content and spoiler text, scoped to the author's instance. With
// a prefetch it reads from the batched lookup; otherwise it queries. It is a
// no-op when no `:shortcode:` candidates are present (the common case).
func (s *Service) loadStatusEmojis(ctx context.Context, status *models.Status, pf *enrichPrefetch) {
	if status == nil {
		return
	}
	var domain *string
	if status.Account != nil {
		domain = status.Account.Domain
	}
	if pf != nil && pf.emojiLookup != nil {
		status.Emojis = resolveFromEmojiLookup(pf.emojiLookup, domain, status.ContentHTML, status.Summary)
		return
	}
	status.Emojis = s.resolveEmojis(ctx, domain, status.ContentHTML, status.Summary)
}

// loadAccountEmojisPF is the prefetch-aware form of LoadAccountEmojis used inside
// batched enrichment; it reads from the lookup when present and otherwise falls
// back to the single-account query path.
func (s *Service) loadAccountEmojisPF(ctx context.Context, account *models.Account, pf *enrichPrefetch) {
	if account == nil {
		return
	}
	if pf != nil && pf.emojiLookup != nil {
		account.Emojis = resolveFromEmojiLookup(pf.emojiLookup, account.Domain, account.DisplayName, account.Note)
		return
	}
	s.LoadAccountEmojis(ctx, account)
}

// LoadAccountEmojis populates account.Emojis with the custom emojis referenced
// by the display name and bio, scoped to the account's instance.
func (s *Service) LoadAccountEmojis(ctx context.Context, account *models.Account) {
	if account == nil {
		return
	}
	account.Emojis = s.resolveEmojis(ctx, account.Domain, account.DisplayName, account.Note)
}

// LoadAccountsEmojis populates Emojis for each account in a list (e.g. follower,
// search, notification-actor, and like/boost lists), resolving all of them in
// one query per instance domain rather than one query per account.
func (s *Service) LoadAccountsEmojis(ctx context.Context, accounts []*models.Account) {
	scopes := make([]emojiScope, 0, len(accounts))
	for _, a := range accounts {
		if a != nil {
			scopes = append(scopes, emojiScope{domain: a.Domain, texts: []string{a.DisplayName, a.Note}})
		}
	}
	lookup := s.buildEmojiLookup(ctx, scopes)
	for _, a := range accounts {
		if a == nil {
			continue
		}
		a.Emojis = resolveFromEmojiLookup(lookup, a.Domain, a.DisplayName, a.Note)
	}
}

// resolveEmojis finds the custom emoji rows referenced (by `:shortcode:`) across
// the given text fragments, scoped to domain (nil = local).
func (s *Service) resolveEmojis(ctx context.Context, domain *string, texts ...string) []models.CustomEmoji {
	shortcodes := uniqueShortcodes(texts...)
	if len(shortcodes) == 0 {
		return nil
	}
	emojis, err := s.store.GetEmojisByShortcodes(ctx, shortcodes, domain)
	if err != nil {
		slog.Debug("app: failed to load custom emojis", "error", err)
		return nil
	}
	return emojisToValues(emojis)
}

// sourceMediaType returns the ActivityPub source mediaType for a status,
// defaulting to plain text for legacy rows that predate content_type.
func sourceMediaType(contentType string) string {
	if contentType == content.ContentTypeMarkdown {
		return content.ContentTypeMarkdown
	}
	return content.ContentTypePlain
}

func renderInputHTML(input, contentType string) string {
	if contentType == content.ContentTypeMarkdown {
		return content.MarkdownToHTML(input)
	}
	if strings.Contains(input, "<") && strings.Contains(input, ">") {
		return string(content.SanitizeHTML(input))
	}
	return string(content.SanitizeHTML(content.PlainTextToHTML(input)))
}

func prefixedReplyContent(input string, author, parentAuthor *models.Account) string {
	input = strings.TrimSpace(input)
	if author == nil || parentAuthor == nil || author.ID == parentAuthor.ID {
		return input
	}
	mention := "@" + acctForAccount(parentAuthor)
	if startsWithMention(input, parentAuthor) {
		return input
	}
	if input == "" {
		return mention
	}
	return mention + " " + input
}

func startsWithMention(input string, account *models.Account) bool {
	if account == nil {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(input))
	for _, candidate := range []string{"@" + account.Username, "@" + acctForAccount(account)} {
		c := strings.ToLower(candidate)
		if lower == c || strings.HasPrefix(lower, c+" ") || strings.HasPrefix(lower, c+"\n") {
			return true
		}
	}
	return false
}

func acctForAccount(account *models.Account) string {
	if account == nil {
		return ""
	}
	if account.Domain != nil && *account.Domain != "" {
		return account.Username + "@" + *account.Domain
	}
	return account.Username
}

// DeliveryFanoutJobKind is the queue kind that expands a follower delivery
// into per-inbox delivery jobs off the request path.
const DeliveryFanoutJobKind = "delivery_fanout"

// LightJobKinds are the job kinds eligible for the optional dedicated light
// worker pool (cfg.Queue.LightWorkers): short, mostly network-bound outbound
// work. Single source of truth for cmd/littlefedi/main.go's pool wiring and
// the admin queue view's per-pool labeling - see README "Job priority and
// the optional light-jobs pool".
var LightJobKinds = []string{"delivery", DeliveryFanoutJobKind, "push_notification", "send_email"}

// IsLightJobKind reports whether kind is one of LightJobKinds.
func IsLightJobKind(kind string) bool {
	for _, k := range LightJobKinds {
		if k == kind {
			return true
		}
	}
	return false
}

type deliveryFanoutPayload struct {
	AccountID  string          `json:"account_id"`
	Activity   json.RawMessage `json:"activity"`
	ExtraInbox string          `json:"extra_inbox,omitempty"`
}

// deliverToFollowers persists ONE fan-out job and returns: the follower inbox
// queries, per-inbox envelope marshalling and the per-inbox job insert all run
// in the worker (HandleDeliveryFanoutJob). Durability is unchanged, since the
// fan-out job itself is committed before the request returns, but the
// request-path cost drops from the whole expansion to a single small insert
// (measured 0.8-1.2s of every post on SD-card hardware).
func (s *Service) deliverToFollowers(ctx context.Context, accountID string, activity map[string]interface{}) {
	s.deliverToFollowersAnd(ctx, accountID, activity, "")
}

func (s *Service) deliverToFollowersAnd(ctx context.Context, accountID string, activity map[string]interface{}, extraInbox string) {
	job := deliveryFanoutJobWithInbox(accountID, activity, extraInbox)
	if err := s.store.EnqueueJob(ctx, job); err != nil {
		// Losing the delivery outright is worse than paying the fan-out on
		// the request: expand inline as the fallback.
		slog.Error("app: enqueue delivery fan-out failed, expanding inline", "error", err)
		activityJSON, _ := json.Marshal(activity)
		if expandErr := expandFollowerDeliveryTo(ctx, s.store, accountID, activityJSON, extraInbox); expandErr != nil {
			slog.Error("app: inline delivery fan-out failed", "error", expandErr)
		}
	}
}

func deliveryFanoutJob(accountID string, activity map[string]interface{}) *models.Job {
	return deliveryFanoutJobWithInbox(accountID, activity, "")
}

func deliveryFanoutJobWithInbox(accountID string, activity map[string]interface{}, extraInbox string) *models.Job {
	activityJSON, _ := json.Marshal(activity)
	payload, _ := json.Marshal(deliveryFanoutPayload{AccountID: accountID, Activity: activityJSON, ExtraInbox: extraInbox})
	return &models.Job{Kind: DeliveryFanoutJobKind, Payload: string(payload), RunAt: time.Now(), MaxAttempts: 3, State: "queued"}
}

func (s *Service) prepareRelayStatus(ctx context.Context, status *models.Status) ([]*models.Job, error) {
	inboxes, err := s.store.GetActiveRelayInboxes(ctx)
	if err != nil {
		return nil, err
	}
	if len(inboxes) == 0 {
		return nil, nil
	}
	actor, err := s.store.GetAccountByUsernameDomain(ctx, s.domain, nil)
	if err != nil {
		return nil, err
	}
	if actor == nil || !actor.IsServiceActor {
		return nil, fmt.Errorf("instance service actor unavailable for relay delivery")
	}
	activity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       fmt.Sprintf("https://%s/relay/announces/%s", s.domain, status.ID),
		"type":     "Announce",
		"actor":    actor.ActorURI,
		"object":   status.URI,
		"to":       []string{"https://www.w3.org/ns/activitystreams#Public"},
	}
	var jobs []*models.Job
	for _, inbox := range inboxes {
		job, err := federation.PrepareDelivery(ctx, s.store, inbox, activity)
		if errors.Is(err, storage.ErrFederationBlocked) {
			continue
		}
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// HandleDeliveryFanoutJob returns the worker handler for DeliveryFanoutJobKind.
func HandleDeliveryFanoutJob(store storage.Storage) func(context.Context, string) error {
	return func(ctx context.Context, payload string) error {
		var p deliveryFanoutPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}
		return expandFollowerDeliveryTo(ctx, store, p.AccountID, p.Activity, p.ExtraInbox)
	}
}

func expandFollowerDelivery(ctx context.Context, store storage.Storage, accountID string, activityJSON json.RawMessage) error {
	return expandFollowerDeliveryTo(ctx, store, accountID, activityJSON, "")
}

func expandFollowerDeliveryTo(ctx context.Context, store storage.Storage, accountID string, activityJSON json.RawMessage, extraInbox string) error {
	return federation.ExpandFollowerDelivery(ctx, store, accountID, activityJSON, extraInbox)
}

func inboxHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (s *Service) deliverToStatusAuthor(ctx context.Context, actor *models.Account, status *models.Status, activity map[string]interface{}) {
	if status.Account == nil || status.Account.Domain == nil {
		return
	}
	s.deliverToAccount(ctx, actor, status.Account, activity)
}

func (s *Service) deliverToAccount(ctx context.Context, actor, target *models.Account, activity map[string]interface{}) {
	if target == nil || target.Domain == nil {
		return
	}
	if s.federator == nil {
		return
	}
	inbox := target.InboxURI
	if target.SharedInboxURI != nil && *target.SharedInboxURI != "" {
		inbox = *target.SharedInboxURI
	}
	if inbox == "" {
		return
	}
	if !s.immediateDeliveries {
		if err := federation.EnqueueDelivery(ctx, s.store, inbox, activity); err != nil {
			slog.Error("app: enqueue direct delivery failed", "inbox", inbox, "error", err)
		}
		return
	}

	// Circuit breaker: skip if instance is unreachable
	domain := *target.Domain
	reachable, err := s.store.IsInstanceReachable(ctx, domain)
	if err != nil {
		slog.Warn("app: failed to check instance reachability", "domain", domain, "error", err)
	} else if !reachable {
		slog.Info("app: skipping delivery to unreachable instance", "domain", domain, "inbox", inbox)
		return
	}

	actorID := "instance"
	if actor != nil {
		actorID = actor.ID
	}
	s.deliveries.Add(1)
	go func() {
		defer s.deliveries.Done()
		if err := s.federator.Deliver(context.Background(), inbox, activity); err != nil {
			slog.Warn("app: federation delivery failed", "actor", actorID, "inbox", inbox, "error", err)
		}
	}()
}

// statusObject builds the ActivityPub Note object for a status along with its
// to/cc addressing. It is shared by the Create and Update (edit) activities so
// both federate an identical representation of the post.
func (s *Service) statusObject(status *models.Status, account *models.Account) (object map[string]interface{}, to, cc []string) {
	// Build addressing based on visibility (Mastodon-compatible)
	var mentionedURIs []string

	var apMentions []*models.Mention
	if status.Mentions != nil {
		for i := range status.Mentions {
			apMentions = append(apMentions, &status.Mentions[i])
		}
	} else {
		apMentions, _ = s.store.GetMentionsForStatus(context.Background(), status.ID)
	}
	for _, m := range apMentions {
		if m.URL != "" {
			mentionedURIs = append(mentionedURIs, m.URL)
		} else if mentioned, err := s.store.GetAccount(context.Background(), m.AccountID); err == nil {
			mentionedURIs = append(mentionedURIs, mentioned.ActorURI)
		}
	}

	switch status.Visibility {
	case models.VisibilityPublic:
		to = []string{"https://www.w3.org/ns/activitystreams#Public"}
		cc = append([]string{account.FollowersURI}, mentionedURIs...)
	case models.VisibilityUnlisted:
		// Unlisted: empty to, followers + public in cc
		to = []string{}
		cc = append([]string{account.FollowersURI, "https://www.w3.org/ns/activitystreams#Public"}, mentionedURIs...)
	case models.VisibilityPrivate:
		to = append([]string{account.FollowersURI}, mentionedURIs...)
	case models.VisibilityDirect:
		to = mentionedURIs
		if len(to) == 0 {
			// If no mentions, address to self (shouldn't happen normally)
			to = []string{account.ActorURI}
		}
	case models.VisibilityLocal:
		// Local: no federation at all, but when creating an AP object for
		// internal use, mirror public addressing.
		to = []string{"https://www.w3.org/ns/activitystreams#Public"}
		cc = append([]string{account.FollowersURI}, mentionedURIs...)
	case models.VisibilityLocalUnlisted:
		// Local Unlisted: same as unlisted, but never federated.
		to = []string{}
		cc = append([]string{account.FollowersURI, "https://www.w3.org/ns/activitystreams#Public"}, mentionedURIs...)
	default:
		// Corrupt legacy rows fail closed instead of becoming public.
		to = []string{account.ActorURI}
	}

	// Quiet: like silence but federation is downgraded to followers-only so the
	// account does not reach new servers. Public and unlisted are bumped down to
	// private; already-private and direct are left alone.
	if account.ModerationState == "quiet" {
		switch status.Visibility {
		case models.VisibilityPublic, models.VisibilityUnlisted:
			to = append([]string{account.FollowersURI}, mentionedURIs...)
			cc = nil
		}
	}

	object = map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           status.URI,
		"type":         "Note",
		"published":    status.CreatedAt.Format(time.RFC3339),
		"url":          status.URL,
		"attributedTo": account.ActorURI,
		"content":      status.ContentHTML,
		"to":           to,
		"cc":           cc,
	}
	if status.Visibility == models.VisibilityPublic || status.Visibility == models.VisibilityUnlisted {
		object["@context"] = []interface{}{"https://www.w3.org/ns/activitystreams", map[string]string{"emojiReactions": "http://fedibird.com/ns#emojiReactions"}}
		object["emojiReactions"] = fmt.Sprintf("https://%s/objects/%s/emoji_reactions", s.domain, status.ID)
	}
	if status.Poll != nil {
		object["type"] = "Question"
		key := "oneOf"
		if status.Poll.Multiple {
			key = "anyOf"
		}
		options := make([]map[string]any, 0, len(status.Poll.Options))
		for _, option := range status.Poll.Options {
			options = append(options, map[string]any{"type": "Note", "name": option.Title, "replies": map[string]any{"type": "Collection", "totalItems": option.VotesCount}})
		}
		object[key] = options
		if status.Poll.ExpiresAt != nil {
			object["endTime"] = status.Poll.ExpiresAt.Format(time.RFC3339)
		}
	}
	if status.EditedAt != nil {
		object["updated"] = status.EditedAt.Format(time.RFC3339)
	}
	// Advertise the original source so Markdown-aware servers (Pleroma,
	// GoToSocial) can show the post's source rather than the rendered HTML.
	if status.ContentSource != "" {
		object["source"] = map[string]interface{}{
			"content":   status.ContentSource,
			"mediaType": sourceMediaType(status.ContentType),
		}
	}
	if status.Summary != "" {
		object["summary"] = status.Summary
	}
	if status.Sensitive {
		object["sensitive"] = true
	}
	if status.InReplyToURI != nil {
		object["inReplyTo"] = *status.InReplyToURI
	}

	// Quote post (FEP-044f): advertise the quoted object and, once granted, the
	// authorization. Always advertise this author's quote interaction policy,
	// which requires the extended @context.
	object["@context"] = federation.QuoteJSONLDContext()
	if status.QuoteOfURI != nil && *status.QuoteOfURI != "" {
		federation.ApplyQuoteFields(object, *status.QuoteOfURI, status.QuoteApprovalURI)
	}
	object["interactionPolicy"] = federation.InteractionPolicyFor(account)

	// Add mention and hashtag tags for ActivityPub.
	tags := make([]map[string]interface{}, 0, len(apMentions))
	for _, m := range apMentions {
		tags = append(tags, map[string]interface{}{
			"type": "Mention",
			"href": m.URL,
			"name": "@" + m.Acct,
		})
	}
	hashtags := make([]*models.Tag, 0, len(status.Tags))
	if status.Tags != nil {
		for i := range status.Tags {
			hashtags = append(hashtags, &status.Tags[i])
		}
	} else {
		hashtags, _ = s.store.GetTagsForStatus(context.Background(), status.ID)
	}
	if len(hashtags) > 0 {
		for _, t := range hashtags {
			tags = append(tags, map[string]interface{}{
				"type": "Hashtag",
				"href": fmt.Sprintf("https://%s/tags/%s", s.domain, t.Name),
				"name": "#" + t.Name,
			})
		}
	}
	if len(tags) > 0 {
		object["tag"] = tags
	}

	// Add media attachments with absolute URLs, plus the link preview as an
	// FEP-8967 Link when this instance already holds one, so peers do not each
	// scrape the page themselves.
	attachments := make([]map[string]interface{}, 0, len(status.MediaAttachments)+1)
	for _, m := range status.MediaAttachments {
		attachments = append(attachments, map[string]interface{}{
			"type":      "Document",
			"mediaType": m.MIMEType,
			"url":       s.absoluteURL(m.URL),
			"name":      m.Description,
		})
	}
	preview := status.Preview
	if preview == nil {
		preview, _ = s.store.GetStatusPreview(context.Background(), status.ID)
	}
	if link := opengraph.LinkAttachment(preview); link != nil {
		attachments = append(attachments, link)
	}
	if len(attachments) > 0 {
		object["attachment"] = attachments
	}

	return object, to, cc
}

// createActivity wraps the status Note object in a Create activity for federation.
func (s *Service) createActivity(status *models.Status, account *models.Account) map[string]interface{} {
	object, to, cc := s.statusObject(status, account)
	return map[string]interface{}{
		"@context":  object["@context"],
		"id":        status.URI + "/activity",
		"type":      "Create",
		"actor":     account.ActorURI,
		"published": status.CreatedAt.Format(time.RFC3339),
		"object":    object,
		"to":        to,
		"cc":        cc,
	}
}

// updateActivity wraps the status Note object in an Update activity, federated
// after a local user edits one of their posts (Mastodon-compatible edit).
func (s *Service) updateActivity(status *models.Status, account *models.Account) map[string]interface{} {
	object, to, cc := s.statusObject(status, account)
	updated := time.Now().Format(time.RFC3339)
	if status.EditedAt != nil {
		updated = status.EditedAt.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"@context":  object["@context"],
		"id":        status.URI + "/updates/" + updated,
		"type":      "Update",
		"actor":     account.ActorURI,
		"published": updated,
		"object":    object,
		"to":        to,
		"cc":        cc,
	}
}

// FederateProfileUpdate sends an Update activity with the full actor object to all followers.
// This is exported so external callers (API, web handlers) can trigger federation after
// making profile changes that bypass UpdateProfile (e.g. avatar/header removal).
func (s *Service) FederateProfileUpdate(ctx context.Context, account *models.Account) {
	s.deliverProfileUpdate(ctx, account)
}

// deliverProfileUpdate sends an Update activity with the full actor object to all followers.
func (s *Service) deliverProfileUpdate(ctx context.Context, account *models.Account) {
	actorObj := s.accountToAPObject(account)
	activity := map[string]interface{}{
		"@context": []interface{}{
			"https://www.w3.org/ns/activitystreams",
			"https://purl.archive.org/socialweb/webfinger",
		},
		"id":     account.ActorURI + "/updates/" + uuid.New().String(),
		"type":   "Update",
		"actor":  account.ActorURI,
		"to":     []string{"https://www.w3.org/ns/activitystreams#Public"},
		"cc":     []string{account.FollowersURI},
		"object": actorObj,
	}
	s.deliverToFollowers(ctx, account.ID, activity)
}

// accountToAPObject builds an ActivityPub Person object from the account model.
func (s *Service) accountToAPObject(account *models.Account) map[string]interface{} {
	obj := map[string]interface{}{
		"id":                        account.ActorURI,
		"type":                      "Person",
		"preferredUsername":         account.Username,
		"webfinger":                 account.Username + "@" + s.localDomain,
		"name":                      account.DisplayName,
		"summary":                   content.ResolveSocialLinks(account.Note, "https://"+s.domain, nil),
		"inbox":                     account.InboxURI,
		"outbox":                    account.OutboxURI,
		"followers":                 account.FollowersURI,
		"following":                 account.FollowingURI,
		"url":                       fmt.Sprintf("https://%s/@%s", s.domain, account.Username),
		"published":                 account.CreatedAt.Format(time.RFC3339),
		"manuallyApprovesFollowers": account.ManuallyApprovesFollowers,
		"discoverable":              true,
	}

	if account.Avatar != "" {
		obj["icon"] = map[string]interface{}{
			"type": "Image",
			"url":  s.absoluteURL(account.Avatar),
		}
	}
	if account.Header != "" {
		obj["image"] = map[string]interface{}{
			"type": "Image",
			"url":  s.absoluteURL(account.Header),
		}
	}
	if account.PublicKeyPem != "" {
		obj["publicKey"] = map[string]interface{}{
			"id":           account.ActorURI + "#main-key",
			"owner":        account.ActorURI,
			"publicKeyPem": account.PublicKeyPem,
		}
	}

	if aliases := accountmigration.AccountAliasesForAP(account); aliases != nil {
		obj["alsoKnownAs"] = aliases
	}
	if account.MovedToURI != "" {
		obj["movedTo"] = account.MovedToURI
	}

	return obj
}

// absoluteURL converts a potentially relative media URL to an absolute one.
// Relative URLs like "/media/..." are prefixed with "https://domain".
// Already-absolute URLs (starting with http) are returned unchanged.
func (s *Service) absoluteURL(pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	return "https://" + s.domain + pathOrURL
}

func (s *Service) likeActivity(account *models.Account, status *models.Status, likeURI string) map[string]interface{} {
	return map[string]interface{}{"@context": "https://www.w3.org/ns/activitystreams", "id": likeURI, "type": "Like", "actor": account.ActorURI, "object": status.URI}
}

func (s *Service) undoLikeActivity(account *models.Account, status *models.Status) map[string]interface{} {
	likeID := fmt.Sprintf("https://%s/users/%s/likes/%s", s.domain, account.Username, status.ID)
	return map[string]interface{}{"@context": "https://www.w3.org/ns/activitystreams", "id": likeID + "/undo", "type": "Undo", "actor": account.ActorURI, "object": map[string]interface{}{"id": likeID, "type": "Like", "actor": account.ActorURI, "object": status.URI}}
}

func hasFavourite(store storage.Storage, ctx context.Context, accountID, statusID string) bool {
	f, err := store.GetFavourite(ctx, accountID, statusID)
	return err == nil && f != nil
}

func hasBookmark(store storage.Storage, ctx context.Context, accountID, statusID string) bool {
	b, err := store.GetBookmark(ctx, accountID, statusID)
	return err == nil && b != nil
}

func hasReblog(store storage.Storage, ctx context.Context, accountID, statusID string) bool {
	r, err := store.GetReblogByAccountStatus(ctx, accountID, statusID)
	return err == nil && r != nil
}

func mustMentions(v []*models.Mention, err error) []*models.Mention {
	if err != nil && err != sql.ErrNoRows {
		return nil
	}
	return v
}

func mustTags(v []*models.Tag, err error) []*models.Tag {
	if err != nil && err != sql.ErrNoRows {
		return nil
	}
	return v
}

func mustMedia(v []*models.MediaAttachment, err error) []*models.MediaAttachment {
	if err != nil && err != sql.ErrNoRows {
		return nil
	}
	return v
}

func mentionsToValues(mentions []*models.Mention) []models.Mention {
	vals := make([]models.Mention, 0, len(mentions))
	for _, mention := range mentions {
		if mention != nil {
			vals = append(vals, *mention)
		}
	}
	return vals
}

func tagsToValues(tags []*models.Tag) []models.Tag {
	vals := make([]models.Tag, 0, len(tags))
	for _, tag := range tags {
		if tag != nil {
			vals = append(vals, *tag)
		}
	}
	return vals
}

func mediaToValues(media []*models.MediaAttachment) []models.MediaAttachment {
	vals := make([]models.MediaAttachment, 0, len(media))
	for _, attachment := range media {
		if attachment != nil {
			vals = append(vals, *attachment)
		}
	}
	return vals
}

func emojisToValues(emojis []*models.CustomEmoji) []models.CustomEmoji {
	vals := make([]models.CustomEmoji, 0, len(emojis))
	for _, emoji := range emojis {
		if emoji != nil {
			vals = append(vals, *emoji)
		}
	}
	return vals
}
