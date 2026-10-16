package app

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
)

// Warmer proactively refreshes recently-active viewers' caches when new
// statuses or notifications arrive, so the next page open is a cache hit
// instead of a rebuild - the visible delay on low-power devices. It is
// best-effort: events may be dropped under pressure and the lazy
// read-time rebuild remains the correctness path.
//
// One Warmer runs a single serialized worker. Bursts (federation delivers in
// clumps) are coalesced by a debounce window into one warm pass, the pass is
// capped to a handful of viewers, and the worker yields between viewers so it
// never competes with foreground requests on a single-core device.
type Warmer struct {
	svc  *Service
	opts WarmerOptions

	mu            sync.Mutex
	viewers       map[string]time.Time // accountID -> last request seen
	publicViewers map[string]time.Time // accountID -> last public-timeline visit

	events  chan warmEvent
	dropped atomic.Int64

	foreground *queue.ForegroundGate
}

// WarmerOptions bounds a Warmer's background work.
type WarmerOptions struct {
	// MaxViewers caps how many viewers one warm pass refreshes.
	MaxViewers int
	// ActiveViewerTTL is how long after their last request a viewer keeps
	// being warmed. Zero or negative means viewers never expire.
	ActiveViewerTTL time.Duration
	// Debounce is how long the worker waits after the first event to coalesce
	// a burst into a single pass.
	Debounce time.Duration
	// PageSize is the head-window size to warm; it must match what the front
	// ends request so the warm read produces the exact cache keys they hit.
	PageSize int
}

type warmEvent struct {
	kind      string
	id        string
	accountID string
}

// NewWarmer creates a Warmer around svc (which must share the decorated store
// and page cache with the front-end services so warmed entries are the ones
// they read).
func NewWarmer(svc *Service, opts WarmerOptions) *Warmer {
	if opts.MaxViewers <= 0 {
		opts.MaxViewers = 8
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 20
	}
	return &Warmer{
		svc:           svc,
		opts:          opts,
		viewers:       make(map[string]time.Time),
		publicViewers: make(map[string]time.Time),
		events:        make(chan warmEvent, 64),
	}
}

func (w *Warmer) SetForegroundGate(gate *queue.ForegroundGate) { w.foreground = gate }

// Touch records that a viewer made an authenticated request. Called from the
// request path, so it is O(1) and never blocks on the worker.
func (w *Warmer) Touch(accountID string) {
	if w == nil || accountID == "" {
		return
	}
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.viewers[accountID]; !ok && len(w.viewers) >= w.opts.MaxViewers*4 {
		// Cap the tracking map by evicting the least recently seen viewer.
		oldestID, oldest := "", now
		for id, seen := range w.viewers {
			if seen.Before(oldest) {
				oldestID, oldest = id, seen
			}
		}
		if oldestID != "" {
			delete(w.viewers, oldestID)
		}
	}
	w.viewers[accountID] = now
}

// TouchPath is Touch plus timeline-kind tracking: viewers who actually open a
// public timeline get their public window warmed; everyone else only pays for
// home. Wired as the front ends' viewer-touch hook with the request path.
func (w *Warmer) TouchPath(accountID, path string) {
	if w == nil || accountID == "" {
		return
	}
	w.Touch(accountID)
	if path == "/public" || path == "/api/v1/timelines/public" {
		w.mu.Lock()
		w.publicViewers[accountID] = time.Now()
		w.mu.Unlock()
	}
}

// wantsPublic reports whether the viewer opened a public timeline within
// ActiveViewerTTL.
func (w *Warmer) wantsPublic(accountID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen, ok := w.publicViewers[accountID]
	if !ok {
		return false
	}
	if w.opts.ActiveViewerTTL > 0 && seen.Before(time.Now().Add(-w.opts.ActiveViewerTTL)) {
		delete(w.publicViewers, accountID)
		return false
	}
	return true
}

// Notify hands a broker event to the worker. It never blocks: the local
// broker publishes synchronously inside the status-creation path, so a full
// queue drops the event (the debounced pass that is inevitably already
// pending covers it) rather than delaying a foreground write.
func (w *Warmer) Notify(kind, id, accountID string) {
	if w == nil {
		return
	}
	select {
	case w.events <- warmEvent{kind: kind, id: id, accountID: accountID}:
	default:
		if n := w.dropped.Add(1); n%64 == 1 {
			slog.Debug("cache: warmer queue full, dropping events", "dropped_total", n)
		}
	}
}

// Run is the single warm worker; it returns when ctx is cancelled.
func (w *Warmer) Run(ctx context.Context) {
	for {
		var first warmEvent
		select {
		case <-ctx.Done():
			return
		case first = <-w.events:
		}

		statusesDirty, notifAll, notifAccounts := false, false, map[string]struct{}{}
		absorb := func(ev warmEvent) {
			switch ev.kind {
			case "notification.created":
				if ev.accountID != "" {
					notifAccounts[ev.accountID] = struct{}{}
				} else {
					notifAll = true
				}
			case "cache.invalidate":
				// Generic write invalidations are followed by a specific status or
				// notification event. Treating every one as a notification change
				// caused needless notification hydration during status bursts.
				statusesDirty = true
			default: // status.created / status.updated / status.deleted
				statusesDirty = true
			}
		}
		absorb(first)

		// Debounce: keep draining so one pass serves a whole federation clump.
		timer := time.NewTimer(w.opts.Debounce)
	drain:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case ev := <-w.events:
				absorb(ev)
			case <-timer.C:
				break drain
			}
		}

		for ctx.Err() == nil && !w.warmPass(ctx, statusesDirty, notifAll, notifAccounts) {
			if w.foreground.WaitIdle(ctx) != nil {
				return
			}
		}
	}
}

// warmPass refreshes the bounded set of recently-active viewers. A false
// result means foreground work preempted it and the coalesced pass must resume.
func (w *Warmer) warmPass(ctx context.Context, statusesDirty, notifAll bool, notifAccounts map[string]struct{}) bool {
	viewers := w.activeViewers()
	if len(viewers) == 0 {
		return true
	}
	start := time.Now()
	warmed := 0
	for i, viewerID := range viewers {
		if i > 0 {
			// Yield the core to foreground requests between viewers.
			select {
			case <-ctx.Done():
				return true
			case <-time.After(25 * time.Millisecond):
			}
		}
		_, wantNotifs := notifAccounts[viewerID]
		wantNotifs = wantNotifs || notifAll
		if !wantNotifs && !statusesDirty {
			continue
		}
		if w.foreground.WaitIdle(ctx) != nil {
			return true
		}
		// No grace: a warming pass only fills caches, so there is nothing to
		// salvage by letting it run on, and a page request should never wait
		// behind speculative work.
		warmCtx, cancel := w.foreground.BackgroundContextWithGrace(ctx, true, 0)
		w.warmViewer(warmCtx, viewerID, statusesDirty, wantNotifs)
		preempted := warmCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if preempted {
			return false
		}
		warmed++
	}
	slog.Debug("cache: warm pass complete", "viewers", warmed,
		"statuses", statusesDirty, "elapsed", time.Since(start))
	return true
}

// warmViewer refreshes caches for one viewer. On status events it touches the
// home-timeline head window: when the window is already cached this is the
// cheap incremental head refresh ("rows newer than the cached head" - an
// indexed, millisecond query) and it re-stamps the window's TTL, so an active
// viewer's window never goes cold; after a restart or eviction it pays the
// full build here in the background instead of on the next foreground request.
// A federation burst advancing the generation mid-build is harmless: the
// window is stored at its build generation and the next read merges the head
// delta incrementally.
func (w *Warmer) warmViewer(ctx context.Context, viewerID string, wantTimeline, wantNotifs bool) {
	// Generous timeout: a cold full home-timeline query can take several
	// seconds on SD-card-class storage. Paying for it here, in the background,
	// is why the warmer exists; cancelling it would interrupt the SQL and push
	// the cost back onto the next foreground request.
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	account, err := w.svc.store.GetAccount(vctx, viewerID)
	if err != nil {
		return
	}

	if wantTimeline {
		// The head-window variants each front end actually requests: same
		// limit, same hide flags. The API's default variant (no excludes)
		// shares the key when the flags are both false.
		statuses, err := w.svc.store.GetHomeTimeline(vctx, viewerID, nil, nil, nil, w.opts.PageSize, account.HideReplies, account.HideBoosts)
		if err == nil {
			w.svc.EnrichStatuses(vctx, statuses, account)
		}
		if account.HideReplies || account.HideBoosts {
			_, _ = w.svc.store.GetHomeTimeline(vctx, viewerID, nil, nil, nil, w.opts.PageSize, false, false)
		}
		// Public is warmed only for viewers who actually opened it recently -
		// a cold public window is a multi-second scan, and speculatively paying
		// it per viewer per burst wasted most of the warm pass on home-only
		// viewers. Once warm it's just the indexed incremental head refresh.
		if w.wantsPublic(viewerID) {
			publics, err := w.svc.store.GetPublicTimeline(vctx, viewerID, false, nil, nil, nil, w.opts.PageSize, account.HideReplies, account.HideBoosts)
			if err == nil {
				w.svc.EnrichStatuses(vctx, publics, account)
			}
		}
	}

	if !wantNotifs {
		return
	}
	// The two variants real callers use: the API default (no excludes)
	// and the web UI (the viewer's notification preferences).
	excluded := NotificationExcludedTypes(account)
	_, _ = w.svc.store.CountUnreadNotificationsFiltered(vctx, viewerID, excluded)
	w.warmNotificationVariant(vctx, account, excluded)
	if len(excluded) > 0 {
		_, _ = w.svc.store.CountUnreadNotificationsFiltered(vctx, viewerID, nil)
		w.warmNotificationVariant(vctx, account, nil)
	}
}

// warmNotificationVariant primes both notification membership and the status
// components embedded by the Mastodon API. Warming only ListNotifications left
// the expensive relation hydration on the first foreground API request.
func (w *Warmer) warmNotificationVariant(ctx context.Context, viewer *models.Account, excluded []string) {
	notifs, err := w.svc.store.ListNotifications(ctx, viewer.ID, nil, w.opts.PageSize, excluded)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(notifs))
	seen := make(map[string]struct{}, len(notifs))
	for _, notif := range notifs {
		if notif == nil || notif.StatusID == nil || *notif.StatusID == "" {
			continue
		}
		if _, ok := seen[*notif.StatusID]; ok {
			continue
		}
		seen[*notif.StatusID] = struct{}{}
		ids = append(ids, *notif.StatusID)
	}
	statusesByID, err := w.svc.store.GetStatusesByIDs(ctx, ids)
	if err != nil {
		return
	}
	statuses := make([]*models.Status, 0, len(statusesByID))
	for _, status := range statusesByID {
		statuses = append(statuses, status)
	}
	w.svc.EnrichStatuses(ctx, statuses, viewer)
}

// activeViewers snapshots the viewers seen within ActiveViewerTTL, most recent
// first, capped at MaxViewers; expired entries are pruned as a side effect.
func (w *Warmer) activeViewers() []string {
	type seenViewer struct {
		id   string
		seen time.Time
	}
	cutoff := time.Time{}
	if w.opts.ActiveViewerTTL > 0 {
		cutoff = time.Now().Add(-w.opts.ActiveViewerTTL)
	}
	w.mu.Lock()
	active := make([]seenViewer, 0, len(w.viewers))
	for id, seen := range w.viewers {
		if seen.Before(cutoff) {
			delete(w.viewers, id)
			continue
		}
		active = append(active, seenViewer{id: id, seen: seen})
	}
	w.mu.Unlock()

	sort.Slice(active, func(i, j int) bool { return active[i].seen.After(active[j].seen) })
	if len(active) > w.opts.MaxViewers {
		active = active[:w.opts.MaxViewers]
	}
	ids := make([]string, len(active))
	for i, v := range active {
		ids[i] = v.id
	}
	return ids
}
