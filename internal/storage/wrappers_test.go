package storage_test

import (
	"context"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage/migrations"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func TestMigrationsAreRegisteredAndIdempotent(t *testing.T) {
	if len(migrations.Migrations.Sorted()) == 0 {
		t.Fatal("no migrations registered")
	}
	store := testutil.NewStore(t)
	if err := store.RunMigrations(context.Background()); err != nil {
		t.Errorf("running migrations a second time: %v", err)
	}
}

// The server stacks these wrappers over the database; reads through them must
// match the database, and writes through them must not serve stale data.
func TestCachingWrappersAreTransparent(t *testing.T) {
	ctx := context.Background()
	inner := testutil.NewStore(t)
	entities := cache.New(1 << 20)
	timelines := cache.New(1 << 20)
	var store storage.Storage = storage.NewCachingStorage(inner, entities, time.Minute)
	store = storage.NewTimelineCachingStorage(store, timelines, time.Minute, 100)

	alice := localAccount(t, store, "alice")
	first, err := store.GetAccount(ctx, alice.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if _, err := store.GetAccount(ctx, alice.ID); err != nil {
		t.Fatalf("second GetAccount: %v", err)
	}
	if entities.Stats().Hits == 0 {
		t.Error("repeated account read never hit the cache")
	}

	first.DisplayName = "Renamed"
	if err := store.UpdateAccount(ctx, first); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if got, _ := store.GetAccount(ctx, alice.ID); got.DisplayName != "Renamed" {
		t.Errorf("cached read after update = %q, want Renamed", got.DisplayName)
	}

	before, err := store.GetPublicTimeline(ctx, "", true, nil, nil, nil, 20, false, false)
	if err != nil {
		t.Fatalf("GetPublicTimeline: %v", err)
	}
	status := post(t, store, alice, "fresh", models.VisibilityPublic)
	after, err := store.GetPublicTimeline(ctx, "", true, nil, nil, nil, 20, false, false)
	if err != nil {
		t.Fatalf("GetPublicTimeline: %v", err)
	}
	if len(after) != len(before)+1 || !containsIDs(after, status.ID) {
		t.Errorf("timeline after a new post = %v, served stale", statusIDs(after))
	}
}

func TestEnqueueSignalStorageNotifies(t *testing.T) {
	signals := 0
	store := storage.NewEnqueueSignalStorageN(testutil.NewStore(t), func(n int) { signals += n })
	if err := store.EnqueueJob(context.Background(), &models.Job{Kind: "k", Payload: "{}"}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if signals != 1 {
		t.Errorf("signals = %d, want 1", signals)
	}
}
