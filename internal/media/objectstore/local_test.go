package objectstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
)

func TestCleanKey(t *testing.T) {
	good := map[string]string{"a/b.jpg": "a/b.jpg", "/a//b.jpg": "a/b.jpg", `a\b.jpg`: "a/b.jpg", "a/./b/../c.jpg": "a/c.jpg"}
	for in, want := range good {
		if got, err := CleanKey(in); err != nil || got != want {
			t.Errorf("CleanKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "../etc/passwd", "a/../../b"} {
		if _, err := CleanKey(bad); err == nil {
			t.Errorf("CleanKey(%q) accepted a key outside the store", bad)
		}
	}
}

func TestLocalPutOpenStatListDelete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := NewLocal(root, "/media/")
	if err := store.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	obj, err := store.Put(ctx, "media/accounts/1/avatar.png", strings.NewReader("png-bytes"), 9, PutOptions{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if obj.Size != 9 {
		t.Errorf("Put size = %d", obj.Size)
	}
	if _, err := os.Stat(filepath.Join(root, "accounts", "1", "avatar.png")); err != nil {
		t.Errorf("file not stored under the root without the media/ prefix: %v", err)
	}

	r, meta, err := store.Open(ctx, "media/accounts/1/avatar.png")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if string(body) != "png-bytes" || meta.Size != 9 {
		t.Errorf("Open = %q, size %d", body, meta.Size)
	}

	if got := store.URL("media/accounts/1/avatar.png"); got != "/media/accounts/1/avatar.png" {
		t.Errorf("URL = %q", got)
	}
	if got := store.URL("exports/archive.zip"); got != "" {
		t.Errorf("private export got a public URL: %q", got)
	}

	var keys []string
	if err := store.List(ctx, "media/", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "media/accounts/1/avatar.png" {
		t.Errorf("List = %v", keys)
	}

	if err := store.Delete(ctx, "media/accounts/1/avatar.png"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Stat(ctx, "media/accounts/1/avatar.png"); err == nil {
		t.Error("object still present after Delete")
	}
	if err := store.Delete(ctx, "media/accounts/1/avatar.png"); err != nil {
		t.Errorf("deleting a missing object should succeed: %v", err)
	}
}

func TestLocalPutRejectsShortWrite(t *testing.T) {
	store := NewLocal(t.TempDir(), "/media/")
	if _, err := store.Put(context.Background(), "media/x", strings.NewReader("abc"), 10, PutOptions{}); err == nil {
		t.Error("Put accepted fewer bytes than declared")
	}
}

func TestCopyBetweenStores(t *testing.T) {
	ctx := context.Background()
	src := NewLocal(t.TempDir(), "/media/")
	dst := NewLocal(t.TempDir(), "/media/")
	if _, err := src.Put(ctx, "media/a.txt", strings.NewReader("hello"), 5, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst, "media/a.txt", PutOptions{}); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if obj, err := dst.Stat(ctx, "media/a.txt"); err != nil || obj.Size != 5 {
		t.Errorf("copied object = %+v, %v", obj, err)
	}
}

func TestNewLocalFromConfig(t *testing.T) {
	store, err := New(config.Media{Backend: "local", Path: t.TempDir()}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.Backend() != "local" || !strings.HasPrefix(store.ID(), "local:") {
		t.Errorf("Backend/ID = %q/%q", store.Backend(), store.ID())
	}
}
