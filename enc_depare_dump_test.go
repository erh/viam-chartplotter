package vc

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"

	"github.com/beetlebugorg/s57/pkg/s57"
	"go.viam.com/rdk/logging"
)

// TestDumpDEPAREForTile dumps every DEPARE polygon overlapping the given tile,
// grouped by cell, so we can see what DRVAL1/DRVAL2 bands the renderer is
// actually keying on.
//
//	go test -count=1 -run TestDumpDEPAREForTile -v ./...
//
// Tunable: DEP_Z DEP_X DEP_Y (defaults to Charleston Harbor z=14).
func TestDumpDEPAREForTile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping debug tile dump in short mode")
	}
	z := envOrInt(t, "DEP_Z", 14)
	x := envOrInt(t, "DEP_X", 4556)
	y := envOrInt(t, "DEP_Y", 6611)

	cacheDir := envOr("DEP_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
	logger := logging.NewTestLogger(t)
	catalog, err := NewENCCatalog(cacheDir, logger)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store, err := NewENCStore(cacheDir, catalog, logger)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer := NewENCRenderer(catalog, store, logger)
	if err := catalog.EnsureFresh(context.Background()); err != nil {
		t.Logf("catalog refresh: %v (continuing)", err)
	}

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)
	t.Logf("tile z=%d x=%d y=%d bbox=[lon %.4f..%.4f, lat %.4f..%.4f]",
		z, x, y, minLon, maxLon, minLat, maxLat)

	cells := catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale < cells[j].CScale })
	t.Logf("%d overlapping cells (finest first):", len(cells))
	for _, c := range cells {
		t.Logf("  %-12s scale=1:%d", c.Name, c.CScale)
	}

	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}
	for _, cell := range cells {
		chart, err := renderer.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		var depares []string
		for _, f := range chart.FeaturesInBounds(bbox) {
			if f.ObjectClass() != "DEPARE" {
				continue
			}
			minD := math.NaN()
			maxD := math.NaN()
			if v, ok := f.Attribute("DRVAL1"); ok {
				minD = numAttr(v)
			}
			if v, ok := f.Attribute("DRVAL2"); ok {
				maxD = numAttr(v)
			}
			depares = append(depares, fmt.Sprintf("    DRVAL1=%5.2fm DRVAL2=%6.2fm  (%5.1fft - %6.1fft)",
				minD, maxD, minD*feetPerMetre, maxD*feetPerMetre))
		}
		if len(depares) == 0 {
			continue
		}
		t.Logf("cell %s (1:%d) — %d DEPARE polygons:", cell.Name, cell.CScale, len(depares))
		for _, line := range depares {
			t.Log(line)
		}
	}
}
