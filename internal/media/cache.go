// Package media provides remote media caching and proxy functionality.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// ErrNotCacheable marks a remote media item that should not be cached and should
// not be retried - e.g. it is too large, isn't a media type we store, or the
// origin returned a permanent (4xx) error. Such media is simply proxied instead.
var ErrNotCacheable = errors.New("remote media is not cacheable")

// CacheDirName is the URL segment under /media/ at which cached remote media is
// served (/media/cache/...). The on-disk location is configurable and need not
// be literally named "cache".
const CacheDirName = "cache"

// cacheURLPrefix is the public URL prefix for cached media. Files are
// content-addressed by a hash of their remote URL: <prefix>/<hash[:2]>/<hash>/original.<ext>.
const cacheURLPrefix = "/media/" + CacheDirName + "/"

// IsCacheURL reports whether a served media URL points at the remote media cache.
// Cache files are content-addressed and may be shared across rows, so callers
// that delete media files for a single row must skip cache URLs (the cache has
// its own reference-counted/age-based pruning).
func IsCacheURL(url string) bool {
	return strings.HasPrefix(url, cacheURLPrefix)
}

// OwnedLocalPath resolves an owned /media/ URL below mediaPath. Database rows
// can contain remote input, so deletion paths must fail closed on traversal or
// non-local URLs.
func OwnedLocalPath(mediaPath, mediaURL string) (string, bool) {
	if !strings.HasPrefix(mediaURL, "/media/") {
		return "", false
	}
	rel := strings.ReplaceAll(strings.TrimPrefix(mediaURL, "/media/"), "\\", "/")
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return filepath.Join(mediaPath, filepath.FromSlash(clean)), true
}

// Cache fetches remote media, processes it (EXIF strip, thumbnail, dimensions
// and blurhash for images) and stores it on the local filesystem so it can be
// served directly instead of proxied. It is the engine behind both the "eager"
// and "lazy" caching modes; the decision of when to call Store lives in the
// wiring layer.
type Cache struct {
	cachePath string // root directory for cached files
	maxBytes  int64  // per-file cap; <= 0 disables the cap
	stripEXIF bool
	client    *http.Client

	inflightMu sync.Mutex
	inflight   map[string]*cacheCall
}

type cacheCall struct {
	done   chan struct{}
	result *StoreResult
	err    error
}

// NewCache creates a remote media cache rooted at cachePath. maxBytes caps the
// size of a single cached file (use 0 to disable).
func NewCache(cachePath string, maxBytes int64, stripEXIF bool) *Cache {
	return &Cache{
		cachePath: cachePath,
		maxBytes:  maxBytes,
		stripEXIF: stripEXIF,
		client:    newRemoteMediaClient(),
		inflight:  make(map[string]*cacheCall),
	}
}

// StoreResult describes a cached media file.
type StoreResult struct {
	URL               string // local path, e.g. /media/cache/ab/<hash>/original.jpg
	PreviewURL        string // local thumbnail path; empty for videos without one
	Blurhash          string // computed for images, empty otherwise
	Meta              string // measured image dimensions as JSON
	MIMEType          string
	FileSize          int64
	StorageBackend    string
	StorageKey        string
	PreviewStorageKey string
}

type cachedImageMetadata struct {
	Blurhash string          `json:"blurhash,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

// cacheDir returns the on-disk directory and content hash for a remote URL.
func (c *Cache) cacheDir(remoteURL string) (dir, hash string) {
	sum := sha256.Sum256([]byte(remoteURL))
	hash = hex.EncodeToString(sum[:])
	dir = filepath.Join(c.cachePath, hash[:2], hash)
	return dir, hash
}

// localURL builds the public URL path for a file inside the cache directory.
func (c *Cache) localURL(hash, filename string) string {
	return cacheURLPrefix + hash[:2] + "/" + hash + "/" + filename
}

// Store fetches and stores a remote media file, returning its local URLs. It is
// idempotent: if the file is already cached it returns the existing paths
// without re-downloading. SSRF-guarded and size-capped.
func (c *Cache) Store(ctx context.Context, remoteURL string) (*StoreResult, error) {
	if remoteURL == "" {
		return nil, fmt.Errorf("empty remote URL")
	}

	dir, hash := c.cacheDir(remoteURL)

	c.inflightMu.Lock()
	if call := c.inflight[remoteURL]; call != nil {
		c.inflightMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return cloneStoreResult(call.result), call.err
		}
	}

	// Fast path: already cached. Find the stored original.* file. This makes no
	// outbound request, so it runs before the SSRF check. It also shares the
	// in-flight lock with leaders, preventing a follower from seeing a file
	// while its leader is still writing or processing it.
	if res := c.cachedResult(dir, hash); res != nil {
		c.inflightMu.Unlock()
		return res, nil
	}

	call := &cacheCall{done: make(chan struct{})}
	c.inflight[remoteURL] = call
	c.inflightMu.Unlock()

	result, err := c.storeLeader(ctx, remoteURL, dir, hash)
	c.inflightMu.Lock()
	call.result = result
	call.err = err
	delete(c.inflight, remoteURL)
	close(call.done)
	c.inflightMu.Unlock()
	return cloneStoreResult(result), err
}

func (c *Cache) cachedResult(dir, hash string) *StoreResult {
	existing := findOriginal(dir)
	if existing == "" {
		return nil
	}

	info, err := os.Stat(existing)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	filename := filepath.Base(existing)
	res := &StoreResult{
		URL:      c.localURL(hash, filename),
		FileSize: info.Size(),
	}
	res.MIMEType = mime.TypeByExtension(filepath.Ext(filename))
	if !strings.HasPrefix(res.MIMEType, "video/") {
		res.PreviewURL = res.URL
	}
	if small := findThumbnail(existing); small != "" {
		res.PreviewURL = c.localURL(hash, filepath.Base(small))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "metadata.json")); err == nil {
		var metadata cachedImageMetadata
		if json.Unmarshal(data, &metadata) == nil {
			res.Blurhash = metadata.Blurhash
			res.Meta = string(metadata.Meta)
		}
	}
	// Cache entries created before metadata.json can still recover the
	// ratio. Prefer the thumbnail because its EXIF orientation is baked in.
	if res.Meta == "" {
		for _, name := range []string{findThumbnail(existing), existing} {
			file, err := os.Open(name)
			if err != nil {
				continue
			}
			cfg, _, err := image.DecodeConfig(file)
			file.Close()
			if err == nil && cfg.Width > 0 && cfg.Height > 0 {
				res.Meta = fmt.Sprintf(`{"width":%d,"height":%d}`, cfg.Width, cfg.Height)
				break
			}
		}
	}
	return res
}

func cloneStoreResult(result *StoreResult) *StoreResult {
	if result == nil {
		return nil
	}
	copy := *result
	return &copy
}

func (c *Cache) storeLeader(ctx context.Context, remoteURL, dir, hash string) (*StoreResult, error) {
	// Caching requires an outbound fetch: guard against SSRF.
	if err := validateRemoteURL(remoteURL); err != nil {
		return nil, fmt.Errorf("refused remote URL: %v: %w", err, ErrNotCacheable)
	}

	// Fetch from the origin.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "littleFedi/"+version.Version+" (media cache)")
	req.Header.Set("Accept", "image/*, video/*, audio/*")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch remote media: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx is a permanent client error (gone/forbidden) - don't retry; 5xx and
		// other codes are transient and worth retrying.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("remote media returned status %d: %w", resp.StatusCode, ErrNotCacheable)
		}
		return nil, fmt.Errorf("remote media returned status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !isProxyableContentType(contentType) {
		return nil, fmt.Errorf("refusing to cache non-media content type %q: %w", contentType, ErrNotCacheable)
	}

	ext := mimeToExt(contentType)
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(remoteURL))
	}
	if ext == "" {
		ext = ".bin"
	}
	filename := "original" + ext
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return nil, fmt.Errorf("create cache parent: %w", err)
	}
	tmpDir, err := os.MkdirTemp(parent, "."+hash+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("create cache staging dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	localPath := filepath.Join(tmpDir, filename)

	out, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create cache file: %w", err)
	}

	// Copy with an optional size cap. We read one byte past the cap to detect
	// oversize responses.
	var reader io.Reader = resp.Body
	if c.maxBytes > 0 {
		reader = io.LimitReader(resp.Body, c.maxBytes+1)
	}
	written, copyErr := io.Copy(out, reader)
	closeErr := out.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("write cache file: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close cache file: %w", closeErr)
	}
	if c.maxBytes > 0 && written > c.maxBytes {
		return nil, fmt.Errorf("remote media exceeds cache size limit (%d bytes): %w", c.maxBytes, ErrNotCacheable)
	}

	res := &StoreResult{
		URL:        c.localURL(hash, filename),
		PreviewURL: c.localURL(hash, filename),
		MIMEType:   contentType,
		FileSize:   written,
	}
	if strings.HasPrefix(contentType, "video/") {
		res.PreviewURL = ""
	}

	// For images, strip EXIF then generate a thumbnail + blurhash, all around
	// a single decode (this is the high-frequency image-processing path on an
	// instance caching remote media).
	if strings.HasPrefix(contentType, "image/") && contentType != "image/svg+xml" {
		scratch := &models.MediaAttachment{MIMEType: contentType, URL: res.URL}
		// PrepareUploadedImage writes the thumbnail next to the original; its
		// own URL computation assumes the file lives under the media root,
		// which the cache directory may not, so we derive the preview URL
		// ourselves.
		if err := PrepareUploadedImage(localPath, contentType, c.stripEXIF, scratch, c.cachePath); err != nil {
			return nil, fmt.Errorf("remote image rejected: %v: %w", err, ErrNotCacheable)
		}
		res.Blurhash = scratch.Blurhash
		res.Meta = scratch.Meta
		if small := findThumbnail(localPath); small != "" {
			res.PreviewURL = c.localURL(hash, filepath.Base(small))
		}
		metadata, err := json.Marshal(cachedImageMetadata{Blurhash: res.Blurhash, Meta: json.RawMessage(res.Meta)})
		if err != nil {
			return nil, fmt.Errorf("encode cached image metadata: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "metadata.json"), metadata, 0o644); err != nil {
			return nil, fmt.Errorf("store cached image metadata: %w", err)
		}
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		if existing := c.cachedResult(dir, hash); existing != nil {
			return existing, nil
		}
		// File-based eviction can leave thumbnails or metadata without an original.
		if err := removeIncompleteCacheDir(dir); err != nil {
			if existing := c.cachedResult(dir, hash); existing != nil {
				return existing, nil
			}
			return nil, fmt.Errorf("remove incomplete cache item: %w", err)
		}
		if err := os.Rename(tmpDir, dir); err != nil {
			if existing := c.cachedResult(dir, hash); existing != nil {
				return existing, nil
			}
			return nil, fmt.Errorf("publish cache item: %w", err)
		}
	}

	slog.Info("media: cached remote media", "remote_url", remoteURL, "local_url", res.URL, "bytes", written)
	return res, nil
}

func removeIncompleteCacheDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "original.") {
			return nil
		}
	}
	// Keep removals tied to this directory even if another publisher replaces
	// its path. The final rmdir cannot remove a newly published, nonempty entry.
	for _, entry := range entries {
		if err := root.Remove(entry.Name()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// findOriginal returns the path to the stored original.* file in dir, or "".
func findOriginal(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "original.*"))
	for _, m := range matches {
		if !strings.Contains(filepath.Base(m), "_small") {
			return m
		}
	}
	return ""
}

// findThumbnail returns the path to the thumbnail variant of an original file, or "".
func findThumbnail(originalPath string) string {
	ext := filepath.Ext(originalPath)
	base := strings.TrimSuffix(originalPath, ext)
	for _, candidate := range []string{base + "_small" + ext, base + "_small.jpg"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// RemoveCacheEntry deletes the on-disk cache directory (under cachePath) backing
// a cached local URL (/media/cache/<h2>/<hash>/...). It is safe to call for
// non-cache URLs (no-op).
func RemoveCacheEntry(cachePath, localURL string) error {
	if !IsCacheURL(localURL) {
		return nil
	}
	rel := strings.TrimPrefix(localURL, cacheURLPrefix)
	parts := strings.Split(rel, "/")
	if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 64 || parts[0] != parts[1][:2] || filepath.Base(parts[2]) != parts[2] {
		return fmt.Errorf("invalid cache URL")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return fmt.Errorf("invalid cache URL")
	}
	return os.RemoveAll(filepath.Join(cachePath, parts[0], parts[1]))
}

// mimeToExt converts a MIME type to a file extension.
func mimeToExt(mime string) string {
	// Content-Type may include parameters, e.g. "image/jpeg; charset=binary".
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	switch strings.TrimSpace(strings.ToLower(mime)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	default:
		return ""
	}
}

// PruneStaleMedia removes media attachments that are unattached and older than the given duration.
// This cleans up uploaded files that were never attached to a status.
func PruneStaleMedia(ctx context.Context, store interface {
	// GetUnattachedMedia takes (ctx, accountID, olderThan) but we pass "" for accountID to match all
	GetUnattachedMedia(ctx context.Context, accountID string, olderThan time.Duration) ([]*models.MediaAttachment, error)
	DeleteMediaAttachment(ctx context.Context, id string) error
}, mediaPath string, maxAge time.Duration, stores ...objectstore.Store) (int, error) {
	unattached, err := store.GetUnattachedMedia(ctx, "", maxAge)
	if err != nil {
		return 0, fmt.Errorf("get unattached media: %w", err)
	}

	pruned := 0
	for _, m := range unattached {
		_ = DeleteAttachmentFiles(ctx, firstStore(stores), mediaPath, m)

		// Remove from database
		if err := store.DeleteMediaAttachment(ctx, m.ID); err != nil {
			slog.Warn("media: prune: failed to delete media attachment", "id", m.ID, "error", err)
			continue
		}
		pruned++
	}

	return pruned, nil
}

// RemoveOrphanedMedia removes media attachments whose status_id points to a non-existent status.
// This cleans up when a status was deleted without its media being properly cleaned up.
func RemoveOrphanedMedia(ctx context.Context, store interface {
	GetOrphanedMediaAttachments(ctx context.Context) ([]*models.MediaAttachment, error)
	DeleteMediaAttachment(ctx context.Context, id string) error
}, mediaPath string, stores ...objectstore.Store) (int, error) {
	orphaned, err := store.GetOrphanedMediaAttachments(ctx)
	if err != nil {
		return 0, fmt.Errorf("get orphaned media: %w", err)
	}

	removed := 0
	for _, m := range orphaned {
		_ = DeleteAttachmentFiles(ctx, firstStore(stores), mediaPath, m)

		// Remove from database
		if err := store.DeleteMediaAttachment(ctx, m.ID); err != nil {
			slog.Warn("media: remove orphaned: failed to delete media attachment", "id", m.ID, "error", err)
			continue
		}
		removed++
	}

	return removed, nil
}

// RemoveUnreferencedMediaFromDisk removes media files from disk that have no corresponding
// database record. This cleans up leftover files from failed uploads or manual DB cleanup.
func RemoveUnreferencedMediaFromDisk(ctx context.Context, store interface {
	GetAllMedia(ctx context.Context) ([]*models.MediaAttachment, error)
}, mediaPath string) (int, error) {
	// Get all known media from the database
	allMedia, err := store.GetAllMedia(ctx)
	if err != nil {
		return 0, fmt.Errorf("get all media: %w", err)
	}

	// Build a set of known media directory IDs (the UUID directory names)
	expectedDirs := make(map[string]bool, len(allMedia))
	for _, m := range allMedia {
		// URL format: /media/{mediaID}/{filename}
		if m.URL != "" {
			trimmed := strings.TrimPrefix(m.URL, "/media/")
			parts := strings.SplitN(trimmed, "/", 2)
			if len(parts) > 0 {
				expectedDirs[parts[0]] = true
			}
		}
		// Also check preview URL
		if m.PreviewURL != "" && m.PreviewURL != m.URL {
			trimmed := strings.TrimPrefix(m.PreviewURL, "/media/")
			parts := strings.SplitN(trimmed, "/", 2)
			if len(parts) > 0 {
				expectedDirs[parts[0]] = true
			}
		}
		// Also consider the media ID itself as the directory name
		expectedDirs[m.ID] = true
	}

	// Read the media directory and find subdirectories without a DB record
	entries, err := os.ReadDir(mediaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read media dir: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		// The remote media cache has its own nested, content-addressed layout and
		// its own age-based pruner; never treat it as an unreferenced upload dir.
		if dirName == CacheDirName || dirName == "exports" || dirName == "imports" {
			continue
		}
		if expectedDirs[dirName] {
			continue // This directory has a matching DB record
		}

		// Remove the entire unreferenced directory
		dirPath := filepath.Join(mediaPath, dirName)
		if err := os.RemoveAll(dirPath); err != nil {
			slog.Warn("media: failed to remove unreferenced directory", "path", dirPath, "error", err)
			continue
		}
		removed++
		slog.Info("media: removed unreferenced media directory", "path", dirPath)
	}

	return removed, nil
}

// validateRemoteURL checks that a URL uses http/https scheme and resolves to
// a non-private, non-loopback, non-link-local IP address. This prevents SSRF
// into internal networks, cloud metadata services, and local services.
func validateRemoteURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Only allow http and https schemes
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported scheme: %s", scheme)
	}
	if u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("remote URL must have a host and no credentials")
	}

	// Resolve the hostname to IP addresses
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %s: %w", host, err)
	}

	for _, ip := range ips {
		if isPrivateOrReservedIP(ip) {
			return fmt.Errorf("host %s resolves to private/reserved IP %s", host, ip)
		}
	}

	return nil
}

// isPrivateOrReservedIP checks if an IP address is in a private, loopback,
// link-local, or other special-purpose range that should not be reachable
// via outbound HTTP requests.
func isPrivateOrReservedIP(ip net.IP) bool {
	return !netutil.IsPublicIP(ip)
}

// removeThumbnails removes all thumbnail variants of a given media file path.
func removeThumbnails(filePath string) {
	ext := filepath.Ext(filePath)
	base := strings.TrimSuffix(filePath, ext)
	// Standard _small thumbnail
	os.Remove(base + "_small" + ext)
	// GIF-to-JPEG converted thumbnail
	os.Remove(base + "_small.jpg")
}

func firstStore(stores []objectstore.Store) objectstore.Store {
	if len(stores) > 0 {
		return stores[0]
	}
	return nil
}

// DeleteAttachmentFiles removes owned attachment objects and legacy local
// files. Shared content-addressed cache objects are reclaimed separately.
func DeleteAttachmentFiles(ctx context.Context, store objectstore.Store, mediaPath string, m *models.MediaAttachment) error {
	if m == nil || IsCacheURL(m.URL) || strings.HasPrefix(m.StorageKey, "cache/") {
		return nil
	}
	var firstErr error
	if store != nil {
		seen := map[string]bool{}
		for _, key := range []string{m.StorageKey, m.PreviewStorageKey} {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if err := store.Delete(ctx, key); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if filePath, ok := OwnedLocalPath(mediaPath, m.URL); ok {
		_ = os.Remove(filePath)
		removeThumbnails(filePath)
		_ = os.Remove(filepath.Dir(filePath))
	}
	return firstErr
}
