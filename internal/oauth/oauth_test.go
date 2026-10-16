package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

const callback = "https://client.example/callback"

type fixture struct {
	store  storage.Storage
	router chi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := testutil.NewStore(t)
	r := chi.NewRouter()
	NewHandler(store, testutil.Domain).RegisterRoutes(r)
	return &fixture{store: store, router: r}
}

func (f *fixture) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) form(t *testing.T, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return f.do(t, req)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body)
	}
	return out
}

func (f *fixture) createApp(t *testing.T) (clientID, secret string) {
	t.Helper()
	rec := f.form(t, "/api/v1/apps", url.Values{
		"client_name": {"Test Client"}, "redirect_uris": {callback}, "scopes": {"read write"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/apps = %d: %s", rec.Code, rec.Body)
	}
	app := decode(t, rec)
	clientID, _ = app["client_id"].(string)
	secret, _ = app["client_secret"].(string)
	if clientID == "" || secret == "" {
		t.Fatalf("app response lacks credentials: %v", app)
	}
	return clientID, secret
}

func TestMetadata(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata = %d", rec.Code)
	}
	meta := decode(t, rec)
	if !strings.Contains(meta["token_endpoint"].(string), testutil.Domain+"/oauth/token") {
		t.Errorf("token_endpoint = %v", meta["token_endpoint"])
	}
}

func TestClientCredentialsGrant(t *testing.T) {
	f := newFixture(t)
	clientID, secret := f.createApp(t)
	rec := f.form(t, "/oauth/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}, "scope": {"read"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token = %d: %s", rec.Code, rec.Body)
	}
	token, _ := decode(t, rec)["access_token"].(string)
	if token == "" {
		t.Fatal("no access token issued")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/verify_credentials", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = f.do(t, req)
	if rec.Code != http.StatusOK || decode(t, rec)["name"] != "Test Client" {
		t.Errorf("verify_credentials = %d: %s", rec.Code, rec.Body)
	}

	rec = f.form(t, "/oauth/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {"wrong"},
	}, nil)
	if rec.Code == http.StatusOK {
		t.Error("a wrong client secret was accepted")
	}
}

func TestAuthorizationCodeFlowWithPKCE(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	clientID, secret := f.createApp(t)
	alice := testutil.CreateLocalAccount(t, f.store, "alice")

	sessionToken := "browser-session-token"
	if err := f.store.CreateSession(ctx, &models.Session{
		ID: "s1", AccountID: alice.ID, TokenHash: sha256Hex(sessionToken),
		CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "session", Value: sessionToken}

	verifier := "a-code-verifier-that-is-long-enough-for-pkce-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	rec := f.form(t, "/oauth/authorize", url.Values{
		"client_id": {clientID}, "redirect_uri": {callback}, "response_type": {"code"},
		"scope": {"read write"}, "state": {"xyz"}, "decision": {"allow"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"csrf_token": {csrfToken(sessionToken)},
	}, cookie)
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Fatalf("authorize decision = %d: %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), callback) {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	if location.Query().Get("state") != "xyz" {
		t.Errorf("state not echoed: %q", location)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %q", location)
	}

	exchange := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {secret},
		"code": {code}, "redirect_uri": {callback}, "code_verifier": {verifier},
	}
	rec = f.form(t, "/oauth/token", exchange, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code exchange = %d: %s", rec.Code, rec.Body)
	}
	tokens := decode(t, rec)
	if tokens["access_token"] == "" || tokens["token_type"] != "Bearer" {
		t.Errorf("token response = %v", tokens)
	}

	if rec := f.form(t, "/oauth/token", exchange, nil); rec.Code == http.StatusOK {
		t.Error("an authorization code was redeemed twice")
	}
}

func TestAuthorizeDecisionRequiresCSRF(t *testing.T) {
	f := newFixture(t)
	clientID, _ := f.createApp(t)
	rec := f.form(t, "/oauth/authorize", url.Values{
		"client_id": {clientID}, "redirect_uri": {callback}, "response_type": {"code"}, "decision": {"allow"},
		"csrf_token": {"forged"},
	}, &http.Cookie{Name: "session", Value: "whatever"})
	if rec.Code == http.StatusFound || strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Errorf("a forged consent issued a code: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}
