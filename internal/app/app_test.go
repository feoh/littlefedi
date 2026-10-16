package app_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

type fixture struct {
	store      storage.Storage
	svc        *app.Service
	alice, bob *models.Account
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.NewStore(t)
	signer, err := federation.SetupInstanceActor(ctx, store, testutil.Domain)
	if err != nil {
		t.Fatal(err)
	}
	federator := federation.NewFederator(store, testutil.Domain, signer, "littleFedi-test")
	svc := app.New(store, federator, testutil.Domain)
	svc.SetImmediateDeliveries(true)
	return &fixture{
		store: store, svc: svc,
		alice: testutil.CreateLocalAccount(t, store, "alice"),
		bob:   testutil.CreateLocalAccount(t, store, "bob"),
	}
}

func TestValidateStatusInput(t *testing.T) {
	if err := app.ValidateStatusInput(app.StatusInput{Content: "hello"}); err != nil {
		t.Errorf("plain post rejected: %v", err)
	}
	bad := []app.StatusInput{
		{Content: "   "},
		{Content: "x", Visibility: "everyone"},
		{Content: "x", PollOptions: []string{"only one"}, PollExpiresIn: time.Hour},
		{Content: "x", PollOptions: []string{"a", "b"}, PollExpiresIn: time.Minute},
	}
	for _, input := range bad {
		if app.ValidateStatusInput(input) == nil {
			t.Errorf("ValidateStatusInput(%+v) accepted", input)
		}
	}
}

func TestParseFollowCSV(t *testing.T) {
	csv := "Account address,Show boosts\nalice@example.com,true\n@bob@example.org,false\nalice@example.com,true\n"
	handles, err := app.ParseFollowCSV(strings.NewReader(csv), 10)
	if err != nil {
		t.Fatalf("ParseFollowCSV: %v", err)
	}
	if want := []string{"alice@example.com", "bob@example.org"}; !reflect.DeepEqual(handles, want) {
		t.Errorf("handles = %v, want %v", handles, want)
	}
	if _, err := app.ParseFollowCSV(strings.NewReader(csv), 1); err == nil {
		t.Error("a CSV over the limit was accepted")
	}
}

func TestCreateStatusWithMentionAndTags(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	status, err := f.svc.CreateStatus(ctx, f.alice, app.StatusInput{
		Content: "Hi @bob, have you seen #golang news?", Visibility: models.VisibilityPublic, Language: "en",
	})
	if err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
	stored, err := f.store.GetStatus(ctx, status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.ContentHTML, `href="https://test.example/tags/golang" class="mention hashtag"`) {
		t.Errorf("hashtag not linked: %s", stored.ContentHTML)
	}
	if !strings.Contains(stored.ContentHTML, `<a href="https://test.example/users/bob" class="mention u-url"`) {
		t.Errorf("mention not linked to bob's actor: %s", stored.ContentHTML)
	}
	if stored.URI == "" || !strings.HasPrefix(stored.URI, "https://"+testutil.Domain+"/") {
		t.Errorf("URI = %q", stored.URI)
	}

	notifications, _ := f.store.ListNotifications(ctx, f.bob.ID, nil, 10, nil)
	found := false
	for _, n := range notifications {
		found = found || (n.Type == "mention" && n.FromAccountID == f.alice.ID)
	}
	if !found {
		t.Error("bob was not notified of the mention")
	}
}

func TestInteractions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	post, err := f.svc.CreateStatus(ctx, f.alice, app.StatusInput{Content: "interact with me", Visibility: models.VisibilityPublic})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Favourite(ctx, f.bob, post.ID); err != nil {
		t.Fatalf("Favourite: %v", err)
	}
	if fave, err := f.store.GetFavourite(ctx, f.bob.ID, post.ID); err != nil || fave == nil {
		t.Errorf("favourite not stored: %v", err)
	}
	if _, err := f.svc.Reblog(ctx, f.bob, post.ID); err != nil {
		t.Fatalf("Reblog: %v", err)
	}
	if _, err := f.svc.Bookmark(ctx, f.bob, post.ID); err != nil {
		t.Fatalf("Bookmark: %v", err)
	}
	if _, err := f.svc.PinStatus(ctx, f.alice, post.ID); err != nil {
		t.Fatalf("PinStatus: %v", err)
	}

	notifications, _ := f.store.ListNotifications(ctx, f.alice.ID, nil, 10, nil)
	kinds := map[string]bool{}
	for _, n := range notifications {
		kinds[n.Type] = true
	}
	if !kinds["favourite"] || !kinds["reblog"] {
		t.Errorf("alice's notifications = %v, want favourite and reblog", kinds)
	}

	if _, err := f.svc.Unfavourite(ctx, f.bob, post.ID); err != nil {
		t.Fatalf("Unfavourite: %v", err)
	}
	if fave, err := f.store.GetFavourite(ctx, f.bob.ID, post.ID); err == nil && fave != nil {
		t.Error("favourite survived Unfavourite")
	}
}

func TestEditAndDeleteStatus(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	post, err := f.svc.CreateStatus(ctx, f.alice, app.StatusInput{Content: "first draft", Visibility: models.VisibilityPublic})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := f.svc.EditStatus(ctx, f.alice, post.ID, app.EditStatusInput{Content: "second draft"})
	if err != nil {
		t.Fatalf("EditStatus: %v", err)
	}
	if !strings.Contains(edited.ContentHTML, "second draft") || edited.EditedAt == nil {
		t.Errorf("edited status = %q, edited at %v", edited.ContentHTML, edited.EditedAt)
	}
	if _, err := f.svc.EditStatus(ctx, f.bob, post.ID, app.EditStatusInput{Content: "hijack"}); err == nil {
		t.Error("bob edited alice's post")
	}

	if err := f.svc.DeleteStatus(ctx, f.bob, post.ID); err == nil {
		t.Error("bob deleted alice's post")
	}
	if err := f.svc.DeleteStatus(ctx, f.alice, post.ID); err != nil {
		t.Fatalf("DeleteStatus: %v", err)
	}
	if _, err := f.store.GetStatus(ctx, post.ID); err == nil {
		t.Error("deleted status still readable")
	}
}

func TestFollowBlockAndRelationship(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.svc.Follow(ctx, f.alice, f.bob.ID); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	rel := f.svc.Relationship(ctx, f.alice.ID, f.bob.ID)
	if !rel.Following || rel.Blocking {
		t.Errorf("relationship after follow = %+v", rel)
	}
	if back := f.svc.Relationship(ctx, f.bob.ID, f.alice.ID); !back.FollowedBy {
		t.Errorf("bob's view = %+v, want FollowedBy", back)
	}

	if err := f.svc.Block(ctx, f.bob, f.alice.ID); err != nil {
		t.Fatalf("Block: %v", err)
	}
	rel = f.svc.Relationship(ctx, f.alice.ID, f.bob.ID)
	if rel.Following || !rel.BlockedBy {
		t.Errorf("relationship after block = %+v, want unfollowed and blocked", rel)
	}
	if err := f.svc.Unblock(ctx, f.bob, f.alice.ID); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
}

func TestVisibility(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	carol := testutil.CreateLocalAccount(t, f.store, "carol")
	dm, err := f.svc.CreateStatus(ctx, f.alice, app.StatusInput{Content: "psst @bob", Visibility: models.VisibilityDirect})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := f.store.GetStatus(ctx, dm.ID)
	if !f.svc.CanViewStatus(ctx, stored, f.bob) {
		t.Error("the mentioned recipient cannot see the direct message")
	}
	if f.svc.CanViewStatus(ctx, stored, carol) {
		t.Error("an unrelated user can see the direct message")
	}
	if f.svc.CanViewStatus(ctx, stored, nil) {
		t.Error("an anonymous viewer can see the direct message")
	}
}

func TestListsAndScheduling(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	list, err := f.svc.CreateList(ctx, f.alice, "Friends", "list", false)
	if err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	if err := f.svc.AddListMembers(ctx, f.alice, list.ID, []string{f.bob.ID}); err == nil {
		t.Error("added an account alice does not follow")
	}
	if _, err := f.svc.Follow(ctx, f.alice, f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddListMembers(ctx, f.alice, list.ID, []string{f.bob.ID}); err != nil {
		t.Fatalf("AddListMembers: %v", err)
	}
	if _, err := f.svc.GetOwnedList(ctx, f.bob, list.ID); err == nil {
		t.Error("bob can read alice's list")
	}

	when := time.Now().Add(time.Hour)
	scheduled, err := f.svc.ScheduleStatus(ctx, f.alice, app.StatusInput{Content: "later", Visibility: models.VisibilityPublic}, when)
	if err != nil {
		t.Fatalf("ScheduleStatus: %v", err)
	}
	if err := f.svc.CancelScheduledStatus(ctx, f.alice.ID, scheduled.ID); err != nil {
		t.Fatalf("CancelScheduledStatus: %v", err)
	}
}
