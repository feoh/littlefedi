package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

// The decryptors below follow the client side (Mastodon for Android and iOS)
// and use the standard library's HKDF, independently of the sender's code.

type device struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newDevice(t *testing.T) device {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return device{priv: priv, auth: auth}
}

func (d device) keys() (p256dh, auth string) {
	return base64.RawURLEncoding.EncodeToString(d.priv.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(d.auth)
}

func headerParam(header, name string) string {
	for _, part := range strings.FieldsFunc(header, func(r rune) bool { return r == ';' || r == ',' }) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(part), name+"="); ok {
			return value
		}
	}
	return ""
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("base64 %q: %v", s, err)
	}
	return b
}

func (d device) decryptLegacy(t *testing.T, h http.Header, body []byte) []byte {
	t.Helper()
	serverPub := b64(t, headerParam(h.Get("Crypto-Key"), "dh"))
	salt := b64(t, headerParam(h.Get("Encryption"), "salt"))
	server, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	secret, _ := d.priv.ECDH(server)
	ikm, _ := hkdf.Key(sha256.New, secret, d.auth, "Content-Encoding: auth\x00", 32)
	clientPub := d.priv.PublicKey().Bytes()
	context := []byte("P-256\x00")
	context = binary.BigEndian.AppendUint16(context, uint16(len(clientPub)))
	context = append(context, clientPub...)
	context = binary.BigEndian.AppendUint16(context, uint16(len(serverPub)))
	context = append(context, serverPub...)
	key, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aesgcm\x00"+string(context), 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00"+string(context), 12)
	return openGCM(t, key, nonce, body, func(plain []byte) []byte {
		pad := int(binary.BigEndian.Uint16(plain))
		return plain[2+pad:]
	})
}

func (d device) decryptStandard(t *testing.T, body []byte) []byte {
	t.Helper()
	salt, keyLen := body[:16], int(body[20])
	serverPub, ciphertext := body[21:21+keyLen], body[21+keyLen:]
	server, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	secret, _ := d.priv.ECDH(server)
	info := append(append([]byte("WebPush: info\x00"), d.priv.PublicKey().Bytes()...), serverPub...)
	ikm, _ := hkdf.Key(sha256.New, secret, d.auth, string(info), 32)
	key, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	return openGCM(t, key, nonce, ciphertext, func(plain []byte) []byte {
		end := strings.LastIndexByte(string(plain), 0x02)
		return plain[:end]
	})
}

func openGCM(t *testing.T, key, nonce, ciphertext []byte, unpad func([]byte) []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return unpad(plain)
}

type captured struct {
	header http.Header
	body   []byte
}

func capture(t *testing.T) (*Sender, chan captured) {
	t.Helper()
	got := make(chan captured, 4)
	sender := senderAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{r.Header.Clone(), body}
		w.WriteHeader(http.StatusCreated)
	})
	return sender, got
}

func TestLegacyEncodingMatchesMastodon(t *testing.T) {
	sender, got := capture(t)
	d := newDevice(t)
	p256dh, auth := d.keys()
	payload := &NotificationPayload{Title: "bob mentioned you", Body: "hello there", NotificationID: 42, NotificationType: "mention"}
	if err := sender.Send(&Subscription{Endpoint: "https://relay.example/push/1", P256DH: p256dh, Auth: auth}, payload, "normal"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := <-got
	h := req.header
	if h.Get("Content-Encoding") != "aesgcm" {
		t.Errorf("Content-Encoding = %q", h.Get("Content-Encoding"))
	}
	if !strings.HasPrefix(h.Get("Authorization"), "WebPush ") {
		t.Errorf("Authorization = %q, want Mastodon's legacy WebPush scheme", h.Get("Authorization"))
	}
	if headerParam(h.Get("Crypto-Key"), "dh") == "" || headerParam(h.Get("Crypto-Key"), "p256ecdsa") == "" || headerParam(h.Get("Encryption"), "salt") == "" {
		t.Errorf("legacy headers incomplete: Crypto-Key %q, Encryption %q", h.Get("Crypto-Key"), h.Get("Encryption"))
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(d.decryptLegacy(t, h, req.body), &decoded); err != nil {
		t.Fatalf("decrypted payload is not JSON: %v", err)
	}
	if decoded["title"] != "bob mentioned you" || decoded["notification_id"] != float64(42) {
		t.Errorf("payload = %v", decoded)
	}
}

func TestStandardEncodingSendsNoLegacyHeaders(t *testing.T) {
	sender, got := capture(t)
	d := newDevice(t)
	p256dh, auth := d.keys()
	if err := sender.Send(&Subscription{Endpoint: "https://relay.example/push/2", P256DH: p256dh, Auth: auth, Standard: true},
		&NotificationPayload{Title: "t", Body: "standard body", NotificationID: 7}, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := <-got
	if req.header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(req.header.Get("Authorization"), "vapid t=") {
		t.Errorf("headers = %v", req.header)
	}
	// Mastodon for Android treats any Crypto-Key or Encryption header as the
	// legacy encoding.
	if req.header.Get("Crypto-Key") != "" || req.header.Get("Encryption") != "" {
		t.Errorf("standard push carries legacy headers: %v", req.header)
	}
	if plain := d.decryptStandard(t, req.body); !strings.Contains(string(plain), "standard body") {
		t.Errorf("decrypted = %s", plain)
	}
}

// mastodonPayload mirrors the iOS app's MastodonPushNotification, whose
// decode fails without a string access_token and an integer notification_id.
type mastodonPayload struct {
	AccessToken      string  `json:"access_token"`
	NotificationID   int64   `json:"notification_id"`
	NotificationType string  `json:"notification_type"`
	PreferredLocale  *string `json:"preferred_locale"`
	Icon             string  `json:"icon"`
	Title            string  `json:"title"`
	Body             string  `json:"body"`
}

func TestHandlePushJobSendsMastodonPayload(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if err := keycrypt.SetKey(base64.StdEncoding.EncodeToString(key)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keycrypt.SetKey("") })

	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	bob := testutil.CreateLocalAccount(t, store, "bob")
	d := newDevice(t)
	p256dh, auth := d.keys()
	sealed, err := keycrypt.Seal("raw-app-token")
	if err != nil || !keycrypt.IsSealed(sealed) {
		t.Fatalf("Seal = %q, %v", sealed, err)
	}
	tokenID := "token-1"
	sub := &models.PushSubscription{AccountID: alice.ID, OAuthTokenID: &tokenID, Endpoint: "https://relay.example/push/3",
		P256DH: p256dh, Auth: auth, NotifyMention: true, Policy: "all", AccessToken: sealed}
	if err := store.CreatePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	notif := &models.Notification{Type: "mention", AccountID: alice.ID, FromAccountID: bob.ID, CreatedAt: time.Now()}
	if err := store.CreateNotification(ctx, notif); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetNotification(ctx, notif.ID)
	if err != nil || stored.APIID == 0 {
		t.Fatalf("notification API ID = %v, %v", stored, err)
	}

	jobs, err := NotificationJobs(ctx, store, notif)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("NotificationJobs = %d, %v", len(jobs), err)
	}
	sender, got := capture(t)
	if err := HandlePushJob(ctx, store, sender, testutil.Domain)(ctx, jobs[0].Payload); err != nil {
		t.Fatalf("HandlePushJob: %v", err)
	}
	req := <-got
	if req.header.Get("Content-Encoding") != "aesgcm" {
		t.Errorf("a subscription that did not ask for standard got %q", req.header.Get("Content-Encoding"))
	}
	var p mastodonPayload
	plain := d.decryptLegacy(t, req.header, req.body)
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload does not decode the way Mastodon clients decode it: %v\n%s", err, plain)
	}
	if p.AccessToken != "raw-app-token" {
		t.Errorf("access_token = %q, want the app's raw token", p.AccessToken)
	}
	if p.NotificationID != stored.APIID {
		t.Errorf("notification_id = %d, want API ID %d", p.NotificationID, stored.APIID)
	}
	if p.NotificationType != "mention" || p.Title == "" || p.Icon == "" || p.PreferredLocale == nil {
		t.Errorf("payload = %+v", p)
	}
	if !strings.Contains(string(plain), fmt.Sprintf(`"notification_id":%d`, stored.APIID)) {
		t.Errorf("notification_id is not a JSON number: %s", plain)
	}
}

func TestFollowerPolicy(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	follower := testutil.CreateLocalAccount(t, store, "fan")
	stranger := testutil.CreateLocalAccount(t, store, "stranger")
	if err := store.CreateFollow(ctx, &models.Follow{AccountID: follower.ID, TargetAccountID: alice.ID, URI: follower.ActorURI + "#f", State: models.FollowAccepted}); err != nil {
		t.Fatal(err)
	}
	if !ValidPolicy("follower") {
		t.Fatal(`"follower" is not a valid policy`)
	}
	p256dh, auth := newDevice(t).keys()
	session := "s"
	if err := store.CreatePushSubscription(ctx, &models.PushSubscription{AccountID: alice.ID, SessionID: &session,
		Endpoint: "https://relay.example/push/4", P256DH: p256dh, Auth: auth, NotifyMention: true, Policy: "follower"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from *models.Account
		want int
	}{{follower, 1}, {stranger, 0}} {
		jobs, err := NotificationJobs(ctx, store, &models.Notification{ID: "n-" + tc.from.Username, Type: "mention", AccountID: alice.ID, FromAccountID: tc.from.ID, CreatedAt: time.Now()})
		if err != nil || len(jobs) != tc.want {
			t.Errorf("mention from %s: %d jobs, %v; want %d", tc.from.Username, len(jobs), err, tc.want)
		}
	}
}

func TestSubscriptionKeepsAlertsTurnedOff(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	p256dh, auth := newDevice(t).keys()
	session := "s"
	sub := &models.PushSubscription{AccountID: alice.ID, SessionID: &session, Endpoint: "https://relay.example/push/5",
		P256DH: p256dh, Auth: auth, Policy: "all", NotifyMention: false, NotifyPoll: false, NotifyStatus: false}
	if err := store.CreatePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPushSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NotifyMention || got.NotifyPoll || got.NotifyStatus {
		t.Errorf("alerts turned off were stored on: mention %v poll %v status %v", got.NotifyMention, got.NotifyPoll, got.NotifyStatus)
	}
}
