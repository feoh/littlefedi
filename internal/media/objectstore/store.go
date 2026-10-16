package objectstore

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
)

type Object struct {
	Key, ContentType, ETag string
	Size                   int64
	ModTime                time.Time
}

type PutOptions struct{ ContentType, CacheControl string }

// Store is small so local-only builds carry no cloud dependencies.
type Store interface {
	Backend() string
	ID() string
	Put(context.Context, string, io.Reader, int64, PutOptions) (Object, error)
	Open(context.Context, string) (io.ReadCloser, Object, error)
	Stat(context.Context, string) (Object, error)
	Delete(context.Context, string) error
	List(context.Context, string, func(Object) error) error
	URL(string) string
	Check(context.Context) error
}

func CleanKey(key string) (string, error) {
	key = strings.TrimPrefix(strings.ReplaceAll(key, "\\", "/"), "/")
	clean := path.Clean(key)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	return clean, nil
}

func privateKey(key string) bool {
	first, _, _ := strings.Cut(key, "/")
	return first == "exports" || first == "imports"
}

func New(cfg config.Media, cache bool) (Store, error) {
	backend := cfg.OwnedBackend()
	if cache {
		backend = cfg.ResolvedCacheBackend()
	}
	if backend == "local" {
		root := cfg.Path
		base := "/media/"
		if cache {
			root = cfg.ResolvedCachePath()
			base = "/media/cache/"
		}
		return NewLocal(root, base), nil
	}
	return newS3(cfg, cache)
}

func Copy(ctx context.Context, src, dst Store, key string, opts PutOptions) (Object, error) {
	r, meta, err := src.Open(ctx, key)
	if err != nil {
		return Object{}, err
	}
	defer r.Close()
	if opts.ContentType == "" {
		opts.ContentType = meta.ContentType
	}
	got, err := dst.Put(ctx, key, r, meta.Size, opts)
	if err != nil {
		return Object{}, err
	}
	if meta.Size >= 0 && got.Size != meta.Size {
		_ = dst.Delete(ctx, key)
		return Object{}, fmt.Errorf("copy verification failed for %s: size %d != %d", key, got.Size, meta.Size)
	}
	return got, nil
}
