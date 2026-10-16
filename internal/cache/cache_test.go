package cache

import (
	"testing"
	"time"
)

func TestSetGetAndStats(t *testing.T) {
	c := New(1024)
	c.Set("a", "alpha", 10, 0)
	v, ok, result := c.GetWithResult("a")
	if !ok || v != "alpha" || result != LookupHit {
		t.Fatalf("Get(a) = %v, %v, %v", v, ok, result)
	}
	if _, ok, result := c.GetWithResult("missing"); ok || result != LookupMissing {
		t.Errorf("Get(missing) = %v, %v", ok, result)
	}
	stats := c.Stats()
	if stats.Hits != 1 || stats.Misses != 1 || stats.Entries != 1 || stats.Bytes != 10 {
		t.Errorf("Stats = %+v", stats)
	}
}

func TestExpiry(t *testing.T) {
	c := New(1024)
	c.Set("a", 1, 1, time.Nanosecond)
	time.Sleep(time.Millisecond)
	if _, ok, result := c.GetWithResult("a"); ok || result != LookupExpired {
		t.Errorf("expired entry = %v, %v", ok, result)
	}
	if c.Stats().Expirations != 1 {
		t.Error("expiration not counted")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(20)
	c.Set("a", 1, 10, 0)
	c.Set("b", 2, 10, 0)
	c.Get("a") // a becomes most recently used
	c.Set("c", 3, 10, 0)
	if _, ok := c.Get("b"); ok {
		t.Error("least recently used entry b survived")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("recently used entry a was evicted")
	}
	if c.Stats().Evictions != 1 {
		t.Errorf("evictions = %d", c.Stats().Evictions)
	}
}

func TestOversizedAndDisabled(t *testing.T) {
	c := New(5)
	c.Set("big", 1, 6, 0)
	if _, ok := c.Get("big"); ok {
		t.Error("an entry larger than the cache was stored")
	}
	var nilCache *MemoryCache
	nilCache.Set("a", 1, 1, 0)
	if _, _, result := nilCache.GetWithResult("a"); result != LookupDisabled {
		t.Errorf("nil cache lookup = %v", result)
	}
}

func TestDeleteAndPrefix(t *testing.T) {
	c := New(1024)
	c.Set("timeline:1", 1, 1, 0)
	c.Set("timeline:2", 2, 1, 0)
	c.Set("account:1", 3, 1, 0)
	c.DeletePrefix("timeline:")
	c.Delete("nothing")
	if c.Stats().Entries != 1 {
		t.Errorf("entries after DeletePrefix = %d", c.Stats().Entries)
	}
	c.Delete("account:1")
	if c.Stats().Entries != 0 {
		t.Error("Delete did not remove the entry")
	}
}

func TestDependencies(t *testing.T) {
	c := New(1024)
	c.SetWithDependencies("home:alice", "feed", 1, 0, []string{"status:42"})
	c.SetWithDependencies("home:bob", "feed", 1, 0, []string{"status:7"})
	if n := c.DeleteDependencies("status:42"); n != 1 {
		t.Errorf("DeleteDependencies removed %d entries, want 1", n)
	}
	if _, ok := c.Get("home:alice"); ok {
		t.Error("dependent entry survived")
	}
	if _, ok := c.Get("home:bob"); !ok {
		t.Error("unrelated entry removed")
	}
}

func TestSetIfVersionIsFencedByInvalidation(t *testing.T) {
	c := New(1024)
	version := c.Version()
	c.DeleteDependencies("status:1")
	if c.SetIfVersionWithDependencies(version, "k", 1, 1, 0, []string{"status:1"}) {
		t.Error("a value read before the invalidation was stored")
	}
	if !c.SetIfVersionWithDependencies(c.Version(), "k", 1, 1, 0, []string{"status:1"}) {
		t.Error("a fresh value was rejected")
	}
	version = c.Version()
	c.Clear()
	if c.SetIfVersion(version, "k2", 1, 1, 0) {
		t.Error("a value read before Clear was stored")
	}
}
