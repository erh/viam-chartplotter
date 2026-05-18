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
		inside := c.MinLon <= minLon && c.MaxLon >= maxLon &&
			c.MinLat <= minLat && c.MaxLat >= maxLat
		t.Logf("  %-12s scale=1:%-7d bbox=lon[%.4f..%.4f] lat[%.4f..%.4f]  fully-covers-tile=%v",
			c.Name, c.CScale, c.MinLon, c.MaxLon, c.MinLat, c.MaxLat, inside)
	}

	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}
	for _, cell := range cells {
		chart, err := renderer.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		var depares []string
		nLND := 0
		nBUA := 0
		for _, f := range chart.FeaturesInBounds(bbox) {
			switch f.ObjectClass() {
			case "DEPARE":
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
			case "LNDARE":
				nLND++
				coords := f.Geometry().Coordinates
				if len(coords) == 0 {
					t.Logf("    LNDARE in %s: <no coords>", cell.Name)
					break
				}
				minLn, maxLn := coords[0][0], coords[0][0]
				minLt, maxLt := coords[0][1], coords[0][1]
				for _, p := range coords {
					if p[0] < minLn {
						minLn = p[0]
					}
					if p[0] > maxLn {
						maxLn = p[0]
					}
					if p[1] < minLt {
						minLt = p[1]
					}
					if p[1] > maxLt {
						maxLt = p[1]
					}
				}
				t.Logf("    LNDARE in %s: %d pts bbox lon[%.4f..%.4f] lat[%.4f..%.4f]",
					cell.Name, len(coords), minLn, maxLn, minLt, maxLt)
			case "BUAARE":
				nBUA++
			}
		}
		if len(depares) == 0 && nLND == 0 && nBUA == 0 {
			continue
		}
		t.Logf("cell %s (1:%d) — %d DEPARE, %d LNDARE, %d BUAARE polygons:", cell.Name, cell.CScale, len(depares), nLND, nBUA)
		for _, line := range depares {
			t.Log(line)
		}
	}
}
