package app_test

import (
	"context"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func notificationsOfType(t *testing.T, f *fixture, accountID, kind string) []*models.Notification {
	t.Helper()
	all, err := f.store.ListNotifications(context.Background(), accountID, nil, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []*models.Notification
	for _, n := range all {
		if n.Type == kind {
			out = append(out, n)
		}
	}
	return out
}

// Mastodon's "poll" notification: when a poll ends, its author and everyone
// who voted in it hear about it.
func TestClosePollNotifiesAuthorAndVoters(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	carol := testutil.CreateLocalAccount(t, f.store, "carol")
	dave := testutil.CreateLocalAccount(t, f.store, "dave") // did not vote

	status, err := f.svc.CreateStatus(ctx, f.alice, app.StatusInput{
		Content: "Which?", Visibility: models.VisibilityPublic,
		PollOptions: []string{"tea", "coffee"}, PollExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
	poll, err := f.store.GetPollForStatus(ctx, status.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, voter := range []*models.Account{f.bob, carol, f.alice} {
		if _, err := f.store.VotePoll(ctx, poll.ID, voter.ID, []int{0}); err != nil {
			t.Fatalf("vote by %s: %v", voter.Username, err)
		}
	}

	if at, err := f.svc.ClosePollAndNotify(ctx, poll.ID); err != nil || at == nil {
		t.Fatalf("closing an open poll = %v, %v; want it rescheduled", at, err)
	}
	if n := notificationsOfType(t, f, f.bob.ID, "poll"); len(n) != 0 {
		t.Fatal("an open poll sent notifications")
	}

	past := time.Now().Add(-time.Minute)
	poll.ExpiresAt = &past
	if err := f.store.SyncPoll(ctx, poll); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ { // a second run must not duplicate anything
		if at, err := f.svc.ClosePollAndNotify(ctx, poll.ID); err != nil || at != nil {
			t.Fatalf("ClosePollAndNotify run %d = %v, %v", run, at, err)
		}
	}
	for _, who := range []*models.Account{f.alice, f.bob, carol} {
		got := notificationsOfType(t, f, who.ID, "poll")
		if len(got) != 1 || got[0].FromAccountID != f.alice.ID || got[0].StatusID == nil || *got[0].StatusID != status.ID {
			t.Errorf("%s: poll notifications = %+v, want one from alice for the poll", who.Username, got)
		}
	}
	if n := notificationsOfType(t, f, dave.ID, "poll"); len(n) != 0 {
		t.Error("someone who did not vote was notified")
	}
	if closed, _ := f.store.GetPoll(ctx, poll.ID, ""); closed == nil || !closed.Expired {
		t.Error("the poll was not closed")
	}
}

func TestClosePollForDeletedPollIsANoOp(t *testing.T) {
	f := newFixture(t)
	if at, err := f.svc.ClosePollAndNotify(context.Background(), "no-such-poll"); err != nil || at != nil {
		t.Errorf("ClosePollAndNotify(missing) = %v, %v", at, err)
	}
}

func TestNotifyAdminsSignUp(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.store.SetAdmin(ctx, f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	newcomer := testutil.CreateLocalAccount(t, f.store, "newcomer")
	f.svc.NotifyAdminsSignUp(ctx, newcomer)
	f.svc.NotifyAdminsSignUp(ctx, newcomer)

	got := notificationsOfType(t, f, f.alice.ID, "admin.sign_up")
	if len(got) != 1 || got[0].FromAccountID != newcomer.ID {
		t.Errorf("admin sign-up notifications = %+v, want one from the newcomer", got)
	}
	if n := notificationsOfType(t, f, f.bob.ID, "admin.sign_up"); len(n) != 0 {
		t.Error("a non-admin was told about the sign-up")
	}
}

// Remote polls have no local close job until a local account votes in one.
func TestVotingInARemotePollSchedulesItsClose(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	domain := "remote.example"
	now := time.Now()
	carol := &models.Account{Username: "carol", Domain: &domain, ActorURI: "https://remote.example/users/carol",
		InboxURI: "https://remote.example/users/carol/inbox", PublicKeyPem: "pem", PublicKeyID: "https://remote.example/users/carol#main-key",
		CreatedAt: now, UpdatedAt: now, FetchedAt: &now, Approved: true}
	if err := f.store.CreateAccount(ctx, carol); err != nil {
		t.Fatal(err)
	}
	status := &models.Status{AccountID: carol.ID, URI: carol.ActorURI + "/statuses/1", ContentHTML: "<p>vote</p>", Visibility: models.VisibilityPublic}
	if err := f.store.CreateStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	ends := now.Add(time.Hour)
	poll := &models.Poll{StatusID: status.ID, ExpiresAt: &ends, Options: []models.PollOption{{Title: "yes"}, {Title: "no"}}}
	if err := f.store.CreatePoll(ctx, poll); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VotePoll(ctx, f.bob, poll.ID, []int{1}); err != nil {
		t.Fatalf("VotePoll: %v", err)
	}
	// The job is not due yet, so it is read from the queue listing rather
	// than dequeued.
	jobs, err := f.store.ListActiveQueueJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found *models.Job
	for i := range jobs {
		if jobs[i].Kind == "poll_close" {
			found = &jobs[i]
		}
	}
	if found == nil {
		t.Fatal("no poll_close job queued for the remote poll")
	}
	if found.RunAt.Sub(ends).Abs() > time.Second {
		t.Errorf("poll_close runs at %v, want the poll's end %v", found.RunAt, ends)
	}
}
