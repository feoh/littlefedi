// Package metrics provides basic Prometheus-style metrics for littleFedi.
// This is a minimal implementation that doesn't require the prometheus client library.
// It exposes counters and gauges in Prometheus text format at the /metrics endpoint.
package metrics

import (
	"database/sql"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	runtimemetrics "runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
)

// Metrics holds the application-level metrics.
var (
	mu sync.RWMutex

	// Request counters
	apiRequests   int64
	inboxRequests int64
	fedRequests   int64
	webRequests   int64

	// Status counters
	statusesCreated int64
	statusesDeleted int64

	// Times the memory watchdog had to raise the Go memory limit because the
	// collector could not reach the goal the previous one implied.
	memoryLimitEscalations int64

	// Federation counters
	deliveriesAttempted       int64
	deliveriesFailed          int64
	deliveriesSucceeded       int64
	inboxProcessed            int64
	inboxFailed               int64
	threadFetchAttempts       int64
	threadFetchCacheSkips     int64
	threadFetchRepliesFetched int64
	threadFetchRepliesStored  int64
	threadFetchFailures       int64
	threadFetchDurationNanos  int64
	threadFetchRejected       = map[string]int64{}
	federationPolicyRejected  = map[string]int64{}

	// Timeline stage timings (count + cumulative nanos per stage)
	timelineStages = map[string]*stageTiming{}

	// Cache invalidations by cause (which write path cooled which cache)
	cacheInvalidations = map[string]int64{}

	// Queue metrics
	queueEnqueued      int64
	queueCompleted     int64
	queueFailed        int64
	queueReserveGrants int64
	queuePreempted     int64

	// Error counters
	errorsTotal int64

	// Gauges (settable)
	queueDepth     int64
	pendingFollows int64

	// Per-worker-pool configured/active counts, keyed by pool name (see
	// queue.WorkerPool.Name). A plain pair of package-level counters can't
	// represent this: an optional second pool (the light-jobs pool) runs
	// concurrently with the main one, and each pool's Start/Stop must report
	// its own gauge rather than the last call overwriting a single shared
	// counter. ActiveWorkers/ConfiguredWorkers in MetricsSnapshot remain the
	// sum across pools for existing dashboards/alerts.
	workerPools = map[string]*WorkerPoolStats{}

	// Instance info (set once)
	domain          string
	version         string
	startAt         time.Time
	cacheProviders  map[string]func() cache.Stats
	dbStatsProvider func() sql.DBStats
)

var durationBuckets = [...]time.Duration{
	5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
	500 * time.Millisecond, time.Second, 2500 * time.Millisecond, 5 * time.Second, 10 * time.Second,
}

type durationHistogram struct {
	buckets [len(durationBuckets) + 1]atomic.Uint64
	count   atomic.Uint64
	sumNS   atomic.Int64
}

var (
	requestDurations = map[string]*durationHistogram{
		"api": {}, "inbox": {}, "web": {}, "federation": {},
	}
	queueAgeDuration = &durationHistogram{}
	jobRunDuration   = &durationHistogram{}
	jobKindDurations = map[string]*durationHistogram{}
	inboxDurations   = map[string]*durationHistogram{}
	inboxStages      = map[string]*durationHistogram{}
	deliveryStages   = map[string]*durationHistogram{}
)

func (h *durationHistogram) observe(d time.Duration) {
	idx := len(durationBuckets)
	for i, bound := range durationBuckets {
		if d <= bound {
			idx = i
			break
		}
	}
	h.buckets[idx].Add(1)
	h.count.Add(1)
	h.sumNS.Add(int64(d))
}

func ObserveRequestDuration(class string, d time.Duration) {
	if h := requestDurations[class]; h != nil {
		h.observe(d)
	}
}

func ObserveQueueAge(d time.Duration) {
	if d < 0 {
		d = 0
	}
	queueAgeDuration.observe(d)
}

func observeNamedDuration(group map[string]*durationHistogram, name string, d time.Duration) {
	mu.Lock()
	h := group[name]
	if h == nil {
		h = &durationHistogram{}
		group[name] = h
	}
	mu.Unlock()
	h.observe(d)
}

func ObserveJobDuration(kind string, d time.Duration) {
	jobRunDuration.observe(d)
	observeNamedDuration(jobKindDurations, kind, d)
}

func ObserveInboxActivityDuration(activityType string, d time.Duration) {
	switch activityType {
	case "Follow", "Accept", "Reject", "Undo", "Create", "Delete", "Update",
		"Like", "EmojiReact", "Announce", "Block", "Flag", "Add", "Remove",
		"Move", "QuoteRequest":
	default:
		activityType = "Other"
	}
	observeNamedDuration(inboxDurations, activityType, d)
}

func ObserveInboxStage(stage string, d time.Duration) {
	observeNamedDuration(inboxStages, stage, d)
}

func ObserveDeliveryStage(stage string, d time.Duration) {
	observeNamedDuration(deliveryStages, stage, d)
}

func SetDBStatsProvider(provider func() sql.DBStats) {
	mu.Lock()
	dbStatsProvider = provider
	mu.Unlock()
}

type stageTiming struct {
	Count int64
	Nanos int64
}

// StageTimingSnapshot reports how often a timeline pipeline stage ran and its
// cumulative and mean duration.
type StageTimingSnapshot struct {
	Count int64
	Total time.Duration
	Mean  time.Duration
}

// MetricsSnapshot is an immutable, point-in-time view of application metrics. It is
// used by both the Prometheus endpoint and the admin UI so callers never need
// to parse the text exposition format.
type MetricsSnapshot struct {
	Domain, Version                                                           string
	StartedAt                                                                 time.Time
	Uptime                                                                    time.Duration
	APIRequests, InboxRequests, FederationRequests, WebRequests               int64
	StatusesCreated, StatusesDeleted                                          int64
	DeliveriesAttempted, DeliveriesFailed, DeliveriesSucceeded                int64
	InboxProcessed, InboxFailed                                               int64
	ThreadFetchAttempts, ThreadFetchCacheSkips                                int64
	ThreadFetchRepliesFetched, ThreadFetchRepliesStored, ThreadFetchFailures  int64
	ThreadFetchDuration                                                       time.Duration
	ThreadFetchRejected                                                       map[string]int64
	FederationPolicyRejected                                                  map[string]int64
	QueueEnqueued, QueueCompleted, QueueFailed                                int64
	QueueReserveGrants, QueuePreempted                                        int64
	ErrorsTotal, QueueDepth, ActiveWorkers, ConfiguredWorkers, PendingFollows int64
	TimelineStages                                                            map[string]StageTimingSnapshot
	CacheInvalidations                                                        map[string]int64
	// WorkerPools breaks ActiveWorkers/ConfiguredWorkers down by pool name
	// (see queue.WorkerPool.Name), e.g. "main" and, when enabled, "light".
	WorkerPools map[string]WorkerPoolStats
}

// SetCacheProviders exposes process-local cache health without coupling cache
// operations to the metrics package.
func SetCacheProviders(providers map[string]func() cache.Stats) {
	mu.Lock()
	defer mu.Unlock()
	cacheProviders = providers
}

// Snapshot returns a consistent-enough lock-free view of counters and
// gauges. The metadata map is copied under its mutex.
func Snapshot() MetricsSnapshot {
	mu.RLock()
	rejected := make(map[string]int64, len(threadFetchRejected))
	for reason, count := range threadFetchRejected {
		rejected[reason] = count
	}
	policyRejected := make(map[string]int64, len(federationPolicyRejected))
	for key, count := range federationPolicyRejected {
		policyRejected[key] = count
	}
	invalidations := make(map[string]int64, len(cacheInvalidations))
	for cause, count := range cacheInvalidations {
		invalidations[cause] = count
	}
	stages := make(map[string]StageTimingSnapshot, len(timelineStages))
	for stage, t := range timelineStages {
		snap := StageTimingSnapshot{Count: t.Count, Total: time.Duration(t.Nanos)}
		if t.Count > 0 {
			snap.Mean = time.Duration(t.Nanos / t.Count)
		}
		stages[stage] = snap
	}
	pools := make(map[string]WorkerPoolStats, len(workerPools))
	var totalConfigured, totalActive int64
	for name, s := range workerPools {
		pools[name] = *s
		totalConfigured += s.Configured
		totalActive += s.Active
	}
	d, v, started := domain, version, startAt
	mu.RUnlock()
	uptime := time.Duration(0)
	if !started.IsZero() {
		uptime = time.Since(started)
	}
	return MetricsSnapshot{
		Domain: d, Version: v, StartedAt: started, Uptime: uptime,
		APIRequests: atomic.LoadInt64(&apiRequests), InboxRequests: atomic.LoadInt64(&inboxRequests),
		FederationRequests: atomic.LoadInt64(&fedRequests), WebRequests: atomic.LoadInt64(&webRequests),
		StatusesCreated: atomic.LoadInt64(&statusesCreated), StatusesDeleted: atomic.LoadInt64(&statusesDeleted),
		DeliveriesAttempted: atomic.LoadInt64(&deliveriesAttempted), DeliveriesFailed: atomic.LoadInt64(&deliveriesFailed),
		DeliveriesSucceeded: atomic.LoadInt64(&deliveriesSucceeded), InboxProcessed: atomic.LoadInt64(&inboxProcessed),
		InboxFailed: atomic.LoadInt64(&inboxFailed), ThreadFetchAttempts: atomic.LoadInt64(&threadFetchAttempts),
		ThreadFetchCacheSkips:     atomic.LoadInt64(&threadFetchCacheSkips),
		ThreadFetchRepliesFetched: atomic.LoadInt64(&threadFetchRepliesFetched),
		ThreadFetchRepliesStored:  atomic.LoadInt64(&threadFetchRepliesStored),
		ThreadFetchFailures:       atomic.LoadInt64(&threadFetchFailures),
		ThreadFetchDuration:       time.Duration(atomic.LoadInt64(&threadFetchDurationNanos)),
		ThreadFetchRejected:       rejected, QueueEnqueued: atomic.LoadInt64(&queueEnqueued),
		FederationPolicyRejected: policyRejected,
		QueueCompleted:           atomic.LoadInt64(&queueCompleted), QueueFailed: atomic.LoadInt64(&queueFailed),
		QueueReserveGrants: atomic.LoadInt64(&queueReserveGrants),
		QueuePreempted:     atomic.LoadInt64(&queuePreempted),
		ErrorsTotal:        atomic.LoadInt64(&errorsTotal), QueueDepth: currentQueueDepth(),
		ActiveWorkers: totalActive, ConfiguredWorkers: totalConfigured,
		PendingFollows:     atomic.LoadInt64(&pendingFollows),
		TimelineStages:     stages,
		CacheInvalidations: invalidations,
		WorkerPools:        pools,
	}
}

// Init sets the static instance information.
func Init(d, v string) {
	mu.Lock()
	defer mu.Unlock()
	domain = d
	version = v
	startAt = time.Now()
}

// IncrementAPIRequests increments the API request counter.
func IncrementAPIRequests() {
	atomic.AddInt64(&apiRequests, 1)
}

// IncrementInboxRequests increments the inbox request counter.
func IncrementInboxRequests() {
	atomic.AddInt64(&inboxRequests, 1)
}

// IncrementMemoryLimitEscalations records that the memory watchdog raised the
// Go memory limit. Any value above zero means the limit the process derived at
// startup was too tight for what it went on to hold.
func IncrementMemoryLimitEscalations() { atomic.AddInt64(&memoryLimitEscalations, 1) }

func IncrementInboxProcessed() { atomic.AddInt64(&inboxProcessed, 1) }
func IncrementInboxFailed()    { atomic.AddInt64(&inboxFailed, 1) }
func IncrementDeliveriesAttempted() {
	atomic.AddInt64(&deliveriesAttempted, 1)
}
func IncrementDeliveriesFailed() {
	atomic.AddInt64(&deliveriesFailed, 1)
}
func IncrementDeliveriesSucceeded() {
	atomic.AddInt64(&deliveriesSucceeded, 1)
}

// IncrementFedRequests increments the federation request counter.
func IncrementFedRequests() {
	atomic.AddInt64(&fedRequests, 1)
}

// IncrementWebRequests increments the web UI request counter.
func IncrementWebRequests() {
	atomic.AddInt64(&webRequests, 1)
}

func IncrementThreadFetchAttempts()       { atomic.AddInt64(&threadFetchAttempts, 1) }
func IncrementThreadFetchCacheSkips()     { atomic.AddInt64(&threadFetchCacheSkips, 1) }
func IncrementThreadFetchRepliesFetched() { atomic.AddInt64(&threadFetchRepliesFetched, 1) }
func IncrementThreadFetchRepliesStored()  { atomic.AddInt64(&threadFetchRepliesStored, 1) }
func IncrementThreadFetchFailures()       { atomic.AddInt64(&threadFetchFailures, 1) }
func ObserveThreadFetchDuration(d time.Duration) {
	atomic.AddInt64(&threadFetchDurationNanos, int64(d))
}
func IncrementThreadFetchRejected(reason string) {
	mu.Lock()
	threadFetchRejected[reason]++
	mu.Unlock()
}

func IncrementFederationPolicyRejected(direction, reason string) {
	mu.Lock()
	federationPolicyRejected[direction+"\x00"+reason]++
	mu.Unlock()
}

// IncrementCacheInvalidation records one cache invalidation event by cause
// (e.g. "timeline_clear", "tag_windows", "viewer_timelines", "responses_full")
// so the write paths that cool the caches are visible without debug logging.
func IncrementCacheInvalidation(cause string) {
	mu.Lock()
	cacheInvalidations[cause]++
	mu.Unlock()
}

// ObserveTimelineStage records one execution of a timeline pipeline stage
// (e.g. "timeline_query", "timeline_refresh", "enrich", "page_build",
// "card_render") so build costs are visible without a profiler.
func ObserveTimelineStage(stage string, d time.Duration) {
	mu.Lock()
	t := timelineStages[stage]
	if t == nil {
		t = &stageTiming{}
		timelineStages[stage] = t
	}
	t.Count++
	t.Nanos += int64(d)
	mu.Unlock()
}

// IncrementQueueEnqueued increments the job enqueued counter.
func IncrementQueueEnqueued() {
	atomic.AddInt64(&queueEnqueued, 1)
}

// IncrementQueueCompleted increments the job completed counter.
func IncrementQueueCompleted() {
	atomic.AddInt64(&queueCompleted, 1)
}

// IncrementQueueFailed increments the job failed counter.
func IncrementQueueFailed() {
	atomic.AddInt64(&queueFailed, 1)
}

// IncrementQueueReserveGrants counts the times a worker ran a job only because
// the frontend had been busy for the whole reserve delay. A rate close to the
// reserve delay means the queue is being carried by the reserve alone, which
// is the signal to add a worker or lower the delay.
func IncrementQueueReserveGrants() {
	atomic.AddInt64(&queueReserveGrants, 1)
}

// IncrementQueuePreempted counts jobs cancelled part-way through and put back
// for a foreground request. Their work is redone on the next attempt, so a
// high count next to a flat completion count means the grace period is too
// short for the jobs this instance actually runs.
func IncrementQueuePreempted() {
	atomic.AddInt64(&queuePreempted, 1)
}

// SetQueueDepthFunc registers a callback evaluated at snapshot/scrape time, so
// the queue-depth COUNT query runs only when someone actually reads /metrics
// instead of on a fixed timer. Call before serving; not safe to swap later.
func SetQueueDepthFunc(f func() int64) {
	queueDepthFunc = f
}

var queueDepthFunc func() int64

func currentQueueDepth() int64 {
	if queueDepthFunc != nil {
		return queueDepthFunc()
	}
	return atomic.LoadInt64(&queueDepth)
}

// WorkerPoolStats is one named worker pool's configured/active worker count,
// as reported by /metrics (labeled) and the admin queue view.
type WorkerPoolStats struct {
	Configured int64
	Active     int64
}

// SetPoolConfiguredWorkers records (or, when count<=0, clears) a named
// worker pool's configured worker count. Each queue.WorkerPool calls this
// under its own Name() from Start/Stop, so a second concurrent pool never
// overwrites another's gauge.
func SetPoolConfiguredWorkers(pool string, count int64) {
	mu.Lock()
	defer mu.Unlock()
	if count <= 0 {
		delete(workerPools, pool)
		return
	}
	s, ok := workerPools[pool]
	if !ok {
		s = &WorkerPoolStats{}
		workerPools[pool] = s
	}
	s.Configured = count
}

func IncrementPoolActiveWorkers(pool string) { addPoolActiveWorkers(pool, 1) }
func DecrementPoolActiveWorkers(pool string) { addPoolActiveWorkers(pool, -1) }

func addPoolActiveWorkers(pool string, delta int64) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := workerPools[pool]
	if !ok {
		s = &WorkerPoolStats{}
		workerPools[pool] = s
	}
	s.Active += delta
}

// Handler returns an http.Handler that serves Prometheus-formatted metrics.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot := Snapshot()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		// Build Prometheus text format output
		fmt.Fprintf(w, `# HELP littlefedi_api_requests_total Total API requests
# TYPE littlefedi_api_requests_total counter
littlefedi_api_requests_total %d
# HELP littlefedi_inbox_requests_total Total inbox (federation) requests
# TYPE littlefedi_inbox_requests_total counter
littlefedi_inbox_requests_total %d
# HELP littlefedi_federation_requests_total Total outbound federation requests
# TYPE littlefedi_federation_requests_total counter
littlefedi_federation_requests_total %d
# HELP littlefedi_web_requests_total Total web UI requests
# TYPE littlefedi_web_requests_total counter
littlefedi_web_requests_total %d
# HELP littlefedi_statuses_created_total Total statuses created
# TYPE littlefedi_statuses_created_total counter
littlefedi_statuses_created_total %d
# HELP littlefedi_statuses_deleted_total Total statuses deleted
# TYPE littlefedi_statuses_deleted_total counter
littlefedi_statuses_deleted_total %d
# HELP littlefedi_deliveries_attempted_total Total delivery attempts
# TYPE littlefedi_deliveries_attempted_total counter
littlefedi_deliveries_attempted_total %d
# HELP littlefedi_deliveries_failed_total Total delivery failures
# TYPE littlefedi_deliveries_failed_total counter
littlefedi_deliveries_failed_total %d
# HELP littlefedi_deliveries_succeeded_total Total successful deliveries
# TYPE littlefedi_deliveries_succeeded_total counter
littlefedi_deliveries_succeeded_total %d
# HELP littlefedi_inbox_processed_total Total inbox activities processed
# TYPE littlefedi_inbox_processed_total counter
littlefedi_inbox_processed_total %d
# HELP littlefedi_inbox_failed_total Total inbox activities failed
# TYPE littlefedi_inbox_failed_total counter
littlefedi_inbox_failed_total %d
# HELP littlefedi_thread_fetch_attempts_total Total remote thread refresh attempts
# TYPE littlefedi_thread_fetch_attempts_total counter
littlefedi_thread_fetch_attempts_total %d
# HELP littlefedi_thread_fetch_cache_skips_total Thread refreshes skipped because cached data was fresh
# TYPE littlefedi_thread_fetch_cache_skips_total counter
littlefedi_thread_fetch_cache_skips_total %d
# HELP littlefedi_thread_fetch_replies_fetched_total Replies accepted during remote thread refresh
# TYPE littlefedi_thread_fetch_replies_fetched_total counter
littlefedi_thread_fetch_replies_fetched_total %d
# HELP littlefedi_thread_fetch_replies_stored_total New context-only replies stored
# TYPE littlefedi_thread_fetch_replies_stored_total counter
littlefedi_thread_fetch_replies_stored_total %d
# HELP littlefedi_thread_fetch_failures_total Remote thread refresh failures
# TYPE littlefedi_thread_fetch_failures_total counter
littlefedi_thread_fetch_failures_total %d
# HELP littlefedi_thread_fetch_duration_seconds_total Cumulative remote thread refresh duration
# TYPE littlefedi_thread_fetch_duration_seconds_total counter
littlefedi_thread_fetch_duration_seconds_total %.6f
# HELP littlefedi_queue_enqueued_total Total jobs enqueued
# TYPE littlefedi_queue_enqueued_total counter
littlefedi_queue_enqueued_total %d
# HELP littlefedi_queue_completed_total Total jobs completed
# TYPE littlefedi_queue_completed_total counter
littlefedi_queue_completed_total %d
# HELP littlefedi_queue_failed_total Total jobs failed
# TYPE littlefedi_queue_failed_total counter
littlefedi_queue_failed_total %d
# HELP littlefedi_queue_reserve_grants_total Jobs run only because the frontend stayed busy past the reserve delay
# TYPE littlefedi_queue_reserve_grants_total counter
littlefedi_queue_reserve_grants_total %d
# HELP littlefedi_queue_preempted_total Jobs cancelled part-way through for a foreground request
# TYPE littlefedi_queue_preempted_total counter
littlefedi_queue_preempted_total %d
# HELP littlefedi_errors_total Total application errors
# TYPE littlefedi_errors_total counter
littlefedi_errors_total %d
# HELP littlefedi_queue_depth Current number of pending jobs
# TYPE littlefedi_queue_depth gauge
littlefedi_queue_depth %d
# HELP littlefedi_active_workers Current number of active worker goroutines
# TYPE littlefedi_active_workers gauge
littlefedi_active_workers %d
# HELP littlefedi_configured_workers Configured number of queue workers
# TYPE littlefedi_configured_workers gauge
littlefedi_configured_workers %d
# HELP littlefedi_pending_follows Current number of pending follow requests
# TYPE littlefedi_pending_follows gauge
littlefedi_pending_follows %d
# HELP littlefedi_uptime_seconds Server uptime in seconds
# TYPE littlefedi_uptime_seconds gauge
littlefedi_uptime_seconds %d
# HELP littlefedi_info Static instance information
# TYPE littlefedi_info gauge
littlefedi_info{domain="%s",version="%s"} 1
`,
			snapshot.APIRequests,
			snapshot.InboxRequests,
			snapshot.FederationRequests,
			snapshot.WebRequests,
			snapshot.StatusesCreated,
			snapshot.StatusesDeleted,
			snapshot.DeliveriesAttempted,
			snapshot.DeliveriesFailed,
			snapshot.DeliveriesSucceeded,
			snapshot.InboxProcessed,
			snapshot.InboxFailed,
			snapshot.ThreadFetchAttempts,
			snapshot.ThreadFetchCacheSkips,
			snapshot.ThreadFetchRepliesFetched,
			snapshot.ThreadFetchRepliesStored,
			snapshot.ThreadFetchFailures,
			float64(snapshot.ThreadFetchDuration)/float64(time.Second),
			snapshot.QueueEnqueued,
			snapshot.QueueCompleted,
			snapshot.QueueFailed,
			snapshot.QueueReserveGrants,
			snapshot.QueuePreempted,
			snapshot.ErrorsTotal,
			snapshot.QueueDepth,
			snapshot.ActiveWorkers,
			snapshot.ConfiguredWorkers,
			snapshot.PendingFollows,
			int64(snapshot.Uptime.Seconds()),
			snapshot.Domain, snapshot.Version,
		)
		fmt.Fprintln(w, "# HELP littlefedi_thread_fetch_rejected_total Remote thread items rejected by reason")
		fmt.Fprintln(w, "# TYPE littlefedi_thread_fetch_rejected_total counter")
		for reason, count := range snapshot.ThreadFetchRejected {
			fmt.Fprintf(w, "littlefedi_thread_fetch_rejected_total{reason=%q} %d\n", reason, count)
		}
		fmt.Fprintln(w, "# HELP littlefedi_federation_policy_rejected_total Federation requests rejected by direction and reason")
		fmt.Fprintln(w, "# TYPE littlefedi_federation_policy_rejected_total counter")
		for key, count := range snapshot.FederationPolicyRejected {
			parts := strings.SplitN(key, "\x00", 2)
			fmt.Fprintf(w, "littlefedi_federation_policy_rejected_total{direction=%q,reason=%q} %d\n", parts[0], parts[1], count)
		}
		fmt.Fprintln(w, "# HELP littlefedi_timeline_stage_runs_total Timeline pipeline stage executions")
		fmt.Fprintln(w, "# TYPE littlefedi_timeline_stage_runs_total counter")
		for stage, t := range snapshot.TimelineStages {
			fmt.Fprintf(w, "littlefedi_timeline_stage_runs_total{stage=%q} %d\n", stage, t.Count)
			fmt.Fprintf(w, "littlefedi_timeline_stage_seconds_total{stage=%q} %.6f\n", stage, t.Total.Seconds())
		}
		fmt.Fprintln(w, "# HELP littlefedi_cache_invalidations_total Cache invalidation events by cause")
		fmt.Fprintln(w, "# TYPE littlefedi_cache_invalidations_total counter")
		for cause, count := range snapshot.CacheInvalidations {
			fmt.Fprintf(w, "littlefedi_cache_invalidations_total{cause=%q} %d\n", cause, count)
		}
		fmt.Fprintln(w, "# HELP littlefedi_worker_pool_configured_workers Configured workers by pool (e.g. main, light)")
		fmt.Fprintln(w, "# TYPE littlefedi_worker_pool_configured_workers gauge")
		fmt.Fprintln(w, "# HELP littlefedi_worker_pool_active_workers Active (busy) workers by pool")
		fmt.Fprintln(w, "# TYPE littlefedi_worker_pool_active_workers gauge")
		poolNames := make([]string, 0, len(snapshot.WorkerPools))
		for name := range snapshot.WorkerPools {
			poolNames = append(poolNames, name)
		}
		sort.Strings(poolNames)
		for _, name := range poolNames {
			s := snapshot.WorkerPools[name]
			fmt.Fprintf(w, "littlefedi_worker_pool_configured_workers{pool=%q} %d\n", name, s.Configured)
			fmt.Fprintf(w, "littlefedi_worker_pool_active_workers{pool=%q} %d\n", name, s.Active)
		}
		mu.RLock()
		providers := make(map[string]func() cache.Stats, len(cacheProviders))
		for name, provider := range cacheProviders {
			providers[name] = provider
		}
		mu.RUnlock()
		fmt.Fprintln(w, "# HELP littlefedi_cache_hits_total Cache hits by layer")
		fmt.Fprintln(w, "# TYPE littlefedi_cache_hits_total counter")
		for name, provider := range providers {
			s := provider()
			fmt.Fprintf(w, "littlefedi_cache_hits_total{layer=%q} %d\n", name, s.Hits)
			fmt.Fprintf(w, "littlefedi_cache_misses_total{layer=%q} %d\n", name, s.Misses)
			fmt.Fprintf(w, "littlefedi_cache_evictions_total{layer=%q} %d\n", name, s.Evictions)
			fmt.Fprintf(w, "littlefedi_cache_expirations_total{layer=%q} %d\n", name, s.Expirations)
			fmt.Fprintf(w, "littlefedi_cache_entries{layer=%q} %d\n", name, s.Entries)
			fmt.Fprintf(w, "littlefedi_cache_bytes{layer=%q} %d\n", name, s.Bytes)
			fmt.Fprintf(w, "littlefedi_cache_max_bytes{layer=%q} %d\n", name, s.MaxBytes)
		}
		writeHistogram := func(name string, labels string, h *durationHistogram) {
			var cumulative uint64
			for i, bound := range durationBuckets {
				cumulative += h.buckets[i].Load()
				fmt.Fprintf(w, "%s_bucket{%sle=\"%.3f\"} %d\n", name, labels, bound.Seconds(), cumulative)
			}
			cumulative += h.buckets[len(durationBuckets)].Load()
			fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", name, labels, cumulative)
			labelSet := strings.TrimSuffix(labels, ",")
			if labelSet == "" {
				fmt.Fprintf(w, "%s_sum %.6f\n", name, float64(h.sumNS.Load())/float64(time.Second))
				fmt.Fprintf(w, "%s_count %d\n", name, h.count.Load())
			} else {
				fmt.Fprintf(w, "%s_sum{%s} %.6f\n", name, labelSet, float64(h.sumNS.Load())/float64(time.Second))
				fmt.Fprintf(w, "%s_count{%s} %d\n", name, labelSet, h.count.Load())
			}
		}
		fmt.Fprintln(w, "# HELP littlefedi_request_duration_seconds Request duration by fixed route class")
		fmt.Fprintln(w, "# TYPE littlefedi_request_duration_seconds histogram")
		for _, class := range []string{"api", "inbox", "web", "federation"} {
			writeHistogram("littlefedi_request_duration_seconds", fmt.Sprintf("class=%q,", class), requestDurations[class])
		}
		fmt.Fprintln(w, "# HELP littlefedi_queue_age_seconds Age of a job when claimed")
		fmt.Fprintln(w, "# TYPE littlefedi_queue_age_seconds histogram")
		writeHistogram("littlefedi_queue_age_seconds", "", queueAgeDuration)
		fmt.Fprintln(w, "# HELP littlefedi_job_duration_seconds Queue job execution duration")
		fmt.Fprintln(w, "# TYPE littlefedi_job_duration_seconds histogram")
		writeHistogram("littlefedi_job_duration_seconds", "", jobRunDuration)
		writeNamed := func(metric, help, label string, group map[string]*durationHistogram) {
			mu.RLock()
			copy := make(map[string]*durationHistogram, len(group))
			for name, histogram := range group {
				copy[name] = histogram
			}
			mu.RUnlock()
			names := make([]string, 0, len(copy))
			for name := range copy {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Fprintf(w, "# HELP %s %s\n", metric, help)
			fmt.Fprintf(w, "# TYPE %s histogram\n", metric)
			for _, name := range names {
				writeHistogram(metric, fmt.Sprintf("%s=%q,", label, name), copy[name])
			}
		}
		writeNamed("littlefedi_job_kind_duration_seconds", "Queue job execution duration by kind", "kind", jobKindDurations)
		writeNamed("littlefedi_inbox_activity_duration_seconds", "Inbox processing duration by ActivityPub type", "type", inboxDurations)
		writeNamed("littlefedi_inbox_stage_duration_seconds", "Inbox processing duration by measured stage", "stage", inboxStages)
		writeNamed("littlefedi_delivery_stage_duration_seconds", "Outbound delivery duration by measured stage", "stage", deliveryStages)

		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		fmt.Fprintf(w, "# TYPE littlefedi_go_heap_alloc_bytes gauge\nlittlefedi_go_heap_alloc_bytes %d\n", mem.HeapAlloc)
		fmt.Fprintf(w, "# TYPE littlefedi_go_heap_sys_bytes gauge\nlittlefedi_go_heap_sys_bytes %d\n", mem.HeapSys)
		fmt.Fprintf(w, "# TYPE littlefedi_go_heap_objects gauge\nlittlefedi_go_heap_objects %d\n", mem.HeapObjects)
		fmt.Fprintf(w, "# TYPE littlefedi_go_gc_cycles_total counter\nlittlefedi_go_gc_cycles_total %d\n", mem.NumGC)
		fmt.Fprintf(w, "# TYPE littlefedi_go_gc_pause_seconds_total counter\nlittlefedi_go_gc_pause_seconds_total %.6f\n", float64(mem.PauseTotalNs)/float64(time.Second))
		// Heap goal beside live heap is the pair that makes a memory limit the
		// collector cannot reach visible at a glance: when the goal stops
		// sitting above the live heap and settles onto it, the GC is restarting
		// the moment it finishes and freeing nothing.
		//
		// Live heap and not HeapAlloc above it, which counts dead objects the
		// collector has not swept yet and therefore climbs toward the goal on
		// every cycle of any busy server. The goal is computed from the live
		// heap, so the live heap is what it has to be read against, and there
		// is no MemStats field carrying it.
		liveHeap := []runtimemetrics.Sample{{Name: "/gc/heap/live:bytes"}}
		runtimemetrics.Read(liveHeap)
		fmt.Fprintf(w, "# TYPE littlefedi_go_gc_heap_live_bytes gauge\nlittlefedi_go_gc_heap_live_bytes %d\n", liveHeap[0].Value.Uint64())
		fmt.Fprintf(w, "# TYPE littlefedi_go_gc_heap_goal_bytes gauge\nlittlefedi_go_gc_heap_goal_bytes %d\n", mem.NextGC)
		fmt.Fprintf(w, "# TYPE littlefedi_go_memory_limit_bytes gauge\nlittlefedi_go_memory_limit_bytes %d\n", debug.SetMemoryLimit(-1))
		fmt.Fprintf(w, "# TYPE littlefedi_go_memory_limit_escalations_total counter\nlittlefedi_go_memory_limit_escalations_total %d\n", atomic.LoadInt64(&memoryLimitEscalations))

		mu.RLock()
		dbProvider := dbStatsProvider
		mu.RUnlock()
		if dbProvider != nil {
			db := dbProvider()
			fmt.Fprintf(w, "# TYPE littlefedi_db_open_connections gauge\nlittlefedi_db_open_connections %d\n", db.OpenConnections)
			fmt.Fprintf(w, "# TYPE littlefedi_db_in_use_connections gauge\nlittlefedi_db_in_use_connections %d\n", db.InUse)
			fmt.Fprintf(w, "# TYPE littlefedi_db_idle_connections gauge\nlittlefedi_db_idle_connections %d\n", db.Idle)
			fmt.Fprintf(w, "# TYPE littlefedi_db_wait_total counter\nlittlefedi_db_wait_total %d\n", db.WaitCount)
			fmt.Fprintf(w, "# TYPE littlefedi_db_wait_seconds_total counter\nlittlefedi_db_wait_seconds_total %.6f\n", db.WaitDuration.Seconds())
			fmt.Fprintf(w, "# TYPE littlefedi_db_max_open_connections gauge\nlittlefedi_db_max_open_connections %d\n", db.MaxOpenConnections)
		}
	})
}
