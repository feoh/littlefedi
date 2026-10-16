package blog_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blog"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

func TestGenerateBuildsStaticSite(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.NewConfig(t, "[blog]\nenabled = true\nall_accounts = true\n")
	cfg.Blog.OutputDir = filepath.Join(t.TempDir(), "blogs")
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")

	created := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	post := &models.Status{
		AccountID: alice.ID, URI: alice.ActorURI + "/statuses/1", Visibility: models.VisibilityPublic,
		ContentHTML: "<h1>Hello Blog</h1><p>First entry body.</p>", ContentSource: "# Hello Blog\n\nFirst entry body.",
		ContentType: "text/markdown", IsBlogPost: true, CreatedAt: created, UpdatedAt: created,
	}
	if err := store.CreateStatus(ctx, post); err != nil {
		t.Fatal(err)
	}
	private := &models.Status{
		AccountID: alice.ID, URI: alice.ActorURI + "/statuses/2", Visibility: models.VisibilityPrivate,
		ContentHTML: "<p>followers only secret</p>", IsBlogPost: true,
	}
	if err := store.CreateStatus(ctx, private); err != nil {
		t.Fatal(err)
	}

	if err := blog.Generate(ctx, store, cfg, alice); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	site := filepath.Join(cfg.Blog.OutputDir, "alice")
	index, err := os.ReadFile(filepath.Join(site, "index.html"))
	if err != nil {
		t.Fatalf("no index.html: %v", err)
	}
	if !strings.Contains(string(index), "Hello Blog") {
		t.Error("index does not list the post")
	}

	var all strings.Builder
	var postPage string
	_ = filepath.Walk(site, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".html") {
			data, _ := os.ReadFile(path)
			all.Write(data)
			if path != filepath.Join(site, "index.html") && strings.Contains(string(data), "First entry body.") {
				postPage = path
			}
		}
		return nil
	})
	if postPage == "" {
		t.Error("no page for the post")
	} else if !strings.Contains(postPage, filepath.Join("2026", "05", "04")) {
		t.Errorf("post page %s does not follow the default date slug", postPage)
	}
	if strings.Contains(all.String(), "followers only secret") {
		t.Error("a non-public post was published to the static blog")
	}
	if _, err := os.Stat(filepath.Join(site, "feed.xml")); err != nil {
		t.Errorf("no feed.xml: %v", err)
	}

	r := chi.NewRouter()
	blog.RegisterRoutes(r, cfg.Blog)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/@alice/blog/", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || !strings.Contains(string(body), "Hello Blog") {
		t.Errorf("GET /@alice/blog/ = %d", rec.Code)
	}
}

func TestGenerateRequiresPermission(t *testing.T) {
	cfg := testutil.NewConfig(t, "[blog]\nenabled = true\n")
	cfg.Blog.OutputDir = t.TempDir()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	if err := blog.Generate(context.Background(), store, cfg, alice); err == nil {
		t.Error("an account without blog permission got a site")
	}
}

func TestRebuildRequestRoundTrip(t *testing.T) {
	req := blog.ParseRebuildRequest(`{"account_id":"a1","post_id":"p1"}`)
	if req.AccountID != "a1" || req.PostID != "p1" || req.Key() != "a1:p1" {
		t.Errorf("ParseRebuildRequest = %+v, key %q", req, req.Key())
	}
}

func TestRebuildCoordination(t *testing.T) {
	key := "test-account"
	if !blog.Claim(key) {
		t.Fatal("first Claim failed")
	}
	if blog.Claim(key) {
		t.Error("a second Claim succeeded while the first is pending")
	}
	blog.Start(key)
	if blog.Claim(key) {
		t.Error("Claim succeeded while the rebuild is running")
	}
	if !blog.Finish(key, true) {
		t.Error("work that arrived mid-build did not request another pass")
	}
	if blog.Finish(key, true) {
		t.Error("a clean finish requested another pass")
	}
	if !blog.Claim(key) {
		t.Error("Claim failed after the previous rebuild finished")
	}
	blog.Cancel(key)
}
