package render

import (
	"math"
	"testing"

	"go.viam.com/test"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

func TestNavTileZoomMatchesRequestedResolution(t *testing.T) {
	// The chosen zoom's cells must be at or finer than what was asked for —
	// coarser would silently plan on less detail than the caller sized for.
	for _, want := range []float64{20, 40, 80, 150, 300} {
		z := navTileZoomFor(want, 41.0)
		test.That(t, z, test.ShouldBeGreaterThanOrEqualTo, navMinZoom)
		test.That(t, z, test.ShouldBeLessThanOrEqualTo, navMaxZoom)
		if z < navMaxZoom {
			test.That(t, navCellSizeM(z, 41.0), test.ShouldBeLessThanOrEqualTo, want)
		}
	}
	// Finer requests select finer tiles.
	test.That(t, navTileZoomFor(20, 41.0), test.ShouldBeGreaterThan, navTileZoomFor(200, 41.0))
}

func TestNavTileBoundsAreContiguous(t *testing.T) {
	// Adjacent tiles must share an edge exactly, or the assembled coverage has
	// seams that read as uncharted water.
	_, minLat, maxLon, maxLat := navTileBounds(10, 300, 380)
	nextMinLon, _, _, nextMaxLat := navTileBounds(10, 301, 380)
	test.That(t, nextMinLon, test.ShouldAlmostEqual, maxLon, 1e-9)
	test.That(t, nextMaxLat, test.ShouldAlmostEqual, maxLat, 1e-9)

	_, belowMinLat, _, belowMaxLat := navTileBounds(10, 300, 381)
	test.That(t, belowMaxLat, test.ShouldAlmostEqual, minLat, 1e-9)
	test.That(t, belowMinLat, test.ShouldBeLessThan, belowMaxLat)
}

func TestNavTilesForBBoxCoversIt(t *testing.T) {
	minLon, minLat, maxLon, maxLat := -71.6, 41.0, -71.0, 41.5
	keys := navTilesForBBox(11, minLon, minLat, maxLon, maxLat)
	test.That(t, len(keys), test.ShouldBeGreaterThan, 0)

	// Every corner of the bbox falls inside one of the returned tiles.
	for _, c := range [][2]float64{{minLon, minLat}, {minLon, maxLat}, {maxLon, minLat}, {maxLon, maxLat}} {
		x, y := lonLatToTile(c[0], c[1], 11)
		found := false
		for _, k := range keys {
			if k[1] == x && k[2] == y {
				found = true
			}
		}
		test.That(t, found, test.ShouldBeTrue)
	}
}

// buildTestTile makes a tile whose southern half is land and whose northern
// half is 10 m of water, so a sampler putting rows in the wrong place is
// immediately visible.
func buildTestTile(z, x, y int) *noaa.NavTile {
	n := noaa.NavTileSize
	tile := &noaa.NavTile{Z: z, X: x, Y: y, Depth: make([]int16, n*n), Flags: make([]uint8, n*n)}
	for row := 0; row < n; row++ {
		for col := 0; col < n; col++ {
			i := row*n + col
			if row < n/2 { // stored rows run north-to-south, so this is the north half
				tile.Depth[i] = 100 // 10.0 m
			} else {
				tile.Depth[i] = noaa.NavDepthUncharted
				tile.Flags[i] = noaa.NavFlagLand
			}
		}
	}
	return tile
}

func TestSampleTilesPutsRowsTheRightWayUp(t *testing.T) {
	// A tile that is water in the north and land in the south must sample into
	// a grid that is water in the north and land in the south. Getting this
	// upside down would route the boat over the land.
	const z = 12
	x, y := lonLatToTile(-71.3, 41.5, z)
	minLon, minLat, maxLon, maxLat := navTileBounds(z, x, y)
	tiles := map[[3]int]*noaa.NavTile{{z, x, y}: buildTestTile(z, x, y)}

	g := newNavGrid(minLon, minLat, maxLon, maxLat, 40000, 5, 1400)
	covered := sampleTilesIntoGrid(g, tiles, z)
	test.That(t, covered, test.ShouldBeGreaterThan, 0)

	// A cell well into the northern quarter is water...
	north, ok := g.cellAt((minLon+maxLon)/2, maxLat-(maxLat-minLat)*0.25)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, g.depth[north], test.ShouldAlmostEqual, 10.0, 0.001)
	test.That(t, g.flags[north]&cellLand, test.ShouldEqual, 0)

	// ...and one well into the southern quarter is land.
	south, ok := g.cellAt((minLon+maxLon)/2, minLat+(maxLat-minLat)*0.25)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, math.IsNaN(g.depth[south]), test.ShouldBeTrue)
	test.That(t, g.flags[south]&cellLand, test.ShouldNotEqual, 0)
}

func TestSampleTilesReportsNoCoverage(t *testing.T) {
	// No tiles means the caller must fall back to the polygons rather than
	// route across a grid that is uniformly "uncharted".
	g := newNavGrid(-71.5, 41.0, -71.4, 41.1, 10000, 5, 1400)
	test.That(t, sampleTilesIntoGrid(g, nil, 12), test.ShouldEqual, 0)
	test.That(t, sampleTilesIntoGrid(g, map[[3]int]*noaa.NavTile{}, 12), test.ShouldEqual, 0)
}

func TestStoredAndLiveFlagsRoundTrip(t *testing.T) {
	for _, live := range []uint8{
		cellLand, cellObstruction, cellDredged, cellUnsurveyed, cellRestricted,
		cellLand | cellRestricted, cellDredged | cellUnsurveyed,
	} {
		test.That(t, liveFlags(storedFlags(live)), test.ShouldEqual, live)
	}
}

func TestClampDecimetresSaturates(t *testing.T) {
	test.That(t, clampDecimetres(12.34), test.ShouldEqual, int16(123))
	test.That(t, clampDecimetres(-1.5), test.ShouldEqual, int16(-15)) // a drying area
	// Absurd depths saturate rather than wrapping — a bad value must never
	// read as its opposite.
	test.That(t, clampDecimetres(1e9), test.ShouldEqual, int16(math.MaxInt16))
	test.That(t, clampDecimetres(-1e9), test.ShouldEqual, int16(noaa.NavDepthUncharted+1))
}
