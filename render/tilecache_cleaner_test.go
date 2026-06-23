package render

import (
	"os"
	"testing"
	"time"
)

func TestTileCacheCleanOlderThan(t *testing.T) {
	c, err := NewENCTileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Two tiles: one fresh, one aged well past the TTL.
	if err := c.Put("v10-wms", 6, 14, 100, 200, []byte("fresh-png")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("v9-wms", 6, 14, 100, 200, []byte("stale-png-bytes")); err != nil {
		t.Fatal(err)
	}
	stale := c.Path("v9-wms", 6, 14, 100, 200)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	removed, freed := c.cleanOlderThan(24 * time.Hour)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if freed != int64(len("stale-png-bytes")) {
		t.Fatalf("freed = %d, want %d", freed, len("stale-png-bytes"))
	}
	// Fresh tile survives; stale tile (and its now-empty version dir) are gone.
	if _, ok := c.Get("v10-wms", 6, 14, 100, 200); !ok {
		t.Fatal("fresh tile was deleted")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tile still present: %v", err)
	}
}
