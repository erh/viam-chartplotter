package vc

import (
	"math"
	"testing"
)

// hrrrLikeGrid returns the projection parameters NCEP publishes for the
// 3 km HRRR CONUS grid. Used in multiple tests; pulled to a helper so
// the constants live in one place.
func hrrrLikeGrid() *lambertGrid {
	return &lambertGrid{
		Nx:          1799,
		Ny:          1059,
		Dx:          3000,
		Dy:          3000,
		La1:         21.138,
		Lo1:         -122.720,
		LaD:         38.5,
		LoV:         -97.5,
		Latin1:      38.5,
		Latin2:      38.5,
		// Real HRRR ships scanMode 64: +i (W→E) and +j (S→N), i.e.
		// row 0 sits at the southern edge (La1 = 21.138°N). Storing
		// 0 here would put row 0 at the *northern* edge, which would
		// disagree with La1 and produce a flipped/empty reprojection.
		ScanMode:    64,
		EarthRadius: 6371229,
	}
}

// TestLambertOriginMapsToZero verifies the projection's identity point:
// (LaD, LoV) should project to (0, 0) regardless of the cone's
// constants. If this fails the precompute is wrong (n, F, or rho0).
func TestLambertOriginMapsToZero(t *testing.T) {
	g := hrrrLikeGrid()
	p := g.precompute()
	x, y := p.forward(g.LaD, g.LoV)
	if math.Abs(x) > 1e-3 {
		t.Errorf("origin x = %f, want ~0", x)
	}
	if math.Abs(y) > 1e-3 {
		t.Errorf("origin y = %f, want ~0", y)
	}
}

// TestLambertCentralMeridianHasZeroX checks that any point on the
// central meridian (lon = LoV) projects to x = 0 regardless of
// latitude — a property of a properly-oriented LCC.
func TestLambertCentralMeridianHasZeroX(t *testing.T) {
	g := hrrrLikeGrid()
	p := g.precompute()
	for _, lat := range []float64{0, 10, 25, 38.5, 50, 70} {
		x, _ := p.forward(lat, g.LoV)
		if math.Abs(x) > 1e-3 {
			t.Errorf("lat=%v lon=LoV: x = %f, want 0", lat, x)
		}
	}
}

// TestLambertReprojectIdentity exercises the reprojection pipeline
// end-to-end on a synthetic field where each cell carries a value
// proportional to its longitude. After reprojection onto a regular
// lat/lon grid, the resampled value at a known lat/lon should fall
// close to that point's longitude.
func TestLambertReprojectIdentity(t *testing.T) {
	g := hrrrLikeGrid()
	p := g.precompute()
	// Anchor the source grid at the projected La1/Lo1.
	x0, y0 := p.forward(g.La1, g.Lo1)
	// Build a synthetic field: value at grid cell (ix, iy) equals the
	// longitude reconstructed by walking forward from the anchor.
	src := make([]float64, g.Nx*g.Ny)
	for iy := 0; iy < g.Ny; iy++ {
		for ix := 0; ix < g.Nx; ix++ {
			// HRRR scan mode 64: +i (W→E), +j (S→N). So x grows with
			// ix; iy=0 sits at La1 (south edge) and y grows with iy.
			x := x0 + float64(ix)*g.Dx
			_ = y0 + float64(iy)*g.Dy // y, but only x is stamped
			// Stamp the value as `x` so we can verify reprojected
			// output equals forward(target).x.
			src[iy*g.Nx+ix] = x
		}
	}
	// Reproject onto a small regular subgrid inside the source domain.
	lonW, lonE, latS, latN := -110.0, -95.0, 30.0, 45.0
	dlon, dlat := 1.0, 1.0
	out, Nlon, Nlat, err := reprojectLambertToLatLon(src, g, lonW, lonE, latS, latN, dlon, dlat)
	if err != nil {
		t.Fatal(err)
	}
	if Nlon != 16 || Nlat != 16 {
		t.Errorf("output grid %dx%d, want 16x16", Nlon, Nlat)
	}
	// At every (lat, lon) the reprojected value should equal the LCC
	// x-coordinate of that lat/lon (since that's what we baked into
	// the source). Tolerance is loose because bilinear interpolation
	// off a 3 km source grid introduces some smoothing.
	for iy := 0; iy < Nlat; iy++ {
		lat := latN - float64(iy)*dlat
		for ix := 0; ix < Nlon; ix++ {
			lon := lonW + float64(ix)*dlon
			expectedX, _ := p.forward(lat, lon)
			got := out[iy*Nlon+ix]
			if math.Abs(got-expectedX) > 5000 { // 5 km tolerance
				t.Errorf("reproject(%v, %v): got %.0f, want ~%.0f (diff %.0f)",
					lat, lon, got, expectedX, got-expectedX)
			}
		}
	}
}

// TestBilinearSampleClamps verifies the out-of-bounds branch returns 0
// rather than panicking, since reprojection target rectangles routinely
// extend past the source grid (e.g. CONUS target → HRRR domain doesn't
// cover Mexico Gulf corners).
func TestBilinearSampleClamps(t *testing.T) {
	src := []float64{1, 2, 3, 4} // 2x2
	if v := bilinearSample(src, 2, 2, -1, 0); v != 0 {
		t.Errorf("negative fx should return 0, got %v", v)
	}
	if v := bilinearSample(src, 2, 2, 5, 0); v != 0 {
		t.Errorf("fx past grid should return 0, got %v", v)
	}
	if v := bilinearSample(src, 2, 2, 0, 0); v != 1 {
		t.Errorf("(0,0) should return src[0], got %v", v)
	}
	if v := bilinearSample(src, 2, 2, 1, 1); v != 4 {
		t.Errorf("(1,1) should return src[3], got %v", v)
	}
	if v := bilinearSample(src, 2, 2, 0.5, 0.5); math.Abs(v-2.5) > 1e-9 {
		t.Errorf("centroid should return mean (2.5), got %v", v)
	}
}
