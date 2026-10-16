package federation

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

const remoteActor = "https://remote.example/users/carol"

func TestHTTPSignatureRoundTrip(t *testing.T) {
	type algorithm struct {
		generate func() (string, string, error)
		derive   func(string) (string, error)
	}
	for name, alg := range map[string]algorithm{
		"rsa":     {GenerateKeyPair, PublicKeyPEMFromPrivate},
		"ed25519": {GenerateEd25519KeyPair, Ed25519PublicKeyPEMFromPrivate},
	} {
		t.Run(name, func(t *testing.T) {
			privatePEM, publicPEM, err := alg.generate()
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if derived, err := alg.derive(privatePEM); err != nil || derived != publicPEM {
				t.Errorf("derived public key does not match: %v", err)
			}
			keyID := "https://test.example/users/alice#main-key"
			signer, err := NewSigner(keyID, privatePEM)
			if err != nil {
				t.Fatalf("NewSigner: %v", err)
			}
			body := []byte(`{"type":"Follow"}`)
			req := httptest.NewRequest(http.MethodPost, "https://remote.example/inbox", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/activity+json")
			if err := signer.SignRequest(req, body); err != nil {
				t.Fatalf("SignRequest: %v", err)
			}
			if req.Header.Get("Signature") == "" && req.Header.Get("Signature-Input") == "" {
				t.Fatal("no signature header set")
			}

			verifier := NewVerifier(func(id string) (string, error) {
				if id != keyID {
					t.Errorf("verifier asked for key %q", id)
				}
				return publicPEM, nil
			})
			signedBy, err := verifier.VerifyRequestWithBody(req, body)
			if err != nil {
				t.Fatalf("VerifyRequestWithBody: %v", err)
			}
			if signedBy != keyID || ExtractKeyOwner(signedBy) != "https://test.example/users/alice" {
				t.Errorf("signed by %q, owner %q", signedBy, ExtractKeyOwner(signedBy))
			}
			if _, err := verifier.VerifyRequestWithBody(req, []byte(`{"type":"Delete"}`)); err == nil {
				t.Error("a request with a swapped body verified")
			}
		})
	}
}

func TestObjectIntegrityProof(t *testing.T) {
	privatePEM, publicPEM, err := GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Create", "id": "https://test.example/activities/1",
		"actor": "https://test.example/users/alice",
	}
	signed, err := SignWithObjectIntegrityProof(doc, privatePEM, "https://test.example/users/alice#ed25519-key")
	if err != nil {
		t.Fatalf("SignWithObjectIntegrityProof: %v", err)
	}
	if err := VerifyObjectIntegrityProof(signed, publicPEM); err != nil {
		t.Fatalf("VerifyObjectIntegrityProof: %v", err)
	}
	signed["actor"] = "https://evil.example/users/mallory"
	if err := VerifyObjectIntegrityProof(signed, publicPEM); err == nil {
		t.Error("a modified document verified")
	}
}

func TestParseDomainBlockImport(t *testing.T) {
	blocks, err := ParseDomainBlockImport(strings.NewReader("#domain,#severity,#reject_media,#reject_reports,#public_comment,#obfuscate\nspam.example,suspend,true,true,spam,false\n"))
	if err != nil {
		t.Fatalf("ParseDomainBlockImport: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Domain != "spam.example" {
		t.Errorf("blocks = %+v", blocks)
	}
}

type fixture struct {
	store  storage.Storage
	alice  *models.Account
	router chi.Router
	inbox  *InboxProcessor

	mu        sync.Mutex
	delivered []map[string]interface{}
}

// routeRemoteTo sends the federator's traffic for any host to a local TLS
// server standing in for remote.example. Federation policy still runs; only
// the SSRF-guarded dialer, which rightly refuses loopback, is replaced.
func (f *fixture) routeRemoteTo(t *testing.T, federator *Federator) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var activity map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&activity); err == nil && r.Method == http.MethodPost {
			f.mu.Lock()
			f.delivered = append(f.delivered, activity)
			f.mu.Unlock()
		}
		if r.Method != http.MethodPost {
			// Discovery probes such as NodeInfo are plain, unsigned reads.
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Signature") == "" && r.Header.Get("Signature-Input") == "" {
			t.Errorf("unsigned delivery to %s", r.URL)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	federator.client.Transport = federationTransport{federator: federator, next: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local test server
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}
}

func (f *fixture) deliveredTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var types []string
	for _, a := range f.delivered {
		if kind, ok := a["type"].(string); ok {
			types = append(types, kind)
		}
	}
	return types
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.NewStore(t)
	privatePEM, publicPEM, edPrivate, edPublic, err := GenerateDualKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	alice := testutil.CreateLocalAccount(t, store, "alice")
	alice.PrivateKeyPem, alice.PublicKeyPem = privatePEM, publicPEM
	alice.Ed25519PrivateKeyPem, alice.Ed25519PublicKeyPem = edPrivate, edPublic
	alice.Ed25519PublicKeyID = alice.ActorURI + "#ed25519-key"
	if err := store.UpdateAccount(ctx, alice); err != nil {
		t.Fatal(err)
	}
	signer, err := SetupInstanceActor(ctx, store, testutil.Domain)
	if err != nil {
		t.Fatalf("SetupInstanceActor: %v", err)
	}
	federator := NewFederator(store, testutil.Domain, signer, "littleFedi-test")
	handler := NewHandler(store, testutil.Domain, NewVerifier(func(string) (string, error) { return "", nil }), federator)
	r := chi.NewRouter()
	handler.RegisterRoutes(r)

	domain := "remote.example"
	now := time.Now()
	carol := &models.Account{
		Username: "carol", Domain: &domain, ActorURI: remoteActor, InboxURI: remoteActor + "/inbox",
		PublicKeyPem: publicPEM, PublicKeyID: remoteActor + "#main-key", DisplayName: "Carol",
		CreatedAt: now, UpdatedAt: now, FetchedAt: &now, Approved: true,
	}
	if err := store.CreateAccount(ctx, carol); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, alice: alice, router: r, inbox: NewInboxProcessor(store, federator, testutil.Domain)}
	f.routeRemoteTo(t, federator)
	return f
}

func (f *fixture) get(t *testing.T, path, accept string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = testutil.Domain
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestDiscoveryEndpoints(t *testing.T) {
	f := newFixture(t)

	code, finger := f.get(t, "/.well-known/webfinger?resource=acct:alice@"+testutil.Domain, "")
	if code != http.StatusOK || finger["subject"] != "acct:alice@"+testutil.Domain {
		t.Fatalf("webfinger = %d %v", code, finger)
	}
	foundSelf := false
	for _, l := range finger["links"].([]interface{}) {
		link := l.(map[string]interface{})
		foundSelf = foundSelf || (link["rel"] == "self" && link["href"] == f.alice.ActorURI)
	}
	if !foundSelf {
		t.Errorf("webfinger has no self link to the actor: %v", finger["links"])
	}
	if code, _ := f.get(t, "/.well-known/webfinger?resource=acct:nobody@"+testutil.Domain, ""); code != http.StatusNotFound {
		t.Errorf("webfinger for a missing user = %d", code)
	}

	code, actor := f.get(t, "/users/alice", "application/activity+json")
	if code != http.StatusOK || actor["type"] != "Person" || actor["id"] != f.alice.ActorURI || actor["inbox"] != f.alice.InboxURI {
		t.Fatalf("actor = %d %v", code, actor)
	}
	if key, _ := actor["publicKey"].(map[string]interface{}); key == nil || !strings.Contains(key["publicKeyPem"].(string), "PUBLIC KEY") {
		t.Errorf("actor publicKey = %v", actor["publicKey"])
	}

	code, wellKnown := f.get(t, "/.well-known/nodeinfo", "")
	if code != http.StatusOK || !strings.Contains(toJSON(wellKnown), "/nodeinfo/2.1") {
		t.Errorf("nodeinfo discovery = %d %v", code, wellKnown)
	}
	code, nodeinfo := f.get(t, "/nodeinfo/2.1", "")
	if code != http.StatusOK || nodeinfo["software"].(map[string]interface{})["name"] != "littlefedi" {
		t.Errorf("nodeinfo = %d %v", code, nodeinfo)
	}
}

func TestInboxFollowAndCreate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	follow := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       remoteActor + "#follows/1", "type": "Follow",
		"actor": remoteActor, "object": f.alice.ActorURI,
	}
	if err := f.inbox.ProcessActivity(ctx, mustJSON(t, follow)); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	carol, err := f.store.GetAccountByURI(ctx, remoteActor)
	if err != nil {
		t.Fatal(err)
	}
	if following, _ := f.store.IsFollowing(ctx, carol.ID, f.alice.ID); !following {
		t.Error("carol does not follow alice after the Follow")
	}
	notifications, _ := f.store.ListNotifications(ctx, f.alice.ID, nil, 10, nil)
	if !hasNotification(notifications, "follow", carol.ID) {
		t.Errorf("no follow notification for alice: %d notifications", len(notifications))
	}
	if types := f.deliveredTypes(); len(types) != 1 || types[0] != "Accept" {
		t.Errorf("deliveries to carol = %v, want one Accept", types)
	}

	note := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       remoteActor + "/statuses/1/activity", "type": "Create", "actor": remoteActor,
		"to": []string{"https://www.w3.org/ns/activitystreams#Public"},
		"object": map[string]interface{}{
			"id": remoteActor + "/statuses/1", "type": "Note", "attributedTo": remoteActor,
			"content":   `<p>Hello <a href="` + f.alice.ActorURI + `" class="mention">@alice</a></p>`,
			"published": time.Now().UTC().Format(time.RFC3339),
			"to":        []string{"https://www.w3.org/ns/activitystreams#Public"},
			"tag":       []map[string]interface{}{{"type": "Mention", "href": f.alice.ActorURI, "name": "@alice@" + testutil.Domain}},
		},
	}
	if err := f.inbox.ProcessActivity(ctx, mustJSON(t, note)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	status, err := f.store.GetStatusByURI(ctx, remoteActor+"/statuses/1")
	if err != nil {
		t.Fatalf("the remote note was not stored: %v", err)
	}
	if status.AccountID != carol.ID || !strings.Contains(status.ContentHTML, "Hello") || status.Visibility != models.VisibilityPublic {
		t.Errorf("stored status = %+v", status)
	}
	notifications, _ = f.store.ListNotifications(ctx, f.alice.ID, nil, 10, nil)
	if !hasNotification(notifications, "mention", carol.ID) {
		t.Error("no mention notification for alice")
	}

	undo := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       remoteActor + "#follows/1/undo", "type": "Undo", "actor": remoteActor, "object": follow,
	}
	if err := f.inbox.ProcessActivity(ctx, mustJSON(t, undo)); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if following, _ := f.store.IsFollowing(ctx, carol.ID, f.alice.ID); following {
		t.Error("carol still follows alice after Undo")
	}
}

func hasNotification(notifications []*models.Notification, kind, from string) bool {
	for _, n := range notifications {
		if n.Type == kind && n.FromAccountID == from {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
