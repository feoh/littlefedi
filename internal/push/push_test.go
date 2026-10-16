package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func clientKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

func TestVAPIDKeysRoundTrip(t *testing.T) {
	keys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	parsed, err := ParseVAPIDKeys(keys.Public, keys.Private)
	if err != nil {
		t.Fatalf("ParseVAPIDKeys: %v", err)
	}
	if parsed.Public != keys.Public || parsed.Private != keys.Private {
		t.Error("parsed keys differ from the generated ones")
	}
	if _, err := ParseVAPIDKeys(keys.Public, "c2hvcnQ"); err == nil {
		t.Error("a short private key was accepted")
	}
	if _, err := ParseVAPIDKeys("", keys.Private); err == nil {
		t.Error("a missing public key was accepted")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	if got, err := NormalizeEndpoint(" https://push.example.com/send/abc "); err != nil || got != "https://push.example.com/send/abc" {
		t.Errorf("NormalizeEndpoint = %q, %v", got, err)
	}
	for _, bad := range []string{"http://push.example.com/x", "https://user:pw@push.example.com/x", "https://127.0.0.1/x", "https://10.0.0.1/x", "not a url"} {
		if _, err := NormalizeEndpoint(bad); err == nil {
			t.Errorf("NormalizeEndpoint(%q) accepted", bad)
		}
	}
}

func TestNormalizeClientKeys(t *testing.T) {
	p256dh, auth := clientKeys(t)
	raw, _ := base64.RawURLEncoding.DecodeString(p256dh)
	gotKey, gotAuth, err := NormalizeClientKeys(base64.StdEncoding.EncodeToString(raw), auth)
	if err != nil {
		t.Fatalf("NormalizeClientKeys: %v", err)
	}
	if gotKey != p256dh || gotAuth != auth {
		t.Error("keys not normalized to unpadded base64url")
	}
	if _, _, err := NormalizeClientKeys("AAAA", auth); err == nil {
		t.Error("a short public key was accepted")
	}
}

func TestValidPolicy(t *testing.T) {
	for _, p := range []string{"all", "followed", "none"} {
		if !ValidPolicy(p) {
			t.Errorf("ValidPolicy(%q) = false", p)
		}
	}
	if ValidPolicy("everyone") {
		t.Error("unknown policy accepted")
	}
}

// The sender refuses non-public addresses, so the test points its client at
// a local TLS server while the endpoint keeps a public-looking host name.
func senderAgainst(t *testing.T, handler http.HandlerFunc) *Sender {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	keys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSender(keys, "mailto:admin@test.example")
	sender.client = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local test server
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}
	return sender
}

func TestSenderSendsEncryptedVAPIDRequest(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	sender := senderAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	p256dh, auth := clientKeys(t)
	err := sender.Send(&Subscription{Endpoint: "https://push.example.com/send/abc", P256DH: p256dh, Auth: auth, Standard: true},
		&NotificationPayload{Title: "New mention", Body: "secret text"}, "high")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotHeaders.Get("Content-Encoding") != "aes128gcm" || gotHeaders.Get("Urgency") != "high" {
		t.Errorf("headers = %v", gotHeaders)
	}
	if !strings.HasPrefix(gotHeaders.Get("Authorization"), "vapid ") {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
	if len(gotBody) == 0 || strings.Contains(string(gotBody), "secret text") {
		t.Error("payload was not encrypted")
	}
}

func TestSenderReportsExpiredSubscription(t *testing.T) {
	sender := senderAgainst(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
	p256dh, auth := clientKeys(t)
	err := sender.Send(&Subscription{Endpoint: "https://push.example.com/x", P256DH: p256dh, Auth: auth}, &NotificationPayload{}, "")
	if !errors.Is(err, ErrSubscriptionExpired) {
		t.Errorf("Send to a gone endpoint = %v", err)
	}
}

func TestNotificationJobs(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	bob := testutil.CreateLocalAccount(t, store, "bob")
	p256dh, auth := clientKeys(t)
	sessionID := "session-1"
	sub := &models.PushSubscription{
		AccountID: alice.ID, SessionID: &sessionID, Endpoint: "https://push.example.com/a",
		P256DH: p256dh, Auth: auth, NotifyMention: true, Policy: "all",
	}
	if err := store.CreatePushSubscription(ctx, sub); err != nil {
		t.Fatalf("CreatePushSubscription: %v", err)
	}

	mention := &models.Notification{ID: "n1", Type: "mention", AccountID: alice.ID, FromAccountID: bob.ID, CreatedAt: time.Now()}
	jobs, err := NotificationJobs(ctx, store, mention)
	if err != nil {
		t.Fatalf("NotificationJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "push_notification" {
		t.Fatalf("jobs = %+v", jobs)
	}
	var payload PushJobPayload
	if err := json.Unmarshal([]byte(jobs[0].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SubscriptionID != sub.ID || payload.FromUsername != "bob" || payload.NotifType != "mention" {
		t.Errorf("payload = %+v", payload)
	}

	follow := &models.Notification{ID: "n2", Type: "follow", AccountID: alice.ID, FromAccountID: bob.ID, CreatedAt: time.Now()}
	if jobs, _ := NotificationJobs(ctx, store, follow); len(jobs) != 0 {
		t.Error("a follow push was queued for a subscription that opted out of follows")
	}
}
