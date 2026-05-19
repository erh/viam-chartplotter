package osmtiler

import (
	"math"
	"testing"
)

func TestLonLatToTile_Origin(t *testing.T) {
	// Every lon/lat lands in tile (0,0) at zoom 0.
	pts := []LonLat{
		{Lon: 0, Lat: 0},
		{Lon: -73.977, Lat: 40.787},
		{Lon: 139.65, Lat: 35.69},
		{Lon: -180, Lat: -85},
	}
	for _, p := range pts {
		x, y := LonLatToTile(p.Lon, p.Lat, 0)
		if x != 0 || y != 0 {
			t.Errorf("LonLatToTile(%v, 0) = (%d,%d), want (0,0)", p, x, y)
		}
	}
}

func TestLonLatToTile_NYC(t *testing.T) {
	// Manhattan at z=10 sits in well-known tile (301, 384). This is
	// the same XYZ scheme tile.openstreetmap.org uses.
	const (
		lon = -73.977
		lat = 40.787
	)
	x, y := LonLatToTile(lon, lat, 10)
	if x != 301 || y != 384 {
		t.Errorf("LonLatToTile(NYC, 10) = (%d,%d), want (301,384)", x, y)
	}
}

func TestLonLatToTilePx_CenterOfTile(t *testing.T) {
	// The center of tile (0,0) at z=0 is (lon=0, lat=0). It should
	// project to pixel (128, 128) — the middle of the 256x256 tile.
	px, py := LonLatToTilePx(0, 0, 0, 0, 0)
	if math.Abs(px-128) > 0.5 || math.Abs(py-128) > 0.5 {
		t.Errorf("center of world projects to (%g,%g), want ~(128,128)", px, py)
	}
}

func TestWorldPx_DoublesPerZoom(t *testing.T) {
	// Moving from zoom z to z+1 doubles the world-pixel coordinate.
	lon, lat := -73.977, 40.787
	x10, y10 := LonLatToWorldPx(lon, lat, 10)
	x11, y11 := LonLatToWorldPx(lon, lat, 11)
	if math.Abs(x11-2*x10) > 1e-6 || math.Abs(y11-2*y10) > 1e-6 {
		t.Errorf("z+1 should double world px: z10=(%g,%g) z11=(%g,%g)", x10, y10, x11, y11)
	}
}

func TestTilesCoveringBBox_Zip10024(t *testing.T) {
	// ZIP 10024 spans roughly the Upper West Side. At z=10 it fits in
	// a single tile; at z=14 it spans a small handful.
	cases := []struct {
		z              int
		wantTilesMin   int
		wantTilesMax   int
	}{
		{0, 1, 1},
		{6, 1, 1},
		{10, 1, 2},
		{14, 1, 6},
		{18, 100, 400}, // sanity bound — ~234 tiles expected
	}
	for _, tc := range cases {
		xMin, yMin, xMax, yMax := TilesCoveringBBox(
			zip10024MinLon, zip10024MinLat,
			zip10024MaxLon, zip10024MaxLat, tc.z)
		n := (xMax - xMin + 1) * (yMax - yMin + 1)
		if n < tc.wantTilesMin || n > tc.wantTilesMax {
			t.Errorf("z=%d tiles=%d (x=[%d..%d] y=[%d..%d]), want in [%d..%d]",
				tc.z, n, xMin, xMax, yMin, yMax, tc.wantTilesMin, tc.wantTilesMax)
		}
	}
}
