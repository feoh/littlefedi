package webui_test

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/webui"
)

const password = "a-long-enough-test-password"

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

type browser struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	store  storage.Storage
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	ctx := context.Background()
	cfg := testutil.NewConfig(t, "")
	store := testutil.NewStore(t)
	signer, err := federation.SetupInstanceActor(ctx, store, testutil.Domain)
	if err != nil {
		t.Fatal(err)
	}
	federator := federation.NewFederator(store, testutil.Domain, signer, "littleFedi-test")
	if _, err := accounts.BootstrapLocalAccount(ctx, store, testutil.Domain, "alice", password, true); err != nil {
		t.Fatal(err)
	}
	handler := webui.NewHandler(store, federator, cfg, testutil.Domain, "", "", cfg.Media.Path,
		cfg.Media.MaxFileSize, true, true, nil, media.NewProxy(false, "", testutil.Domain))
	r := chi.NewRouter()
	handler.RegisterRoutes(r)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &browser{t: t, server: server, client: client, store: store}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	req.Header.Set("Origin", b.server.URL)
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.server.URL+path, nil)
	return b.do(req)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

func (b *browser) signIn(user, pass string) *http.Response {
	b.t.Helper()
	resp, _ := b.post("/auth/sign_in", url.Values{"username": {user}, "password": {pass}})
	return resp
}

func TestSignInPageRenders(t *testing.T) {
	b := newBrowser(t)
	resp, body := b.get("/auth/sign_in")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Errorf("sign-in page = %d", resp.StatusCode)
	}
	for _, path := range []string{"/static/app.js", "/static/app.css"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
		}
	}
}

func TestSignInRejectsWrongPassword(t *testing.T) {
	b := newBrowser(t)
	b.signIn("alice", "not the password")
	if _, body := b.get("/"); strings.Contains(body, `id="composer-text"`) {
		t.Error("a wrong password produced a signed-in session")
	}
}

func TestSignInComposeAndSeePost(t *testing.T) {
	ctx := context.Background()
	b := newBrowser(t)
	resp := b.signIn("alice", password)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in = %d", resp.StatusCode)
	}

	resp, home := b.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home = %d", resp.StatusCode)
	}
	if !strings.Contains(home, `id="composer-text"`) {
		t.Fatal("home page has no composer")
	}
	// The fork's keyboard shortcut: the composer listens for Ctrl/Cmd+Enter
	// and the Post button advertises it.
	if !strings.Contains(home, `@keydown="submitShortcut($event)"`) {
		t.Error("composer form does not wire the Ctrl+Enter handler")
	}
	// html/template writes "+" as &#43; inside attributes; browsers decode it.
	if decoded := html.UnescapeString(home); !strings.Contains(decoded, `aria-keyshortcuts="Control+Enter Meta+Enter"`) || !strings.Contains(decoded, `title="Ctrl+Enter (⌘+Enter on a Mac)"`) {
		t.Error("Post button does not advertise the shortcut")
	}

	match := csrfPattern.FindStringSubmatch(home)
	if match == nil {
		t.Fatal("no CSRF token on the home page")
	}
	resp, _ = b.post("/statuses", url.Values{"csrf_token": {match[1]}, "status": {"Posted from the web composer"}, "visibility": {"public"}})
	if resp.StatusCode >= 400 {
		t.Fatalf("compose = %d", resp.StatusCode)
	}

	alice, _ := b.store.GetAccountByUsernameDomain(ctx, "alice", nil)
	statuses, err := b.store.GetAccountStatuses(ctx, alice.ID, alice.ID, nil, nil, nil, 20, false, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range statuses {
		found = found || strings.Contains(s.ContentHTML, "Posted from the web composer")
	}
	if !found {
		t.Fatal("the composed post was not stored")
	}

	if _, profile := b.get("/@alice"); !strings.Contains(profile, "Posted from the web composer") {
		t.Error("the post does not appear on alice's profile")
	}
}

func TestComposeRequiresCSRFToken(t *testing.T) {
	b := newBrowser(t)
	b.signIn("alice", password)
	resp, _ := b.post("/statuses", url.Values{"csrf_token": {"forged"}, "status": {"forged post"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("compose with a forged token = %d, want 403", resp.StatusCode)
	}
}

func TestCrossSiteSignInRejected(t *testing.T) {
	b := newBrowser(t)
	req, _ := http.NewRequest(http.MethodPost, b.server.URL+"/auth/sign_in", strings.NewReader(url.Values{"username": {"alice"}, "password": {password}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodPost, b.server.URL+"/auth/sign_in", strings.NewReader(url.Values{"username": {"alice"}, "password": {password}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err = b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin sign-in = %d, want 403", resp.StatusCode)
	}
}
