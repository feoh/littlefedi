package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// cacheHookThrottle is the minimum interval between enqueueing lazy cache jobs
// for the same remote URL, preventing a flood of duplicate jobs while a popular
// item is being accessed before its first cache completes.
const cacheHookThrottle = 5 * time.Minute

const maxRecentCacheHooks = 4096
const maxProxyResponseBytes int64 = 512 << 20

// proxyPath is the route prefix where the media proxy is mounted.
const proxyPath = "/proxy/media"

// proxySigLen is the number of hex characters kept from the HMAC signature.
// 32 hex chars = 128 bits, which is plenty to prevent forgery while keeping
// URLs reasonably short.
const proxySigLen = 32

// Proxy streams remote media through the local instance so that local users'
// browsers never connect directly to the originating remote server. This
// protects user privacy (IP address, request timing, User-Agent) from remote
// instances.
//
// Unlike RemoteMediaStore, Proxy does not persist anything to disk: it is a
// pure pass-through stream. Optional on-disk caching/reprocessing of proxied
// media is a planned future enhancement (see RemoteMediaStore and the
// CacheRemote config flag) and is not wired up here yet.
type Proxy struct {
	enabled bool
	secret  []byte
	domain  string
	client  *http.Client

	// cacheHook, when set (lazy cache mode), is invoked with the remote URL the
	// first time an item is proxied so it can be cached in the background.
	cacheHook   func(remoteURL string)
	hookedMu    sync.Mutex
	recentHooks map[string]time.Time

	// rejectHost, when set, reports whether media from a given host must not be
	// fetched (e.g. a reject_media or suspend-severity domain block).
	rejectHost      func(host string) bool
	localMediaHosts map[string]struct{}
}

// AddLocalMediaBase marks a public object/CDN origin as local media so cached
// S3 objects are returned directly instead of being fed back through the proxy.
func (p *Proxy) AddLocalMediaBase(raw string) {
	if p == nil {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return
	}
	if p.localMediaHosts == nil {
		p.localMediaHosts = make(map[string]struct{})
	}
	p.localMediaHosts[strings.ToLower(u.Hostname())] = struct{}{}
}

// SetRejectHook installs a predicate that blocks proxying media from hosts it
// returns true for. Used to enforce reject_media / suspend domain blocks.
func (p *Proxy) SetRejectHook(fn func(host string) bool) {
	if p != nil {
		p.rejectHost = fn
	}
}

// NewProxy creates a media proxy. If secret is empty a random one is generated;
// because proxy URLs are signed and re-generated on every API/web response
// (never persisted), an ephemeral per-process secret is safe. A stable secret
// can be supplied to keep signed URLs valid across restarts or multiple
// instances (useful for downstream HTTP caching/CDNs).
func NewProxy(enabled bool, secret, domain string) *Proxy {
	key := []byte(secret)
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			slog.Error("media: failed to generate proxy secret; disabling proxy", "error", err)
			enabled = false
		}
	}

	return &Proxy{
		enabled: enabled,
		secret:  key,
		domain:  domain,
		client:  newRemoteMediaClient(),
	}
}

// Enabled reports whether remote media proxying is active.
func (p *Proxy) Enabled() bool {
	return p != nil && p.enabled
}

// SetCacheHook registers a callback (lazy cache mode) invoked with the remote
// URL the first time an item is proxied. The hook is throttled per-URL so
// repeated accesses don't enqueue duplicate background cache jobs.
func (p *Proxy) SetCacheHook(fn func(remoteURL string)) {
	p.cacheHook = fn
	p.recentHooks = make(map[string]time.Time)
}

// fireCacheHook invokes the cache hook for remoteURL if one is set and the URL
// hasn't been hooked within the throttle window.
func (p *Proxy) fireCacheHook(remoteURL string) {
	if p.cacheHook == nil {
		return
	}
	now := time.Now()
	p.hookedMu.Lock()
	if last, ok := p.recentHooks[remoteURL]; ok && now.Sub(last) < cacheHookThrottle {
		p.hookedMu.Unlock()
		return
	}
	// Opportunistically evict stale entries, then cap cardinality even during
	// a high-cardinality burst. A full clear can only enqueue an extra
	// best-effort cache job; proxy streaming remains independent.
	if len(p.recentHooks) >= maxRecentCacheHooks {
		for key, seenAt := range p.recentHooks {
			if now.Sub(seenAt) >= cacheHookThrottle {
				delete(p.recentHooks, key)
			}
		}
		if len(p.recentHooks) >= maxRecentCacheHooks {
			clear(p.recentHooks)
		}
	}
	p.recentHooks[remoteURL] = now
	p.hookedMu.Unlock()

	p.cacheHook(remoteURL)
}

// sign returns the truncated hex HMAC-SHA256 signature for a remote URL.
func (p *Proxy) sign(remoteURL string) string {
	mac := hmac.New(sha256.New, p.secret)
	mac.Write([]byte(remoteURL))
	sig := hex.EncodeToString(mac.Sum(nil))
	if len(sig) > proxySigLen {
		sig = sig[:proxySigLen]
	}
	return sig
}

// URL rewrites a remote http(s) media URL into a signed local proxy URL. It
// returns the input unchanged when proxying is disabled, when the input is
// empty, relative, or already points at our own domain (i.e. local media).
func (p *Proxy) URL(raw string) string {
	if !p.Enabled() {
		return raw
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		// Relative path: local media served by this instance.
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return raw
	}
	if strings.EqualFold(u.Hostname(), p.domain) {
		// Already our own media; no need to proxy.
		return raw
	}
	if _, ok := p.localMediaHosts[strings.ToLower(u.Hostname())]; ok {
		return raw
	}

	// Clients such as Moshidon infer download/share names and types from the URL path.
	filename := MediaFilename(u.Path, "", false)
	path := proxyPath
	// shortcut: extensionless origins keep the old path; add MIME hints if those need client downloads.
	if filename != ".." && filepath.Ext(filename) != "" {
		path += "/" + filename
	}
	return fmt.Sprintf("https://%s%s?url=%s&sig=%s",
		p.domain, path, url.QueryEscape(raw), p.sign(raw))
}

// Handler returns the HTTP handler that validates a signed proxy URL and
// streams the remote media back to the client. It enforces the HMAC signature
// (so the endpoint cannot be used as an open proxy) and the SSRF guard, and
// forwards Range requests so video/audio seeking keeps working.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteURL := r.URL.Query().Get("url")
		sig := r.URL.Query().Get("sig")
		if remoteURL == "" || sig == "" {
			http.Error(w, "missing url or sig parameter", http.StatusBadRequest)
			return
		}

		// Verify the signature before touching the network: this restricts the
		// proxy to URLs that this instance itself generated.
		if !hmac.Equal([]byte(sig), []byte(p.sign(remoteURL))) {
			slog.Warn("media: proxy: invalid signature", "url", remoteURL)
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}

		// SSRF guard: block private/loopback/link-local targets.
		if err := validateRemoteURL(remoteURL); err != nil {
			slog.Warn("media: proxy: SSRF guard blocked", "url", remoteURL, "error", err)
			http.Error(w, "invalid url", http.StatusBadRequest)
			return
		}

		// Domain-block guard: refuse media from rejected hosts.
		if p.rejectHost != nil {
			if u, err := url.Parse(remoteURL); err == nil && p.rejectHost(u.Hostname()) {
				slog.Debug("media: proxy: host rejected by domain block", "url", remoteURL)
				http.Error(w, "media blocked", http.StatusForbidden)
				return
			}
		}

		// Lazy cache: schedule background caching on first access.
		p.fireCacheHook(remoteURL)

		p.stream(w, r, remoteURL)
	})
}

// stream fetches the remote media and copies it to the response writer.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, remoteURL string) {
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(r.Context(), method, remoteURL, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", "littleFedi/"+version.Version+" (media proxy)")
	req.Header.Set("Accept", "image/*, video/*, audio/*")
	// Forward Range so clients can seek within audio/video.
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	for _, key := range []string{"If-None-Match", "If-Modified-Since", "If-Range"} {
		if value := r.Header.Get(key); value != "" {
			req.Header.Set(key, value)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		slog.Warn("media: proxy: upstream fetch failed", "url", remoteURL, "error", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		copyHeader(w.Header(), resp.Header, "Last-Modified")
		copyHeader(w.Header(), resp.Header, "ETag")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		slog.Warn("media: proxy: upstream returned error", "url", remoteURL, "status", resp.StatusCode)
		http.Error(w, "upstream returned error", http.StatusBadGateway)
		return
	}
	if resp.ContentLength > maxProxyResponseBytes {
		http.Error(w, "upstream media too large", http.StatusBadGateway)
		return
	}

	// Only relay media content types; never proxy HTML or other documents.
	contentType := resp.Header.Get("Content-Type")
	if !isProxyableContentType(contentType) {
		slog.Warn("media: proxy: refused non-media content type", "url", remoteURL, "content_type", contentType)
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	// Relay a conservative set of headers needed for correct rendering and
	// range support, then cache aggressively (remote media is immutable).
	h := w.Header()
	h.Set("Content-Type", contentType)
	copyHeader(h, resp.Header, "Content-Length")
	copyHeader(h, resp.Header, "Content-Range")
	copyHeader(h, resp.Header, "Accept-Ranges")
	copyHeader(h, resp.Header, "Last-Modified")
	copyHeader(h, resp.Header, "ETag")
	h.Set("Cache-Control", "public, max-age=86400, immutable")
	// Defense-in-depth: instruct browsers not to sniff a different type.
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	bufp := proxyBufPool.Get().(*[]byte)
	defer proxyBufPool.Put(bufp)
	if _, err := io.CopyBuffer(w, io.LimitReader(resp.Body, maxProxyResponseBytes), *bufp); err != nil {
		slog.Debug("media: proxy: copy interrupted", "url", remoteURL, "error", err)
	}
}

// proxyBufPool reuses stream-copy buffers across proxied media requests so
// concurrent media loads don't each allocate a fresh buffer.
var proxyBufPool = sync.Pool{New: func() any {
	b := make([]byte, 64<<10)
	return &b
}}

// isProxyableContentType reports whether a media content type is safe to relay.
func isProxyableContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	// SVG is an active document format. Relaying attacker-controlled SVG from
	// the instance origin would give it a trusted same-origin URL.
	if ct == "image/svg+xml" {
		return false
	}
	return strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/")
}

// copyHeader copies a single header value from src to dst when present.
func copyHeader(dst, src http.Header, key string) {
	if v := src.Get(key); v != "" {
		dst.Set(key, v)
	}
}
