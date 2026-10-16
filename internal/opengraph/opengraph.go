// Package opengraph extracts and fetches link preview metadata for posts.
package opengraph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
	"golang.org/x/net/html"
)

const (
	maxDocumentSize = 2 << 20
	maxCacheEntries = 2048
)

// Card is the subset of Open Graph metadata used by the web interface.
type Card struct {
	URL         string
	Title       string
	Description string
	Image       string
	SiteName    string
}

type cacheEntry struct {
	card    *Card
	err     error // kept so a cached failure reports its cause instead of an empty result
	expires time.Time
	ready   chan struct{}
}

// Fetcher safely fetches and caches Open Graph metadata.
type Fetcher struct {
	client *http.Client
	store  Store
	images ImageCacher
	mu     sync.Mutex
	cache  map[string]*cacheEntry
}

// Store is the durable preview persistence used by Fetcher.
type Store interface {
	GetStatusPreview(ctx context.Context, statusID string) (*models.StatusPreview, error)
	UpsertStatusPreview(ctx context.Context, preview *models.StatusPreview) error
}

// ImageCacher is implemented by media.Cache when remote media caching is on.
type ImageCacher interface {
	Store(ctx context.Context, remoteURL string) (*media.StoreResult, error)
}

type Option func(*Fetcher)

func WithStore(store Store) Option            { return func(f *Fetcher) { f.store = store } }
func WithImageCache(cache ImageCacher) Option { return func(f *Fetcher) { f.images = cache } }

// NewFetcher returns a fetcher suitable for untrusted URLs found in posts.
func NewFetcher(options ...Option) *Fetcher {
	// The guard lives in Control so it inspects the address actually being
	// connected to, and so the dialer keeps its own address walk: a host with
	// several A/AAAA records still reaches a working one when the first is
	// unreachable, which resolve-then-dial-ips[0] could not do.
	dialer := &net.Dialer{
		Timeout:       4 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !safeIP(ip) {
				return fmt.Errorf("unsafe preview address %q", host)
			}
			return nil
		},
	}
	transport := &http.Transport{
		ResponseHeaderTimeout: 4 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	}
	f := &Fetcher{
		client: &http.Client{
			// Links posted by a mesh peer point at that peer, so previews
			// have to be able to follow a circuit.
			Transport: mesh.WrapDefault(netutil.WithIPv4Fallback(transport, dialer)),
			Timeout:   6 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 4 {
					return fmt.Errorf("too many redirects")
				}
				return validateURL(req.URL)
			},
		},
		cache: make(map[string]*cacheEntry),
	}
	for _, option := range options {
		option(f)
	}
	return f
}

// PreviewForStatus returns a durable preview tied to statusID. A successful
// origin fetch is stored once and remains until that status is deleted. If the
// post is edited to point elsewhere, the URL mismatch refreshes the row.
func (f *Fetcher) PreviewForStatus(ctx context.Context, statusID, rawURL string) (*Card, error) {
	if f.store == nil || statusID == "" {
		return f.Preview(ctx, rawURL)
	}
	if card, cachedErr, ok := f.cachedCard(rawURL); ok {
		return card, cachedErr
	}
	stored, err := f.store.GetStatusPreview(ctx, statusID)
	if err == nil && stored.SourceURL == rawURL {
		if stored.ImageRemote != "" && stored.CachedAt == nil && f.images != nil {
			f.cacheImage(ctx, stored)
			if err := f.store.UpsertStatusPreview(ctx, stored); err != nil {
				return nil, fmt.Errorf("update cached preview image: %w", err)
			}
		}
		card := cardFromModel(stored)
		f.remember(rawURL, card)
		return card, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load cached preview: %w", err)
	}

	card, err := f.Preview(ctx, rawURL)
	if err != nil || card == nil {
		return card, err
	}
	now := time.Now()
	stored = &models.StatusPreview{
		StatusID: statusID, SourceURL: rawURL, URL: card.URL, Title: card.Title,
		Description: card.Description, Image: card.Image, ImageRemote: card.Image,
		SiteName: card.SiteName, FetchedAt: now,
	}
	if stored.ImageRemote != "" && f.images != nil {
		f.cacheImage(ctx, stored)
	}
	if err := f.store.UpsertStatusPreview(ctx, stored); err != nil {
		return nil, fmt.Errorf("store cached preview: %w", err)
	}
	card = cardFromModel(stored)
	f.remember(rawURL, card)
	return card, nil
}

func (f *Fetcher) cachedCard(rawURL string) (*Card, error, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.cache[rawURL]
	if entry == nil || entry.ready != nil || !time.Now().Before(entry.expires) {
		return nil, nil, false
	}
	return entry.card, entry.err, true
}

func (f *Fetcher) remember(rawURL string, card *Card) {
	now := time.Now()
	f.mu.Lock()
	f.cache[rawURL] = &cacheEntry{card: card, expires: now.Add(24 * time.Hour)}
	f.pruneLocked(now)
	f.mu.Unlock()
}

func (f *Fetcher) cacheImage(ctx context.Context, preview *models.StatusPreview) {
	result, err := f.images.Store(ctx, preview.ImageRemote)
	if err != nil {
		if errors.Is(err, media.ErrNotCacheable) {
			now := time.Now()
			preview.CachedAt = &now // suppress retry storms until age pruning resets it
		}
		return
	}
	now := time.Now()
	preview.Image = result.URL
	preview.CachedAt = &now
}

func cardFromModel(preview *models.StatusPreview) *Card {
	return &Card{URL: preview.URL, Title: preview.Title, Description: preview.Description, Image: preview.Image, SiteName: preview.SiteName}
}

// URLFromHTML returns the first ordinary HTTP(S) link in post HTML. Mention
// and hashtag links are ignored because they are navigation, not preview URLs.
func URLFromHTML(content string) string {
	z := html.NewTokenizer(strings.NewReader(content))
	var href, class, rel string
	var label strings.Builder
	inAnchor := false
	finish := func() string {
		text := strings.TrimSpace(label.String())
		if hasClass(class, "mention") || hasClass(class, "hashtag") || hasClass(rel, "tag") ||
			strings.HasPrefix(text, "@") || strings.HasPrefix(text, "#") {
			return ""
		}
		u, err := url.Parse(strings.TrimSpace(href))
		if err != nil || validateURL(u) != nil {
			return ""
		}
		return u.String()
	}
	for {
		switch z.Next() {
		case html.ErrorToken:
			if inAnchor {
				return finish()
			}
			return ""
		case html.StartTagToken:
			t := z.Token()
			if t.Data != "a" {
				continue
			}
			// HTML implicitly closes an open anchor when another starts.
			if inAnchor {
				if found := finish(); found != "" {
					return found
				}
			}
			href, class, rel = "", "", ""
			label.Reset()
			inAnchor = true
			for _, attr := range t.Attr {
				switch strings.ToLower(attr.Key) {
				case "href":
					href = attr.Val
				case "class":
					class = attr.Val
				case "rel":
					rel = attr.Val
				}
			}
		case html.TextToken:
			if inAnchor {
				label.Write(z.Text())
			}
		case html.EndTagToken:
			if inAnchor {
				t, _ := z.TagName()
				if string(t) == "a" {
					if found := finish(); found != "" {
						return found
					}
					inAnchor = false
				}
			}
		}
	}
}

// Preview returns cached metadata or fetches it once for concurrent callers.
// Failed lookups are cached briefly to avoid repeatedly hitting broken links.
func (f *Fetcher) Preview(ctx context.Context, rawURL string) (*Card, error) {
	u, err := url.Parse(rawURL)
	if err != nil || validateURL(u) != nil {
		return nil, fmt.Errorf("invalid preview URL")
	}
	rawURL = u.String()
	now := time.Now()

	f.mu.Lock()
	if entry, ok := f.cache[rawURL]; ok {
		if entry.ready != nil {
			ready := entry.ready
			f.mu.Unlock()
			select {
			case <-ready:
				return f.cached(rawURL)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if now.Before(entry.expires) {
			card, cachedErr := entry.card, entry.err
			f.mu.Unlock()
			return card, cachedErr
		}
		delete(f.cache, rawURL)
	}
	entry := &cacheEntry{ready: make(chan struct{})}
	f.cache[rawURL] = entry
	f.mu.Unlock()

	card, fetchErr := f.fetch(ctx, u)
	f.mu.Lock()
	entry.card, entry.err = card, fetchErr
	if fetchErr == nil && card != nil {
		entry.expires = now.Add(24 * time.Hour)
	} else {
		entry.expires = now.Add(30 * time.Minute)
	}
	close(entry.ready)
	entry.ready = nil
	f.pruneLocked(now)
	f.mu.Unlock()
	return card, fetchErr
}

func (f *Fetcher) cached(rawURL string) (*Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.cache[rawURL]
	if entry == nil {
		return nil, nil
	}
	return entry.card, entry.err
}

func (f *Fetcher) fetch(ctx context.Context, pageURL *url.URL) (*Card, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "littleFedi/"+version.Version+" (link preview)")
	req.Header.Set("Accept", "text/html, application/xhtml+xml")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("preview returned %s", resp.Status)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "text/html") && !strings.HasPrefix(contentType, "application/xhtml+xml") {
		return nil, fmt.Errorf("preview is not HTML")
	}
	card, err := parseHTML(io.LimitReader(resp.Body, maxDocumentSize+1), resp.Request.URL)
	if err != nil {
		return nil, err
	}
	if card.Title == "" {
		return nil, fmt.Errorf("preview has no title")
	}
	// Images load automatically in the browser, unlike ordinary links. Do not
	// emit an image URL unless its current DNS answers are all public.
	if card.Image != "" && !safeRemoteURL(ctx, card.Image) {
		card.Image = ""
	}
	return card, nil
}

func parseHTML(r io.Reader, pageURL *url.URL) (*Card, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	card := &Card{URL: pageURL.String()}
	var fallbackTitle, fallbackDescription string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if fallbackTitle == "" && n.FirstChild != nil {
					fallbackTitle = n.FirstChild.Data
				}
			case "meta":
				attrs := attributes(n)
				key := strings.ToLower(firstNonEmpty(attrs["property"], attrs["name"]))
				value := attrs["content"]
				switch key {
				case "og:title":
					card.Title = value
				case "og:description":
					card.Description = value
				case "og:image", "og:image:url", "og:image:secure_url":
					if card.Image == "" {
						card.Image = resolveURL(pageURL, value)
					}
				case "og:site_name":
					card.SiteName = value
				case "og:url":
					if resolved := resolveURL(pageURL, value); resolved != "" {
						card.URL = resolved
					}
				case "description":
					fallbackDescription = value
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	card.Title = clean(firstNonEmpty(card.Title, fallbackTitle), 300)
	card.Description = clean(firstNonEmpty(card.Description, fallbackDescription), 500)
	card.SiteName = clean(card.SiteName, 100)
	if card.SiteName == "" {
		card.SiteName = pageURL.Hostname()
	}
	return card, nil
}

func attributes(n *html.Node) map[string]string {
	attrs := make(map[string]string, len(n.Attr))
	for _, attr := range n.Attr {
		attrs[strings.ToLower(attr.Key)] = strings.TrimSpace(attr.Val)
	}
	return attrs
}

func validateURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("unsupported URL")
	}
	return nil
}

func safeIP(ip net.IP) bool {
	return netutil.IsPublicIP(ip)
}

func safeRemoteURL(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || validateURL(u) != nil {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return safeIP(ip)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, resolved := range ips {
		if !safeIP(resolved.IP) {
			return false
		}
	}
	return true
}

func resolveURL(base *url.URL, raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || raw == "" {
		return ""
	}
	resolved := base.ResolveReference(u)
	if validateURL(resolved) != nil {
		return ""
	}
	return resolved.String()
}

func clean(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > max {
		s = string(runes[:max]) + "…"
	}
	return s
}

func hasClass(classes, target string) bool {
	for _, class := range strings.Fields(classes) {
		if class == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (f *Fetcher) pruneLocked(now time.Time) {
	if len(f.cache) <= maxCacheEntries {
		return
	}
	for key, entry := range f.cache {
		if entry.ready == nil && now.After(entry.expires) {
			delete(f.cache, key)
		}
	}
	for key, entry := range f.cache {
		if len(f.cache) <= maxCacheEntries {
			break
		}
		if entry.ready == nil {
			delete(f.cache, key)
		}
	}
}
