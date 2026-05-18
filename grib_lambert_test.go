package vc

import (
	"encoding/binary"
	"math"
	"testing"
)

// buildSyntheticLambertSection3 hand-rolls a GRIB2 section 3 body that
// encodes a known Lambert Conformal grid (template 3.30) using the
// real-HRRR field values. The test below parses these bytes and
// verifies every field round-trips — that's the only way to catch
// off-by-one offset bugs without a real .grib2 sample to compare
// against. Layout follows the WMO Manual on Codes; octet numbers in
// comments are 1-indexed.
func buildSyntheticLambertSection3() []byte {
	// We pin the synthetic section length at 81 octets — the smallest
	// valid template-3.30 body — and fill from the top.
	s := make([]byte, 81)
	binary.BigEndian.PutUint32(s[0:4], 81)                                // 1-4:   section length
	s[4] = 3                                                              // 5:     section number
	s[5] = 0                                                              // 6:     source of grid def
	binary.BigEndian.PutUint32(s[6:10], 1799*1059)                         // 7-10:  N data points
	s[10] = 0                                                             // 11:    octets for optional list
	s[11] = 0                                                             // 12:    interpretation
	binary.BigEndian.PutUint16(s[12:14], 30)                              // 13-14: template number = 30
	s[14] = 6                                                             // 15:    shape of earth = 6 (sphere)
	binary.BigEndian.PutUint32(s[30:34], 1799)                            // 31-34: Nx
	binary.BigEndian.PutUint32(s[34:38], 1059)                            // 35-38: Ny
	binary.BigEndian.PutUint32(s[38:42], 21138000)                        // 39-42: La1 (×1e6 deg) = 21.138°
	binary.BigEndian.PutUint32(s[42:46], 237280000)                       // 43-46: Lo1 = 237.28° (= -122.72°, stored 0-360)
	s[46] = 0                                                             // 47:    res + component flags
	binary.BigEndian.PutUint32(s[47:51], 38500000)                        // 48-51: LaD = 38.5°
	binary.BigEndian.PutUint32(s[51:55], 262500000)                       // 52-55: LoV = 262.5° (= -97.5°)
	binary.BigEndian.PutUint32(s[55:59], 3000000)                         // 56-59: Dx (×10⁻³ m) = 3 km
	binary.BigEndian.PutUint32(s[59:63], 3000000)                         // 60-63: Dy (×10⁻³ m) = 3 km
	s[63] = 0                                                             // 64:    projection centre flag
	s[64] = 64                                                            // 65:    scanning mode = 64 (+i, +j)
	binary.BigEndian.PutUint32(s[65:69], 38500000)                        // 66-69: Latin1 = 38.5°
	binary.BigEndian.PutUint32(s[69:73], 38500000)                        // 70-73: Latin2 = 38.5°
	return s
}

// TestParseLambertGridOffsets is the byte-offset sanity check. If any
// field of template 3.30 is read from the wrong octet the parsed grid
// will disagree with the inputs above — and the bug that surfaced as
// "all wind 57 kt uniformly" downstream would have been caught here.
func TestParseLambertGridOffsets(t *testing.T) {
	s := buildSyntheticLambertSection3()
	g, err := parseLambertGrid(s)
	if err != nil {
		t.Fatalf("parseLambertGrid: %v", err)
	}
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"Nx", float64(g.Nx), 1799},
		{"Ny", float64(g.Ny), 1059},
		{"La1", g.La1, 21.138},
		// Lo1 of 237.28° is normalised by parseLambertGrid into -122.72°
		// so the projection math sees a [-180,180] longitude.
		{"Lo1", g.Lo1, -122.72},
		{"LaD", g.LaD, 38.5},
		{"LoV", g.LoV, -97.5},
		{"Dx", g.Dx, 3000},
		{"Dy", g.Dy, 3000},
		{"Latin1", g.Latin1, 38.5},
		{"Latin2", g.Latin2, 38.5},
		{"ScanMode", float64(g.ScanMode), 64},
		{"EarthRadius", g.EarthRadius, 6371229},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > 1e-6 {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

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
