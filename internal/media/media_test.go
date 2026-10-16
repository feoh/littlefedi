package media_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffUpload(t *testing.T) {
	data := testPNG(t, 4, 4)
	mime, r, err := media.SniffUpload(bytes.NewReader(data), "application/octet-stream")
	if err != nil || mime != "image/png" {
		t.Fatalf("SniffUpload = %q, %v", mime, err)
	}
	if replay, _ := io.ReadAll(r); !bytes.Equal(replay, data) {
		t.Error("the returned reader does not replay the whole upload")
	}
	if _, _, err := media.SniffUpload(strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"); err == nil {
		t.Error("an SVG upload was accepted")
	}
	if _, _, err := media.SniffUpload(strings.NewReader(""), "image/png"); err == nil {
		t.Error("an empty upload was accepted")
	}
}

func TestStoreUploadProcessesImage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := objectstore.NewLocal(root, "/media/")
	data := testPNG(t, 800, 400)
	m := &models.MediaAttachment{ID: "01MEDIA", MIMEType: "image/png"}

	if err := media.StoreUpload(ctx, store, bytes.NewReader(data), int64(len(data)), m, "My Holiday.png", true); err != nil {
		t.Fatalf("StoreUpload: %v", err)
	}
	if m.MIMEType != "image/png" || !strings.HasPrefix(m.URL, "/media/01MEDIA/") || m.FileSize == 0 {
		t.Errorf("attachment = %+v", m)
	}
	if w, h := m.Dimensions(); w != 800 || h != 400 {
		t.Errorf("Dimensions = %dx%d, want 800x400", w, h)
	}
	if m.Blurhash == "" {
		t.Error("no blurhash computed")
	}
	if colour, ok := media.AverageColour(m.Blurhash); !ok || !strings.HasPrefix(colour, "#") {
		t.Errorf("AverageColour = %q, %v", colour, ok)
	}
	if m.PreviewURL == "" || m.PreviewURL == m.URL {
		t.Errorf("no separate thumbnail stored: preview %q, original %q", m.PreviewURL, m.URL)
	}
	if _, err := store.Stat(ctx, m.StorageKey); err != nil {
		t.Errorf("original not in the object store: %v", err)
	}
	if _, err := store.Stat(ctx, m.PreviewStorageKey); err != nil {
		t.Errorf("thumbnail not in the object store: %v", err)
	}
}

func TestMediaFilename(t *testing.T) {
	if got := media.MediaFilename("My Holiday (1).png", "image/png", false); got != "My-Holiday--1-.png" {
		t.Errorf("kept name = %q", got)
	}
	if got := media.MediaFilename("secret-name.png", "image/png", true); got != "image.png" {
		t.Errorf("renamed = %q", got)
	}
	if got := media.MediaFilename("../../etc/passwd", "", false); strings.Contains(got, "/") {
		t.Errorf("path survived: %q", got)
	}
}

func TestOwnedLocalPath(t *testing.T) {
	if got, ok := media.OwnedLocalPath("/srv/media", "/media/01/image.png"); !ok || got != filepath.Join("/srv/media", "01", "image.png") {
		t.Errorf("OwnedLocalPath = %q, %v", got, ok)
	}
	for _, bad := range []string{"/media/../etc/passwd", "https://remote.example/media/x", "/static/x"} {
		if _, ok := media.OwnedLocalPath("/srv/media", bad); ok {
			t.Errorf("OwnedLocalPath(%q) accepted", bad)
		}
	}
	if !media.IsCacheURL("/media/cache/ab/x.png") || media.IsCacheURL("/media/01/x.png") {
		t.Error("IsCacheURL misclassifies")
	}
}

func TestProxyURLSigning(t *testing.T) {
	p := media.NewProxy(true, "proxy-secret", "test.example")
	if got := p.URL("https://test.example/media/x.png"); got != "https://test.example/media/x.png" {
		t.Errorf("local media was proxied: %q", got)
	}
	proxied := p.URL("https://remote.example/files/cat.png")
	u, err := url.Parse(proxied)
	if err != nil || u.Host != "test.example" || u.Query().Get("url") != "https://remote.example/files/cat.png" || u.Query().Get("sig") == "" {
		t.Fatalf("proxied URL = %q", proxied)
	}

	tampered := *u
	q := tampered.Query()
	q.Set("url", "https://evil.example/x.png")
	tampered.RawQuery = q.Encode()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tampered.RequestURI(), nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("tampered proxy request = %d, want 403", rec.Code)
	}

	if disabled := media.NewProxy(false, "s", "test.example"); disabled.URL("https://remote.example/x.png") != "https://remote.example/x.png" {
		t.Error("a disabled proxy rewrote a URL")
	}
}
