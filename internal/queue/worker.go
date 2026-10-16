// Package queue provides a simple background worker pool
// backed by the database job queue.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"net/url"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// JobHandler is a function that processes a job.
type JobHandler func(ctx context.Context, payload string) error

type permanentError struct{ error }

type rescheduleError struct{ at time.Time }

func (e rescheduleError) Error() string { return "job has more work" }

// Reschedule lets a long task release its worker between steps without using a retry.
func Reschedule(at time.Time) error { return rescheduleError{at: at} }

func (e permanentError) Unwrap() error { return e.error }

// Permanent marks malformed or semantically invalid work that another attempt
// cannot fix. The worker records it once instead of burning every retry.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func isPermanent(err error) bool {
	var target permanentError
	return errors.As(err, &target)
}

const (
	// DefaultQueueReserveDelay is how long worker 0 yields to interactive
	// requests before it is allowed one job anyway. It bounds how long the
	// queue can stall under uninterrupted frontend traffic without giving
	// background work a standing claim on the machine.
	DefaultQueueReserveDelay = 10 * time.Second

	// preemptionMaxAge is the age past which a job stops being preemptible.
	preemptionMaxAge = 2 * time.Minute

	// BackupJobKind never yields, since its snapshot has to finish once started.
	// For the same reason it must not sit on the pool's shared slots.
	BackupJobKind = "backup"
)

// WorkerPool manages background job workers.
type WorkerPool struct {
	store               storage.Storage
	handlers            map[string]JobHandler
	kinds               []string
	allowedKinds        map[string]bool
	numWorkers          int
	name                string
	terminalRetention   time.Duration
	staleRunningTimeout time.Duration
	maintenanceInterval time.Duration
	pollInterval        time.Duration
	pollBackoffMax      time.Duration
	wake                chan struct{}
	queueReserveDelay   time.Duration
	backgroundLimit     chan struct{}
	wg                  sync.WaitGroup
	cancel              context.CancelFunc
	stopOnce            sync.Once
	foreground          *ForegroundGate
}

// SetAllowedKinds restricts this process to a durable subset of queue kinds.
// It is used by self-destruct mode so normal queued work remains untouched.
func (wp *WorkerPool) SetAllowedKinds(kinds ...string) {
	wp.allowedKinds = make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		wp.allowedKinds[kind] = true
	}
}

func (wp *WorkerPool) SetForegroundGate(gate *ForegroundGate) { wp.foreground = gate }

// SetQueueReserveDelay overrides DefaultQueueReserveDelay: worker 0 yields to
// foreground work for the given duration before it may take one job anyway.
// Zero drops the reserve entirely, so every worker yields for as long as
// interactive requests keep arriving.
func (wp *WorkerPool) SetQueueReserveDelay(delay time.Duration) { wp.queueReserveDelay = delay }

// NewBackgroundLimit builds the limiter for pools whose jobs hold a database
// connection for most of their run. It leaves one connection for interactive
// work, so a request that arrives mid-job finds a free one instead of queueing
// behind an open transaction. With a single-connection pool there is nothing to
// hold back and the foreground gate remains the only arbiter.
//
// Give it only to such a pool. The light kinds are the counter-example: a
// delivery spends about 84% of its time waiting for the remote inbox to answer
// and well under 1% talking to the database, so a slot held across the whole
// job would serialize outbound federation to buy back a connection that was
// free almost the entire time.
func NewBackgroundLimit(maxOpenConns int) chan struct{} {
	slots := maxOpenConns - 1
	if slots < 1 {
		slots = 1
	}
	return make(chan struct{}, slots)
}

// SetBackgroundLimit caps how many jobs this pool runs at once. Pools given the
// same limiter share its slots. Pass the value from NewBackgroundLimit.
func (wp *WorkerPool) SetBackgroundLimit(limit chan struct{}) { wp.backgroundLimit = limit }

// SetName labels this pool in logs, metrics, and the admin queue view - e.g.
// "main" vs "light" when a deployment runs a dedicated pool alongside the
// main one (see cfg.Queue.LightWorkers). Unset defaults to "main", so a
// single-pool deployment (or a test that never calls SetName) still reports
// a stable, meaningful name instead of an empty label.
func (wp *WorkerPool) SetName(name string) { wp.name = name }

// Name returns this pool's label, defaulting to "main" when unset.
func (wp *WorkerPool) Name() string {
	if wp.name == "" {
		return "main"
	}
	return wp.name
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(store storage.Storage, numWorkers int) *WorkerPool {
	if numWorkers < 1 {
		numWorkers = 1
	}
	return &WorkerPool{
		store:               store,
		handlers:            make(map[string]JobHandler),
		numWorkers:          numWorkers,
		terminalRetention:   24 * time.Hour,
		staleRunningTimeout: 15 * time.Minute,
		maintenanceInterval: time.Hour,
		pollInterval:        2 * time.Second,
		pollBackoffMax:      30 * time.Second,
		wake:                make(chan struct{}, numWorkers),
		queueReserveDelay:   DefaultQueueReserveDelay,
	}
}

// ConfigureMaintenance sets queue recovery and terminal-job retention policy.
func (wp *WorkerPool) ConfigureMaintenance(retention, staleTimeout, interval time.Duration) {
	wp.terminalRetention = retention
	if staleTimeout > 0 {
		wp.staleRunningTimeout = staleTimeout
	}
	if interval > 0 {
		wp.maintenanceInterval = interval
	}
}

// ConfigurePolling sets the base poll interval and the idle backoff cap.
// Non-positive values keep the defaults. Local enqueues and retry scheduling
// both self-wake a worker, so the poll is a safety net for crash recovery and,
// in postgres multi-process mode, jobs enqueued by other processes - the cap
// bounds only that cross-process pickup latency.
func (wp *WorkerPool) ConfigurePolling(interval, backoffMax time.Duration) {
	if interval > 0 {
		wp.pollInterval = interval
	}
	if backoffMax > 0 {
		wp.pollBackoffMax = backoffMax
	}
	if wp.pollBackoffMax < wp.pollInterval {
		wp.pollBackoffMax = wp.pollInterval
	}
}

// Notify wakes an idle worker after a local enqueue, so just-enqueued jobs run
// immediately instead of waiting out the poll interval. Non-blocking and safe
// from any goroutine; the channel is bounded by the configured worker count.
func (wp *WorkerPool) Notify() {
	wp.NotifyN(1)
}

// NotifyN wakes up to n workers after a batch enqueue. It is non-blocking;
// excess signals are unnecessary because every awakened worker drains jobs.
func (wp *WorkerPool) NotifyN(n int) {
	if n > wp.numWorkers {
		n = wp.numWorkers
	}
	for range n {
		select {
		case wp.wake <- struct{}{}:
		default:
			return
		}
	}
}

// RegisterHandler registers a handler for a specific job kind. All handlers
// must be registered before Start: the kinds slice is read by the workers
// without locking.
func (wp *WorkerPool) RegisterHandler(kind string, handler JobHandler) {
	if wp.allowedKinds != nil && !wp.allowedKinds[kind] {
		return
	}
	if _, exists := wp.handlers[kind]; !exists {
		wp.kinds = append(wp.kinds, kind)
	}
	wp.handlers[kind] = handler
}

// Start starts the worker pool.
func (wp *WorkerPool) Start(ctx context.Context) {
	ctx, wp.cancel = context.WithCancel(ctx)
	wp.runMaintenance(ctx)
	// Named per-pool so a second concurrent pool (e.g. the optional light
	// pool) doesn't overwrite this one's gauge - see metrics.SetPoolConfiguredWorkers.
	metrics.SetPoolConfiguredWorkers(wp.Name(), int64(wp.numWorkers))

	for i := 0; i < wp.numWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(ctx, i)
	}
	wp.wg.Add(1)
	go wp.maintenanceLoop(ctx)

	slog.Info("worker pool started", "pool", wp.Name(), "workers", wp.numWorkers)
}

// Stop gracefully stops the worker pool.
func (wp *WorkerPool) Stop() {
	wp.stopOnce.Do(func() {
		if wp.cancel != nil {
			wp.cancel()
		}
		wp.wg.Wait()
		metrics.SetPoolConfiguredWorkers(wp.Name(), 0)
		slog.Info("worker pool stopped", "pool", wp.Name())
	})
}

func (wp *WorkerPool) maintenanceLoop(ctx context.Context) {
	defer wp.wg.Done()
	ticker := time.NewTicker(wp.maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			wp.runMaintenance(ctx)
		}
	}
}

func (wp *WorkerPool) runMaintenance(ctx context.Context) {
	// Maintenance runs from every pool sharing this jobs table (harmless,
	// idempotent duplication - see cmd/littlefedi/main.go); the pool label
	// on these logs is what makes that duplication legible instead of
	// looking like the same recovery ran twice by mistake.
	if recovered, err := wp.store.RecoverStaleJobs(ctx, time.Now().Add(-wp.staleRunningTimeout)); err != nil {
		slog.Warn("queue: stale job recovery failed", "pool", wp.Name(), "error", err)
	} else if recovered > 0 {
		slog.Info("queue: recovered stale running jobs", "pool", wp.Name(), "count", recovered)
	}
	// Shared fan-out payload rows are safe to prune once older than any
	// possible delivery retry (~2 days with DeliveryMaxAttempts; see
	// retryBackoffCap). 4 days leaves margin for a paused or backlogged queue.
	if deleted, err := wp.store.DeleteJobPayloadsBefore(ctx, time.Now().Add(-4*24*time.Hour)); err != nil {
		slog.Warn("queue: job payload cleanup failed", "pool", wp.Name(), "error", err)
	} else if deleted > 0 {
		slog.Debug("queue: pruned shared job payloads", "pool", wp.Name(), "count", deleted)
	}
	if wp.terminalRetention <= 0 {
		return
	}
	for {
		deleted, err := wp.store.DeleteTerminalJobsBefore(ctx, time.Now().Add(-wp.terminalRetention), 1000)
		if err != nil {
			slog.Warn("queue: terminal job cleanup failed", "pool", wp.Name(), "error", err)
			return
		}
		if deleted < 1000 {
			return
		}
	}
}

func (wp *WorkerPool) worker(ctx context.Context, id int) {
	defer wp.wg.Done()

	slog.Debug("worker started", "pool", wp.Name(), "id", id)

	timer := time.NewTimer(wp.pollInterval)
	defer timer.Stop()

	delay := wp.pollInterval
	for {
		select {
		case <-ctx.Done():
			slog.Debug("worker stopping", "pool", wp.Name(), "id", id)
			return
		case <-wp.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			delay = wp.pollInterval
		case <-timer.C:
		}

		// Drain everything currently runnable, then adjust the idle delay:
		// reset to the base interval after real work, back off exponentially
		// (capped) while the queue stays empty so an idle instance isn't
		// polling the database twice a second forever.
		worked := false
		for wp.processNextJob(ctx, id) {
			worked = true
			if ctx.Err() != nil {
				return
			}
		}
		if worked {
			delay = wp.pollInterval
		} else {
			delay *= 2
			if delay > wp.pollBackoffMax {
				delay = wp.pollBackoffMax
			}
		}
		timer.Reset(delay)
	}
}

// processNextJob claims and runs one job. It reports whether a job was
// dequeued (regardless of its outcome), so the caller knows the queue may
// still hold runnable work.
func (wp *WorkerPool) processNextJob(ctx context.Context, workerID int) bool {
	// Worker 0 is the pool's reserve: it yields like everyone else, but once
	// the frontend has been busy for queueReserveDelay it takes a single job
	// anyway so the queue cannot stall indefinitely. Every other worker yields
	// for as long as interactive requests keep arriving.
	forced := false
	releaseReserve := func() {}
	if workerID == 0 && wp.queueReserveDelay > 0 {
		var err error
		forced, releaseReserve, err = wp.foreground.WaitIdleOrReserve(ctx, wp.queueReserveDelay)
		if err != nil {
			return false
		}
	} else if err := wp.foreground.WaitIdle(ctx); err != nil {
		return false
	}
	defer releaseReserve()

	// Held across the claim as well as the work: claiming a job is itself a
	// write, so releasing earlier would let every worker pile onto the pool
	// again at exactly the moment a burst arrives.
	releaseLimit := func() {}
	if wp.backgroundLimit != nil {
		select {
		case wp.backgroundLimit <- struct{}{}:
			var once sync.Once
			releaseLimit = func() { once.Do(func() { <-wp.backgroundLimit }) }
			defer releaseLimit()
		case <-ctx.Done():
			return false
		}
	}

	job, err := wp.store.DequeueJob(ctx, wp.kinds)
	if err != nil {
		// No jobs available
		return false
	}

	// A backup spends its run in an external process and on the backup target,
	// not on a connection, so neither shared slot is doing anything for it. Both
	// are sized for jobs that do hold a connection, and a two-connection pool
	// gets one slot: keeping it across a slow snapshot stops the pool outright.
	if job.Kind == BackupJobKind {
		releaseLimit()
		releaseReserve()
	}

	started := time.Now()
	metrics.ObserveQueueAge(started.Sub(job.CreatedAt))
	defer func() { metrics.ObserveJobDuration(job.Kind, time.Since(started)) }()
	if !forced && wp.foreground.Active() {
		if err := wp.foreground.WaitIdle(ctx); err != nil {
			return true
		}
		_ = wp.store.YieldJob(context.WithoutCancel(ctx), job.ID, time.Now())
		return true
	}
	metrics.IncrementPoolActiveWorkers(wp.Name())
	defer metrics.DecrementPoolActiveWorkers(wp.Name())

	slog.Debug("worker processing job",
		"pool", wp.Name(),
		"worker", workerID,
		"kind", job.Kind,
		"id", job.ID,
	)

	handler, ok := wp.handlers[job.Kind]
	if !ok {
		// Try prefix match
		for kind, h := range wp.handlers {
			// Check if job.Kind starts with kind (e.g., "inbox_" prefix)
			if len(job.Kind) >= len(kind) && job.Kind[:len(kind)] == kind {
				handler = h
				break
			}
		}
		if handler == nil {
			slog.Warn("no handler for job kind", "kind", job.Kind)
			if err := wp.store.CompleteJob(ctx, job.ID); err != nil {
				slog.Error("queue: failed to complete unhandled job", "kind", job.Kind, "id", job.ID, "error", err)
			}
			return true
		}
	}

	// A backup must finish its consistent SQLite snapshot once started. A job
	// that has been bouncing off foreground traffic for longer than
	// preemptionMaxAge also runs to completion: yielding costs it no attempt,
	// so without this it never reaches a verdict on a busy instance, and a
	// delivery to a dead host keeps coming back instead of arming the
	// unreachable breaker.
	preemptible := !forced && job.Kind != BackupJobKind && time.Since(job.CreatedAt) < preemptionMaxAge
	jobCtx, cancelJob := wp.foreground.BackgroundContext(ctx, preemptible)
	defer cancelJob()

	// Pre-processing: for jobs that contact a single remote host (delivery,
	// media_cache), skip the work entirely when we have already given up on that
	// host, completing the job to free the worker slot. The gate differs by kind:
	// media caching is non-critical and paused on the first sign of trouble, while
	// delivery keeps trying until the host is fully offline. This is what stops one
	// dead instance's media from filling the queue with thousands of retrying jobs.
	if job.Kind != models.SelfDestructDeliveryJobKind {
		if host := jobTargetHost(job.Kind, job.Payload); host != "" {
			var ok bool
			var err error
			if job.Kind == "media_cache" {
				ok, err = wp.store.IsInstanceMediaCacheable(jobCtx, host)
			} else {
				ok, err = wp.store.IsInstanceReachable(jobCtx, host)
			}
			if err != nil {
				slog.Warn("queue: failed to check instance reachability", "domain", host, "kind", job.Kind, "error", err)
			} else if !ok {
				slog.Info("queue: skipping job to unreachable instance, completing",
					"domain", host, "kind", job.Kind, "job", job.ID)
				if err := wp.store.CompleteJob(ctx, job.ID); err != nil {
					slog.Error("queue: failed to complete skipped job", "domain", host, "kind", job.Kind, "id", job.ID, "error", err)
				}
				return true
			}
		}
	}

	if err := handler(jobCtx, job.Payload); err != nil {
		if ctx.Err() != nil {
			if yieldErr := wp.store.YieldJob(context.WithoutCancel(ctx), job.ID, time.Now()); yieldErr != nil {
				slog.Warn("queue: failed to return interrupted job", "kind", job.Kind, "id", job.ID, "error", yieldErr)
			}
			return true
		}
		var continuation rescheduleError
		if errors.As(err, &continuation) {
			if yieldErr := wp.store.YieldJob(context.WithoutCancel(ctx), job.ID, continuation.at); yieldErr != nil {
				slog.Error("queue: failed to reschedule job", "kind", job.Kind, "id", job.ID, "error", yieldErr)
			} else {
				delay := time.Until(continuation.at)
				if delay > 0 {
					time.AfterFunc(delay, wp.Notify)
				}
			}
			return true
		}
		// jobCtx.Err() is the direct preemption signal: BackgroundContext
		// cancels jobCtx when a foreground request arrives. Checking it
		// directly (rather than errors.Is(err, context.Canceled)) also
		// catches drivers (e.g. sqlite) that surface cancellation as their
		// own error text instead of wrapping context.Canceled.
		if jobCtx.Err() != nil && ctx.Err() == nil {
			metrics.IncrementQueuePreempted()
			if waitErr := wp.foreground.WaitIdle(ctx); waitErr != nil {
				return true
			}
			if yieldErr := wp.store.YieldJob(context.WithoutCancel(ctx), job.ID, time.Now().Add(100*time.Millisecond)); yieldErr != nil {
				slog.Warn("queue: failed to yield job for foreground request", "kind", job.Kind, "id", job.ID, "error", yieldErr)
			} else {
				slog.Debug("queue: job yielded to foreground request", "kind", job.Kind, "id", job.ID)
			}
			return true
		}
		slog.Error("job failed",
			"pool", wp.Name(),
			"kind", job.Kind,
			"id", job.ID,
			"error", err,
			"attempts", job.Attempts,
		)

		if isPermanent(err) || job.Attempts >= job.MaxAttempts {
			if failErr := wp.store.FailJob(ctx, job.ID, err); failErr != nil {
				slog.Error("queue: failed to mark job failed", "kind", job.Kind, "id", job.ID, "error", failErr)
			}
		} else {
			delay := retryBackoff(job.Attempts)
			if requeueErr := wp.store.RequeueJob(ctx, job.ID, time.Now().Add(delay)); requeueErr != nil {
				slog.Error("queue: failed to requeue job", "kind", job.Kind, "id", job.ID, "error", requeueErr)
				return true
			}
			// Self-wake when the retry comes due, so retry pickup doesn't
			// depend on the idle poll cadence (the poll stays as a safety net
			// for crash recovery and cross-process enqueues).
			time.AfterFunc(delay+time.Second, wp.Notify)
		}
		return true
	}

	if err := wp.store.CompleteJob(ctx, job.ID); err != nil {
		slog.Error("queue: failed to complete job", "kind", job.Kind, "id", job.ID, "error", err)
		return true
	}
	slog.Debug("job completed", "pool", wp.Name(), "kind", job.Kind, "id", job.ID)
	return true
}

// retryBackoffCap bounds the exponential growth so a high MaxAttempts can't
// push a single retry interval to absurd lengths. It is several
// hours, not minutes: federation delivery jobs use a high MaxAttempts so a
// remote instance that is down for routine maintenance (often overnight) still
// eventually receives the activity - mirroring Mastodon, whose delivery worker
// keeps retrying for ~2 days. The total retry window is governed by MaxAttempts
// per job kind (delivery long, media/push short), the cap only limits spacing.
const retryBackoffCap = 6 * time.Hour

// retryBackoff returns the delay before the next attempt of a job that has
// failed `attempts` times. It grows exponentially (2^attempts * 10s, capped at
// retryBackoffCap), then applies "equal jitter": half the delay is fixed and
// half is randomised. The jitter spreads out retries so that a batch of jobs
// failing together (e.g. a remote instance going down) doesn't re-fire in
// lockstep and stampede the same host on recovery.
func retryBackoff(attempts int) time.Duration {
	base := time.Duration(1<<uint(attempts)) * 10 * time.Second
	if base <= 0 || base > retryBackoffCap {
		base = retryBackoffCap
	}
	half := base / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// jobTargetHost returns the remote host a job will contact, for circuit-breaker
// checks, or "" if the job has no single target host. Kinds are matched by the
// literal strings the handlers register under ("delivery", "media_cache") to
// keep this package free of import cycles.
func jobTargetHost(kind, payload string) string {
	var raw string
	switch kind {
	case "delivery":
		var dp struct {
			Inbox string `json:"inbox"`
		}
		if json.Unmarshal([]byte(payload), &dp) == nil {
			raw = dp.Inbox
		}
	case "media_cache":
		var mp struct {
			RemoteURL string `json:"remote_url"`
		}
		if json.Unmarshal([]byte(payload), &mp) == nil {
			raw = mp.RemoteURL
		}
	}
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil {
		return parsed.Hostname()
	}
	return ""
}
