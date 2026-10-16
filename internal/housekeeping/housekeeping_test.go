package housekeeping_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/housekeeping"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func newHousekeeper(t *testing.T, store storage.Storage, pruneRemoteDays int) *housekeeping.Housekeeper {
	t.Helper()
	media := t.TempDir()
	return housekeeping.New(store, testutil.Domain, media, time.Hour,
		pruneRemoteDays, 0, 0, 0, 0, 100, 0, 0, filepath.Join(media, "cache"))
}

func remoteAccount(t *testing.T, store storage.Storage) *models.Account {
	t.Helper()
	domain := "remote.example"
	base := "https://remote.example/users/carol"
	account := &models.Account{
		Username: "carol", Domain: &domain, ActorURI: base, InboxURI: base + "/inbox",
		PublicKeyPem: "pem", PublicKeyID: base + "#main-key", DisplayName: "carol",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Approved: true,
	}
	if err := store.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account
}

func remoteStatus(t *testing.T, store storage.Storage, author *models.Account, id string, age time.Duration) *models.Status {
	t.Helper()
	created := time.Now().Add(-age)
	status := &models.Status{
		AccountID: author.ID, URI: author.ActorURI + "/statuses/" + id, ContentHTML: "<p>" + id + "</p>",
		Visibility: models.VisibilityPublic, CreatedAt: created, UpdatedAt: created,
	}
	if err := store.CreateStatus(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPruneRemoteStatuses(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	carol := remoteAccount(t, store)
	old := remoteStatus(t, store, carol, "old", 40*24*time.Hour)
	fresh := remoteStatus(t, store, carol, "fresh", time.Hour)

	if err := newHousekeeper(t, store, 30).PruneRemoteStatuses(ctx); err != nil {
		t.Fatalf("PruneRemoteStatuses: %v", err)
	}
	if _, err := store.GetStatus(ctx, old.ID); err == nil {
		t.Error("a remote status older than the retention window survived")
	}
	if _, err := store.GetStatus(ctx, fresh.ID); err != nil {
		t.Errorf("a recent remote status was pruned: %v", err)
	}
}

func TestExpireStatus(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	expired := &models.Status{AccountID: alice.ID, URI: alice.ActorURI + "/statuses/1", ContentHTML: "<p>bye</p>", Visibility: models.VisibilityPublic, ExpiresAt: &past}
	pending := &models.Status{AccountID: alice.ID, URI: alice.ActorURI + "/statuses/2", ContentHTML: "<p>later</p>", Visibility: models.VisibilityPublic, ExpiresAt: &future}
	for _, s := range []*models.Status{expired, pending} {
		if err := store.CreateStatus(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	h := newHousekeeper(t, store, 0)
	for _, s := range []*models.Status{expired, pending} {
		if err := h.ExpireStatus(ctx, s.ID); err != nil {
			t.Fatalf("ExpireStatus(%s): %v", s.ID, err)
		}
	}
	if _, err := store.GetStatus(ctx, expired.ID); err == nil {
		t.Error("an expired status survived")
	}
	if _, err := store.GetStatus(ctx, pending.ID); err != nil {
		t.Errorf("a status that has not expired yet was deleted: %v", err)
	}
	if err := h.ExpireStatus(ctx, "no-such-status"); err != nil {
		t.Errorf("expiring a missing status should be a no-op: %v", err)
	}
}

func TestRunOnceCompletes(t *testing.T) {
	store := testutil.NewStore(t)
	remoteStatus(t, store, remoteAccount(t, store), "old", 40*24*time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	newHousekeeper(t, store, 30).RunOnce(ctx)
	if ctx.Err() != nil {
		t.Fatal("RunOnce did not finish in time")
	}
}
