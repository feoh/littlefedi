package objectstore

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Local struct{ root, baseURL, keyPrefix string }

func NewLocal(root, baseURL string) *Local {
	p := ""
	if strings.TrimRight(baseURL, "/") == "/media" {
		p = "media/"
	}
	if strings.TrimRight(baseURL, "/") == "/media/cache" {
		p = "cache/"
	}
	return &Local{root: root, baseURL: strings.TrimRight(baseURL, "/") + "/", keyPrefix: p}
}
func (l *Local) Backend() string { return "local" }
func (l *Local) ID() string      { abs, _ := filepath.Abs(l.root); return "local:" + filepath.Clean(abs) }
func (l *Local) filename(key string) (string, error) {
	k, err := CleanKey(key)
	if err != nil {
		return "", err
	}
	if k == strings.TrimSuffix(l.keyPrefix, "/") {
		k = ""
	} else {
		k = strings.TrimPrefix(k, l.keyPrefix)
	}
	return filepath.Join(l.root, filepath.FromSlash(k)), nil
}
func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64, _ PutOptions) (Object, error) {
	name, err := l.filename(key)
	if err != nil {
		return Object{}, err
	}
	if err = os.MkdirAll(filepath.Dir(name), 0750); err != nil {
		return Object{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".upload-*")
	if err != nil {
		return Object{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	n, copyErr := io.Copy(tmp, &contextReader{ctx: ctx, r: r})
	closeErr := tmp.Close()
	if copyErr != nil {
		return Object{}, copyErr
	}
	if closeErr != nil {
		return Object{}, closeErr
	}
	if size >= 0 && n != size {
		return Object{}, fmt.Errorf("short media write: %d != %d", n, size)
	}
	if err = os.Rename(tmpName, name); err != nil {
		return Object{}, err
	}
	return l.Stat(ctx, key)
}
func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, Object, error) {
	name, err := l.filename(key)
	if err != nil {
		return nil, Object{}, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, Object{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, Object{}, err
	}
	return f, Object{Key: key, Size: info.Size(), ModTime: info.ModTime()}, nil
}
func (l *Local) Stat(_ context.Context, key string) (Object, error) {
	name, err := l.filename(key)
	if err != nil {
		return Object{}, err
	}
	i, err := os.Stat(name)
	if err != nil {
		return Object{}, err
	}
	return Object{Key: key, Size: i.Size(), ModTime: i.ModTime()}, nil
}
func (l *Local) Delete(_ context.Context, key string) error {
	name, err := l.filename(key)
	if err != nil {
		return err
	}
	err = os.Remove(name)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (l *Local) List(_ context.Context, prefix string, fn func(Object) error) error {
	root, err := l.filename(prefix)
	if err != nil {
		return err
	}
	return filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if i.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(l.root, p)
		key := filepath.ToSlash(rel)
		if l.keyPrefix == "cache/" {
			key = l.keyPrefix + key
		} else if l.keyPrefix == "media/" && !strings.HasPrefix(key, "exports/") && !strings.HasPrefix(key, "imports/") && !strings.HasPrefix(key, "cache/") {
			key = l.keyPrefix + key
		}
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		return fn(Object{Key: key, Size: i.Size(), ModTime: i.ModTime()})
	})
}
func (l *Local) URL(key string) string {
	k, err := CleanKey(key)
	if err != nil || privateKey(k) {
		return ""
	}
	k = strings.TrimPrefix(k, l.keyPrefix)
	return l.baseURL + strings.ReplaceAll(url.PathEscape(k), "%2F", "/")
}
func (l *Local) Check(_ context.Context) error {
	if l.root == "" {
		return fmt.Errorf("local media path is empty")
	}
	return os.MkdirAll(l.root, 0750)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}
