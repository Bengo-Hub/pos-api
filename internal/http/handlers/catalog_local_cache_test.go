package handlers

import (
	"testing"
	"time"
)

// TestLocalCatalogSource covers the in-process (per-pod) cache added in front of
// cachedCatalogSource's Redis round-trip — see catalogSourceLocalTTL's doc comment. Pure
// in-memory logic (no DB/Redis involved), so no live-dependency skip is needed.
func TestLocalCatalogSource(t *testing.T) {
	h := &CatalogHandler{}

	t.Run("miss on an empty cache returns nil", func(t *testing.T) {
		if got := h.localCatalogSource("missing-key"); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("a fresh write is returned by a subsequent read", func(t *testing.T) {
		src := &catalogSourceSet{Items: []inventoryProxyItem{{SKU: "A"}}}
		h.setLocalCatalogSource("k1", src)
		got := h.localCatalogSource("k1")
		if got != src {
			t.Fatalf("expected the exact cached pointer back, got %+v", got)
		}
	})

	t.Run("an expired entry is treated as a miss", func(t *testing.T) {
		src := &catalogSourceSet{Items: []inventoryProxyItem{{SKU: "B"}}}
		h.setLocalCatalogSource("k2", src)
		// White-box: backdate the entry's expiry instead of sleeping past the real TTL.
		h.localSrcMu.Lock()
		e := h.localSrc["k2"]
		e.expires = time.Now().Add(-time.Second)
		h.localSrc["k2"] = e
		h.localSrcMu.Unlock()

		if got := h.localCatalogSource("k2"); got != nil {
			t.Fatalf("expected nil for an expired entry, got %+v", got)
		}
	})

	t.Run("writes beyond the cap sweep out already-expired entries", func(t *testing.T) {
		hh := &CatalogHandler{}
		hh.localSrcMu.Lock()
		hh.localSrc = make(map[string]localCatalogEntry, localCatalogSourceCap+1)
		for i := 0; i < localCatalogSourceCap; i++ {
			hh.localSrc[time.Now().Format(time.RFC3339Nano)+string(rune(i))] = localCatalogEntry{
				src:     &catalogSourceSet{},
				expires: time.Now().Add(-time.Minute), // already expired
			}
		}
		sizeBefore := len(hh.localSrc)
		hh.localSrcMu.Unlock()
		if sizeBefore != localCatalogSourceCap {
			t.Fatalf("setup: expected %d entries, got %d", localCatalogSourceCap, sizeBefore)
		}

		hh.setLocalCatalogSource("fresh", &catalogSourceSet{Items: []inventoryProxyItem{{SKU: "C"}}})

		hh.localSrcMu.RLock()
		sizeAfter := len(hh.localSrc)
		hh.localSrcMu.RUnlock()
		if sizeAfter > sizeBefore {
			t.Fatalf("expected expired entries to be swept before growing past the cap: before=%d after=%d", sizeBefore, sizeAfter)
		}
		if got := hh.localCatalogSource("fresh"); got == nil {
			t.Fatal("expected the just-written fresh entry to survive the sweep")
		}
	})
}
