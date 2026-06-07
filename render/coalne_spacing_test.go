package render

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/erh/viam-chartplotter/mapdata/noaa"

	"go.viam.com/rdk/logging"
)

// TestRenderZ6Z7Comparison renders z=6/19/24 and z=7/38/48 to /tmp so the
// before-vs-after of the cellScaleRangeFor + phantom-jump fixes is easy to
// flip through in Preview. Diagnostic-only; keep around while iterating on
// overview-zoom rendering.
func TestRenderZ6Z7Comparison(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	cacheDir := envOr("DEP_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
	logger := logging.NewTestLogger(t)
	catalog, err := noaa.NewCatalog(cacheDir, logger)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store, err := noaa.NewStore(cacheDir, catalog, logger)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer := NewENCRenderer(catalog, store, logger)
	if err := catalog.EnsureFresh(context.Background()); err != nil {
		t.Logf("catalog refresh: %v (continuing)", err)
	}

	for _, tile := range []struct {
		z, x, y int
		out     string
	}{
		{6, 19, 24, "/tmp/t6-after.png"},
		{7, 38, 48, "/tmp/t7-after.png"},
	} {
		png, _, err := renderer.RenderTile(tile.z, tile.x, tile.y, RenderOptions{
			SafeDepthM: 1.8288,
			Style:      StyleECDIS,
		})
		if err != nil {
			t.Fatalf("render z=%d: %v", tile.z, err)
		}
		if err := os.WriteFile(tile.out, png, 0o644); err != nil {
			t.Fatalf("write %s: %v", tile.out, err)
		}
		t.Logf("wrote %s (%d bytes)", tile.out, len(png))
	}
}
