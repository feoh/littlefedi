package api_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const testVAPIDKey = "BEXAMPLEvapidPublicKeyForTests"

func pushKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

// postForm sends a form-encoded request, as Tuba and other clients do.
func (f *fixture) postForm(t *testing.T, user, path string, form url.Values, out interface{}) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Host = "test.example"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+f.tokens[user])
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("response is not JSON: %s", rec.Body)
		}
	}
	if rec.Code >= 300 {
		t.Logf("%s %s -> %d %s", http.MethodPost, path, rec.Code, rec.Body)
	}
	return rec.Code
}

// Tuba registers UnifiedPush with form fields and standard=true.
func TestPushSubscriptionFromForm(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p256dh, auth := pushKeys(t)
	form := url.Values{
		"subscription[endpoint]":      {"https://ntfy.example/upABC"},
		"subscription[keys][p256dh]":  {p256dh},
		"subscription[keys][auth]":    {auth},
		"subscription[standard]":      {"true"},
		"data[alerts][mention]":       {"true"},
		"data[alerts][favourite]":     {"false"},
		"data[alerts][follow]":        {"true"},
		"data[alerts][status]":        {"false"},
		"data[alerts][admin.sign_up]": {"true"},
		"data[policy]":                {"all"},
	}
	var sub map[string]interface{}
	if code := f.postForm(t, "alice", "/api/v1/push/subscription", form, &sub); code != http.StatusOK {
		t.Fatalf("form subscription = %d", code)
	}
	if sub["server_key"] != testVAPIDKey || sub["standard"] != true {
		t.Errorf("response server_key %v, standard %v", sub["server_key"], sub["standard"])
	}
	alerts := sub["alerts"].(map[string]interface{})
	if alerts["mention"] != true || alerts["favourite"] != false || alerts["follow"] != true || alerts["status"] != false {
		t.Errorf("alerts = %v", alerts)
	}

	stored, err := f.store.GetPushSubscriptionByEndpoint(ctx, "https://ntfy.example/upABC")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Standard || stored.NotifyStatus || stored.NotifyFavourite {
		t.Errorf("stored subscription = %+v", stored)
	}
	if token, err := keycrypt.Open(stored.AccessToken); err != nil || token != f.tokens["alice"] {
		t.Errorf("stored access token = %q, %v; want the bearer token", token, err)
	}
}

// Mastodon's own apps send JSON and refuse a response without server_key.
// A client that does not ask for standard gets the legacy encoding.
func TestPushSubscriptionFromJSON(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p256dh, auth := pushKeys(t)
	body := map[string]interface{}{
		"subscription": map[string]interface{}{
			"endpoint": "https://relay.example/relay-to/production/device1",
			"keys":     map[string]string{"p256dh": p256dh, "auth": auth},
		},
		"data": map[string]interface{}{
			"policy": "follower",
			"alerts": map[string]interface{}{"mention": true, "reblog": "true", "poll": false},
		},
	}
	var sub map[string]interface{}
	if code := f.call(t, "alice", http.MethodPost, "/api/v1/push/subscription", body, &sub); code != http.StatusOK {
		t.Fatalf("JSON subscription = %d", code)
	}
	if sub["server_key"] != testVAPIDKey || sub["standard"] != false || sub["policy"] != "follower" {
		t.Errorf("response = %v", sub)
	}
	stored, err := f.store.GetPushSubscriptionByEndpoint(ctx, "https://relay.example/relay-to/production/device1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Standard || !stored.NotifyReblog || stored.NotifyPoll {
		t.Errorf("stored subscription = %+v", stored)
	}

	var got map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, "/api/v1/push/subscription", nil, &got); code != http.StatusOK || got["server_key"] != testVAPIDKey {
		t.Errorf("GET subscription = %d %v", code, got)
	}
}

// Signing in again on the same device gives the app a new token but the
// same push endpoint; the new login replaces the old subscription.
func TestSameAccountResubscribeReplacesEndpoint(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.store.CreateOAuthToken(ctx, newTokenFor(f, "alice", "second-login")); err != nil {
		t.Fatal(err)
	}
	f.tokens["alice2"] = "second-login"
	p256dh, auth := pushKeys(t)
	body := map[string]interface{}{"subscription": map[string]interface{}{
		"endpoint": "https://relay.example/device", "keys": map[string]string{"p256dh": p256dh, "auth": auth}}}

	if code := f.call(t, "alice", http.MethodPost, "/api/v1/push/subscription", body, nil); code != http.StatusOK {
		t.Fatalf("first login = %d", code)
	}
	if code := f.call(t, "alice2", http.MethodPost, "/api/v1/push/subscription", body, nil); code != http.StatusOK {
		t.Fatalf("second login on the same device = %d", code)
	}
	stored, err := f.store.GetPushSubscriptionByEndpoint(ctx, "https://relay.example/device")
	if err != nil {
		t.Fatal(err)
	}
	if token, _ := keycrypt.Open(stored.AccessToken); token != "second-login" {
		t.Errorf("endpoint still belongs to the old login (token %q)", token)
	}
	if code := f.call(t, "bob", http.MethodPost, "/api/v1/push/subscription", body, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("another account taking the endpoint = %d, want 422", code)
	}
}

func TestGroupedNotificationIDsAreStrings(t *testing.T) {
	f := newFixture(t)
	f.call(t, "bob", http.MethodPost, "/api/v1/accounts/"+f.ids["alice"]+"/follow", map[string]interface{}{}, nil)
	var grouped map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, "/api/v2/notifications", nil, &grouped); code != http.StatusOK {
		t.Fatalf("v2 notifications = %d", code)
	}
	groups := grouped["notification_groups"].([]interface{})
	if len(groups) == 0 {
		t.Fatal("no notification groups")
	}
	group := groups[0].(map[string]interface{})
	if _, ok := group["most_recent_notification_id"].(string); !ok {
		t.Errorf("most_recent_notification_id = %#v, want a string", group["most_recent_notification_id"])
	}
	if group["type"] != "follow" {
		t.Errorf("group type = %v", group["type"])
	}
}

func TestInstanceAdvertisesVAPIDKey(t *testing.T) {
	f := newFixture(t)
	var instance map[string]interface{}
	f.call(t, "", http.MethodGet, "/api/v2/instance", nil, &instance)
	vapid, _ := instance["configuration"].(map[string]interface{})["vapid"].(map[string]interface{})
	if vapid["public_key"] != testVAPIDKey {
		t.Errorf("configuration.vapid = %v", vapid)
	}
}

func newTokenFor(f *fixture, user, token string) *models.OAuthToken {
	return &models.OAuthToken{AccessToken: token, RefreshToken: "refresh-" + token, AccountID: f.ids[user], ApplicationID: "app1", Scopes: "read write follow push"}
}
