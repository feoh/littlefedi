package federation

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const (
	hardMaxThreadReplies   = 20
	hardMaxAncestorDepth   = 100
	hardMaxCollectionPages = 5
	hardMaxThreadTimeout   = 10 * time.Second

	// Background completion runs off the request path, so it is allowed a much
	// larger budget to assemble the full conversation tree.
	hardMaxBackgroundReplies = 1000
	hardMaxBackgroundPages   = 50
	hardMaxBackgroundTimeout = 2 * time.Minute
)

// ThreadFetchConfig controls bounded on-demand remote thread completion.
type ThreadFetchConfig struct {
	Enabled          bool
	StaleAfter       time.Duration
	Timeout          time.Duration
	MaxReplies       int
	MaxAncestorDepth int
	MaxPages         int

	// Background crawl: a larger, off-request completion pass that walks the
	// whole thread (including cross-instance replies) and fills the cache.
	BackgroundEnabled     bool
	BackgroundTimeout     time.Duration
	BackgroundMaxReplies  int
	BackgroundMaxPages    int
	BackgroundConcurrency int
}

// fetchBudget bounds a single completion pass. The interactive (request-bound)
// and background passes use different budgets drawn from ThreadFetchConfig.
type fetchBudget struct {
	maxReplies       int
	maxAncestorDepth int
	maxPages         int
}

// ThreadResolver completes remote status context from inReplyTo links and
// ActivityStreams replies collections. All errors are best-effort: callers
// should continue by serving the locally cached thread.
type ThreadResolver struct {
	store       storage.Storage
	federator   *Federator
	importer    *InboxProcessor
	config      ThreadFetchConfig
	fetchObject func(context.Context, string) (map[string]interface{}, error)

	mu              sync.Mutex
	inflight        map[string]chan struct{}
	backgroundSlots chan struct{}
	shutdownCtx     context.Context
}

func NewThreadResolver(store storage.Storage, federator *Federator, domain string, cfg ThreadFetchConfig) *ThreadResolver {
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 15 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Timeout > hardMaxThreadTimeout {
		cfg.Timeout = hardMaxThreadTimeout
	}
	if cfg.MaxReplies <= 0 {
		cfg.MaxReplies = 5
	}
	if cfg.MaxReplies > hardMaxThreadReplies {
		cfg.MaxReplies = hardMaxThreadReplies
	}
	if cfg.MaxAncestorDepth <= 0 {
		cfg.MaxAncestorDepth = 20
	}
	if cfg.MaxAncestorDepth > hardMaxAncestorDepth {
		cfg.MaxAncestorDepth = hardMaxAncestorDepth
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 1
	}
	if cfg.MaxPages > hardMaxCollectionPages {
		cfg.MaxPages = hardMaxCollectionPages
	}
	if cfg.BackgroundTimeout <= 0 {
		cfg.BackgroundTimeout = 30 * time.Second
	}
	if cfg.BackgroundTimeout > hardMaxBackgroundTimeout {
		cfg.BackgroundTimeout = hardMaxBackgroundTimeout
	}
	if cfg.BackgroundMaxReplies <= 0 {
		cfg.BackgroundMaxReplies = 200
	}
	if cfg.BackgroundMaxReplies > hardMaxBackgroundReplies {
		cfg.BackgroundMaxReplies = hardMaxBackgroundReplies
	}
	if cfg.BackgroundMaxReplies < cfg.MaxReplies {
		cfg.BackgroundMaxReplies = cfg.MaxReplies
	}
	if cfg.BackgroundMaxPages <= 0 {
		cfg.BackgroundMaxPages = 20
	}
	if cfg.BackgroundMaxPages > hardMaxBackgroundPages {
		cfg.BackgroundMaxPages = hardMaxBackgroundPages
	}
	if cfg.BackgroundMaxPages < cfg.MaxPages {
		cfg.BackgroundMaxPages = cfg.MaxPages
	}
	if cfg.BackgroundConcurrency <= 0 {
		cfg.BackgroundConcurrency = 1
	}
	if cfg.BackgroundConcurrency > 16 {
		cfg.BackgroundConcurrency = 16
	}
	return &ThreadResolver{
		store: store, federator: federator,
		importer: NewInboxProcessor(store, federator, domain),
		config:   cfg, inflight: make(map[string]chan struct{}),
		backgroundSlots: make(chan struct{}, cfg.BackgroundConcurrency), shutdownCtx: context.Background(),
		fetchObject: federator.FetchObject,
	}
}

func (r *ThreadResolver) SetShutdownContext(ctx context.Context) {
	if r == nil || ctx == nil {
		return
	}
	r.shutdownCtx = ctx
	r.importer.ConfigureBackfill(ctx, r.config.BackgroundConcurrency)
}

func (r *ThreadResolver) interactiveBudget() fetchBudget {
	return fetchBudget{
		maxReplies:       r.config.MaxReplies,
		maxAncestorDepth: r.config.MaxAncestorDepth,
		maxPages:         r.config.MaxPages,
	}
}

func (r *ThreadResolver) backgroundBudget() fetchBudget {
	return fetchBudget{
		maxReplies:       r.config.BackgroundMaxReplies,
		maxAncestorDepth: r.config.MaxAncestorDepth,
		maxPages:         r.config.BackgroundMaxPages,
	}
}

// RefreshInBackground completes status context off the request path using the
// larger background budget, then returns immediately. It reports whether a
// crawl is in flight (started here or already running) so callers can hint the
// UI to poll for the thread filling in. A nil/disabled resolver, a non-remote
// status, or fresh context returns false.
func (r *ThreadResolver) RefreshInBackground(status *models.Status) bool {
	if r == nil || !r.config.Enabled || !r.config.BackgroundEnabled || r.federator == nil ||
		status == nil || status.Account == nil || status.Account.Domain == nil {
		return false
	}
	if status.ContextFetchedAt != nil && time.Since(*status.ContextFetchedAt) < r.config.StaleAfter {
		return false
	}
	// Background crawls use a distinct inflight namespace so they neither block
	// nor are blocked by the synchronous (API) Refresh path on the same status.
	key := "bg:" + status.URI
	owner, wait := r.acquire(key)
	if !owner {
		// A crawl is already running for this thread; keep the UI polling.
		return true
	}
	select {
	case r.backgroundSlots <- struct{}{}:
		// Capacity reserved before starting the goroutine, so both active work
		// and goroutine count remain bounded.
	default:
		r.release(key, wait)
		return false
	}
	// Snapshot identity; the caller's *status may be mutated or reused after we
	// return. The goroutine runs under its own background-scoped deadline.
	snapshot := *status
	go func() {
		defer func() { <-r.backgroundSlots }()
		defer r.release(key, wait)
		ctx, cancel := context.WithTimeout(r.shutdownCtx, r.config.BackgroundTimeout)
		defer cancel()
		started := time.Now()
		metrics.IncrementThreadFetchAttempts()
		if err := r.store.SetStatusContextFetchedAt(ctx, snapshot.ID, started); err != nil {
			slog.Warn("thread fetch (bg): failed to mark attempt", "status", snapshot.URI, "error", err)
		}
		if err := r.refresh(ctx, &snapshot, r.backgroundBudget()); err != nil {
			metrics.IncrementThreadFetchFailures()
			slog.Warn("thread fetch (bg): refresh failed; serving cached context", "status", snapshot.URI, "error", err)
		}
		metrics.ObserveThreadFetchDuration(time.Since(started))
	}()
	return true
}

// Fetching reports whether a background crawl for this thread URI is currently
// in flight. It starts nothing; the thread page uses it to decide whether to
// keep polling for newly imported replies.
func (r *ThreadResolver) Fetching(uri string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	_, ok := r.inflight["bg:"+uri]
	r.mu.Unlock()
	return ok
}

// Refresh completes status context within a single request-wide deadline.
func (r *ThreadResolver) Refresh(ctx context.Context, status *models.Status) {
	if r == nil || !r.config.Enabled || r.federator == nil || status == nil || status.Account == nil || status.Account.Domain == nil {
		return
	}
	if status.ContextFetchedAt != nil && time.Since(*status.ContextFetchedAt) < r.config.StaleAfter {
		metrics.IncrementThreadFetchCacheSkips()
		return
	}

	owner, wait := r.acquire(status.URI)
	if !owner {
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	defer r.release(status.URI, wait)

	// Re-check after obtaining ownership; a preceding caller may have refreshed it.
	if current, err := r.store.GetStatus(ctx, status.ID); err == nil && current.ContextFetchedAt != nil && time.Since(*current.ContextFetchedAt) < r.config.StaleAfter {
		metrics.IncrementThreadFetchCacheSkips()
		return
	}

	started := time.Now()
	metrics.IncrementThreadFetchAttempts()
	fetchCtx, cancel := context.WithTimeout(ctx, r.config.Timeout)
	defer cancel()
	now := time.Now()
	if err := r.store.SetStatusContextFetchedAt(ctx, status.ID, now); err != nil {
		slog.Warn("thread fetch: failed to mark attempt", "status", status.URI, "error", err)
	}

	if err := r.refresh(fetchCtx, status, r.interactiveBudget()); err != nil {
		metrics.IncrementThreadFetchFailures()
		slog.Warn("thread fetch: refresh failed; serving cached context", "status", status.URI, "error", err)
	}
	metrics.ObserveThreadFetchDuration(time.Since(started))
}

func (r *ThreadResolver) acquire(key string) (bool, chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.inflight[key]; ok {
		return false, ch
	}
	ch := make(chan struct{})
	r.inflight[key] = ch
	return true, ch
}

func (r *ThreadResolver) release(key string, ch chan struct{}) {
	r.mu.Lock()
	delete(r.inflight, key)
	close(ch)
	r.mu.Unlock()
}

func (r *ThreadResolver) refresh(ctx context.Context, focus *models.Status, budget fetchBudget) error {
	if err := r.ensureAllowedURI(ctx, focus.URI); err != nil {
		return err
	}
	if err := r.resolveAncestors(ctx, focus, budget); err != nil && ctx.Err() != nil {
		return err
	}

	doc, err := r.fetchObject(ctx, focus.URI)
	if err != nil {
		return err
	}
	if err := validateFetchedIdentity(focus.URI, doc); err != nil {
		return err
	}

	type pending struct {
		parentURI string
		doc       map[string]interface{}
	}
	queue := []pending{{parentURI: focus.URI, doc: doc}}
	seen := map[string]bool{focus.URI: true}
	stored := 0
	for len(queue) > 0 && stored < budget.maxReplies && ctx.Err() == nil {
		item := queue[0]
		queue = queue[1:]
		// Read each status's own replies collection relative to its own host so
		// cross-instance sub-threads are paginated against the correct origin.
		candidates, err := r.replyCandidates(ctx, item.doc["replies"], item.parentURI, budget)
		if err != nil {
			slog.Debug("thread fetch: replies collection unavailable", "parent", item.parentURI, "error", err)
			continue
		}
		for _, candidate := range candidates {
			if stored >= budget.maxReplies {
				break
			}
			uri := activityStreamsLink(candidate)
			if uri == "" || seen[uri] {
				continue
			}
			seen[uri] = true
			// Replies are commonly authored on other instances - Mastodon and
			// GTS aggregate the whole conversation into the replies collection.
			// An inline object is only trustworthy when the collection's own
			// origin also hosts it; anything cross-host is re-fetched from its
			// author's server so the object is self-asserted. importDocument
			// then enforces that the reply's host matches its attributedTo and
			// that it genuinely replies to item.parentURI, which is what makes
			// accepting a cross-origin reply safe.
			var replyDoc map[string]interface{}
			embedded, isEmbedded := candidate.(map[string]interface{})
			if isEmbedded && embedded["type"] != nil && sameURLHost(item.parentURI, uri) {
				replyDoc = embedded
			} else {
				if err := r.ensureAllowedURI(ctx, uri); err != nil {
					metrics.IncrementThreadFetchRejected("moderation")
					continue
				}
				fetched, err := r.fetchObject(ctx, uri)
				if err != nil {
					continue
				}
				replyDoc = fetched
			}
			status, created, err := r.importDocument(ctx, uri, item.parentURI, replyDoc)
			if err != nil {
				metrics.IncrementThreadFetchRejected("invalid")
				slog.Debug("thread fetch: rejected reply", "uri", uri, "error", err)
				continue
			}
			stored++
			metrics.IncrementThreadFetchRepliesFetched()
			if created {
				metrics.IncrementThreadFetchRepliesStored()
			}
			queue = append(queue, pending{parentURI: status.URI, doc: replyDoc})
		}
	}
	return ctx.Err()
}

func (r *ThreadResolver) resolveAncestors(ctx context.Context, status *models.Status, budget fetchBudget) error {
	current := status
	seen := map[string]bool{status.URI: true}
	for depth := 0; depth < budget.maxAncestorDepth && current.InReplyToURI != nil && *current.InReplyToURI != ""; depth++ {
		parentURI := *current.InReplyToURI
		if seen[parentURI] {
			return fmt.Errorf("ancestor cycle at %s", parentURI)
		}
		seen[parentURI] = true
		parent, err := r.store.GetStatusByURI(ctx, parentURI)
		if err != nil {
			if err := r.ensureAllowedURI(ctx, parentURI); err != nil {
				return err
			}
			doc, err := r.fetchObject(ctx, parentURI)
			if err != nil {
				return err
			}
			parent, _, err = r.importDocument(ctx, parentURI, "", doc)
			if err != nil {
				return err
			}
		}
		if err := r.store.LinkStatusRepliesByURI(ctx, parent.URI, parent.ID); err != nil {
			return err
		}
		current = parent
	}
	return nil
}

func (r *ThreadResolver) importDocument(ctx context.Context, requestedURI, expectedParent string, doc map[string]interface{}) (*models.Status, bool, error) {
	if err := validateFetchedIdentity(requestedURI, doc); err != nil {
		return nil, false, err
	}
	objType, _ := doc["type"].(string)
	if !isStatusObjectType(objType) {
		return nil, false, fmt.Errorf("unsupported object type %q", objType)
	}
	if expectedParent != "" && activityStreamsLink(doc["inReplyTo"]) != expectedParent {
		return nil, false, fmt.Errorf("reply parent mismatch")
	}
	authorURI := activityStreamsLink(doc["attributedTo"])
	if authorURI == "" || !sameURLHost(requestedURI, authorURI) {
		return nil, false, fmt.Errorf("status and actor hosts differ")
	}
	if err := r.ensureAllowedURI(ctx, authorURI); err != nil {
		return nil, false, err
	}
	author, err := r.federator.FetchActorLight(ctx, authorURI)
	if err != nil {
		return nil, false, err
	}
	status, err := r.importer.parseRemoteStatusInternal(ctx, doc, author, false)
	if err != nil {
		return nil, false, err
	}
	if status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted {
		return nil, false, fmt.Errorf("non-public reply visibility %q", status.Visibility)
	}
	status.ContextOnly = true
	marked := time.Now()
	status.ContextFetchedAt = &marked
	if existing, err := r.store.GetStatusByURI(ctx, status.URI); err == nil {
		if expectedParent != "" && (existing.InReplyToURI == nil || *existing.InReplyToURI != expectedParent) {
			return nil, false, fmt.Errorf("existing reply parent mismatch")
		}
		_ = r.store.SetStatusContextFetchedAt(ctx, existing.ID, marked)
		// existing may predate its parent (out-of-order federation delivery left
		// in_reply_to_id null even though in_reply_to_uri is correct) - repair the
		// link here too, not just on first creation, or GetStatusContext's
		// in_reply_to_id-keyed descendant walk will never surface it as a nested
		// reply again.
		r.relinkToParent(ctx, existing)
		return existing, false, nil
	}
	if err := r.store.CreateStatus(ctx, status); err != nil {
		if existing, getErr := r.store.GetStatusByURI(ctx, status.URI); getErr == nil {
			r.relinkToParent(ctx, existing)
			return existing, false, nil
		}
		return nil, false, err
	}
	// Tags and mentions are persisted by CreateStatus from status.Tags/Mentions.
	if err := r.store.LinkStatusRepliesByURI(ctx, status.URI, status.ID); err != nil {
		return nil, true, err
	}
	r.relinkToParent(ctx, status)
	return status, true, nil
}

// relinkToParent repairs status.InReplyToID from status.InReplyToURI when the
// parent is now known locally. Best-effort: errors just leave the link stale
// for the next pass.
func (r *ThreadResolver) relinkToParent(ctx context.Context, status *models.Status) {
	if status.InReplyToURI == nil {
		return
	}
	if parent, err := r.store.GetStatusByURI(ctx, *status.InReplyToURI); err == nil {
		_ = r.store.LinkStatusRepliesByURI(ctx, parent.URI, parent.ID)
	}
}

func (r *ThreadResolver) replyCandidates(ctx context.Context, collection interface{}, referenceURI string, budget fetchBudget) ([]interface{}, error) {
	if collection == nil {
		return nil, nil
	}
	var doc map[string]interface{}
	switch value := collection.(type) {
	case string:
		if !sameURLHost(referenceURI, value) {
			return nil, fmt.Errorf("collection host mismatch")
		}
		if err := r.ensureAllowedURI(ctx, value); err != nil {
			return nil, err
		}
		fetched, err := r.fetchObject(ctx, value)
		if err != nil {
			return nil, err
		}
		if id := activityStreamsLink(fetched["id"]); id != "" && id != value {
			return nil, fmt.Errorf("collection canonical id mismatch")
		}
		doc = fetched
	case map[string]interface{}:
		doc = value
	default:
		return nil, fmt.Errorf("invalid replies collection")
	}

	// Descend into the first page when the wrapper Collection carries no inline
	// items. Mastodon and most ActivityStreams servers embed the first page as
	// an object (with self-reply items and a next link to other-account
	// replies) rather than a fetchable link; reading it costs no network fetch.
	// max_pages bounds only fetched pages, so an inline first page never
	// consumes the budget and the next link can still be followed.
	if !hasInlineItems(doc) {
		switch first := doc["first"].(type) {
		case map[string]interface{}:
			doc = first
		case string:
			if first != "" {
				page, err := r.fetchCollectionPage(ctx, first, referenceURI)
				if err != nil {
					return nil, err
				}
				doc = page
			}
		}
	}

	items := make([]interface{}, 0, budget.maxReplies)
	fetched := 0
	seenPages := map[string]bool{}
	maxIterations := budget.maxPages*2 + 4
	for iterations := 0; doc != nil && len(items) < budget.maxReplies && iterations <= maxIterations; iterations++ {
		if raw, ok := doc["orderedItems"].([]interface{}); ok {
			items = append(items, raw...)
		}
		if raw, ok := doc["items"].([]interface{}); ok {
			items = append(items, raw...)
		}
		if len(items) > budget.maxReplies {
			items = items[:budget.maxReplies]
		}

		// An embedded next page costs no fetch; a linked one consumes budget.
		switch next := doc["next"].(type) {
		case map[string]interface{}:
			doc = next
			continue
		case string:
			if next == "" || seenPages[next] || !sameURLHost(referenceURI, next) {
				return items, nil
			}
			if fetched >= budget.maxPages {
				return items, nil
			}
			seenPages[next] = true
			page, err := r.fetchCollectionPage(ctx, next, referenceURI)
			if err != nil {
				return items, err
			}
			fetched++
			doc = page
		default:
			return items, nil
		}
	}
	return items, nil
}

// hasInlineItems reports whether a collection document already carries its
// items directly, meaning there is no need to descend into a first page.
func hasInlineItems(doc map[string]interface{}) bool {
	if _, ok := doc["orderedItems"].([]interface{}); ok {
		return true
	}
	_, ok := doc["items"].([]interface{})
	return ok
}

// fetchCollectionPage retrieves a linked collection page, enforcing that it
// stays on the reference origin and that its canonical id matches the request.
func (r *ThreadResolver) fetchCollectionPage(ctx context.Context, link, referenceURI string) (map[string]interface{}, error) {
	if !sameURLHost(referenceURI, link) {
		return nil, fmt.Errorf("collection page host mismatch")
	}
	if err := r.ensureAllowedURI(ctx, link); err != nil {
		return nil, err
	}
	doc, err := r.fetchObject(ctx, link)
	if err != nil {
		return nil, err
	}
	if id := activityStreamsLink(doc["id"]); id != "" && id != link {
		return nil, fmt.Errorf("collection page canonical id mismatch")
	}
	return doc, nil
}

func validateFetchedIdentity(requestedURI string, doc map[string]interface{}) error {
	id := activityStreamsLink(doc["id"])
	if id == "" {
		return fmt.Errorf("object has no id")
	}
	if id != requestedURI {
		return fmt.Errorf("canonical id mismatch: got %s", id)
	}
	if !sameURLHost(requestedURI, id) {
		return fmt.Errorf("canonical host mismatch")
	}
	return nil
}

func sameURLHost(a, b string) bool {
	au, errA := url.Parse(a)
	bu, errB := url.Parse(b)
	return errA == nil && errB == nil && au.Hostname() != "" &&
		strings.EqualFold(au.Scheme, bu.Scheme) && strings.EqualFold(au.Host, bu.Host)
}

func (r *ThreadResolver) ensureAllowedURI(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("invalid remote URI")
	}
	host := strings.ToLower(u.Hostname())
	if block, err := r.store.GetDomainBlock(ctx, host); err == nil && block.Severity == "suspend" {
		return fmt.Errorf("domain %s is suspended", host)
	}
	if instance, err := r.store.GetInstance(ctx, host); err == nil && instance.Suspended {
		return fmt.Errorf("instance %s is suspended", host)
	}
	return nil
}
