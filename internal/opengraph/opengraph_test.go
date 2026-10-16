package opengraph

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const page = `<!doctype html><html><head>
<title>Fallback title</title>
<meta property="og:title" content="  The Real Title ">
<meta property="og:description" content="A short description">
<meta property="og:site_name" content="Example Site">
<meta name="description" content="ignored because og:description wins">
</head><body>hi</body></html>`

func TestPreviewParsesOpenGraph(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent sent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}))
	defer server.Close()

	f := NewFetcher()
	f.client = server.Client() // the real client refuses loopback addresses
	card, err := f.Preview(context.Background(), server.URL+"/article")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if card.Title != "The Real Title" || card.Description != "A short description" || card.SiteName != "Example Site" {
		t.Errorf("card = %+v", card)
	}
	if card.URL != server.URL+"/article" {
		t.Errorf("card URL = %q", card.URL)
	}

	if _, err := f.Preview(context.Background(), server.URL+"/article"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("page fetched %d times, want the second preview served from cache", hits.Load())
	}
}

func TestPreviewRejectsNonHTMLAndBadURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	f := NewFetcher()
	f.client = server.Client()
	if _, err := f.Preview(context.Background(), server.URL); err == nil {
		t.Error("a JSON response produced a preview")
	}
	for _, bad := range []string{"ftp://example.com/x", "https://user:pw@example.com/", "not a url"} {
		if _, err := f.Preview(context.Background(), bad); err == nil {
			t.Errorf("Preview(%q) accepted", bad)
		}
	}
}

func TestRealFetcherRefusesLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, page)
	}))
	defer server.Close()
	if _, err := NewFetcher().Preview(context.Background(), server.URL); err == nil {
		t.Error("the fetcher connected to a loopback address")
	}
}

func TestURLFromHTML(t *testing.T) {
	content := `<p>Hi <a href="https://test.example/@bob" class="u-url mention">@bob</a> ` +
		`<a href="https://test.example/tags/go" class="mention hashtag" rel="tag">#go</a> ` +
		`see <a href="https://news.example.com/story">https://news.example.com/story</a></p>`
	if got := URLFromHTML(content); got != "https://news.example.com/story" {
		t.Errorf("URLFromHTML = %q, want the plain link rather than a mention or tag", got)
	}
	if got := URLFromHTML("<p>no links</p>"); got != "" {
		t.Errorf("URLFromHTML(no links) = %q", got)
	}
}

func TestLinkAttachmentRoundTrip(t *testing.T) {
	preview := &models.StatusPreview{URL: "https://news.example.com/story", Title: "Story", Description: "About things"}
	attachment := LinkAttachment(preview)
	if attachment == nil || attachment["type"] != "Link" {
		t.Fatalf("LinkAttachment = %v", attachment)
	}
	back, ok := PreviewFromLinkAttachment(attachment)
	if !ok || back.URL != preview.URL || back.Title != preview.Title {
		t.Errorf("round trip = %+v, %v", back, ok)
	}
	if _, ok := PreviewFromLinkAttachment(map[string]interface{}{"type": "Image", "href": "https://x.example"}); ok {
		t.Error("a non-Link attachment was accepted")
	}
}
