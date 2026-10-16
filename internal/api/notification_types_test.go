package api_test

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// seedNotifications gives alice one notification of each listed stored type.
func seedNotifications(t *testing.T, f *fixture, types ...string) {
	t.Helper()
	for i, kind := range types {
		from := createAccount(t, f, "from"+string(rune('a'+i)))
		if err := f.store.CreateNotification(context.Background(), &models.Notification{
			Type: kind, AccountID: f.ids["alice"], FromAccountID: from, CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("create %s notification: %v", kind, err)
		}
	}
}

func notificationTypes(t *testing.T, f *fixture, path string) []string {
	t.Helper()
	var list []map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, path, nil, &list); code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, code)
	}
	var kinds []string
	for _, n := range list {
		kinds = append(kinds, n["type"].(string))
	}
	sort.Strings(kinds)
	return kinds
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Mastodon's apps show littleFedi's own types as "Unexpected Notification
// Type", so clients only get them by asking with types[].
func TestNotificationListsKeepToMastodonTypes(t *testing.T) {
	f := newFixture(t)
	seedNotifications(t, f, "follow", "mention", "follow_accept", "emoji_reaction", "unfollow", "follow_import.complete:import-1", "admin.account_deleted")

	if got := notificationTypes(t, f, "/api/v1/notifications"); !equalStrings(got, []string{"follow", "mention"}) {
		t.Errorf("default list = %v, want only the Mastodon types", got)
	}
	if got := notificationTypes(t, f, "/api/v1/notifications?types[]=follow_accept&types[]=pleroma:emoji_reaction"); !equalStrings(got, []string{"follow_accept", "pleroma:emoji_reaction"}) {
		t.Errorf("types[] list = %v", got)
	}
	if got := notificationTypes(t, f, "/api/v1/notifications?types[]=mention"); !equalStrings(got, []string{"mention"}) {
		t.Errorf("types[]=mention = %v", got)
	}
	if got := notificationTypes(t, f, "/api/v1/notifications?exclude_types[]=follow"); !equalStrings(got, []string{"mention"}) {
		t.Errorf("exclude_types[]=follow = %v", got)
	}

	var count map[string]interface{}
	f.call(t, "alice", http.MethodGet, "/api/v1/notifications/unread_count", nil, &count)
	if count["count"] != float64(2) {
		t.Errorf("unread_count = %v, want 2", count["count"])
	}

	var grouped map[string]interface{}
	f.call(t, "alice", http.MethodGet, "/api/v2/notifications", nil, &grouped)
	var kinds []string
	for _, g := range grouped["notification_groups"].([]interface{}) {
		kinds = append(kinds, g.(map[string]interface{})["type"].(string))
	}
	sort.Strings(kinds)
	if !equalStrings(kinds, []string{"follow", "mention"}) {
		t.Errorf("grouped types = %v", kinds)
	}
}

// The two Mastodon types littleFedi now creates come out in Mastodon's shape:
// a poll result carries its status and poll, a sign-up its account.
func TestPollAndSignUpNotificationsSerialize(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var status map[string]interface{}
	f.call(t, "alice", http.MethodPost, "/api/v1/statuses", map[string]interface{}{
		"status": "Vote!", "poll": map[string]interface{}{"options": []string{"a", "b"}, "expires_in": 3600},
	}, &status)
	statusID := status["id"].(string)
	for _, n := range []*models.Notification{
		{Type: "poll", AccountID: f.ids["alice"], FromAccountID: f.ids["alice"], StatusID: &statusID, CreatedAt: time.Now()},
		{Type: "admin.sign_up", AccountID: f.ids["alice"], FromAccountID: f.ids["bob"], CreatedAt: time.Now().Add(time.Second)},
	} {
		if err := f.store.CreateNotification(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	var list []map[string]interface{}
	f.call(t, "alice", http.MethodGet, "/api/v1/notifications", nil, &list)
	byType := map[string]map[string]interface{}{}
	for _, n := range list {
		byType[n["type"].(string)] = n
	}
	poll, ok := byType["poll"]
	if !ok {
		t.Fatalf("no poll notification in %v", list)
	}
	if s, _ := poll["status"].(map[string]interface{}); s == nil || s["id"] != statusID || s["poll"] == nil {
		t.Errorf("poll notification status = %v", poll["status"])
	}
	signUp, ok := byType["admin.sign_up"]
	if !ok {
		t.Fatalf("no admin.sign_up notification in %v", list)
	}
	if a, _ := signUp["account"].(map[string]interface{}); a == nil || a["username"] != "bob" {
		t.Errorf("admin.sign_up account = %v", signUp["account"])
	}
}
