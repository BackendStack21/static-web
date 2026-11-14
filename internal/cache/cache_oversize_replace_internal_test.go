package cache

import "testing"

// A replacement larger than the whole cache is not stored. Put returned
// before touching the cache, so the previous entry for the key stayed and
// kept being served although the file had changed. It must be removed.
func TestCachePut_OversizeReplacementRemovesStaleEntry(t *testing.T) {
	c := NewCache(10000)
	c.Put("/a", sized(4000))
	c.Put("/b", sized(1000))
	checkAccounting(t, c, "before")

	c.Put("/a", sized(20000)) // larger than maxBytes
	if f, ok := c.Get("/a"); ok {
		t.Errorf("Get(/a) returned the stale %d-byte entry after an oversize replacement", len(f.Data))
	}
	checkAccounting(t, c, "after oversize replace")
	if _, ok := c.Get("/b"); !ok {
		t.Error("an unrelated entry was dropped")
	}
	if got, want := c.Stats().EntryCount, 1; got != want {
		t.Errorf("EntryCount = %d, want %d", got, want)
	}

	// An oversize Put for a key that is not cached changes nothing.
	c.Put("/c", sized(20000))
	checkAccounting(t, c, "oversize put of a new key")
	if got, want := c.Stats().EntryCount, 1; got != want {
		t.Errorf("EntryCount = %d, want %d", got, want)
	}
}
