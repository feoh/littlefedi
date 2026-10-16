package storage

// This decorator caches timeline membership as compact ordered status-ID
// windows. The database remains authoritative: cache misses, expired windows,
// partial hydration, and all errors fall back to the wrapped Storage.

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type timelineCachingStorage struct {
	Storage
	c          *cache.MemoryCache
	ttl        time.Duration
	maxEntries int
	responses  *cache.MemoryCache
	mu         sync.Mutex
	inflight   map[string]*timelineCacheCall
	generation uint64
	// homeGeneration is the global home counter, bumped by events that affect
	// every viewer (moderation, follows fallback). homeViewerGens adds
	// per-viewer bumps for ordinary status arrivals, so one user's new post
	// leaves other users' cached home pages valid. A viewer's effective home
	// generation is the sum of both. The map only ever holds local account IDs
	// (HomeTimelineViewers filters to local accounts), so it is bounded by the
	// instance's user count.
	homeGeneration uint64
	homeViewerGens map[string]uint64
	// kindGens adds per-window-kind bumps on top of the global generation, so a
	// new status stales only the window kinds its visibility can actually reach:
	// an unlisted remote post (the bulk of shared-inbox traffic) can never enter
	// a public window, and bumping one shared counter for it forced a pointless
	// multi-second public head refresh per inbound message on slow hardware.
	// Keys are window kinds ("public", "tag", "bubble", "direct") plus
	// "account:<authorID>" for that author's profile windows. A key's effective
	// generation is generation + kindGens[kind].
	kindGens map[string]uint64
}

// maxKindGens bounds the kindGens map ("account:<id>" keys grow with distinct
// remote authors). On overflow the global generation advances (staling every
// window once) and the map resets.
const maxKindGens = 4096

type timelineCacheCall struct {
	done chan struct{}
	ids  []string
	err  error
}

type timelineCacheEntry struct {
	ids        []string
	generation uint64
}

// NewTimelineCachingStorage adds a bounded timeline-membership cache. It is
// a separate decorator from the entity cache so budgets and TTLs
// can be tuned independently.
func NewTimelineCachingStorage(inner Storage, c *cache.MemoryCache, ttl time.Duration, maxEntries int) Storage {
	return NewTimelineCachingStorageWithResponses(inner, c, nil, ttl, maxEntries)
}

// NewTimelineCachingStorageWithResponses keeps timeline membership and public
// federation responses in separate LRU partitions while coordinating writes.
func NewTimelineCachingStorageWithResponses(inner Storage, c, responses *cache.MemoryCache, ttl time.Duration, maxEntries int) Storage {
	if c == nil || maxEntries <= 0 {
		return inner
	}
	return &timelineCachingStorage{Storage: inner, c: c, responses: responses, ttl: ttl, maxEntries: maxEntries, inflight: make(map[string]*timelineCacheCall), homeViewerGens: make(map[string]uint64), kindGens: make(map[string]uint64)}
}

func timelinePtr(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}

func timelineKey(kind string, parts ...string) string {
	return "tl:" + kind + ":" + strings.Join(parts, "\x1f")
}

func boolString(v bool) string { return strconv.FormatBool(v) }

func statusIDs(statuses []*models.Status) []string {
	ids := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if st != nil && st.ID != "" {
			ids = append(ids, st.ID)
		}
	}
	return ids
}

func (s *timelineCachingStorage) hydrate(ctx context.Context, ids []string) ([]*models.Status, bool) {
	rows, err := s.Storage.GetStatusesByIDs(ctx, ids)
	if err != nil {
		return nil, false
	}
	out := make([]*models.Status, 0, len(ids))
	for _, id := range ids {
		st, ok := rows[id]
		if !ok {
			return nil, false
		}
		out = append(out, st)
	}
	return out, true
}

func mergeTimelineIDs(newer, existing []string, limit int) []string {
	seen := make(map[string]struct{}, len(newer)+len(existing))
	out := make([]string, 0, min(limit, len(newer)+len(existing)))
	for _, ids := range [][]string{newer, existing} {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) == limit {
				return out
			}
		}
	}
	return out
}

// cached serves an exact window. For an unpaginated head window, refresh may
// fetch only rows newer than the old head after a CreateStatus. Other mutations
// invalidate entries outright.
func (s *timelineCachingStorage) cached(ctx context.Context, key string, limit int, refresh func(string) ([]*models.Status, error), build func() ([]*models.Status, error)) ([]*models.Status, error) {
	v, ok, lookup := s.c.GetWithResult(key)
	if ok {
		entry := v.(timelineCacheEntry)
		s.mu.Lock()
		generation := s.generationForKey(key)
		s.mu.Unlock()
		if entry.generation != generation && refresh != nil && len(entry.ids) > 0 {
			// Single-flight the refresh on the same inflight map as the full
			// build: the foreground request and the warmer race to refresh the
			// same head window after a post, and running the query twice on a
			// single-core device doubles the latency for both.
			s.mu.Lock()
			if call := s.inflight[key]; call != nil {
				s.mu.Unlock()
				select {
				case <-call.done:
					return s.cached(ctx, key, limit, refresh, build)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			call := &timelineCacheCall{done: make(chan struct{})}
			s.inflight[key] = call
			s.mu.Unlock()
			release := func() {
				s.mu.Lock()
				delete(s.inflight, key)
				close(call.done)
				s.mu.Unlock()
			}
			slog.Debug("cache: timeline incremental refresh", "timeline", timelineKind(key), "cached_generation", entry.generation, "current_generation", generation, "cached_ids", len(entry.ids))
			cacheVersion := s.c.Version()
			refreshStart := time.Now()
			rows, err := refresh(entry.ids[0])
			metrics.ObserveTimelineStage("timeline_refresh", time.Since(refreshStart))
			if err == nil {
				newIDs := statusIDs(rows)
				entry.ids = mergeTimelineIDs(newIDs, entry.ids, limit)
				entry.generation = generation
				if !s.c.SetIfVersion(cacheVersion, key, entry, timelineEntrySize(key, entry.ids), s.ttl) {
					// A concurrent invalidation fenced the store. The merged IDs
					// still came from the authoritative DB moments ago, so serve
					// them uncached instead of paying a full cold rebuild on the
					// foreground request: a destructive write landing mid-refresh
					// is reflected one request late for this viewer only, and the
					// next read rebuilds cold.
					metrics.IncrementCacheInvalidation("timeline_refresh_fenced")
					slog.Debug("cache: timeline incremental refresh fenced", "timeline", timelineKind(key))
					s.c.Delete(key)
					release()
					if out, complete := s.hydrate(ctx, entry.ids); complete {
						return out, nil
					}
					return s.cached(ctx, key, limit, refresh, build)
				}
				slog.Debug("cache: timeline incrementally refreshed", "timeline", timelineKind(key), "new_ids", len(newIDs), "window_ids", len(entry.ids))
			} else {
				slog.Debug("cache: timeline incremental refresh failed", "timeline", timelineKind(key), "error", err)
			}
			release()
		}
		if entry.generation == generation {
			if out, complete := s.hydrate(ctx, entry.ids); complete {
				slog.Debug("cache: timeline hit", "timeline", timelineKind(key), "ids", len(entry.ids))
				return out, nil
			}
			slog.Debug("cache: timeline hydration incomplete", "timeline", timelineKind(key), "ids", len(entry.ids))
		}
		if entry.generation == generation && len(entry.ids) == 0 {
			return []*models.Status{}, nil
		}
		if out, complete := s.hydrate(ctx, entry.ids); complete && refresh == nil {
			return out, nil
		}
		lookup = cache.LookupResult("stale_or_incomplete")
		s.c.Delete(key)
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		stats := s.c.Stats()
		slog.Debug("cache: timeline rebuild", "timeline", timelineKind(key), "reason", lookup, "ttl", s.ttl, "entries", stats.Entries, "bytes", stats.Bytes, "max_bytes", stats.MaxBytes, "misses", stats.Misses, "evictions", stats.Evictions, "expirations", stats.Expirations)
	}

	s.mu.Lock()
	if call := s.inflight[key]; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return s.cached(ctx, key, limit, refresh, build)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &timelineCacheCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	s.mu.Lock()
	buildGeneration := s.generationForKey(key)
	s.mu.Unlock()
	cacheVersion := s.c.Version()
	buildStart := time.Now()
	rows, err := build()
	metrics.ObserveTimelineStage("timeline_query", time.Since(buildStart))
	if err == nil {
		call.ids = statusIDs(rows)
		// IDs dominate neither entity data nor rendered HTML; bill key and slice
		// overhead as well so the global byte bound remains conservative.
		s.mu.Lock()
		unchanged := buildGeneration == s.generationForKey(key)
		s.mu.Unlock()
		stored := false
		// A concurrent CreateStatus only advances the timeline generation; it
		// does not invalidate the cache version. Keep the completed window at
		// buildGeneration and let the next read merge the missing head delta.
		// Otherwise a busy federated instance can spend seconds rebuilding a
		// public/home window and discard it forever because another post always
		// arrives before the query completes. Destructive/membership-changing
		// writes advance the cache version, so SetIfVersion still fences them.
		if len(call.ids) <= s.maxEntries {
			stored = s.c.SetIfVersion(cacheVersion, key, timelineCacheEntry{ids: call.ids, generation: buildGeneration}, timelineEntrySize(key, call.ids), s.ttl)
		}
		slog.Debug("cache: timeline rebuilt", "timeline", timelineKind(key), "ids", len(call.ids), "stored", stored, "generation_unchanged", unchanged, "within_entry_limit", len(call.ids) <= s.maxEntries)
	} else {
		slog.Debug("cache: timeline rebuild failed", "timeline", timelineKind(key), "error", err)
	}
	call.err = err
	s.mu.Lock()
	delete(s.inflight, key)
	close(call.done)
	s.mu.Unlock()
	return rows, err
}

func timelineKind(key string) string {
	key = strings.TrimPrefix(key, "tl:")
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

func timelineEntrySize(key string, ids []string) int {
	size := 80 + len(key)
	for _, id := range ids {
		size += 16 + len(id)
	}
	return size
}

func (s *timelineCachingStorage) invalidateTimelines(cause string) {
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		stats := s.c.Stats()
		slog.Debug("cache: timeline invalidated", "cause", cause, "entries", stats.Entries, "bytes", stats.Bytes)
	}
	metrics.IncrementCacheInvalidation("timeline_clear:" + cause)
	s.c.Clear()
	s.advanceGenerations()
}

// invalidateTagTimelines drops every cached tag-timeline window (tag keys embed
// the viewer, so a per-tag prefix drop is not possible) and advances only the
// tag kind counter. Used when a status's tag set actually changes: far cheaper
// than the previous full Clear(), which cooled home/public windows on every
// remote edit.
func (s *timelineCachingStorage) invalidateTagTimelines() {
	metrics.IncrementCacheInvalidation("tag_windows")
	s.c.DeletePrefix("tl:tag:")
	s.bumpKinds("tag")
}

// advanceGenerations marks every generation-stamped snapshot (timeline windows
// and app-layer page entries) stale. Called by membership/moderation writes -
// all user-paced and rare - so dependent caches revalidate on next read
// instead of serving pre-write content until TTL.
func (s *timelineCachingStorage) advanceGenerations() {
	s.mu.Lock()
	s.generation++
	s.homeGeneration++
	s.mu.Unlock()
}

// TimelineGenerationFor exposes the effective generation of one window kind so
// the app-layer page cache can validate snapshots on read instead of being
// eagerly wiped on every status event. kind is a window kind ("public", "tag",
// "bubble", "direct", "bookmarks", ...) or "account:<id>" for profile windows;
// "home" resolves per viewer (global bumps plus this viewer's own bumps). An
// unknown kind degrades to the global generation, which only moderation-class
// events advance.
func (s *timelineCachingStorage) TimelineGenerationFor(viewerID, kind string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "home" {
		return s.homeGeneration + s.homeViewerGens[viewerID]
	}
	return s.generation + s.kindGens[kind]
}

// bumpKinds advances the given window kinds' counters under the lock, folding
// into a global bump when the map would grow unbounded.
func (s *timelineCachingStorage) bumpKinds(kinds ...string) {
	s.mu.Lock()
	if len(s.kindGens) >= maxKindGens {
		s.generation++
		s.kindGens = make(map[string]uint64)
	}
	for _, k := range kinds {
		s.kindGens[k]++
	}
	s.mu.Unlock()
}

// invalidateViewerTimelines drops only one viewer's timeline windows. Blocks,
// mutes, follows and per-user domain blocks change what that viewer may see -
// nobody else's windows are affected, and wiping them (the previous behavior)
// forced every user back onto the multi-second cold rebuild after any single
// user's moderation action.
func (s *timelineCachingStorage) invalidateViewerTimelines(viewerIDs ...string) {
	metrics.IncrementCacheInvalidation("viewer_timelines")
	for _, id := range viewerIDs {
		if id == "" {
			continue
		}
		for _, kind := range []string{"home", "list", "public", "tag", "bubble", "direct"} {
			s.c.DeletePrefix("tl:" + kind + ":" + id + "\x1f")
		}
	}
	s.advanceGenerations()
	slog.Debug("cache: viewer timelines invalidated", "viewers", viewerIDs)
}

// invalidateViewerHome drops only the viewer's home windows - follows and
// followed tags change home membership and nothing else.
func (s *timelineCachingStorage) invalidateViewerHome(viewerID string) {
	if viewerID == "" {
		return
	}
	metrics.IncrementCacheInvalidation("viewer_home")
	s.c.DeletePrefix("tl:home:" + viewerID + "\x1f")
	s.advanceGenerations()
	slog.Debug("cache: viewer home invalidated", "viewer", viewerID)
}
func (s *timelineCachingStorage) invalidateResponses() {
	if s.responses != nil {
		metrics.IncrementCacheInvalidation("responses_full")
		s.responses.DeletePrefix("fed:")
	}
}

// invalidateStatusResponses drops only one object's cached federation
// responses (the object document plus its likes/shares/quote-authorization
// collections, every Accept class and page - they all share the URI-first key
// prefix). Interaction writes use this instead of the full wipe: a remote
// boost or like storm delivers an inbound activity alongside a flood of
// fetches for the same object, and flushing the entire response cache on each
// activity would evict exactly the entries the flood needs.
func (s *timelineCachingStorage) invalidateStatusResponses(statusID string) {
	if s.responses != nil && statusID != "" {
		s.responses.DeletePrefix("fed:http:/objects/" + statusID)
	}
}

// invalidateAccountResponses drops one local actor's cached federation
// responses: the actor document plus its outbox/followers/following/featured
// collections, which all share the "/users/<username>" URI prefix.
func (s *timelineCachingStorage) invalidateAccountResponses(username string) {
	if s.responses != nil && username != "" {
		s.responses.DeletePrefix("fed:http:/users/" + username)
	}
}

// invalidateLocalStatusResponses drops only documents whose representation is
// changed by one local status write. It reports false when the author identity
// cannot be resolved, allowing callers to retain the full-wipe safety fallback.
func (s *timelineCachingStorage) invalidateLocalStatusResponses(ctx context.Context, st *models.Status) bool {
	if st == nil {
		return false
	}
	author := st.Account
	if author == nil && st.AccountID != "" {
		author, _ = s.Storage.GetAccount(ctx, st.AccountID)
	}
	if author == nil || author.Username == "" {
		return false
	}
	s.invalidateAccountResponses(author.Username)
	s.invalidateStatusResponses(st.ID)
	for _, ref := range []*string{st.BoostOfID, st.InReplyToID, st.QuoteOfID} {
		if ref != nil {
			s.invalidateStatusResponses(*ref)
		}
	}
	if s.responses != nil {
		s.responses.DeletePrefix("fed:http:/nodeinfo/")
	}
	metrics.IncrementCacheInvalidation("responses_local_status_scoped")
	return true
}

// isLocalStatus reports whether the status is one of ours: local statuses
// (including local boost wrappers) are always minted with the canonical
// ".../objects/<id>" URI, while ingested remote statuses keep the remote URI
// with the remote server's own path and identifier.
func isLocalStatus(st *models.Status) bool {
	return st != nil && strings.HasSuffix(st.URI, "/objects/"+st.ID)
}

// generationForKey returns the generation the cached window must match. Home
// windows embed the owning account as the key's first part, so they validate
// against that viewer's effective home generation; account (profile) windows
// validate against the profiled account's own counter; every other kind
// validates against its kind counter.
func (s *timelineCachingStorage) generationForKey(key string) uint64 {
	kind := timelineKind(key)
	switch kind {
	case "home":
		return s.homeGeneration + s.homeViewerGens[timelineKeyOwner(key, kind)]
	case "list":
		// List content is a subset of the owner's home content, so list
		// windows validate against the owner's effective home generation:
		// statusCreated already bumps exactly the owners whose lists can
		// change, and membership/policy writes drop the windows outright.
		return s.homeGeneration + s.homeViewerGens[timelineKeyOwner(key, kind)]
	case "account":
		return s.generation + s.kindGens["account:"+timelineKeyOwner(key, kind)]
	default:
		return s.generation + s.kindGens[kind]
	}
}

// timelineKeyOwner extracts the first key part (the owning account ID) from a
// window key ("tl:<kind>:<accountID>\x1f...").
func timelineKeyOwner(key, kind string) string {
	rest := strings.TrimPrefix(key, "tl:"+kind+":")
	if i := strings.IndexByte(rest, '\x1f'); i >= 0 {
		return rest[:i]
	}
	return rest
}

type homeTimelineAudience interface {
	HomeTimelineViewers(context.Context, *models.Status) ([]string, error)
}

func (s *timelineCachingStorage) statusCreated(ctx context.Context, status *models.Status) {
	if status == nil {
		return
	}
	// Bump only the window kinds this status's visibility can reach. Context-only
	// thread backfills appear in no timeline at all; unlisted posts (the bulk of
	// shared-inbox bot traffic) can reach tag/bubble windows but never public
	// ones; private posts reach only their author's followers' homes.
	if !status.ContextOnly {
		switch status.Visibility {
		case models.VisibilityPublic, models.VisibilityLocal:
			s.bumpKinds("public", "tag", "bubble", "account:"+status.AccountID)
		case models.VisibilityUnlisted, models.VisibilityLocalUnlisted:
			s.bumpKinds("tag", "bubble", "account:"+status.AccountID)
		case models.VisibilityPrivate:
			// Reaches no shared timeline, but the author's own profile window
			// shows it.
			s.bumpKinds("account:" + status.AccountID)
		case models.VisibilityDirect:
			s.bumpKinds("direct")
		}
		slog.Debug("cache: timeline kinds advanced", "status", status.ID, "visibility", status.Visibility)
	}
	if authorGen, ok := s.bumpHomeViewers(ctx, status); ok {
		s.prependOwnStatusToHomeWindows(status, authorGen)
	}
}

// bumpHomeViewers bumps only the affected local viewers' home generations.
// Shared-inbox deliveries from unfollowed actors bump nobody; a post from a
// followed author marks exactly the followers' home snapshots stale. On error
// (or for alternative Storage implementations) fall back to the global bump.
// When the author themself is among the bumped viewers (a local post), the
// author's post-bump home generation is returned, captured atomically with
// the bump so the write-through fast path can stamp windows safely.
func (s *timelineCachingStorage) bumpHomeViewers(ctx context.Context, status *models.Status) (uint64, bool) {
	// Fan-out already computed the audience during CreateStatus and stashed it
	// on the status; reuse it instead of re-running the follower/tag-follower
	// JOIN on the write path. nil means not computed (query below).
	if status.HomeFeedViewers != nil {
		return s.bumpViewerHomeGens(status, status.HomeFeedViewers)
	}
	if audience, ok := s.Storage.(homeTimelineAudience); ok {
		viewers, err := audience.HomeTimelineViewers(ctx, status)
		if err == nil {
			return s.bumpViewerHomeGens(status, viewers)
		}
		slog.Debug("cache: home audience check failed", "status", status.ID, "error", err)
	}
	s.mu.Lock()
	s.homeGeneration++
	homeGeneration := s.homeGeneration
	s.mu.Unlock()
	slog.Debug("cache: home timeline generation advanced globally", "generation", homeGeneration, "status", status.ID)
	return 0, false
}

// bumpViewerHomeGens advances the given viewers' home generations and returns
// the author's post-bump generation when the author is among them (captured
// atomically with the bump for the write-through fast path).
func (s *timelineCachingStorage) bumpViewerHomeGens(status *models.Status, viewers []string) (uint64, bool) {
	var authorGen uint64
	authorBumped := false
	if len(viewers) > 0 {
		s.mu.Lock()
		for _, id := range viewers {
			s.homeViewerGens[id]++
			if id == status.AccountID {
				authorBumped = true
			}
		}
		if authorBumped {
			authorGen = s.homeGeneration + s.homeViewerGens[status.AccountID]
		}
		s.mu.Unlock()
		slog.Debug("cache: home generation advanced for viewers", "status", status.ID, "viewers", len(viewers))
	} else {
		slog.Debug("cache: home timeline unchanged", "status", status.ID, "account", status.AccountID)
	}
	return authorGen, authorBumped
}

// prependOwnStatusToHomeWindows write-through-caches the author's own new
// status into their cached home head windows, so the post-redirect home read
// is a pure window hit instead of an SD-card refresh query. Scope is
// narrow:
//   - Only the author's own windows. Prepending into other viewers' windows
//     would bypass read-time moderation (blocks/mutes); other viewers are
//     served stale pages with a background refresh anyway.
//   - Only windows that were fresh immediately before this post
//     (entry.generation == authorGen-1). A window already lagging other posts
//     must not be stamped current with only this one added.
//   - Eligibility mirrors the GetHomeTimeline predicates (visibility set,
//     context_only, reply/boost flags). A window whose flags exclude this
//     status is stamped without the ID - its content is unchanged by the post.
//
// Correctness never depends on this: any skipped or fenced window simply
// stays generation-stale and takes the normal incremental refresh.
func (s *timelineCachingStorage) prependOwnStatusToHomeWindows(status *models.Status, authorGen uint64) {
	if status.ContextOnly || status.ID == "" || status.AccountID == "" {
		return
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityLocal, models.VisibilityLocalUnlisted, models.VisibilityPrivate:
	default:
		return
	}
	isReply := (status.InReplyToID != nil && *status.InReplyToID != "") ||
		(status.InReplyToURI != nil && *status.InReplyToURI != "")
	isBoost := status.BoostOfID != nil
	window := strconv.Itoa(headWindowLimit)
	for _, excludeReplies := range []bool{false, true} {
		for _, excludeReblogs := range []bool{false, true} {
			key := timelineKey("home", status.AccountID, timelinePtr(nil), timelinePtr(nil), timelinePtr(nil), window, boolString(excludeReplies), boolString(excludeReblogs))
			v, ok := s.c.Get(key)
			if !ok {
				continue
			}
			entry := v.(timelineCacheEntry)
			if entry.generation != authorGen-1 {
				continue
			}
			version := s.c.Version()
			if !(excludeReplies && isReply) && !(excludeReblogs && isBoost) {
				entry.ids = mergeTimelineIDs([]string{status.ID}, entry.ids, headWindowLimit)
			}
			entry.generation = authorGen
			if s.c.SetIfVersion(version, key, entry, timelineEntrySize(key, entry.ids), s.ttl) {
				slog.Debug("cache: home window write-through", "status", status.ID, "author", status.AccountID, "exclude_replies", excludeReplies, "exclude_reblogs", excludeReblogs)
			}
		}
	}
}

// TimelineStatusCreated applies a status-created event received from another
// process through the PostgreSQL broker. It advances generations without
// deleting the cached ID windows, allowing the next read to use the incremental
// head refresh instead of rebuilding the full timeline.
func (s *timelineCachingStorage) TimelineStatusCreated(ctx context.Context, status *models.Status) {
	if status != nil {
		s.statusCreated(ctx, status)
	}
}

// headWindowLimit is the shared size of cached head windows. Front ends ask
// for different page sizes (web UI 20, Mastodon apps anywhere from 15 to 40);
// caching one window per limit meant each client's first request was a cold
// multi-second rebuild. Head requests up to this size share one window and
// get a slice of it.
const headWindowLimit = 40

func headWindow(head bool, limit int) int {
	if head && limit < headWindowLimit {
		return headWindowLimit
	}
	return limit
}

func sliceHead(rows []*models.Status, err error, limit int) ([]*models.Status, error) {
	if err == nil && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, err
}

func (s *timelineCachingStorage) GetHomeTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	key := timelineKey("home", accountID, timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), boolString(excludeReplies), boolString(excludeReblogs))
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetHomeTimeline(ctx, accountID, nil, &newest, nil, window, excludeReplies, excludeReblogs)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetHomeTimeline(ctx, accountID, maxID, minID, sinceID, window, excludeReplies, excludeReblogs)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetListTimeline(ctx context.Context, list *models.List, maxID, minID, sinceID *string, limit int) ([]*models.Status, error) {
	if list == nil {
		return s.Storage.GetListTimeline(ctx, list, maxID, minID, sinceID, limit)
	}
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	// Owner first so timelineKeyOwner resolves the home generation; the policy
	// fields are part of the key because an UpdateList drops the prefix anyway,
	// but a stale *models.List from a caller must not alias another policy's rows.
	key := timelineKey("list", list.AccountID, list.ID, timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), list.RepliesPolicy)
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetListTimeline(ctx, list, nil, &newest, nil, window)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetListTimeline(ctx, list, maxID, minID, sinceID, window)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetPublicTimeline(ctx context.Context, viewerID string, local bool, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	// Mirror the local-timeline reblog rule the inner storage applies, so both
	// hide-boosts preferences share one cache entry instead of filling two with
	// identical rows.
	if local {
		excludeReblogs = true
	}
	key := timelineKey("public", viewerID, boolString(local), timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), boolString(excludeReplies), boolString(excludeReblogs))
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetPublicTimeline(ctx, viewerID, local, nil, &newest, nil, window, excludeReplies, excludeReblogs)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetPublicTimeline(ctx, viewerID, local, maxID, minID, sinceID, window, excludeReplies, excludeReblogs)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetAccountStatuses(ctx context.Context, viewerID, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, onlyMedia, pinnedOnly bool) ([]*models.Status, error) {
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	// Only the owner's view differs (it includes followers-only posts), so the
	// key splits on that bit rather than on the full viewer ID.
	key := timelineKey("account", accountID, boolString(viewerID != "" && viewerID == accountID), timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), boolString(excludeReplies), boolString(excludeReblogs), boolString(onlyMedia), boolString(pinnedOnly))
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetAccountStatuses(ctx, viewerID, accountID, nil, &newest, nil, window, excludeReplies, excludeReblogs, onlyMedia, pinnedOnly)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetAccountStatuses(ctx, viewerID, accountID, maxID, minID, sinceID, window, excludeReplies, excludeReblogs, onlyMedia, pinnedOnly)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetTagTimeline(ctx context.Context, viewerID, tag string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, localOnly bool) ([]*models.Status, error) {
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	key := timelineKey("tag", viewerID, strings.ToLower(tag), timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), boolString(excludeReplies), boolString(excludeReblogs), boolString(localOnly))
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetTagTimeline(ctx, viewerID, tag, nil, &newest, nil, window, excludeReplies, excludeReblogs, localOnly)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetTagTimeline(ctx, viewerID, tag, maxID, minID, sinceID, window, excludeReplies, excludeReblogs, localOnly)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetBubbleTimeline(ctx context.Context, viewerID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	head := maxID == nil && minID == nil && sinceID == nil
	window := headWindow(head, limit)
	key := timelineKey("bubble", viewerID, timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(window), boolString(excludeReplies), boolString(excludeReblogs))
	var refresh func(string) ([]*models.Status, error)
	if head {
		refresh = func(newest string) ([]*models.Status, error) {
			return s.Storage.GetBubbleTimeline(ctx, viewerID, nil, &newest, nil, window, excludeReplies, excludeReblogs)
		}
	}
	rows, err := s.cached(ctx, key, window, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetBubbleTimeline(ctx, viewerID, maxID, minID, sinceID, window, excludeReplies, excludeReblogs)
	})
	return sliceHead(rows, err, limit)
}

func (s *timelineCachingStorage) GetDirectTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int) ([]*models.Status, error) {
	key := timelineKey("direct", accountID, timelinePtr(maxID), timelinePtr(minID), timelinePtr(sinceID), strconv.Itoa(limit))
	var refresh func(string) ([]*models.Status, error)
	if maxID == nil && minID == nil && sinceID == nil {
		refresh = func(head string) ([]*models.Status, error) {
			return s.Storage.GetDirectTimeline(ctx, accountID, nil, &head, nil, limit)
		}
	}
	return s.cached(ctx, key, limit, refresh, func() ([]*models.Status, error) {
		return s.Storage.GetDirectTimeline(ctx, accountID, maxID, minID, sinceID, limit)
	})
}

func (s *timelineCachingStorage) ListBookmarks(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error) {
	key := timelineKey("bookmarks", accountID, timelinePtr(maxID), strconv.Itoa(limit))
	return s.cached(ctx, key, limit, nil, func() ([]*models.Status, error) { return s.Storage.ListBookmarks(ctx, accountID, maxID, limit) })
}

func (s *timelineCachingStorage) ListFavourites(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error) {
	key := timelineKey("favourites", accountID, timelinePtr(maxID), strconv.Itoa(limit))
	return s.cached(ctx, key, limit, nil, func() ([]*models.Status, error) { return s.Storage.ListFavourites(ctx, accountID, maxID, limit) })
}

// Writes invalidate membership only after the database commit succeeds. Broad
// invalidation is conservative: correctness and moderation take
// priority, while lazy rebuilding keeps write latency independent of fan-out.
func (s *timelineCachingStorage) CreateStatus(ctx context.Context, st *models.Status) error {
	err := s.Storage.CreateStatus(ctx, st)
	s.afterStatusCreated(ctx, st, err)
	return err
}

func (s *timelineCachingStorage) CreateStatusWithJobs(ctx context.Context, st *models.Status, jobs []*models.Job) error {
	err := s.Storage.CreateStatusWithJobs(ctx, st, jobs)
	s.afterStatusCreated(ctx, st, err)
	return err
}

func (s *timelineCachingStorage) afterStatusCreated(ctx context.Context, st *models.Status, err error) {
	if err == nil {
		s.statusCreated(ctx, st)
		if st != nil && !isLocalStatus(st) {
			// Remote statuses arrive with nearly every inbound activity, so
			// invalidation must be surgical or the response cache never
			// survives federation traffic. A remote boost wrapper changes only
			// the boosted object's shares collection; a remote original/reply
			// changes at most the local objects it references. Nothing else we
			// serve (actor documents, outboxes, discovery) is affected.
			if st.BoostOfID != nil {
				s.invalidateStatusResponses(*st.BoostOfID)
			}
			if st.InReplyToID != nil {
				s.invalidateStatusResponses(*st.InReplyToID)
			}
			if st.QuoteOfID != nil {
				s.invalidateStatusResponses(*st.QuoteOfID)
			}
		} else if !s.invalidateLocalStatusResponses(ctx, st) {
			s.invalidateResponses()
		}
	}
}
func (s *timelineCachingStorage) UpdateStatus(ctx context.Context, st *models.Status) error {
	before, beforeErr := s.Storage.GetStatus(ctx, st.ID)
	err := s.Storage.UpdateStatus(ctx, st)
	if err == nil {
		if beforeErr != nil || statusMembershipChanged(before, st) {
			s.invalidateTimelines("update_status")
		}
		if isLocalStatus(st) {
			if !s.invalidateLocalStatusResponses(ctx, st) {
				s.invalidateResponses()
			}
		} else {
			// A remote edit changes no locally served actor/outbox document;
			// at most the local objects it references embed stale interaction
			// counts, and those are federation_ttl-bounded. Drop only the
			// object's own response prefix (harmless if we never served it).
			s.invalidateStatusResponses(st.ID)
		}
	}
	return err
}

func (s *timelineCachingStorage) UpdateStatusContent(ctx context.Context, st *models.Status) error {
	err := s.Storage.UpdateStatusContent(ctx, st)
	if err == nil {
		if isLocalStatus(st) {
			if !s.invalidateLocalStatusResponses(ctx, st) {
				s.invalidateResponses()
			}
		} else {
			s.invalidateStatusResponses(st.ID)
		}
	}
	return err
}

func (s *timelineCachingStorage) UpdateStatusQuoteState(ctx context.Context, statusID, state string, approvalURI *string) error {
	status, statusErr := s.Storage.GetStatus(ctx, statusID)
	err := s.Storage.UpdateStatusQuoteState(ctx, statusID, state, approvalURI)
	if err == nil {
		s.invalidateTimelines("update_status_quote")
		if statusErr == nil && isLocalStatus(status) {
			if !s.invalidateLocalStatusResponses(ctx, status) {
				s.invalidateResponses()
			}
		} else {
			s.invalidateStatusResponses(statusID)
		}
	}
	return err
}

func (s *timelineCachingStorage) AdjustStatusRepliesCount(ctx context.Context, statusID string, delta int) error {
	err := s.Storage.AdjustStatusRepliesCount(ctx, statusID, delta)
	if err == nil {
		s.invalidateStatusResponses(statusID)
	}
	return err
}

func statusMembershipChanged(a, b *models.Status) bool {
	if a == nil || b == nil {
		return true
	}
	// CreatedAt is compared with a tolerance: the DB roundtrip truncates to
	// microseconds while an in-memory status carries the clock's nanoseconds,
	// so CreateStatus's own follow-up UpdateStatus (mention-link rewrite) would
	// otherwise register as a membership change and wipe every timeline window
	// on each local post. Real re-datings are far larger than the truncation.
	createdAtDrift := a.CreatedAt.Sub(b.CreatedAt)
	if createdAtDrift < 0 {
		createdAtDrift = -createdAtDrift
	}
	return a.AccountID != b.AccountID || a.Visibility != b.Visibility || createdAtDrift >= time.Millisecond ||
		!sameStringPtr(a.InReplyToID, b.InReplyToID) || !sameStringPtr(a.BoostOfID, b.BoostOfID) ||
		a.ContextOnly != b.ContextOnly
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func (s *timelineCachingStorage) DeleteStatus(ctx context.Context, id string) error {
	before, beforeErr := s.Storage.GetStatus(ctx, id)
	boostAccounts, _ := s.Storage.ListReblogAccounts(ctx, id, -1)
	err := s.Storage.DeleteStatus(ctx, id)
	if err == nil {
		// Timeline windows self-heal: hydration misses the deleted ID and only
		// the windows that contained it rebuild. Everything else stays warm.
		if beforeErr != nil || before == nil || (isLocalStatus(before) && !s.invalidateLocalStatusResponses(ctx, before)) {
			s.invalidateResponses()
		} else if !isLocalStatus(before) {
			s.invalidateStatusResponses(id)
		}
		// Deleting an original also deletes its Announce wrappers. Their local
		// authors' outbox/featured documents must not retain those wrappers.
		for _, account := range boostAccounts {
			if account != nil && account.Domain == nil {
				s.invalidateAccountResponses(account.Username)
			}
		}
	}
	return err
}
func (s *timelineCachingStorage) UpdateAccount(ctx context.Context, account *models.Account) error {
	before, beforeErr := s.Storage.GetAccount(ctx, account.ID)
	err := s.Storage.UpdateAccount(ctx, account)
	if err == nil {
		if beforeErr != nil || accountModerationChanged(before, account) {
			// Moderation state changes what every timeline may show.
			s.invalidateTimelines("account_moderation")
			s.invalidateResponses()
		} else if account.Domain == nil {
			// A local profile edit only changes that actor's own documents
			// (actor, outbox, collections - they share the URI prefix).
			s.invalidateAccountResponses(account.Username)
		}
		// Remote account refreshes (profile/count merges happen on nearly
		// every inbound activity) change no timeline membership and no
		// locally served federation response: leave both caches warm.
		// Component/page copies of the profile are page_ttl-bounded.
	}
	return err
}

// accountModerationChanged reports whether an account update changes timeline
// visibility - the only account fields `applyTimelineModeration` filters on.
// Routine profile edits and the remote follower/status count merges that
// accompany nearly every inbound activity must not flush the timeline cache,
// or the incremental head-refresh path never gets a window to refresh.
func accountModerationChanged(a, b *models.Account) bool {
	if a == nil || b == nil {
		return true
	}
	suspendedA := a.SuspendedAt != nil
	suspendedB := b.SuspendedAt != nil
	return a.ModerationState != b.ModerationState || suspendedA != suspendedB
}

// DeleteAccount removes the account and its statuses (bulk, in bunStorage).
// Remote actor self-deletes arrive constantly on a federated instance, so no
// global wipe here: windows containing the deleted statuses self-heal via
// incomplete hydration (same as DeleteStatus), pages revalidate through the
// advanced generations, and only a local account's served documents change.
func (s *timelineCachingStorage) DeleteAccount(ctx context.Context, id string) error {
	before, beforeErr := s.Storage.GetAccount(ctx, id)
	err := s.Storage.DeleteAccount(ctx, id)
	if err == nil {
		s.advanceGenerations()
		metrics.IncrementCacheInvalidation("account_delete_scoped")
		if beforeErr != nil || before == nil || before.Domain == nil {
			// Local (or unknown) account: its actor/outbox documents were
			// served - take the full response wipe (admin-paced, rare).
			s.invalidateResponses()
		}
	}
	return err
}

func (s *timelineCachingStorage) ScheduleAccountDeletion(ctx context.Context, accountID string, deadline time.Time) (*models.AccountDeletionRun, bool, error) {
	run, created, err := s.Storage.ScheduleAccountDeletion(ctx, accountID, deadline)
	if err == nil && created {
		s.advanceGenerations()
		s.invalidateResponses()
		metrics.IncrementCacheInvalidation("account_deletion_requested")
	}
	return run, created, err
}

func (s *timelineCachingStorage) CompleteAccountDeletion(ctx context.Context, runID string) error {
	err := s.Storage.CompleteAccountDeletion(ctx, runID)
	if err == nil {
		s.advanceGenerations()
		s.invalidateResponses()
		metrics.IncrementCacheInvalidation("account_deletion_complete")
	}
	return err
}
func (s *timelineCachingStorage) CreateFollow(ctx context.Context, f *models.Follow) error {
	err := s.Storage.CreateFollow(ctx, f)
	if err == nil {
		if f != nil {
			s.invalidateViewerHome(f.AccountID)
		}
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expected string, follow *models.Follow, jobs []*models.Job) (bool, error) {
	advanced, err := s.Storage.AdvanceFollowImport(ctx, imp, expected, follow, jobs)
	if err == nil && advanced && follow != nil {
		s.invalidateViewerHome(follow.AccountID)
		s.invalidateResponses()
	}
	return advanced, err
}
func (s *timelineCachingStorage) DeleteFollow(ctx context.Context, id string) error {
	follow, followErr := s.Storage.GetFollow(ctx, id)
	err := s.Storage.DeleteFollow(ctx, id)
	if err == nil {
		if followErr == nil && follow != nil {
			s.invalidateViewerHome(follow.AccountID)
			// Unfollow removes the target from the follower's lists (bun-layer
			// hook), so the follower's list windows may have lost an author.
			s.c.DeletePrefix("tl:list:" + follow.AccountID + "\x1f")
		} else {
			// Unknown follower: fall back to the safe full wipe.
			s.invalidateTimelines("delete_follow_unknown")
		}
		s.invalidateResponses()
	}
	return err
}

// List membership and policy writes cannot be expressed as incremental head
// refreshes (rows may need to disappear from the middle of a window), so they
// drop the affected list windows outright. They also go through
// invalidateViewerHome because an exclusive list changes which authors the
// owner's home hides - and the generation advance is what makes stale page
// snapshots revalidate. All of these are user-paced writes.
func (s *timelineCachingStorage) listMembershipChanged(ownerID, listID string) {
	if ownerID == "" {
		// Unknown owner: safe full wipe, same fallback as unknown follows.
		s.invalidateTimelines("list_write_unknown")
		return
	}
	if listID == "" {
		s.c.DeletePrefix("tl:list:" + ownerID + "\x1f")
	} else {
		s.c.DeletePrefix(timelineKey("list", ownerID, listID) + "\x1f")
	}
	s.invalidateViewerHome(ownerID)
}

// cachedListOwner resolves a list's owner for invalidation scoping (the
// read-before-write pattern DeleteFollow/AcceptFollow use).
func (s *timelineCachingStorage) cachedListOwner(ctx context.Context, listID string) string {
	list, err := s.Storage.GetList(ctx, listID)
	if err != nil || list == nil {
		return ""
	}
	return list.AccountID
}

func (s *timelineCachingStorage) AddAccountToList(ctx context.Context, listID, accountID string) error {
	owner := s.cachedListOwner(ctx, listID)
	err := s.Storage.AddAccountToList(ctx, listID, accountID)
	if err == nil {
		s.listMembershipChanged(owner, listID)
	}
	return err
}

func (s *timelineCachingStorage) RemoveAccountFromList(ctx context.Context, listID, accountID string) error {
	owner := s.cachedListOwner(ctx, listID)
	err := s.Storage.RemoveAccountFromList(ctx, listID, accountID)
	if err == nil {
		s.listMembershipChanged(owner, listID)
	}
	return err
}

func (s *timelineCachingStorage) RemoveAccountFromAllLists(ctx context.Context, ownerID, memberID string) (bool, error) {
	removed, err := s.Storage.RemoveAccountFromAllLists(ctx, ownerID, memberID)
	if err == nil && removed {
		s.listMembershipChanged(ownerID, "")
	}
	return removed, err
}

func (s *timelineCachingStorage) UpdateList(ctx context.Context, list *models.List) error {
	err := s.Storage.UpdateList(ctx, list)
	if err == nil && list != nil {
		// replies_policy/exclusive flips change window content.
		s.listMembershipChanged(list.AccountID, list.ID)
	}
	return err
}

func (s *timelineCachingStorage) DeleteList(ctx context.Context, id string) error {
	owner := s.cachedListOwner(ctx, id)
	err := s.Storage.DeleteList(ctx, id)
	if err == nil {
		s.listMembershipChanged(owner, id)
	}
	return err
}

// AcceptFollow and UpdateFollow change follow state (pending -> accepted), which
// changes home membership for the follower. Without these overrides a follow
// acceptance left the follower's cached home window without the new followee's
// back-catalog until TTL or LRU eviction.
func (s *timelineCachingStorage) AcceptFollow(ctx context.Context, id string) error {
	follow, followErr := s.Storage.GetFollow(ctx, id)
	err := s.Storage.AcceptFollow(ctx, id)
	if err == nil {
		if followErr == nil && follow != nil {
			s.invalidateViewerHome(follow.AccountID)
		} else {
			s.invalidateTimelines("accept_follow_unknown")
		}
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) UpdateFollow(ctx context.Context, f *models.Follow) error {
	err := s.Storage.UpdateFollow(ctx, f)
	if err == nil {
		if f != nil {
			s.invalidateViewerHome(f.AccountID)
		}
		s.invalidateResponses()
	}
	return err
}
func (s *timelineCachingStorage) FollowTag(ctx context.Context, accountID, tagID string) error {
	err := s.Storage.FollowTag(ctx, accountID, tagID)
	if err == nil {
		s.invalidateViewerHome(accountID)
	}
	return err
}
func (s *timelineCachingStorage) UnfollowTag(ctx context.Context, accountID, tagID string) error {
	err := s.Storage.UnfollowTag(ctx, accountID, tagID)
	if err == nil {
		s.invalidateViewerHome(accountID)
	}
	return err
}
func (s *timelineCachingStorage) CreateBookmark(ctx context.Context, b *models.Bookmark) error {
	err := s.Storage.CreateBookmark(ctx, b)
	if err == nil && b != nil {
		// A bookmark only changes the bookmarking user's own bookmark feed.
		s.c.DeletePrefix(timelineKey("bookmarks", b.AccountID))
	}
	return err
}
func (s *timelineCachingStorage) DeleteBookmark(ctx context.Context, accountID, statusID string) error {
	err := s.Storage.DeleteBookmark(ctx, accountID, statusID)
	if err == nil {
		s.c.DeletePrefix(timelineKey("bookmarks", accountID))
	}
	return err
}
func (s *timelineCachingStorage) CreateBlock(ctx context.Context, b *models.Block) error {
	err := s.Storage.CreateBlock(ctx, b)
	if err == nil && b != nil {
		// Blocks hide in both directions, so both parties' windows go stale.
		s.invalidateViewerTimelines(b.AccountID, b.TargetAccountID)
	}
	return err
}
func (s *timelineCachingStorage) DeleteBlock(ctx context.Context, accountID, targetID string) error {
	err := s.Storage.DeleteBlock(ctx, accountID, targetID)
	if err == nil {
		s.invalidateViewerTimelines(accountID, targetID)
	}
	return err
}
func (s *timelineCachingStorage) CreateMute(ctx context.Context, m *models.Mute) error {
	err := s.Storage.CreateMute(ctx, m)
	if err == nil && m != nil {
		s.invalidateViewerTimelines(m.AccountID)
	}
	return err
}
func (s *timelineCachingStorage) DeleteMute(ctx context.Context, accountID, targetID string) error {
	err := s.Storage.DeleteMute(ctx, accountID, targetID)
	if err == nil {
		s.invalidateViewerTimelines(accountID)
	}
	return err
}
func (s *timelineCachingStorage) CreateAccountDomainBlock(ctx context.Context, accountID, domain string) error {
	err := s.Storage.CreateAccountDomainBlock(ctx, accountID, domain)
	if err == nil {
		s.invalidateViewerTimelines(accountID)
	}
	return err
}
func (s *timelineCachingStorage) DeleteAccountDomainBlock(ctx context.Context, accountID, domain string) error {
	err := s.Storage.DeleteAccountDomainBlock(ctx, accountID, domain)
	if err == nil {
		s.invalidateViewerTimelines(accountID)
	}
	return err
}

func (s *timelineCachingStorage) DeleteExpiredMutes(ctx context.Context) ([]string, error) {
	viewers, err := s.Storage.DeleteExpiredMutes(ctx)
	if err == nil && len(viewers) > 0 {
		// Only the muting viewers' windows can change; everyone else stays warm.
		s.invalidateViewerTimelines(viewers...)
	}
	return viewers, err
}

func (s *timelineCachingStorage) CreateDomainBlock(ctx context.Context, b *models.DomainBlock) error {
	err := s.Storage.CreateDomainBlock(ctx, b)
	if err == nil {
		s.invalidateTimelines("domain_block")
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) DeleteDomainBlock(ctx context.Context, id string) error {
	err := s.Storage.DeleteDomainBlock(ctx, id)
	if err == nil {
		s.invalidateTimelines("domain_block")
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) SuspendInstance(ctx context.Context, id string, suspended bool) error {
	err := s.Storage.SuspendInstance(ctx, id, suspended)
	if err == nil {
		s.invalidateTimelines("suspend_instance")
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) CreateBubbleInstance(ctx context.Context, b *models.BubbleInstance) error {
	err := s.Storage.CreateBubbleInstance(ctx, b)
	if err == nil {
		s.invalidateTimelines("bubble")
	}
	return err
}

func (s *timelineCachingStorage) DeleteBubbleInstance(ctx context.Context, id string) error {
	err := s.Storage.DeleteBubbleInstance(ctx, id)
	if err == nil {
		s.invalidateTimelines("bubble")
	}
	return err
}

func (s *timelineCachingStorage) PinStatus(ctx context.Context, statusID string) error {
	err := s.Storage.PinStatus(ctx, statusID)
	if err == nil {
		s.invalidateTimelines("pin")
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) UnpinStatus(ctx context.Context, statusID string) error {
	err := s.Storage.UnpinStatus(ctx, statusID)
	if err == nil {
		s.invalidateTimelines("pin")
		s.invalidateResponses()
	}
	return err
}

func (s *timelineCachingStorage) CreateMentions(ctx context.Context, mentions []*models.Mention) error {
	err := s.Storage.CreateMentions(ctx, mentions)
	if err == nil {
		// Mentions render inside their status object only.
		for _, m := range mentions {
			if m != nil {
				s.invalidateStatusResponses(m.StatusID)
			}
		}
	}
	return err
}

func (s *timelineCachingStorage) CreateMention(ctx context.Context, mention *models.Mention) error {
	err := s.Storage.CreateMention(ctx, mention)
	if err == nil {
		if mention != nil {
			s.invalidateStatusResponses(mention.StatusID)
		}
	}
	return err
}

func (s *timelineCachingStorage) CreateStatusTags(ctx context.Context, tags []*models.StatusTag) error {
	err := s.Storage.CreateStatusTags(ctx, tags)
	if err == nil {
		seen := make(map[string]struct{}, len(tags))
		for _, t := range tags {
			if t != nil {
				if _, ok := seen[t.StatusID]; !ok {
					seen[t.StatusID] = struct{}{}
					if status, getErr := s.Storage.GetStatus(ctx, t.StatusID); getErr == nil {
						// Late-attached tags change tag-window membership and can
						// pull the status into tag-followers' homes; other window
						// kinds already took their bump in CreateStatus.
						if !status.ContextOnly {
							s.bumpKinds("tag")
						}
						// Chain the author write-through: a local post with
						// hashtags takes this second bump right after
						// CreateStatus, and without the re-stamp the author's
						// just-refreshed window would lag one generation and
						// pay a refresh query on the post-redirect read.
						if authorGen, ok := s.bumpHomeViewers(ctx, status); ok {
							s.prependOwnStatusToHomeWindows(status, authorGen)
						}
					} else {
						s.invalidateTimelines("status_tags_fallback")
					}
				}
				s.invalidateStatusResponses(t.StatusID)
			}
		}
	}
	return err
}

func (s *timelineCachingStorage) DeleteStatusTags(ctx context.Context, statusID string) error {
	err := s.Storage.DeleteStatusTags(ctx, statusID)
	if err == nil {
		// Tag edits change tag-timeline membership of an existing status, which
		// the generation head refresh cannot express: full timeline wipe (rare,
		// edit-paced), but only the status's own responses.
		s.invalidateTimelines("delete_status_tags")
		s.invalidateStatusResponses(statusID)
	}
	return err
}

// SyncStatusTags invalidates only when the tag set actually changed. Remote
// edits (and Mastodon poll tally refreshes, delivered as Update) run this on
// every activity; with unchanged tags nothing any timeline serves is affected,
// and the previous unconditional full Clear() was the main reason timeline
// windows never survived routine federation traffic.
func (s *timelineCachingStorage) SyncStatusTags(ctx context.Context, statusID string, tags []*models.Tag) (bool, error) {
	changed, err := s.Storage.SyncStatusTags(ctx, statusID, tags)
	if err == nil && changed {
		s.invalidateTagTimelines()
		s.invalidateStatusResponses(statusID)
		// A changed tag set can add the status to (or drop it from) tag-followers'
		// home feeds; mark exactly those viewers' homes stale.
		if status, getErr := s.Storage.GetStatus(ctx, statusID); getErr == nil {
			s.bumpHomeViewers(ctx, status)
		}
	}
	return changed, err
}

// A reblog-count bump only changes the status's served object (same contract
// as an inbound Like); timeline windows stay warm.
func (s *timelineCachingStorage) AdjustStatusReblogsCount(ctx context.Context, statusID string, delta int) error {
	err := s.Storage.AdjustStatusReblogsCount(ctx, statusID, delta)
	if err == nil {
		s.invalidateStatusResponses(statusID)
	}
	return err
}

func (s *timelineCachingStorage) CreateFavourite(ctx context.Context, f *models.Favourite) error {
	err := s.Storage.CreateFavourite(ctx, f)
	if err == nil && f != nil {
		// A favourite only changes the status object, its likes collection,
		// and the favouriting user's own favourites feed.
		s.invalidateStatusResponses(f.StatusID)
		s.c.DeletePrefix(timelineKey("favourites", f.AccountID))
	}
	return err
}
func (s *timelineCachingStorage) DeleteFavourite(ctx context.Context, accountID, statusID string) error {
	err := s.Storage.DeleteFavourite(ctx, accountID, statusID)
	if err == nil {
		s.invalidateStatusResponses(statusID)
		s.c.DeletePrefix(timelineKey("favourites", accountID))
	}
	return err
}

func (s *timelineCachingStorage) CreateEmojiReaction(ctx context.Context, reaction *models.EmojiReaction) (bool, error) {
	created, err := s.Storage.CreateEmojiReaction(ctx, reaction)
	if err == nil && created && reaction != nil {
		s.invalidateStatusResponses(reaction.StatusID)
	}
	return created, err
}

func (s *timelineCachingStorage) DeleteEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error) {
	reaction, err := s.Storage.DeleteEmojiReaction(ctx, accountID, statusID, key)
	if err == nil {
		s.invalidateStatusResponses(statusID)
	}
	return reaction, err
}

func (s *timelineCachingStorage) DeleteEmojiReactionByURI(ctx context.Context, accountID, uri string) (*models.EmojiReaction, error) {
	reaction, err := s.Storage.DeleteEmojiReactionByURI(ctx, accountID, uri)
	if err == nil && reaction != nil {
		s.invalidateStatusResponses(reaction.StatusID)
	}
	return reaction, err
}

// Polls arrive attached to remote statuses (and their tallies as Updates);
// only the owning status's served object changes.
func (s *timelineCachingStorage) CreatePoll(ctx context.Context, p *models.Poll) error {
	err := s.Storage.CreatePoll(ctx, p)
	if err == nil && p != nil && p.StatusID != "" {
		s.invalidateStatusResponses(p.StatusID)
	}
	return err
}
func (s *timelineCachingStorage) SyncPoll(ctx context.Context, p *models.Poll) error {
	err := s.Storage.SyncPoll(ctx, p)
	if err == nil && p != nil && p.StatusID != "" {
		s.invalidateStatusResponses(p.StatusID)
	}
	return err
}
func (s *timelineCachingStorage) VotePoll(ctx context.Context, pollID, accountID string, choices []int) (*models.Poll, error) {
	p, err := s.Storage.VotePoll(ctx, pollID, accountID, choices)
	if err == nil && p != nil {
		// A vote only changes the poll's status object representation.
		s.invalidateStatusResponses(p.StatusID)
	}
	return p, err
}
func (s *timelineCachingStorage) ClosePoll(ctx context.Context, pollID string) error {
	poll, pollErr := s.Storage.GetPoll(ctx, pollID, "")
	err := s.Storage.ClosePoll(ctx, pollID)
	if err == nil {
		if pollErr == nil && poll != nil && poll.StatusID != "" {
			s.invalidateStatusResponses(poll.StatusID)
		} else {
			s.invalidateResponses()
		}
	}
	return err
}

// Media rows arrive with nearly every remote status carrying an attachment;
// only the owning status's served object can change, so scope the wipe.
// Media not yet bound to a status (fresh local uploads) changes nothing we
// serve until the status referencing it is created.
func (s *timelineCachingStorage) CreateMediaAttachment(ctx context.Context, m *models.MediaAttachment) error {
	err := s.Storage.CreateMediaAttachment(ctx, m)
	if err == nil && m != nil && m.StatusID != nil && *m.StatusID != "" {
		s.invalidateStatusResponses(*m.StatusID)
	}
	return err
}
func (s *timelineCachingStorage) UpdateMediaAttachment(ctx context.Context, m *models.MediaAttachment) error {
	err := s.Storage.UpdateMediaAttachment(ctx, m)
	if err == nil && m != nil && m.StatusID != nil && *m.StatusID != "" {
		s.invalidateStatusResponses(*m.StatusID)
	}
	return err
}
func (s *timelineCachingStorage) DeleteMediaAttachment(ctx context.Context, id string) error {
	err := s.Storage.DeleteMediaAttachment(ctx, id)
	if err == nil {
		s.invalidateResponses()
	}
	return err
}
func (s *timelineCachingStorage) UpsertEmoji(ctx context.Context, emoji *models.CustomEmoji) error {
	err := s.Storage.UpsertEmoji(ctx, emoji)
	if err == nil && emoji != nil && s.responses != nil {
		// Emojis are upserted whenever a remote status carries them; only the
		// emoji's own served document changes (statuses embedding it are
		// federation_ttl-bounded).
		s.responses.DeletePrefix("fed:http:/emojis/" + emoji.ID)
	}
	return err
}
func (s *timelineCachingStorage) UpdateEmoji(ctx context.Context, emoji *models.CustomEmoji) error {
	err := s.Storage.UpdateEmoji(ctx, emoji)
	if err == nil && emoji != nil && s.responses != nil {
		s.responses.DeletePrefix("fed:http:/emojis/" + emoji.ID)
	}
	return err
}

// A quote authorization changes the quoted status's stamp document and
// collections, which share the status's URI prefix.
func (s *timelineCachingStorage) CreateQuoteAuthorization(ctx context.Context, qa *models.QuoteAuthorization) error {
	err := s.Storage.CreateQuoteAuthorization(ctx, qa)
	if err == nil {
		if qa != nil && qa.QuotedStatusID != "" {
			s.invalidateStatusResponses(qa.QuotedStatusID)
		} else {
			s.invalidateResponses()
		}
	}
	return err
}

func (s *timelineCachingStorage) EditStatusWithJobs(ctx context.Context, st *models.Status, expected time.Time, jobs []*models.Job) error {
	err := s.Storage.EditStatusWithJobs(ctx, st, expected, jobs)
	if err == nil {
		s.invalidateTimelines("edit_status")
		if !s.invalidateLocalStatusResponses(ctx, st) {
			s.invalidateResponses()
		}
	}
	return err
}

func (s *timelineCachingStorage) CreateFavouriteWithNotification(ctx context.Context, f *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateFavouriteWithNotification(ctx, f, notif, jobs)
	if err == nil {
		s.invalidateStatusResponses(f.StatusID)
		s.c.DeletePrefix(timelineKey("favourites", f.AccountID))
	}
	return notified, err
}

func (s *timelineCachingStorage) CreateBoostWithNotification(ctx context.Context, st *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateBoostWithNotification(ctx, st, notif, jobs)
	if err == nil {
		s.afterStatusCreated(ctx, st, nil)
	}
	return notified, err
}

func (s *timelineCachingStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, st *models.Status, jobs []*models.Job) error {
	err := s.Storage.PublishScheduledStatusWithJobs(ctx, expected, st, jobs)
	s.afterStatusCreated(ctx, st, err)
	return err
}

func (s *timelineCachingStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	id := expected.ID
	before, beforeErr := s.Storage.GetStatus(ctx, id)
	boostAccounts, _ := s.Storage.ListReblogAccounts(ctx, id, -1)
	err := s.Storage.DeleteStatusWithJobs(ctx, expected, expiredOnly, tombstone, jobs)
	if err == nil {
		// Timeline windows self-heal: hydration misses the deleted ID and only
		// the windows that contained it rebuild. Everything else stays warm.
		if beforeErr != nil || before == nil || (isLocalStatus(before) && !s.invalidateLocalStatusResponses(ctx, before)) {
			s.invalidateResponses()
		} else if !isLocalStatus(before) {
			s.invalidateStatusResponses(id)
		}
		// Deleting an original also deletes its Announce wrappers. Their local
		// authors' outbox/featured documents must not retain those wrappers.
		for _, account := range boostAccounts {
			if account != nil && account.Domain == nil {
				s.invalidateAccountResponses(account.Username)
			}
		}
	}
	return err
}
