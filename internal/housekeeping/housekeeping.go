// Package housekeeping provides automatic periodic cleanup tasks:
// pruning remote statuses and auto-deleting own low-interaction statuses,
// with proper federation of all deletions.
package housekeeping

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mediacache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Housekeeper runs periodic cleanup tasks.
type Housekeeper struct {
	store                  storage.Storage
	domain                 string
	mediaPath              string
	interval               time.Duration
	pruneRemote            int           // days
	pruneOwn               int           // days (server default; users can override)
	pruneOwnLikes          int           // min likes to keep
	pruneOwnBoosts         int           // min boosts to keep
	tombstoneRetentionDays int           // days to keep tombstones (0 = forever)
	maxDelete              int           // max deletions per run (0 = unlimited)
	homeFeedRetention      int           // home_feed rows kept per user (0 = storage default)
	cacheMaxAge            time.Duration // max age of cached remote media (0 = keep forever)
	cacheMaxBytes          int64         // max total size of the cache directory (0 = unbounded)
	cacheNonMediaBytes     int64         // cache bytes the per-media pass cannot see, from the last walk
	cachePath              string        // directory holding cached remote media
	ownedStore             objectstore.Store
	cacheStore             objectstore.Store
	lastObjectReconcile    time.Time
	lastOrphanSweep        time.Time // last orphaned-remote-account sweep; runs at most daily
	lastCountRepair        time.Time // last local-account count repair; runs at most daily
	remoteFirst            bool
	localStart             int
}

// New creates a new Housekeeper.
// Pass 0 for any prune setting to disable that task. homeFeedRetention <= 0
// falls back to the storage layer's default (800 rows per user).
func New(store storage.Storage, domain, mediaPath string, interval time.Duration, pruneRemoteDays, pruneOwnDays, pruneOwnMinLikes, pruneOwnMinBoosts, tombstoneRetentionDays, maxDelete, homeFeedRetention int, cacheMaxAge time.Duration, cachePath string, stores ...objectstore.Store) *Housekeeper {
	h := &Housekeeper{
		store:                  store,
		domain:                 domain,
		mediaPath:              mediaPath,
		interval:               interval,
		pruneRemote:            pruneRemoteDays,
		pruneOwn:               pruneOwnDays,
		pruneOwnLikes:          pruneOwnMinLikes,
		pruneOwnBoosts:         pruneOwnMinBoosts,
		tombstoneRetentionDays: tombstoneRetentionDays,
		maxDelete:              maxDelete,
		homeFeedRetention:      homeFeedRetention,
		cacheMaxAge:            cacheMaxAge,
		cachePath:              cachePath,
	}
	if len(stores) > 0 {
		h.ownedStore = stores[0]
	}
	if len(stores) > 1 {
		h.cacheStore = stores[1]
	}
	return h
}

// SetCacheMaxBytes bounds the total size of cached remote media. Zero or less
// leaves cacheMaxAge as the only bound. It is a setter because New already
// takes thirteen positional arguments, and a fourteenth would be one more
// unlabelled number at every call site.
func (h *Housekeeper) SetCacheMaxBytes(limit int64) {
	if limit < 0 {
		limit = 0
	}
	h.cacheMaxBytes = limit
}

// PruneRemoteStatuses prunes remote statuses older than the configured threshold.
// This is an exported wrapper around the unexported implementation.
func (h *Housekeeper) PruneRemoteStatuses(ctx context.Context) error {
	_, err := h.pruneRemoteStatuses(ctx, h.maxDelete)
	return err
}

// PruneOwnStatuses prunes own low-interaction statuses for all local accounts.
// This is an exported wrapper around the unexported implementation.
func (h *Housekeeper) PruneOwnStatuses(ctx context.Context) error {
	_, err := h.pruneOwnStatuses(ctx, h.maxDelete)
	return err
}

// RunOnce runs a single housekeeping cycle.
func (h *Housekeeper) RunOnce(ctx context.Context) {
	slog.Debug("housekeeping: starting run")

	remaining := h.maxDelete
	unlimited := remaining <= 0
	runRemote := func() {
		if h.pruneRemote <= 0 || (!unlimited && remaining == 0) {
			return
		}
		deleted, err := h.pruneRemoteStatuses(ctx, remaining)
		if err != nil {
			slog.Error("housekeeping: prune remote statuses failed", "error", err)
		}
		if !unlimited {
			remaining -= deleted
		}
	}
	runLocal := func() {
		if !unlimited && remaining == 0 {
			return
		}
		deleted, err := h.pruneOwnStatuses(ctx, remaining)
		if err != nil {
			slog.Error("housekeeping: prune own statuses failed", "error", err)
		}
		if !unlimited {
			remaining -= deleted
		}
	}
	if h.remoteFirst {
		runRemote()
		runLocal()
	} else {
		runLocal()
		runRemote()
	}
	h.remoteFirst = !h.remoteFirst

	now := time.Now().UTC()
	pruned, err := h.store.PruneExpiredData(ctx, now, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour), h.maxDelete)
	if err != nil {
		slog.Error("housekeeping: prune expired data failed", "error", err)
	} else {
		slog.Info("housekeeping: pruned expired data", "counts", pruned)
	}
	// The orphan sweep probes every remote account against a dozen tables and
	// then deletes each hit row by row, so it is by far the most expensive task
	// here. It is pure garbage collection: run it at most daily.
	if time.Since(h.lastOrphanSweep) >= 24*time.Hour {
		h.lastOrphanSweep = time.Now()
		// A week of grace after we first cached the profile: long enough that
		// nothing in flight (inbox jobs, an open profile page) loses its actor,
		// short enough that one-off lookups don't linger for a month.
		// Keep this expensive daily sweep capped separately from status pruning.
		accounts, err := h.store.GetOrphanedRemoteAccounts(ctx, now.Add(-7*24*time.Hour), 500)
		if err != nil {
			slog.Error("housekeeping: list orphaned remote accounts failed", "error", err)
		} else {
			deleted := 0
			for _, account := range accounts {
				if err := h.store.DeleteAccount(ctx, account.ID); err != nil {
					slog.Warn("housekeeping: delete orphaned remote account failed", "id", account.ID, "error", err)
					continue
				}
				deleted++
			}
			if deleted > 0 {
				slog.Info("housekeeping: pruned orphaned remote accounts", "count", deleted)
			}
		}
	}

	// Prune old tombstones
	if h.tombstoneRetentionDays > 0 {
		if err := h.pruneTombstones(ctx); err != nil {
			slog.Error("housekeeping: prune tombstones failed", "error", err)
		}
	}

	// Drop expired duration-limited mutes so their effect (hidden content and
	// silenced notifications) lifts once the window has elapsed.
	if viewers, err := h.store.DeleteExpiredMutes(ctx); err != nil {
		slog.Error("housekeeping: prune expired mutes failed", "error", err)
	} else if len(viewers) > 0 {
		slog.Debug("housekeeping: pruned expired mutes", "viewers", len(viewers))
	}

	// Keep SQLite planner statistics fresh so timeline queries stay on their
	// intended indexes as tables grow.
	if err := h.store.Optimize(ctx); err != nil {
		slog.Error("housekeeping: optimize failed", "error", err)
	}

	// Cap the materialized home feeds so fan-out rows don't grow unbounded.
	if n, err := h.store.PruneHomeFeeds(ctx, h.homeFeedRetention); err != nil {
		slog.Error("housekeeping: prune home feeds failed", "error", err)
	} else if n > 0 {
		slog.Debug("housekeeping: pruned home feed rows", "count", n)
	}

	// Drop expired media-cache failure entries so a permanently-dead remote URL is
	// retried once per retry window, and so the negative cache stays bounded in
	// lazy mode (where the eager sweep that also prunes it never runs).
	if n, err := h.store.PruneMediaCacheFailures(ctx, time.Now().Add(-mediacache.FailureRetryInterval)); err != nil {
		slog.Error("housekeeping: prune media cache failures failed", "error", err)
	} else if n > 0 {
		slog.Debug("housekeeping: pruned media cache failures", "count", n)
	}

	// Prune aged-out cached remote media so the cache doesn't grow indefinitely.
	if h.cacheMaxAge > 0 {
		if err := h.pruneCachedMedia(ctx); err != nil {
			slog.Error("housekeeping: prune cached media failed", "error", err)
		}
	}
	// Then bound what age retention decided to keep. Age first means this pass
	// has less to look at, and never evicts something the operator's retention
	// window would have dropped anyway.
	if err := h.pruneCachedMediaBySize(ctx); err != nil {
		slog.Error("housekeeping: prune cached media by size failed", "error", err)
	}

	if time.Since(h.lastObjectReconcile) >= 24*time.Hour {
		if counts, err := h.reconcileObjectStores(ctx, time.Now().UTC()); err != nil {
			slog.Error("housekeeping: object-store reconciliation failed", "error", err)
		} else {
			h.lastObjectReconcile = time.Now()
			slog.Info("housekeeping: reconciled object stores", "counts", counts)
			// Same daily cadence: the walk that just ran is the only thing that
			// sees cached avatars, emoji and preview images, which carry no
			// recorded size for the per-media pass to sum.
			h.enforceCacheStoreBudget(ctx)
		}
	}

	// Re-sync the denormalized local-account counters (followers/following/
	// statuses) daily: the API and web UI serve them directly, and this
	// backstops any bulk write path that bypasses the incremental updates.
	// Bounded by the local account count (WHERE domain IS NULL), so it is
	// safe on 32-bit WASM SQLite hosts.
	if time.Since(h.lastCountRepair) >= 24*time.Hour {
		if err := h.store.RepairLocalAccountCounts(ctx); err != nil {
			slog.Error("housekeeping: repair local account counts failed", "error", err)
		} else {
			h.lastCountRepair = time.Now()
		}
	}

	// Send notification digest emails to users who have opted in
	if err := h.sendEmailDigests(ctx); err != nil {
		slog.Error("housekeeping: email digests failed", "error", err)
	}

	slog.Debug("housekeeping: run complete")
}

// sendEmailDigests sends notification digest emails to users who have opted in
// and haven't received one in the last 24 hours.
func (h *Housekeeper) sendEmailDigests(ctx context.Context) error {
	cutoff := time.Now().Add(-24 * time.Hour)
	accounts, err := h.store.GetEmailDigestDue(ctx, cutoff)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}

	for _, account := range accounts {
		if !account.EmailVerified {
			continue
		}
		unread, err := h.store.CountUnreadNotificationsFiltered(ctx, account.ID, emailDigestExcludedTypes(account))
		if err != nil {
			continue
		}
		if unread == 0 {
			continue
		}

		subject := i18n.Tn(account.Locale, unread, "mail.digest.subject", h.domain, unread)
		body := i18n.Tn(account.Locale, unread, "mail.digest.body",
			account.Username, unread, h.domain, h.domain, h.domain,
		)

		if err := h.store.UpsertEmailDigestState(ctx, account.ID, time.Now()); err != nil {
			slog.Error("housekeeping: update digest state", "account", account.Username, "error", err)
			continue
		}

		payload := mail.EmailJobPayload{
			To:               account.Email,
			Subject:          subject,
			Body:             body,
			AccountID:        account.ID,
			NotificationType: "digest",
		}
		payloadBytes, _ := json.Marshal(payload)
		job := &models.Job{
			Kind:        "send_email",
			Payload:     string(payloadBytes),
			MaxAttempts: mail.MaxSendAttempts,
			State:       "queued",
		}
		if err := h.store.EnqueueJob(ctx, job); err != nil {
			slog.Error("housekeeping: enqueue digest email", "account", account.Username, "error", err)
			continue
		}
		slog.Debug("housekeeping: sent notification digest", "account", account.Username, "unread", unread)
	}
	return nil
}

func emailDigestExcludedTypes(account *models.Account) []string {
	if account == nil {
		return nil
	}
	var excluded []string
	if !account.EmailNotifyMention {
		excluded = append(excluded, "mention")
	}
	if !account.EmailNotifyFollow {
		excluded = append(excluded, "follow", "follow_accept", "follow_reject")
	}
	if !account.EmailNotifyReblog {
		excluded = append(excluded, "reblog")
	}
	if !account.EmailNotifyFavourite {
		excluded = append(excluded, "favourite")
	}
	if !account.EmailNotifyPoll {
		excluded = append(excluded, "poll")
	}
	if !account.EmailNotifyFollowRequest {
		excluded = append(excluded, "follow_request")
	}
	if !account.EmailNotifyQuote {
		excluded = append(excluded, "quote")
	}
	if !account.EmailNotifyReport {
		excluded = append(excluded, "admin.report")
	}
	return excluded
}

// pruneCachedMedia resets cached remote media older than cacheMaxAge back to its
// remote origin (so it is proxied/re-cached on next access) and deletes the
// local cache files once no other attachment still references them.
func (h *Housekeeper) pruneCachedMedia(ctx context.Context) error {
	if h.cacheMaxAge <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-h.cacheMaxAge)
	limit := h.maxDelete
	rows, err := h.store.GetCachedMediaOlderThan(ctx, cutoff, limit)
	if err != nil {
		return fmt.Errorf("get cached media older than cutoff: %w", err)
	}
	pruned := 0
	for _, m := range rows {
		if h.uncacheMedia(ctx, m) {
			pruned++
		}
	}

	previews, err := h.store.GetCachedStatusPreviewsOlderThan(ctx, cutoff, limit)
	if err != nil {
		return fmt.Errorf("get cached status previews older than cutoff: %w", err)
	}
	for _, preview := range previews {
		if preview.ImageRemote != "" {
			preview.Image = preview.ImageRemote
		}
		preview.CachedAt = nil
		if err := h.store.UpsertStatusPreview(ctx, preview); err != nil {
			slog.Warn("housekeeping: failed to reset cached preview image", "status_id", preview.StatusID, "error", err)
			continue
		}
		pruned++
	}

	slog.Info("housekeeping: pruned cached remote media", "count", pruned, "older_than", h.cacheMaxAge.String())
	return nil
}

// uncacheMedia repoints one attachment back at its remote origin and clears the
// cached marker, so displayMediaURL proxies it again (and, in lazy mode,
// re-caches it) on next access. It reports whether the row was changed.
//
// An attachment with no remote origin is left alone: the cached copy would be
// the only copy, and clearing the marker on it strands a file nothing can fetch
// again.
func (h *Housekeeper) uncacheMedia(ctx context.Context, m *models.MediaAttachment) bool {
	if m.RemoteURL == "" {
		return false
	}
	cachedURL := m.URL
	m.URL = m.RemoteURL
	if m.PreviewURL == cachedURL || m.PreviewStorageKey != "" || strings.HasPrefix(m.PreviewURL, "/media/cache/") {
		if m.Type == "video" {
			m.PreviewURL = ""
		} else {
			m.PreviewURL = m.RemoteURL
		}
	}
	m.CachedAt = nil
	if err := h.store.UpdateMediaAttachment(ctx, m); err != nil {
		slog.Warn("housekeeping: failed to reset cached media", "id", m.ID, "error", err)
		return false
	}
	return true
}

// pruneCachedMediaBySize evicts the oldest cached remote media until the cache
// fits cacheMaxBytes. It runs after the age pass, so it only ever sees what age
// retention chose to keep.
//
// What makes the bound reachable is that it measures and evicts the same set:
// cached remote media. Owned uploads are outside the total and are never
// touched, so there is always enough cache to give back. Link-preview images
// record no size, so they neither count against the budget nor get evicted for
// it; cacheMaxAge remains their only bound.
func (h *Housekeeper) pruneCachedMediaBySize(ctx context.Context) error {
	if h.cacheMaxBytes <= 0 {
		return nil
	}
	total, err := h.store.CachedMediaBytes(ctx)
	if err != nil {
		return fmt.Errorf("total cached media bytes: %w", err)
	}
	// Media may only have what the rest of the cache leaves it. cacheNonMediaBytes
	// is the avatars, headers, emoji and preview images the last whole-directory
	// walk found, none of which record a size this pass could sum. Without that
	// subtraction media would be allowed to fill the entire allowance and put the
	// directory over it until the next walk.
	budget := h.cacheMaxBytes - h.cacheNonMediaBytes
	if budget < 0 {
		budget = 0
	}
	if total <= budget {
		return nil
	}
	// Evict to nine tenths of the budget. Stopping exactly at the limit puts the
	// next cached file back over it, which turns a bound that should bite rarely
	// into an eviction on every run.
	target := budget - budget/10

	rows, err := h.store.GetCachedMediaOlderThan(ctx, time.Now(), h.maxDelete)
	if err != nil {
		return fmt.Errorf("get cached media by age: %w", err)
	}
	pruned := 0
	for _, m := range rows {
		if total <= target {
			break
		}
		// A row with no recorded size is not in the total either, so evicting it
		// cannot bring the cache down. Leave it to the age pass rather than
		// spend this run's deletion budget on it.
		if m.FileSize <= 0 {
			continue
		}
		if !h.uncacheMedia(ctx, m) {
			continue
		}
		total -= m.FileSize
		pruned++
	}
	if total > budget {
		slog.Warn("housekeeping: cached media still over its size budget; continuing next run",
			"pruned", pruned, "remaining_bytes", total, "budget_bytes", budget, "max_delete_per_run", h.maxDelete)
		return nil
	}
	slog.Info("housekeeping: pruned cached remote media over size budget",
		"count", pruned, "remaining_bytes", total, "budget_bytes", budget)
	return nil
}

// cacheEntry is one cached file considered for size-based eviction. The walk
// keeps these rather than whole objectstore records so a large cache costs a
// key, a size and a timestamp per file and nothing more.
type cacheEntry struct {
	key  string
	size int64
	mod  time.Time
}

// trimCacheStoreToBudget walks the cache object store, totals what it actually
// holds, and evicts the oldest files until the whole cache fits cacheMaxBytes.
// It returns the total after eviction.
//
// This is the pass that makes the budget mean the cache directory rather than
// one table's worth of it. Avatars, headers, custom emoji and link-preview
// images record no size in the database, so a SQL sum cannot see them; a walk
// can see everything. It runs on the daily object-reconcile cadence because
// that walk is already paid for there.
//
// Eviction order is file write time, not last use: atime is off on every host
// this runs on, so "least recently cached" is the best available stand-in for
// "least recently wanted". The age-based prune makes the same approximation.
func (h *Housekeeper) trimCacheStoreToBudget(ctx context.Context) (int64, int, error) {
	limit := h.maxDelete
	if limit <= 0 {
		limit = 1000
	}
	var total int64
	// Keeping twice the eviction limit and trimming back to it costs one sort
	// per limit files seen, and bounds the walk's memory whatever the cache
	// grew to before the budget was first set.
	oldest := make([]cacheEntry, 0, 2*limit)
	err := h.cacheStore.List(ctx, "cache/", func(obj objectstore.Object) error {
		total += obj.Size
		oldest = append(oldest, cacheEntry{key: obj.Key, size: obj.Size, mod: obj.ModTime})
		if len(oldest) >= 2*limit {
			sortCacheEntriesByAge(oldest)
			oldest = oldest[:limit]
		}
		return nil
	})
	if err != nil {
		return total, 0, err
	}
	if total <= h.cacheMaxBytes {
		return total, 0, nil
	}
	sortCacheEntriesByAge(oldest)

	target := h.cacheMaxBytes - h.cacheMaxBytes/10
	evicted := 0
	for _, entry := range oldest {
		if total <= target || evicted >= limit {
			break
		}
		// Repoint the owning row before removing the file, so nothing advertises
		// a cache URL that has already gone. Should this miss, the request that
		// finds the file absent repairs the row through the same call.
		if _, err := h.store.ReconcileMissingCacheURL(ctx, h.cacheStore.URL(entry.key)); err != nil {
			slog.Warn("housekeeping: failed to release cached object", "key", entry.key, "error", err)
			continue
		}
		if err := h.cacheStore.Delete(ctx, entry.key); err != nil {
			slog.Warn("housekeeping: failed to delete cached object", "key", entry.key, "error", err)
			continue
		}
		total -= entry.size
		evicted++
	}
	return total, evicted, nil
}

func sortCacheEntriesByAge(entries []cacheEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
}

// enforceCacheStoreBudget runs the whole-directory pass and records how much of
// the cache the per-media pass cannot see. Between daily walks that figure is
// what lets the frequent pass aim at a media budget which leaves room for the
// rest, instead of letting media fill the whole allowance and putting the
// directory over it until the next walk.
func (h *Housekeeper) enforceCacheStoreBudget(ctx context.Context) {
	if h.cacheMaxBytes <= 0 || h.cacheStore == nil {
		return
	}
	mediaBytes, err := h.store.CachedMediaBytes(ctx)
	if err != nil {
		slog.Warn("housekeeping: cached media total unavailable", "error", err)
		return
	}
	total, evicted, err := h.trimCacheStoreToBudget(ctx)
	if err != nil {
		slog.Error("housekeeping: cache size enforcement failed", "error", err)
		return
	}
	if nonMedia := total - mediaBytes; nonMedia > 0 {
		h.cacheNonMediaBytes = nonMedia
	} else {
		h.cacheNonMediaBytes = 0
	}
	if evicted > 0 {
		slog.Info("housekeeping: trimmed cache store to its size budget",
			"evicted", evicted, "total_bytes", total, "budget_bytes", h.cacheMaxBytes)
	}
	if total > h.cacheMaxBytes {
		slog.Warn("housekeeping: cache store still over its size budget; continuing next walk",
			"total_bytes", total, "budget_bytes", h.cacheMaxBytes, "max_delete_per_run", h.maxDelete)
	}
}

// cacheGCGrace is how recent a cache file must be to be spared from GC even when
// unreferenced, avoiding a race with an in-flight cache write that hasn't yet
// updated its DB row.
const cacheGCGrace = time.Hour

var errReconcileLimit = errors.New("object reconciliation limit reached")

func (h *Housekeeper) reconcileObjectStores(ctx context.Context, now time.Time) (map[string]int, error) {
	counts := map[string]int{"media": 0, "exports": 0, "imports": 0, "cache": 0}
	limit := h.maxDelete
	if limit <= 0 {
		limit = 500
	}
	media, err := h.store.GetAllMedia(ctx)
	if err != nil {
		return counts, err
	}
	mediaRefs := make(map[string]struct{}, len(media)*4)
	cacheRefs := make(map[string]struct{}, len(media)*2)
	for _, m := range media {
		for _, ref := range []string{m.StorageKey, m.PreviewStorageKey, m.URL, m.PreviewURL} {
			if ref != "" {
				mediaRefs[ref] = struct{}{}
				if strings.HasPrefix(ref, "cache/") || strings.Contains(ref, "/media/cache/") {
					cacheRefs[ref] = struct{}{}
				}
			}
		}
	}
	urls, err := h.store.GetReferencedCachePaths(ctx)
	if err != nil {
		return counts, err
	}
	for ref := range urls {
		cacheRefs[ref] = struct{}{}
	}

	if h.ownedStore != nil {
		if counts["media"], err = reconcileObjects(ctx, h.ownedStore, "media/", now.Add(-time.Hour), mediaRefs, limit); err != nil {
			return counts, err
		}
		if counts["exports"], err = reconcileObjects(ctx, h.ownedStore, "exports/", now.Add(-48*time.Hour), nil, limit); err != nil {
			return counts, err
		}
		if counts["imports"], err = reconcileObjects(ctx, h.ownedStore, "imports/", now.Add(-7*24*time.Hour), nil, limit); err != nil {
			return counts, err
		}
	}
	if h.cacheStore != nil {
		counts["cache"], err = reconcileObjects(ctx, h.cacheStore, "cache/", now.Add(-time.Hour), cacheRefs, limit)
	}
	return counts, err
}

func reconcileObjects(ctx context.Context, store objectstore.Store, prefix string, cutoff time.Time, refs map[string]struct{}, limit int) (int, error) {
	removed := 0
	err := store.List(ctx, prefix, func(obj objectstore.Object) error {
		if obj.ModTime.IsZero() || !obj.ModTime.Before(cutoff) {
			return nil
		}
		if refs != nil {
			if _, ok := refs[obj.Key]; ok {
				return nil
			}
			if _, ok := refs[store.URL(obj.Key)]; ok {
				return nil
			}
		}
		if removed >= limit {
			return errReconcileLimit
		}
		if err := store.Delete(ctx, obj.Key); err != nil {
			return err
		}
		removed++
		return nil
	})
	if errors.Is(err, errReconcileLimit) {
		err = nil
	}
	return removed, err
}

// gcCacheFiles removes cache files (and their hash directories) that are no
// longer referenced by any media attachment or account image. The cache is laid
// out as <cachePath>/<hash[:2]>/<hash>/<files>.
func (h *Housekeeper) gcCacheFiles(ctx context.Context) error {
	referenced, err := h.store.GetReferencedCachePaths(ctx)
	if err != nil {
		return fmt.Errorf("get referenced cache paths: %w", err)
	}

	shards, err := os.ReadDir(h.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache dir: %w", err)
	}

	removed := 0
	cutoff := time.Now().Add(-cacheGCGrace)
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(h.cachePath, shard.Name())
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, hashDir := range entries {
			if !hashDir.IsDir() {
				continue
			}
			dirPath := filepath.Join(shardPath, hashDir.Name())
			if h.cacheDirReferencedOrFresh(dirPath, shard.Name(), hashDir.Name(), referenced, cutoff) {
				continue
			}
			if err := os.RemoveAll(dirPath); err != nil {
				slog.Warn("housekeeping: failed to remove cache dir", "path", dirPath, "error", err)
				continue
			}
			removed++
		}
		// Drop the shard directory if it is now empty.
		if leftover, err := os.ReadDir(shardPath); err == nil && len(leftover) == 0 {
			_ = os.Remove(shardPath)
		}
	}

	if removed > 0 {
		slog.Info("housekeeping: garbage-collected unreferenced cache entries", "count", removed)
	}
	return nil
}

// cacheDirReferencedOrFresh reports whether a cache hash directory should be
// kept: either one of its files is still referenced, or it was written within
// the GC grace window (a likely in-flight cache).
func (h *Housekeeper) cacheDirReferencedOrFresh(dirPath, shard, hash string, referenced map[string]struct{}, cutoff time.Time) bool {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return true // be conservative on error
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		url := "/media/cache/" + shard + "/" + hash + "/" + f.Name()
		if _, ok := referenced[url]; ok {
			return true
		}
		if info, err := f.Info(); err == nil && info.ModTime().After(cutoff) {
			return true
		}
	}
	return false
}

// pruneRemoteStatuses deletes remote statuses older than the configured threshold,
// cleaning up associated media, notifications, and tags.
func (h *Housekeeper) pruneRemoteStatuses(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500 // safety cap
	}

	statuses, err := h.store.GetRemoteStatusesOlderThan(ctx, h.pruneRemote, limit)
	if err != nil {
		return 0, fmt.Errorf("query remote statuses: %w", err)
	}

	if len(statuses) == 0 {
		return 0, nil
	}

	slog.Info("housekeeping: pruning remote statuses", "count", len(statuses), "older_than_days", h.pruneRemote)

	deleted := 0
	for _, status := range statuses {
		if err := h.deleteStatus(ctx, status, nil); err != nil {
			slog.Warn("housekeeping: failed to delete remote status", "id", status.ID, "uri", status.URI, "error", err)
			continue
		}
		deleted++
	}

	return deleted, nil
}

// pruneTombstones removes tombstones older than the configured retention period.
func (h *Housekeeper) pruneTombstones(ctx context.Context) error {
	since := time.Now().AddDate(0, 0, -h.tombstoneRetentionDays)
	tombstones, err := h.store.ListTombstonesOlderThan(ctx, since, 1000)
	if err != nil {
		return fmt.Errorf("list tombstones: %w", err)
	}
	if len(tombstones) == 0 {
		return nil
	}
	slog.Info("housekeeping: pruning old tombstones", "count", len(tombstones), "older_than_days", h.tombstoneRetentionDays)
	for _, t := range tombstones {
		if err := h.store.DeleteTombstone(ctx, t.ID); err != nil {
			slog.Warn("housekeeping: failed to delete tombstone", "id", t.ID, "uri", t.URI, "error", err)
		}
	}
	return nil
}

// pruneOwnStatuses deletes local statuses older than a threshold
// that have fewer than a minimum of likes and boosts.
// The server provides defaults; each user can override them.
// If neither server nor user has set a positive days value, that user is skipped.
// Deletions are federated to followers.
func (h *Housekeeper) pruneOwnStatuses(ctx context.Context, limit int) (int, error) {
	// Get all local accounts (including their per-user prune settings)
	accounts, err := h.store.GetLocalAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("get local accounts: %w", err)
	}

	if len(accounts) == 0 {
		return 0, nil
	}
	start := h.localStart % len(accounts)
	h.localStart = (start + 1) % len(accounts)
	deleted := 0
	for offset := range len(accounts) {
		acct := accounts[(start+offset)%len(accounts)]
		if limit > 0 && deleted >= limit {
			break
		}
		// Determine effective thresholds for this user
		days := h.pruneOwn // server default (may be 0)
		minLikes := h.pruneOwnLikes
		minBoosts := h.pruneOwnBoosts

		// Override with user-level settings if set
		userDays := acct.PruneOwnStatusesAfterDays
		if userDays != nil {
			if *userDays <= 0 {
				// User has explicitly opted out of pruning
				continue
			}
			days = *userDays
		}
		if acct.PruneOwnStatusesMinLikes != nil {
			minLikes = *acct.PruneOwnStatusesMinLikes
		}
		if acct.PruneOwnStatusesMinBoosts != nil {
			minBoosts = *acct.PruneOwnStatusesMinBoosts
		}

		// If days is still 0 (neither server nor user set a positive value), skip.
		if days <= 0 {
			continue
		}

		// Query this user's old low-interaction statuses
		accountLimit := 500
		if limit > 0 {
			accountLimit = limit - deleted
		}
		statuses, err := h.store.GetLocalStatusesOlderThanForAccount(ctx, acct.ID, days, minLikes, minBoosts, accountLimit)
		if err != nil {
			slog.Warn("housekeeping: failed to query statuses for account", "account_id", acct.ID, "error", err)
			continue
		}

		if len(statuses) == 0 {
			continue
		}

		slog.Info("housekeeping: pruning own low-interaction statuses",
			"account", acct.Username,
			"count", len(statuses),
			"older_than_days", days,
			"min_likes", minLikes,
			"min_boosts", minBoosts,
		)

		for _, status := range statuses {
			if err := h.deleteStatus(ctx, status, acct); err != nil {
				slog.Warn("housekeeping: failed to delete own status", "id", status.ID, "uri", status.URI, "error", err)
				continue
			}
			deleted++
		}
	}

	return deleted, nil
}

// deleteStatus deletes a status from the database and disk, and federates the
// deletion if the author is provided (local account).
func (h *Housekeeper) deleteStatus(ctx context.Context, status *models.Status, author *models.Account) error {
	return h.deleteStatusVersion(ctx, status, author, false)
}

func (h *Housekeeper) deleteStatusVersion(ctx context.Context, status *models.Status, author *models.Account, expiredOnly bool) error {
	tombstone, jobs, err := federation.PrepareStatusDeletion(ctx, h.store, h.domain, author, status)
	if err != nil {
		return err
	}
	mediaAttachments, mediaErr := h.store.GetMediaForStatus(ctx, status.ID)
	if mediaErr != nil {
		return mediaErr
	}
	if err := h.store.DeleteStatusWithJobs(ctx, status, expiredOnly, tombstone, jobs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}

	// Files left behind by a failed cleanup are handled by object reconciliation.
	for _, m := range mediaAttachments {
		if err := medialib.DeleteAttachmentFiles(ctx, h.ownedStore, h.mediaPath, m); err != nil {
			slog.Warn("housekeeping: failed to remove media object", "id", m.ID, "error", err)
		}
	}

	return nil
}

// ExpireStatus deletes a status whose lifetime has elapsed. It is the job
// handler for StatusExpireJobKind. Idempotent: a status already deleted
// (e.g. manually, before the job ran) is not an error. Re-reads the current
// expires_at rather than trusting the job payload: editing a post can cancel
// (nil) or push back its expiry after this job was already enqueued, and the
// stale job must no-op rather than delete early - a fresh job enqueued by the
// edit (if any) carries the up-to-date RunAt.
func (h *Housekeeper) ExpireStatus(ctx context.Context, statusID string) error {
	status, err := h.store.GetStatus(ctx, statusID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.ExpiresAt == nil || time.Now().Before(*status.ExpiresAt) {
		return nil
	}
	author, err := h.store.GetAccount(ctx, status.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		author = nil
	} else if err != nil {
		return err
	}
	return h.deleteStatusVersion(ctx, status, author, true)
}
