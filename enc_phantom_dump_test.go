package vc

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"

	"github.com/erh/viam-chartplotter/mapdata/noaa"

	"github.com/beetlebugorg/s57/pkg/s57"
	"go.viam.com/rdk/logging"
)

// TestDumpPhantomJumpsForArea scans a lon/lat box (not a single tile) for
// any Polygon/LineString with phantom-edge jumps, reporting the worst
// offenders. Use this when the visible bug spans multiple tiles — pass the
// approximate bbox of the on-screen view via PA_MINLON / PA_MAXLON / PA_MINLAT
// / PA_MAXLAT.
//
//	go test -count=1 -run TestDumpPhantomJumpsForArea -v ./...
func TestDumpPhantomJumpsForArea(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping phantom-jump area dump in short mode")
	}
	minLon := envOrFloat(t, "PA_MINLON", -76.78)
	maxLon := envOrFloat(t, "PA_MAXLON", -76.60)
	minLat := envOrFloat(t, "PA_MINLAT", 34.66)
	maxLat := envOrFloat(t, "PA_MAXLAT", 34.75)
	cacheDir := envOr("PA_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
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

	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}
	cells := catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale < cells[j].CScale })
	t.Logf("scanning %d cells over bbox lon[%.4f..%.4f] lat[%.4f..%.4f]",
		len(cells), minLon, maxLon, minLat, maxLat)

	areaWidthM := (maxLon - minLon) * 111_320.0 * math.Cos(((minLat+maxLat)/2)*math.Pi/180)
	areaHeightM := (maxLat - minLat) * 111_320.0
	t.Logf("area extent ~ %.0fm x %.0fm", areaWidthM, areaHeightM)

	// Threshold: anything with an edge bigger than 10% of the visible
	// extent is the kind of phantom we want to call out.
	threshold := 0.1 * math.Max(areaWidthM, areaHeightM)
	t.Logf("flagging features with any edge > %.0f m", threshold)

	type hit struct {
		class  string
		id     int64
		cell   string
		geom   string
		npts   int
		worst  float64
		bboxW  float64
		bboxH  float64
		coords [][]float64
	}
	var hits []hit

	for _, cell := range cells {
		chart, err := renderer.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypeLineString && geom.Type != s57.GeometryTypePolygon {
				continue
			}
			coords := geom.Coordinates
			if len(coords) < 2 {
				continue
			}
			worst := 0.0
			minLn, maxLn := coords[0][0], coords[0][0]
			minLt, maxLt := coords[0][1], coords[0][1]
			for i := 1; i < len(coords); i++ {
				if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
					continue
				}
				if coords[i][0] < minLn {
					minLn = coords[i][0]
				}
				if coords[i][0] > maxLn {
					maxLn = coords[i][0]
				}
				if coords[i][1] < minLt {
					minLt = coords[i][1]
				}
				if coords[i][1] > maxLt {
					maxLt = coords[i][1]
				}
				midLat := (coords[i][1] + coords[i-1][1]) / 2
				mPerDegLat := 111_320.0
				mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
				dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
				dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
				d := math.Sqrt(dlon*dlon + dlat*dlat)
				if d > worst {
					worst = d
				}
			}
			if worst < threshold {
				continue
			}
			// Skip the obvious continent-scale overview rings we already
			// filter at render time.
			bboxW := (maxLn - minLn) * 111_320.0 * math.Cos(((minLt+maxLt)/2)*math.Pi/180)
			bboxH := (maxLt - minLt) * 111_320.0
			if bboxW > 50*areaWidthM || bboxH > 50*areaHeightM {
				continue
			}
			hits = append(hits, hit{
				class:  f.ObjectClass(),
				id:     f.ID(),
				cell:   cell.Name,
				geom:   geom.Type.String(),
				npts:   len(coords),
				worst:  worst,
				bboxW:  bboxW,
				bboxH:  bboxH,
				coords: coords,
			})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].worst > hits[j].worst })
	t.Logf("=== %d phantom-edge features (excluding overview-scale outliers) ===", len(hits))
	for _, h := range hits {
		t.Logf("  %s id=%d cell=%s %s npts=%d worst=%.0fm bbox=%.0fm x %.0fm",
			h.class, h.id, h.cell, h.geom, h.npts, h.worst, h.bboxW, h.bboxH)
		// Print only first/last vertices for compactness; full coords
		// would explode the log for high-vertex features.
		for i, c := range h.coords {
			if i >= 6 && i < len(h.coords)-3 {
				if i == 6 {
					t.Logf("    ... %d more vertices ...", len(h.coords)-9)
				}
				continue
			}
			t.Logf("    [%3d] %.6f, %.6f", i, c[0], c[1])
		}
	}
}

// TestDumpPhantomJumpsForTile scans every Polygon and LineString feature
// overlapping the given tile and reports any class where consecutive
// vertices jump more than the structurePhantomJumpM threshold. This is the
// "who else has the multi-edge concatenation bug" sweep — once we know the
// classes, we can extend the splitPathOnLongJumps gate to cover them.
//
//	go test -count=1 -run TestDumpPhantomJumpsForTile -v ./...
//
// Tunable: PH_Z PH_X PH_Y (defaults to Beaufort tile).
func TestDumpPhantomJumpsForTile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping phantom-jump dump in short mode")
	}
	z := envOrInt(t, "PH_Z", 14)
	x := envOrInt(t, "PH_X", 4702)
	y := envOrInt(t, "PH_Y", 6505)

	cacheDir := envOr("PH_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
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

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)
	t.Logf("tile z=%d x=%d y=%d bbox=[lon %.6f..%.6f, lat %.6f..%.6f]",
		z, x, y, minLon, maxLon, minLat, maxLat)

	cells := catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale < cells[j].CScale })

	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}

	// Per-class summary: class -> count of features with at least one
	// jump > threshold, max jump distance seen.
	type classStat struct {
		nFeat     int
		nBad      int
		maxJump   float64
		exampleID int64
	}
	stats := map[string]*classStat{}

	for _, cell := range cells {
		chart, err := renderer.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypeLineString && geom.Type != s57.GeometryTypePolygon {
				continue
			}
			coords := geom.Coordinates
			if len(coords) < 2 {
				continue
			}
			class := f.ObjectClass()
			st, ok := stats[class]
			if !ok {
				st = &classStat{}
				stats[class] = st
			}
			st.nFeat++
			// Walk vertex pairs in metres.
			worst := 0.0
			for i := 1; i < len(coords); i++ {
				if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
					continue
				}
				midLat := (coords[i][1] + coords[i-1][1]) / 2
				mPerDegLat := 111_320.0
				mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
				dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
				dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
				d := math.Sqrt(dlon*dlon + dlat*dlat)
				if d > worst {
					worst = d
				}
			}
			if worst > structurePhantomJumpM {
				st.nBad++
				if worst > st.maxJump {
					st.maxJump = worst
					st.exampleID = f.ID()
				}
			}
		}
	}

	t.Logf("classes overlapping the tile and their phantom-jump counts:")
	type row struct {
		class string
		st    *classStat
	}
	rows := make([]row, 0, len(stats))
	for c, s := range stats {
		rows = append(rows, row{c, s})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].st.maxJump > rows[j].st.maxJump })
	for _, r := range rows {
		marker := ""
		if r.st.nBad > 0 {
			marker = " ***"
		}
		t.Logf("  %-8s feats=%-4d phantom=%-3d  worst_jump=%8.1f m  example_id=%d%s",
			r.class, r.st.nFeat, r.st.nBad, r.st.maxJump, r.st.exampleID, marker)
	}

	// Second pass: dump full coord lists for small-bbox phantom features in
	// classes that paint visibly on the tile (the ones in the screenshot).
	// We skip classes already culled by isOversizedPolygon at render time.
	tileExtDeg := math.Max(maxLon-minLon, maxLat-minLat)
	classDump := map[string]bool{
		"BRIDGE": true, "BUAARE": true, "BUISGL": true,
		"CBLOHD": true, "PIPOHD": true, "CONVYR": true,
		"DRGARE": true, "FAIRWY": true, "SLCONS": true,
		"COALNE": true,
	}
	t.Logf("--- detailed coords for small-bbox phantom features in visible classes ---")
	for _, cell := range cells {
		chart, err := renderer.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			class := f.ObjectClass()
			if !classDump[class] {
				continue
			}
			coords := f.Geometry().Coordinates
			if len(coords) < 2 {
				continue
			}
			// bbox of the feature
			minLn, maxLn := coords[0][0], coords[0][0]
			minLt, maxLt := coords[0][1], coords[0][1]
			worst := 0.0
			for i := 1; i < len(coords); i++ {
				if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
					continue
				}
				if coords[i][0] < minLn {
					minLn = coords[i][0]
				}
				if coords[i][0] > maxLn {
					maxLn = coords[i][0]
				}
				if coords[i][1] < minLt {
					minLt = coords[i][1]
				}
				if coords[i][1] > maxLt {
					maxLt = coords[i][1]
				}
				midLat := (coords[i][1] + coords[i-1][1]) / 2
				mPerDegLat := 111_320.0
				mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
				dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
				dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
				d := math.Sqrt(dlon*dlon + dlat*dlat)
				if d > worst {
					worst = d
				}
			}
			if worst <= structurePhantomJumpM {
				continue
			}
			featExt := math.Max(maxLn-minLn, maxLt-minLt)
			ratio := featExt / tileExtDeg
			t.Logf("  %s id=%d cell=%s geom=%s npts=%d bbox_factor=%.1fx worst_jump=%.0fm",
				class, f.ID(), cell.Name, f.Geometry().Type, len(coords), ratio, worst)
			for i, c := range coords {
				if len(c) < 2 {
					continue
				}
				marker := ""
				if i > 0 && len(coords[i-1]) >= 2 {
					midLat := (c[1] + coords[i-1][1]) / 2
					mPerDegLat := 111_320.0
					mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
					dlon := (c[0] - coords[i-1][0]) * mPerDegLon
					dlat := (c[1] - coords[i-1][1]) * mPerDegLat
					d := math.Sqrt(dlon*dlon + dlat*dlat)
					if d > structurePhantomJumpM {
						marker = fmt.Sprintf("  *** %.0fm ***", d)
					}
				}
				t.Logf("    [%2d] %.6f, %.6f%s", i, c[0], c[1], marker)
			}
		}
	}
}
