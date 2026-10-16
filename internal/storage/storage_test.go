package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func localAccount(t *testing.T, store storage.Storage, username string) *models.Account {
	t.Helper()
	base := "https://" + testutil.Domain + "/users/" + username
	account := &models.Account{
		Username: username, ActorURI: base, InboxURI: base + "/inbox", OutboxURI: base + "/outbox",
		FollowersURI: base + "/followers", FollowingURI: base + "/following",
		PublicKeyPem: "pem", PublicKeyID: base + "#main-key", DisplayName: username,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Approved: true,
	}
	if err := store.CreateAccount(context.Background(), account); err != nil {
		t.Fatalf("CreateAccount(%s): %v", username, err)
	}
	return account
}

func post(t *testing.T, store storage.Storage, author *models.Account, text string, vis models.Visibility) *models.Status {
	t.Helper()
	status := &models.Status{AccountID: author.ID, ContentHTML: "<p>" + text + "</p>", ContentSource: text, Visibility: vis}
	status.URI = fmt.Sprintf("%s/statuses/%d", author.ActorURI, time.Now().UnixNano())
	if err := store.CreateStatus(context.Background(), status); err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
	return status
}

func TestAccountLifecycle(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := localAccount(t, store, "alice")
	if alice.ID == "" {
		t.Fatal("CreateAccount did not assign an ID")
	}

	byID, err := store.GetAccount(ctx, alice.ID)
	if err != nil || byID.Username != "alice" {
		t.Fatalf("GetAccount = %+v, %v", byID, err)
	}
	byName, err := store.GetAccountByUsernameDomain(ctx, "alice", nil)
	if err != nil || byName.ID != alice.ID {
		t.Fatalf("GetAccountByUsernameDomain = %+v, %v", byName, err)
	}
	byURI, err := store.GetAccountByURI(ctx, alice.ActorURI)
	if err != nil || byURI.ID != alice.ID {
		t.Fatalf("GetAccountByURI = %+v, %v", byURI, err)
	}

	byID.DisplayName = "Alice Liddell"
	if err := store.UpdateAccount(ctx, byID); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if again, _ := store.GetAccount(ctx, alice.ID); again.DisplayName != "Alice Liddell" {
		t.Errorf("DisplayName = %q after update", again.DisplayName)
	}

	results, err := store.SearchAccounts(ctx, "ali", 0, 10)
	if err != nil || len(results) != 1 || results[0].ID != alice.ID {
		t.Errorf("SearchAccounts = %v, %v", results, err)
	}
	if _, err := store.GetAccountByUsernameDomain(ctx, "nobody", nil); err == nil {
		t.Error("lookup of a missing account succeeded")
	}
}

func TestStatusesAndTimelines(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := localAccount(t, store, "alice")
	bob := localAccount(t, store, "bob")

	if err := store.CreateFollow(ctx, &models.Follow{AccountID: bob.ID, TargetAccountID: alice.ID, URI: bob.ActorURI + "#follows/1", State: models.FollowAccepted}); err != nil {
		t.Fatalf("CreateFollow: %v", err)
	}
	following, err := store.IsFollowing(ctx, bob.ID, alice.ID)
	if err != nil || !following {
		t.Fatalf("IsFollowing = %v, %v", following, err)
	}
	if n, _ := store.CountFollowers(ctx, alice.ID); n != 1 {
		t.Errorf("CountFollowers = %d", n)
	}

	first := post(t, store, alice, "hello world", models.VisibilityPublic)
	second := post(t, store, alice, "second post", models.VisibilityPublic)

	got, err := store.GetStatus(ctx, first.ID)
	if err != nil || got.ContentSource != "hello world" {
		t.Fatalf("GetStatus = %+v, %v", got, err)
	}
	if byURI, err := store.GetStatusByURI(ctx, first.URI); err != nil || byURI.ID != first.ID {
		t.Fatalf("GetStatusByURI = %+v, %v", byURI, err)
	}

	home, err := store.GetHomeTimeline(ctx, bob.ID, nil, nil, nil, 20, false, false)
	if err != nil {
		t.Fatalf("GetHomeTimeline: %v", err)
	}
	if !containsIDs(home, first.ID, second.ID) {
		t.Errorf("bob's home timeline = %v, want alice's posts", statusIDs(home))
	}
	if len(home) >= 2 && home[0].ID != second.ID {
		t.Errorf("home timeline is not newest first: %v", statusIDs(home))
	}

	public, err := store.GetPublicTimeline(ctx, "", true, nil, nil, nil, 20, false, false)
	if err != nil || !containsIDs(public, first.ID, second.ID) {
		t.Errorf("local public timeline = %v, %v", statusIDs(public), err)
	}
	profile, err := store.GetAccountStatuses(ctx, bob.ID, alice.ID, nil, nil, nil, 20, false, false, false, false)
	if err != nil || !containsIDs(profile, first.ID, second.ID) {
		t.Errorf("alice's profile = %v, %v", statusIDs(profile), err)
	}

	reply := &models.Status{AccountID: bob.ID, ContentHTML: "<p>hi</p>", Visibility: models.VisibilityPublic, InReplyToID: &first.ID, URI: bob.ActorURI + "/statuses/reply"}
	if err := store.CreateStatus(ctx, reply); err != nil {
		t.Fatalf("CreateStatus(reply): %v", err)
	}
	_, descendants, err := store.GetStatusContext(ctx, first.ID)
	if err != nil || !containsIDs(descendants, reply.ID) {
		t.Errorf("GetStatusContext descendants = %v, %v", statusIDs(descendants), err)
	}

	if err := store.DeleteStatus(ctx, second.ID); err != nil {
		t.Fatalf("DeleteStatus: %v", err)
	}
	if _, err := store.GetStatus(ctx, second.ID); err == nil {
		t.Error("deleted status is still readable")
	}
}

func TestFavouritesAndNotifications(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := localAccount(t, store, "alice")
	bob := localAccount(t, store, "bob")
	status := post(t, store, alice, "like me", models.VisibilityPublic)

	if err := store.CreateFavourite(ctx, &models.Favourite{AccountID: bob.ID, StatusID: status.ID}); err != nil {
		t.Fatalf("CreateFavourite: %v", err)
	}
	if fave, err := store.GetFavourite(ctx, bob.ID, status.ID); err != nil || fave == nil {
		t.Fatalf("GetFavourite = %v, %v", fave, err)
	}
	faves, err := store.ListFavourites(ctx, bob.ID, nil, 10)
	if err != nil || !containsIDs(faves, status.ID) {
		t.Errorf("ListFavourites = %v, %v", statusIDs(faves), err)
	}

	notif := &models.Notification{Type: "favourite", AccountID: alice.ID, FromAccountID: bob.ID, StatusID: &status.ID}
	if err := store.CreateNotification(ctx, notif); err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}
	notifs, err := store.ListNotifications(ctx, alice.ID, nil, 10, nil)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	found := false
	for _, n := range notifs {
		found = found || (n.Type == "favourite" && n.FromAccountID == bob.ID)
	}
	if !found {
		t.Errorf("favourite notification missing from %d notifications", len(notifs))
	}

	if err := store.DeleteFavourite(ctx, bob.ID, status.ID); err != nil {
		t.Fatalf("DeleteFavourite: %v", err)
	}
	if fave, err := store.GetFavourite(ctx, bob.ID, status.ID); err == nil && fave != nil {
		t.Error("favourite still present after delete")
	}
}

func TestJobQueue(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)

	job := &models.Job{Kind: "test_kind", Payload: `{"n":1}`, RunAt: time.Now().Add(-time.Second)}
	if err := store.EnqueueJob(ctx, job); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if n, err := store.CountPendingJobs(ctx); err != nil || n < 1 {
		t.Fatalf("CountPendingJobs = %d, %v", n, err)
	}
	claimed, err := store.DequeueJob(ctx, []string{"test_kind"})
	if err != nil || claimed == nil {
		t.Fatalf("DequeueJob = %v, %v", claimed, err)
	}
	if claimed.Payload != `{"n":1}` || claimed.State != "running" {
		t.Errorf("claimed job = %+v", claimed)
	}
	if again, err := store.DequeueJob(ctx, []string{"test_kind"}); err == nil && again != nil {
		t.Error("a running job was handed out twice")
	}
	if err := store.CompleteJob(ctx, claimed.ID); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	failing := &models.Job{Kind: "test_kind", Payload: "{}", RunAt: time.Now().Add(-time.Second), MaxAttempts: 1}
	if err := store.EnqueueJob(ctx, failing); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.DequeueJob(ctx, []string{"test_kind"})
	if err != nil || claimed == nil {
		t.Fatalf("DequeueJob = %v, %v", claimed, err)
	}
	if err := store.FailJob(ctx, claimed.ID, errors.New("boom")); err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	problems, err := store.ListQueueProblemJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueueProblemJobs: %v", err)
	}
	if len(problems) != 1 {
		t.Errorf("problem jobs = %d, want the exhausted job", len(problems))
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := localAccount(t, store, "alice")
	session := &models.Session{ID: "s1", AccountID: alice.ID, TokenHash: "hash-1", CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := store.GetSessionByTokenHash(ctx, "hash-1")
	if err != nil || got.AccountID != alice.ID {
		t.Fatalf("GetSessionByTokenHash = %+v, %v", got, err)
	}
	if err := store.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := store.GetSessionByTokenHash(ctx, "hash-1"); err == nil {
		t.Error("session still valid after delete")
	}
}

func TestNewStatusIDIsTimeOrdered(t *testing.T) {
	earlier := storage.NewStatusID(time.Now().Add(-time.Hour))
	later := storage.NewStatusID(time.Now())
	if !(earlier < later) {
		t.Errorf("status IDs do not sort by time: %q !< %q", earlier, later)
	}
}

func containsIDs(statuses []*models.Status, ids ...string) bool {
	have := map[string]bool{}
	for _, s := range statuses {
		have[s.ID] = true
	}
	for _, id := range ids {
		if !have[id] {
			return false
		}
	}
	return true
}

func statusIDs(statuses []*models.Status) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = s.ID
	}
	return out
}
