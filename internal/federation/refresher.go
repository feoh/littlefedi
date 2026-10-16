package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const ActorRefreshJobKind = "actor_refresh"

type actorRefreshPayload struct {
	AccountID string `json:"account_id"`
	Force     bool   `json:"force,omitempty"`
}

// ActorRefresher is a low-cost periodic dispatcher. It only claims and queues
// bounded work; durable queue workers perform network I/O.
type ActorRefresher struct {
	store                                    storage.Storage
	federator                                *Federator
	interval, refreshAge, onDemandAge, lease time.Duration
	batchSize, highWatermark                 int
}

func NewActorRefresher(store storage.Storage, federator *Federator, interval, refreshAge, onDemandAge, lease time.Duration, batchSize, highWatermark int) *ActorRefresher {
	if refreshAge <= 0 {
		refreshAge = 7 * 24 * time.Hour
	}
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	if onDemandAge <= 0 {
		onDemandAge = 24 * time.Hour
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	if highWatermark <= 0 {
		highWatermark = 100
	}
	return &ActorRefresher{store: store, federator: federator, interval: interval, refreshAge: refreshAge, onDemandAge: onDemandAge, lease: lease, batchSize: batchSize, highWatermark: highWatermark}
}

// EnqueueIfStale implements stale-while-revalidate for non-critical reads. It
// performs only bounded local DB work and always returns the cached account to
// the caller; network I/O remains in a low-priority queue worker.
func (r *ActorRefresher) EnqueueIfStale(ctx context.Context, account *models.Account) bool {
	if account == nil || account.Domain == nil || account.SuspendedAt != nil {
		return false
	}
	// A light fetch (inbox actor discovery, e.g. from a boost) sets FetchedAt
	// but never enriches counts, so a zero-counts account is always due for
	// refresh even if "fresh" by time alone - otherwise it can sit at 0
	// followers/following/posts for a full onDemandAge window.
	neverEnriched := account.FollowersCount == 0 && account.FollowingCount == 0 && account.StatusesCount == 0
	if !neverEnriched && account.FetchedAt != nil && time.Since(*account.FetchedAt) < r.onDemandAge {
		return false
	}
	pending, err := r.store.CountPendingJobs(ctx)
	if err != nil || pending >= r.highWatermark {
		return false
	}
	now := time.Now()
	claimed, err := r.store.ClaimActorRefresh(ctx, account.ID, now, now.Add(r.lease))
	if err != nil || !claimed {
		return false
	}
	payload, _ := json.Marshal(actorRefreshPayload{AccountID: account.ID, Force: true})
	if err := r.store.EnqueueJob(ctx, &models.Job{Kind: ActorRefreshJobKind, Payload: string(payload), RunAt: now, MaxAttempts: 1, State: "queued", Priority: -100}); err != nil {
		_ = r.store.ReleaseActorRefreshClaim(ctx, account.ID)
		return false
	}
	return true
}

// EnqueueAll queues a forced refresh for every known remote actor, regardless
// of its schedule. Used by the `admin refresh-actors` CLI command; the actual
// fetches still happen in the normal queue workers, not here.
func (r *ActorRefresher) EnqueueAll(ctx context.Context) (queued, total int, err error) {
	afterID := ""
	now := time.Now()
	for {
		accounts, err := r.store.ListRemoteAccounts(ctx, afterID, 500)
		if err != nil {
			return queued, total, err
		}
		if len(accounts) == 0 {
			return queued, total, nil
		}
		for _, account := range accounts {
			total++
			afterID = account.ID
			if account.SuspendedAt != nil {
				continue
			}
			claimed, err := r.store.ClaimActorRefresh(ctx, account.ID, now, now.Add(r.lease))
			if err != nil || !claimed {
				continue
			}
			payload, _ := json.Marshal(actorRefreshPayload{AccountID: account.ID, Force: true})
			if err := r.store.EnqueueJob(ctx, &models.Job{Kind: ActorRefreshJobKind, Payload: string(payload), RunAt: now, MaxAttempts: 1, State: "queued", Priority: -100}); err != nil {
				_ = r.store.ReleaseActorRefreshClaim(ctx, account.ID)
				continue
			}
			queued++
		}
		if len(accounts) < 500 {
			return queued, total, nil
		}
	}
}

// Run waits for the first tick so startup stays cheap.
func (r *ActorRefresher) Run(ctx context.Context) {
	if r.interval <= 0 {
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("actor refresh dispatcher: stopped")
			return
		case <-ticker.C:
			r.dispatch(ctx)
		}
	}
}

func (r *ActorRefresher) dispatch(ctx context.Context) {
	pending, err := r.store.CountPendingJobs(ctx)
	if err != nil {
		slog.Warn("actor refresh dispatcher: queue depth", "error", err)
		return
	}
	if pending >= r.highWatermark {
		return
	}
	limit := r.batchSize
	if available := r.highWatermark - pending; limit > available {
		limit = available
	}
	now := time.Now()
	accounts, err := r.store.ClaimDueActorRefreshes(ctx, now, now.Add(r.lease), limit)
	if err != nil {
		slog.Error("actor refresh dispatcher: claim", "error", err)
		return
	}
	for _, account := range accounts {
		payload, _ := json.Marshal(actorRefreshPayload{AccountID: account.ID})
		job := &models.Job{Kind: ActorRefreshJobKind, Payload: string(payload), RunAt: now, MaxAttempts: 1, State: "queued", Priority: -100}
		if err := r.store.EnqueueJob(ctx, job); err != nil {
			_ = r.store.ReleaseActorRefreshClaim(ctx, account.ID)
			slog.Warn("actor refresh dispatcher: enqueue", "account", account.ID, "error", err)
		}
	}
}

// ProcessJob records fetch failures as actor-level backoff rather than queue
// retries, preventing dead domains from filling the shared queue.
func (r *ActorRefresher) ProcessJob(ctx context.Context, payload string) error {
	var p actorRefreshPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.AccountID == "" {
		return fmt.Errorf("invalid actor refresh payload")
	}
	account, err := r.store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return err
	}
	if account.Domain == nil || account.SuspendedAt != nil {
		return r.store.ReleaseActorRefreshClaim(ctx, account.ID)
	}
	// Lazily initialize schedules for existing and newly discovered actors. This
	// avoids a migration-wide UPDATE and avoids re-fetching a profile that was
	// just learned on the next dispatcher tick.
	if !p.Force && account.ActorNextRefreshAt == nil && account.FetchedAt != nil && time.Since(*account.FetchedAt) < r.refreshAge {
		next := account.FetchedAt.Add(r.refreshAge + deterministicJitter(account.ID, r.refreshAge/4))
		return r.store.SetActorRefreshSchedule(ctx, account.ID, next, account.ActorRefreshFailures)
	}
	if reachable, err := r.store.IsInstanceReachable(ctx, *account.Domain); err == nil && !reachable {
		return r.scheduleFailure(ctx, account)
	}
	// EnrichCounts: this queue is now the only place remote profile counts are
	// refreshed (the inbox uses the light fetch - enriching on every inbound
	// activity cost several signed HTTPS fetches per message on slow hardware).
	if _, err := r.federator.RefreshActor(ctx, account.ActorURI, ActorRefreshOptions{Force: true, EnrichCounts: true}); err != nil {
		slog.Warn("actor refresh: fetch failed", "uri", account.ActorURI, "error", err)
		return r.scheduleFailure(ctx, account)
	}
	next := time.Now().Add(r.refreshAge + deterministicJitter(account.ID, r.refreshAge/4))
	return r.store.SetActorRefreshSchedule(ctx, account.ID, next, 0)
}

func (r *ActorRefresher) scheduleFailure(ctx context.Context, account *models.Account) error {
	failures := account.ActorRefreshFailures + 1
	delay := 30 * time.Minute
	for i := 1; i < failures && delay < 7*24*time.Hour; i++ {
		delay *= 4
		if delay > 7*24*time.Hour {
			delay = 7 * 24 * time.Hour
		}
	}
	delay += deterministicJitter(account.ID, delay/4)
	return r.store.SetActorRefreshSchedule(ctx, account.ID, time.Now().Add(delay), failures)
}

func deterministicJitter(id string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return time.Duration(h.Sum64() % uint64(window))
}
