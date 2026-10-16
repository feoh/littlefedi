package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
)

// ObjectCache uses the existing hardened downloader/processor as a staging
// pipeline and commits its output to an object store. Cache keys remain
// content-addressed and separate from owned media.
type ObjectCache struct {
	staging string
	local   *Cache
	store   objectstore.Store
}

func NewObjectCache(store objectstore.Store, maxBytes int64, stripEXIF bool) (*ObjectCache, error) {
	d, e := os.MkdirTemp("", "littlefedi-cache-*")
	if e != nil {
		return nil, e
	}
	return &ObjectCache{staging: d, local: NewCache(d, maxBytes, stripEXIF), store: store}, nil
}
func (c *ObjectCache) Store(ctx context.Context, remote string) (*StoreResult, error) {
	dir, _ := c.local.cacheDir(remote)
	defer os.RemoveAll(dir)
	r, e := c.local.Store(ctx, remote)
	if e != nil {
		return nil, e
	}
	put := func(u, mime string) (string, string, error) {
		rel := strings.TrimPrefix(u, cacheURLPrefix)
		key := "cache/" + rel
		name := filepath.Join(c.staging, filepath.FromSlash(rel))
		f, e := os.Open(name)
		if e != nil {
			return "", "", e
		}
		defer f.Close()
		i, _ := f.Stat()
		_, e = c.store.Put(ctx, key, f, i.Size(), objectstore.PutOptions{ContentType: mime, CacheControl: "public, max-age=31536000, immutable"})
		return c.store.URL(key), key, e
	}
	url, key, e := put(r.URL, r.MIMEType)
	if e != nil {
		return nil, e
	}
	preview, pk := "", ""
	if r.PreviewURL == r.URL {
		preview, pk = url, key
	} else if r.PreviewURL != "" {
		preview, pk, e = put(r.PreviewURL, previewMIME(r.PreviewURL, r.MIMEType))
		if e != nil {
			return nil, e
		}
	}
	r.URL = url
	r.PreviewURL = preview
	r.StorageBackend = c.store.ID()
	r.StorageKey = key
	r.PreviewStorageKey = pk
	return r, nil
}
