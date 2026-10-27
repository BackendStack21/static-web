package cache

import (
	"testing"
	"time"
)

// B14: removing an expired entry must not evict a fresher entry stored under
// the same key in the meantime (compare-and-remove).
func TestRemoveIfSame_KeepsFreshEntry(t *testing.T) {
	c := NewCache(1<<20, time.Hour)
	old := &CachedFile{Data: make([]byte, 100)}
	c.Put("/k", old)
	got, ok := c.Get("/k")
	if !ok || got != old {
		t.Fatal("setup: old entry not found")
	}

	fresh := &CachedFile{Data: make([]byte, 300)}
	c.Put("/k", fresh)

	// A stale reader that observed `old` as expired now tries to remove it.
	c.removeIfSame("/k", old)

	cur, ok := c.Get("/k")
	if !ok || cur != fresh {
		t.Fatal("fresh entry was evicted by removal of the stale one")
	}
	if want := fresh.totalSize(); c.Stats().CurrentBytes != want {
		t.Errorf("CurrentBytes = %d, want %d", c.Stats().CurrentBytes, want)
	}
}

func TestRemoveIfSame_RemovesSameEntry(t *testing.T) {
	c := NewCache(1<<20, time.Hour)
	f := &CachedFile{Data: make([]byte, 100)}
	c.Put("/k", f)
	c.removeIfSame("/k", f)
	if _, ok := c.Get("/k"); ok {
		t.Error("entry should have been removed")
	}
	if got := c.Stats().CurrentBytes; got != 0 {
		t.Errorf("CurrentBytes = %d, want 0", got)
	}
}
