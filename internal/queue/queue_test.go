package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerPoolRunsJobs(t *testing.T) {
	store := testutil.NewStore(t)
	pool := queue.NewWorkerPool(store, 2)
	pool.ConfigurePolling(20*time.Millisecond, 100*time.Millisecond)

	payloads := make(chan string, 1)
	pool.RegisterHandler("echo", func(_ context.Context, payload string) error {
		payloads <- payload
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	if err := store.EnqueueJob(ctx, &models.Job{Kind: "echo", Payload: "hello", RunAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pool.Notify()

	select {
	case got := <-payloads:
		if got != "hello" {
			t.Errorf("handler got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job never ran")
	}
	waitFor(t, "the job to complete", func() bool {
		n, err := store.CountPendingJobs(ctx)
		return err == nil && n == 0
	})
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	store := testutil.NewStore(t)
	pool := queue.NewWorkerPool(store, 1)
	pool.ConfigurePolling(20*time.Millisecond, 100*time.Millisecond)

	calls := make(chan struct{}, 10)
	pool.RegisterHandler("broken", func(context.Context, string) error {
		calls <- struct{}{}
		return queue.Permanent(errors.New("malformed payload"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	if err := store.EnqueueJob(ctx, &models.Job{Kind: "broken", Payload: "{}", RunAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pool.Notify()
	waitFor(t, "the job to be recorded as failed", func() bool {
		problems, err := store.ListQueueProblemJobs(ctx, 10)
		return err == nil && len(problems) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if len(calls) != 1 {
		t.Errorf("permanent failure ran %d times, want 1", len(calls))
	}
}

func TestPermanentWrapsError(t *testing.T) {
	base := errors.New("bad")
	if err := queue.Permanent(base); !errors.Is(err, base) {
		t.Error("Permanent hides the underlying error")
	}
	if queue.Reschedule(time.Now()).Error() == "" {
		t.Error("Reschedule error has no message")
	}
}

func TestForegroundGate(t *testing.T) {
	gate := queue.NewForegroundGate()
	if gate.Active() {
		t.Fatal("new gate is active")
	}
	leave := gate.Enter()
	if !gate.Active() {
		t.Error("gate not active after Enter")
	}
	leave()
	if gate.Active() {
		t.Error("gate still active after leaving")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.WaitIdle(ctx); err != nil {
		t.Errorf("WaitIdle on an idle gate: %v", err)
	}
}

func TestNewBackgroundLimit(t *testing.T) {
	// One connection is always left free for foreground requests.
	if got := cap(queue.NewBackgroundLimit(8)); got != 7 {
		t.Errorf("capacity for 8 connections = %d, want 7", got)
	}
	if got := cap(queue.NewBackgroundLimit(1)); got != 1 {
		t.Errorf("capacity for 1 connection = %d, want 1", got)
	}
}
