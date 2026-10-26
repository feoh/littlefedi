package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func notificationPage(t *testing.T, f *fixture, path string) ([]string, map[string]*url.URL) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "untrusted.example"
	req.Header.Set("Authorization", "Bearer "+f.tokens["alice"])
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
	}
	var ids []string
	if strings.HasPrefix(req.URL.Path, "/api/v2/") {
		var page struct {
			Groups []struct {
				ID string `json:"page_max_id"`
			} `json:"notification_groups"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, g := range page.Groups {
			ids = append(ids, g.ID)
		}
	} else {
		var page []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, n := range page {
			ids = append(ids, n.ID)
		}
	}
	links := map[string]*url.URL{}
	if header := rec.Header().Get("Link"); header != "" {
		for _, part := range strings.Split(header, ", ") {
			target, rel, ok := strings.Cut(part, `>; rel="`)
			if !ok {
				t.Fatalf("invalid Link header: %q", header)
			}
			u, err := url.Parse(strings.TrimPrefix(target, "<"))
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "https" || u.Host != testutil.Domain || u.Path != req.URL.Path {
				t.Fatalf("pagination URL must use the configured origin and endpoint: %s", u)
			}
			links[strings.TrimSuffix(rel, `"`)] = u
		}
	}
	return ids, links
}

func TestNotificationPaginationLinks(t *testing.T) {
	for _, endpoint := range []string{"/api/v1/notifications", "/api/v2/notifications"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			base := time.Now().Add(-time.Hour)
			sequence := 0
			add := func(kind string, offset time.Duration) string {
				t.Helper()
				sequence++
				from := createAccount(t, f, "follower"+strconv.Itoa(sequence))
				n := &models.Notification{
					Type: kind, AccountID: f.ids["alice"], FromAccountID: from, CreatedAt: base.Add(offset),
				}
				if err := f.store.CreateNotification(ctx, n); err != nil {
					t.Fatal(err)
				}
				return strconv.FormatInt(n.APIID, 10)
			}
			oldest := add("follow", 0)
			middle := add("follow", time.Minute)
			newest := add("follow", 2*time.Minute)
			filters := "types[]=follow&types[]=favourite&exclude_types[]=mention&exclude_types[]=poll&limit=1"
			ids, links := notificationPage(t, f, endpoint+"?"+filters+"&since_id="+oldest)
			if len(ids) != 1 || ids[0] != newest {
				t.Fatalf("head IDs = %v, want [%s]", ids, newest)
			}
			for _, rel := range []string{"next", "prev"} {
				u := links[rel]
				if u == nil {
					t.Fatalf("missing %s pagination link", rel)
				}
				q := u.Query()
				if q.Get("limit") != "1" || strings.Join(q["types[]"], ",") != "follow,favourite" || strings.Join(q["exclude_types[]"], ",") != "mention,poll" {
					t.Errorf("%s link lost limit or type filters: %s", rel, u)
				}
				if q.Has("since_id") {
					t.Errorf("%s link retained stale since_id: %s", rel, u)
				}
			}
			if links["next"].Query().Get("max_id") != newest || links["next"].Query().Has("min_id") {
				t.Fatalf("invalid next cursor: %s", links["next"])
			}
			if links["prev"].Query().Get("min_id") != newest || links["prev"].Query().Has("max_id") {
				t.Fatalf("invalid prev cursor: %s", links["prev"])
			}
			ids, olderLinks := notificationPage(t, f, links["next"].String())
			if len(ids) != 1 || ids[0] != middle {
				t.Fatalf("older page IDs = %v, want [%s]", ids, middle)
			}
			if olderLinks["prev"] == nil || olderLinks["prev"].Query().Has("max_id") {
				t.Fatalf("older page refresh link retained max_id: %v", olderLinks)
			}
			arrived := add("follow", 3*time.Minute)
			add("mention", 4*time.Minute) // Refresh must keep the original type filters.
			ids, _ = notificationPage(t, f, links["prev"].String())
			if len(ids) != 1 || ids[0] != arrived {
				t.Fatalf("refresh IDs = %v, want [%s]", ids, arrived)
			}
			ids, emptyLinks := notificationPage(t, f, endpoint+"?types[]=poll")
			if len(ids) != 0 || len(emptyLinks) != 0 {
				t.Fatalf("empty page = %v, links = %v", ids, emptyLinks)
			}
		})
	}
}

// Conversion may remove a status notification (for example, a now-invisible
// private post). Even an empty visible page needs cursors over the raw rows so
// clients can continue past it instead of getting stuck.
func TestNotificationPaginationLinksForHiddenStatuses(t *testing.T) {
	for _, endpoint := range []string{"/api/v1/notifications", "/api/v2/notifications"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newFixture(t)
			var status map[string]interface{}
			f.call(t, "bob", http.MethodPost, "/api/v1/statuses", map[string]interface{}{
				"status": "followers only", "visibility": "private",
			}, &status)
			sid := status["id"].(string)
			n := &models.Notification{Type: "mention", AccountID: f.ids["alice"], FromAccountID: f.ids["bob"], StatusID: &sid, CreatedAt: time.Now()}
			if err := f.store.CreateNotification(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			ids, links := notificationPage(t, f, endpoint+"?limit=1")
			if len(ids) != 0 {
				t.Fatalf("invisible status leaked: %v", ids)
			}
			for _, rel := range []string{"next", "prev"} {
				if links[rel] == nil {
					t.Fatalf("missing %s link over hidden notification", rel)
				}
			}
			if links["next"].Query().Get("max_id") != strconv.FormatInt(n.APIID, 10) {
				t.Fatalf("cursor is not the raw notification API ID: %s", links["next"])
			}
		})
	}
}
