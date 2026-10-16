package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func TestExtractCLICommand(t *testing.T) {
	tests := []struct {
		args       []string
		cmd, cfg   string
		subcommand []string
	}{
		{[]string{"littlefedi"}, "", "", nil},
		{[]string{"littlefedi", "--config", "x.toml"}, "", "", nil},
		{[]string{"littlefedi", "--config", "x.toml", "admin", "list-users"}, "admin", "x.toml", []string{"list-users"}},
		{[]string{"littlefedi", "--config=y.toml", "migrate"}, "migrate", "y.toml", []string{}},
		{[]string{"littlefedi", "--version"}, "version", "", nil},
		{[]string{"littlefedi", "-h"}, "help", "", nil},
	}
	for _, tt := range tests {
		cmd, args, cfg := extractCLICommand(tt.args)
		if cmd != tt.cmd || (tt.cmd != "" && cfg != tt.cfg) || (tt.subcommand != nil && !reflect.DeepEqual(args, tt.subcommand)) {
			t.Errorf("extractCLICommand(%v) = %q %v %q", tt.args, cmd, args, cfg)
		}
	}
}

func TestParsePostOptions(t *testing.T) {
	opts, err := parsePostOptions([]string{"--username", "alice", "--visibility", "Unlisted", "--markdown", "--cw", "spoilers", "--media", "m1", "--media", "m2", "post.md"})
	if err != nil {
		t.Fatalf("parsePostOptions: %v", err)
	}
	if opts.username != "alice" || opts.visibility != models.VisibilityUnlisted || opts.contentType != content.ContentTypeMarkdown ||
		opts.summary != "spoilers" || opts.file != "post.md" || !reflect.DeepEqual([]string(opts.mediaIDs), []string{"m1", "m2"}) {
		t.Errorf("options = %+v", opts)
	}

	defaults, err := parsePostOptions(nil)
	if err != nil || defaults.visibility != models.VisibilityPublic || defaults.contentType != content.ContentTypePlain || defaults.language != "en" {
		t.Errorf("defaults = %+v, %v", defaults, err)
	}

	for _, bad := range [][]string{
		{"--visibility", "everyone"},
		{"--blog-slug", "x"},
		{"--blog", "--reply-to", "123"},
		{"--content-type", "html"},
		{"--file", "a.txt", "b.txt"},
	} {
		if _, err := parsePostOptions(bad); err == nil {
			t.Errorf("parsePostOptions(%v) accepted", bad)
		}
	}
	if _, err := parsePostOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help = %v", err)
	}
}

func TestReadPostContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "post.txt")
	if err := os.WriteFile(path, []byte("  from a file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPostContent(path, nil); err != nil || got != "from a file" {
		t.Errorf("file content = %q, %v", got, err)
	}
	if got, err := readPostContent("-", strings.NewReader("from stdin\n")); err != nil || got != "from stdin" {
		t.Errorf("stdin content = %q, %v", got, err)
	}
}

func TestCreatePostFromCLI(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.NewConfig(t, "")
	store, err := storage.NewBunStorage(cfg.DB.DSN, 4, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.BootstrapLocalAccount(ctx, store, testutil.Domain, "alice", "a-long-enough-test-password", true); err != nil {
		t.Fatal(err)
	}
	store.Close()

	opts, _ := parsePostOptions([]string{"--username", "alice", "--markdown"})
	postURL, err := createPostFromCLI(ctx, cfg, opts, strings.NewReader("**Posted** from the command line"))
	if err != nil {
		t.Fatalf("createPostFromCLI: %v", err)
	}
	if !strings.HasPrefix(postURL, "https://"+testutil.Domain+"/@alice/") {
		t.Errorf("post URL = %q", postURL)
	}

	store, _ = storage.NewBunStorage(cfg.DB.DSN, 4, 4, false)
	defer store.Close()
	alice, _ := store.GetAccountByUsernameDomain(ctx, "alice", nil)
	statuses, _ := store.GetAccountStatuses(ctx, alice.ID, alice.ID, nil, nil, nil, 20, false, false, false, false)
	found := false
	for _, s := range statuses {
		found = found || strings.Contains(s.ContentHTML, "<strong>Posted</strong> from the command line")
	}
	if !found {
		t.Error("the CLI post was not stored as rendered Markdown")
	}

	empty, _ := parsePostOptions([]string{"--username", "alice"})
	if _, err := createPostFromCLI(ctx, cfg, empty, strings.NewReader("   ")); err == nil {
		t.Error("an empty post was accepted")
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-littleFedi-CSP-Nonce") == "" {
			t.Error("no CSP nonce passed to the handler")
		}
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(rec, req)
	h := rec.Header()
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", h)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'nonce-") {
		t.Errorf("CSP = %q", csp)
	}
	if h.Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS over HTTPS")
	}
}

func TestClientIPHonoursOnlyTrustedProxies(t *testing.T) {
	trusted, err := newTrustedProxySet([]string{"10.0.0.1", "192.168.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 192.168.1.2")
	if got := clientIP(req, trusted); got != "203.0.113.7" {
		t.Errorf("client behind trusted proxies = %q", got)
	}

	spoofed := httptest.NewRequest(http.MethodGet, "/", nil)
	spoofed.RemoteAddr = "198.51.100.9:5000"
	spoofed.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(spoofed, trusted); got != "198.51.100.9" {
		t.Errorf("an untrusted peer spoofed its address: %q", got)
	}
	if _, err := newTrustedProxySet([]string{"not-an-ip"}); err == nil {
		t.Error("an invalid proxy entry was accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := newIPRateLimiter(2, 60_000_000_000)
	handler := limitPOSTPaths(limiter, "/auth/sign_in")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	codes := []int{}
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/auth/sign_in", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusOK || codes[3] != http.StatusTooManyRequests {
		t.Errorf("status codes = %v, want the burst then 429", codes)
	}
	rec := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/auth/sign_in", nil)
	get.RemoteAddr = "203.0.113.5:1234"
	handler.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Errorf("GET was rate limited: %d", rec.Code)
	}
}

func TestQuietRequestPath(t *testing.T) {
	for _, p := range []string{"/static/app.js", "/media/x.png", "/readyz", "/health"} {
		if !quietRequestPath(p) {
			t.Errorf("%s should log at debug", p)
		}
	}
	if quietRequestPath("/api/v1/statuses") {
		t.Error("API requests should log at info")
	}
}
