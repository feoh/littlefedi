package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/api"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/oauth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

type fixture struct {
	store  storage.Storage
	router chi.Router
	tokens map[string]string
	ids    map[string]string
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
	handler := api.NewHandler(store, federator, api.Config{
		Domain: testutil.Domain, MediaPath: t.TempDir(), MaxStatusChars: 500, MaxMedia: 4,
		InstanceName: "Test Instance",
	})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, oauth.NewHandler(store, testutil.Domain))

	application := &models.OAuthApplication{ID: "app1", Name: "Test App", RedirectURIs: "urn:ietf:wg:oauth:2.0:oob", ClientID: "client", ClientSecret: "secret", Scopes: "read write follow push", CreatedAt: time.Now()}
	if err := store.CreateOAuthApp(ctx, application); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, router: r, tokens: map[string]string{}, ids: map[string]string{}}
	for _, name := range []string{"alice", "bob"} {
		account := testutil.CreateLocalAccount(t, store, name)
		token := "token-for-" + name
		if err := store.CreateOAuthToken(ctx, &models.OAuthToken{AccessToken: token, RefreshToken: "refresh-" + token, AccountID: account.ID, ApplicationID: application.ID, Scopes: application.Scopes}); err != nil {
			t.Fatal(err)
		}
		f.tokens[name], f.ids[name] = token, account.ID
	}
	return f
}

// call performs a request as user ("" for anonymous) and decodes a JSON reply.
func (f *fixture) call(t *testing.T, user, method, path string, body interface{}, out interface{}) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = testutil.Domain
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[user])
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: response is not JSON: %s", method, path, rec.Body)
		}
	}
	return rec.Code
}

func TestInstanceInfo(t *testing.T) {
	f := newFixture(t)
	var v1 map[string]interface{}
	if code := f.call(t, "", http.MethodGet, "/api/v1/instance", nil, &v1); code != http.StatusOK {
		t.Fatalf("GET /api/v1/instance = %d", code)
	}
	if v1["uri"] != testutil.Domain || v1["title"] != "Test Instance" {
		t.Errorf("instance v1 = %v", v1)
	}
	var v2 map[string]interface{}
	if code := f.call(t, "", http.MethodGet, "/api/v2/instance", nil, &v2); code != http.StatusOK || v2["domain"] != testutil.Domain {
		t.Errorf("instance v2 = %d %v", code, v2["domain"])
	}
}

func TestVerifyCredentials(t *testing.T) {
	f := newFixture(t)
	var me map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, "/api/v1/accounts/verify_credentials", nil, &me); code != http.StatusOK {
		t.Fatalf("verify_credentials = %d", code)
	}
	if me["username"] != "alice" || me["id"] != f.ids["alice"] {
		t.Errorf("verify_credentials = %v", me)
	}
	if code := f.call(t, "", http.MethodGet, "/api/v1/accounts/verify_credentials", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous verify_credentials = %d, want 401", code)
	}
}

func TestPostFollowFavouriteAndTimeline(t *testing.T) {
	f := newFixture(t)

	var rel []map[string]interface{}
	if code := f.call(t, "bob", http.MethodPost, "/api/v1/accounts/"+f.ids["alice"]+"/follow", map[string]interface{}{}, nil); code != http.StatusOK {
		t.Fatalf("follow = %d", code)
	}
	if code := f.call(t, "bob", http.MethodGet, "/api/v1/accounts/relationships?id[]="+f.ids["alice"], nil, &rel); code != http.StatusOK || len(rel) != 1 || rel[0]["following"] != true {
		t.Errorf("relationships = %d %v", code, rel)
	}

	var status map[string]interface{}
	if code := f.call(t, "alice", http.MethodPost, "/api/v1/statuses", map[string]interface{}{"status": "Hello from the API #testing", "visibility": "public"}, &status); code != http.StatusOK {
		t.Fatalf("POST /api/v1/statuses = %d", code)
	}
	id, _ := status["id"].(string)
	if id == "" || !strings.Contains(status["content"].(string), "Hello from the API") || status["visibility"] != "public" {
		t.Fatalf("created status = %v", status)
	}

	var fetched map[string]interface{}
	if code := f.call(t, "bob", http.MethodGet, "/api/v1/statuses/"+id, nil, &fetched); code != http.StatusOK || fetched["id"] != id {
		t.Errorf("GET of a public status = %d", code)
	}
	// The client API serves statuses to authenticated callers only.
	if code := f.call(t, "", http.MethodGet, "/api/v1/statuses/"+id, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous GET of a status = %d, want 401", code)
	}

	var home []map[string]interface{}
	if code := f.call(t, "bob", http.MethodGet, "/api/v1/timelines/home", nil, &home); code != http.StatusOK {
		t.Fatalf("home timeline = %d", code)
	}
	if !containsStatus(home, id) {
		t.Errorf("bob's home timeline lacks alice's post: %d items", len(home))
	}

	var faved map[string]interface{}
	if code := f.call(t, "bob", http.MethodPost, "/api/v1/statuses/"+id+"/favourite", map[string]interface{}{}, &faved); code != http.StatusOK || faved["favourited"] != true {
		t.Errorf("favourite = %d %v", code, faved["favourited"])
	}

	var notifications []map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, "/api/v1/notifications", nil, &notifications); code != http.StatusOK {
		t.Fatalf("notifications = %d", code)
	}
	kinds := map[string]bool{}
	for _, n := range notifications {
		kinds[n["type"].(string)] = true
	}
	if !kinds["follow"] || !kinds["favourite"] {
		t.Errorf("alice's notification types = %v, want follow and favourite", kinds)
	}

	if code := f.call(t, "bob", http.MethodDelete, "/api/v1/statuses/"+id, nil, nil); code == http.StatusOK {
		t.Error("bob deleted alice's status")
	}
	if code := f.call(t, "alice", http.MethodDelete, "/api/v1/statuses/"+id, nil, nil); code != http.StatusOK {
		t.Errorf("alice deleting her status = %d", code)
	}
	if code := f.call(t, "bob", http.MethodGet, "/api/v1/statuses/"+id, nil, nil); code != http.StatusNotFound {
		t.Errorf("GET of a deleted status = %d, want 404", code)
	}
}

func TestWriteRequiresScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.store.CreateOAuthToken(ctx, &models.OAuthToken{AccessToken: "read-only", RefreshToken: "refresh-read-only", AccountID: f.ids["alice"], ApplicationID: "app1", Scopes: "read"}); err != nil {
		t.Fatal(err)
	}
	f.tokens["reader"] = "read-only"
	if code := f.call(t, "reader", http.MethodPost, "/api/v1/statuses", map[string]interface{}{"status": "should fail"}, nil); code != http.StatusForbidden {
		t.Errorf("posting with a read-only token = %d, want 403", code)
	}
}

func containsStatus(items []map[string]interface{}, id string) bool {
	for _, item := range items {
		if item["id"] == id {
			return true
		}
	}
	return false
}
