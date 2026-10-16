package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed templates/*.html templates/components/*.html templates/admin/*.html static/*.css static/*.js static/*.webmanifest static/*.png static/*.ico static/*.svg static/*.html static/*.txt static/*.woff2
var embeddedFS embed.FS

type staticAsset struct {
	body        []byte
	gzipBody    []byte // nil when compression doesn't shrink the asset
	etag        string
	contentType string
}

var (
	staticAssetsOnce sync.Once
	staticAssets     map[string]*staticAsset
)

// iconsAssetURL is literal in templates to avoid a reflected template-function
// call for every icon on every card. TestVersionedApplicationAssets fences it
// to the embedded sprite hash, so changing icons.svg requires changing this.
const iconsAssetURL = "/static/icons.svg?v=adb0669912b5d0ff"

func baseStaticAssets() map[string]*staticAsset {
	staticAssetsOnce.Do(func() {
		staticAssets = buildStaticAssets()
	})
	return staticAssets
}

// buildStaticAssets loads every embedded static file once, precomputing its
// ETag and a gzip variant, so serving is a map lookup plus one Write.
func buildStaticAssets() map[string]*staticAsset {
	assets := make(map[string]*staticAsset)
	_ = fs.WalkDir(embeddedFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := embeddedFS.ReadFile(p)
		if err != nil {
			return nil
		}
		contentType := "application/octet-stream"
		switch {
		case strings.HasSuffix(p, ".js"):
			contentType = "application/javascript; charset=utf-8"
		case strings.HasSuffix(p, ".css"):
			contentType = "text/css; charset=utf-8"
		case strings.HasSuffix(p, ".webmanifest"):
			contentType = "application/manifest+json"
		case strings.HasSuffix(p, ".png"):
			contentType = "image/png"
		case strings.HasSuffix(p, ".ico"):
			contentType = "image/x-icon"
		case strings.HasSuffix(p, ".svg"):
			contentType = "image/svg+xml"
		case strings.HasSuffix(p, ".html"):
			contentType = "text/html; charset=utf-8"
		case strings.HasSuffix(p, ".txt"):
			contentType = "text/plain; charset=utf-8"
		case strings.HasSuffix(p, ".woff2"):
			contentType = "font/woff2"
		}
		sum := sha256.Sum256(data)
		asset := &staticAsset{
			body:        data,
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
			contentType: contentType,
		}
		var buf bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := gz.Write(data); err == nil && gz.Close() == nil && buf.Len() < len(data) {
			asset.gzipBody = append([]byte(nil), buf.Bytes()...)
		}
		assets[p] = asset
		return nil
	})
	return assets
}

func staticAssetURL(name string) string {
	key := path.Join("static", strings.TrimPrefix(name, "/"))
	asset := baseStaticAssets()[key]
	if asset == nil {
		return "/" + key
	}
	return "/" + key + "?v=" + strings.Trim(asset.etag, `"`)
}

// StaticHandler serves embedded static files (JS, CSS, etc.)
func StaticHandler(instanceNames ...string) http.Handler {
	assets := maps.Clone(baseStaticAssets())
	instanceName := configuredInstanceName(instanceNames)
	name, _ := json.Marshal(instanceName)
	assets["static/manifest.webmanifest"] = dynamicStaticAsset(bytes.ReplaceAll(assets["static/manifest.webmanifest"].body, []byte(`"littleFedi"`), name), "application/manifest+json")
	assets["static/offline.html"] = dynamicStaticAsset(bytes.ReplaceAll(assets["static/offline.html"].body, []byte("littleFedi"), []byte(html.EscapeString(instanceName))), "text/html; charset=utf-8")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filePath := strings.TrimPrefix(r.URL.Path, "/static")
		filePath = path.Join("static", filePath)

		asset, ok := assets[filePath]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", asset.contentType)
		if filePath == "static/manifest.webmanifest" || filePath == "static/offline.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		}
		w.Header().Set("ETag", asset.etag)
		if filePath == "static/offline.html" {
			// The offline shell is a standalone static document with one embedded
			// style block and no script or remote resource.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		}
		if r.Header.Get("If-None-Match") == asset.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := asset.body
		if asset.gzipBody != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			body = asset.gzipBody
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// RobotsHandler serves robots.txt at the root, since crawlers only ever look
// for it there, optionally advertising generated sitemap indexes.
func RobotsHandler(sitemapURLs ...string) http.Handler {
	asset := baseStaticAssets()["static/robots.txt"]
	if asset != nil {
		body := append([]byte(nil), asset.body...)
		for _, sitemapURL := range sitemapURLs {
			if sitemapURL == "" {
				continue
			}
			if len(body) > 0 && body[len(body)-1] != '\n' {
				body = append(body, '\n')
			}
			body = append(body, "Sitemap: "+sitemapURL+"\n"...)
		}
		asset = dynamicStaticAsset(body, asset.contentType)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asset == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("ETag", asset.etag)
		if r.Header.Get("If-None-Match") == asset.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(asset.body)
	})
}

// ServiceWorkerHandler serves the service worker script at whatever path it's
// mounted on (the root, so its default scope covers the whole origin) with
// no-cache: unlike the immutable /static/* assets, the SW file must be
// revalidated on every registration check so deploys roll out promptly.
func ServiceWorkerHandler(instanceNames ...string) http.Handler {
	assets := maps.Clone(baseStaticAssets())
	instanceName := configuredInstanceName(instanceNames)
	body := bytes.ReplaceAll(assets["static/sw.js"].body, []byte(`'littleFedi'`), []byte(strconv.Quote(instanceName)))
	body = bytes.ReplaceAll(body, []byte("__APP_CSS_URL__"), []byte(staticAssetURL("app.css")))
	body = bytes.ReplaceAll(body, []byte("__APP_JS_URL__"), []byte(staticAssetURL("app.js")))
	body = bytes.ReplaceAll(body, []byte("__STREAMING_JS_URL__"), []byte(staticAssetURL("streaming.js")))
	body = bytes.ReplaceAll(body, []byte("__EMOJI_MAP_URL__"), []byte(staticAssetURL("emoji_map.js")))
	// The precache version covers every fingerprinted stylesheet, so editing a
	// theme invalidates the service worker's copy of it too. Both the URL list
	// and the version are derived from the registry, so a new theme is one
	// entry in themes and nothing here.
	version := strings.Trim(assets["static/app.css"].etag, `"`) + "-" + strings.Trim(assets["static/app.js"].etag, `"`) +
		"-" + strings.Trim(assets["static/streaming.js"].etag, `"`) + "-" + strings.Trim(assets["static/emoji_map.js"].etag, `"`)
	themeURLs := make([]string, 0, len(themes))
	for _, t := range Themes() {
		if t.Stylesheet == "" {
			continue
		}
		themeURLs = append(themeURLs, strconv.Quote(staticAssetURL(t.Stylesheet)))
		version += "-" + strings.Trim(assets["static/"+t.Stylesheet].etag, `"`)
	}
	body = bytes.ReplaceAll(body, []byte(`'__THEME_CSS_URLS__'`), []byte(strings.Join(themeURLs, ",")))
	body = bytes.ReplaceAll(body, []byte("__STATIC_VERSION__"), []byte(version))
	assets["static/sw.js"] = dynamicStaticAsset(body, "application/javascript; charset=utf-8")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset, ok := assets["static/sw.js"]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", asset.etag)
		if r.Header.Get("If-None-Match") == asset.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := asset.body
		if asset.gzipBody != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			body = asset.gzipBody
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func configuredInstanceName(names []string) string {
	if len(names) > 0 && strings.TrimSpace(names[0]) != "" {
		return strings.TrimSpace(names[0])
	}
	return "littleFedi"
}

func dynamicStaticAsset(body []byte, contentType string) *staticAsset {
	sum := sha256.Sum256(body)
	asset := &staticAsset{
		body:        body,
		etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		contentType: contentType,
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := gz.Write(body); err == nil && gz.Close() == nil && buf.Len() < len(body) {
		asset.gzipBody = append([]byte(nil), buf.Bytes()...)
	}
	return asset
}
