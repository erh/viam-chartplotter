package weather

import (
	"math"
	"strings"
	"testing"
)

func TestAllTilesCoverGlobeWithoutGaps(t *testing.T) {
	tiles := AllTiles()
	if len(tiles) != tileGridCols*tileGridRows {
		t.Fatalf("len=%d, want %d", len(tiles), tileGridCols*tileGridRows)
	}
	// Every nominal bbox must abut its neighbours exactly — no gaps,
	// no overlap. The published bbox grows by overlap on each edge so
	// neighbouring published bboxes *do* overlap (that's the point of
	// publishing margin). Only the nominal bboxes are tested for the
	// "cover the globe" property.
	covered := map[[2]int]Tile{}
	for _, tl := range tiles {
		covered[[2]int{tl.Col, tl.Row}] = tl
	}
	for row := 0; row < tileGridRows; row++ {
		for col := 0; col < tileGridCols; col++ {
			if _, ok := covered[[2]int{col, row}]; !ok {
				t.Errorf("missing tile at col=%d row=%d", col, row)
			}
		}
	}
	// Sum of nominal areas equals globe area (360×180 = 64800).
	var area float64
	for _, tl := range tiles {
		area += (tl.NominalBbox[2] - tl.NominalBbox[0]) * (tl.NominalBbox[3] - tl.NominalBbox[1])
	}
	if math.Abs(area-360*180) > 1e-6 {
		t.Errorf("nominal area sum = %v, want 64800", area)
	}
}

func TestTileKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range AllTiles() {
		if seen[tl.Key] {
			t.Errorf("duplicate key: %q", tl.Key)
		}
		seen[tl.Key] = true
	}
}

func TestTileKeyFormat(t *testing.T) {
	cases := []struct {
		lonW, latS float64
		want       string
	}{
		{-180, -90, "lonW180_latS90"},
		{-120, -60, "lonW120_latS60"},
		{0, 0, "lonE0_latN0"},
		{60, 30, "lonE60_latN30"},
		{120, 60, "lonE120_latN60"},
	}
	for _, c := range cases {
		got := tileKey(c.lonW, c.latS)
		if got != c.want {
			t.Errorf("tileKey(%v, %v) = %q, want %q", c.lonW, c.latS, got, c.want)
		}
		// Path-safe: must not contain "/" or whitespace, since the key
		// becomes a URL path component when uploaded to R2.
		if strings.ContainsAny(got, "/ \t") {
			t.Errorf("tileKey(%v, %v) = %q contains unsafe chars", c.lonW, c.latS, got)
		}
	}
}

func TestPublishedBboxOverlap(t *testing.T) {
	tiles := AllTiles()
	for _, tl := range tiles {
		// Each published bbox extends overlap° past the nominal on
		// every edge, clamped to ±90 in latitude.
		wantW := tl.NominalBbox[0] - tileOverlapDeg
		wantS := tl.NominalBbox[1] - tileOverlapDeg
		wantE := tl.NominalBbox[2] + tileOverlapDeg
		wantN := tl.NominalBbox[3] + tileOverlapDeg
		if wantS < -90 {
			wantS = -90
		}
		if wantN > 90 {
			wantN = 90
		}
		got := tl.PublishedBbox
		if got[0] != wantW || got[1] != wantS || got[2] != wantE || got[3] != wantN {
			t.Errorf("%s published bbox = %v, want [%v %v %v %v]", tl.Key, got, wantW, wantS, wantE, wantN)
		}
	}
}

func TestTileForBboxResolvesCenter(t *testing.T) {
	cases := []struct {
		name     string
		viewport [4]float64
		wantKey  string
		wantFit  bool
	}{
		{"US east coast", [4]float64{-80, 35, -70, 45}, "lonW120_latN30", true},
		{"Iceland", [4]float64{-25, 62, -13, 67}, "lonW60_latN60", true},
		{"Mediterranean", [4]float64{5, 35, 25, 42}, "lonE0_latN30", true},
		{"Tokyo", [4]float64{135, 30, 145, 40}, "lonE120_latN30", true},
		{"Equator+IDL", [4]float64{170, -5, 175, 5}, "lonE120_latN0", true},
		{"way too wide (whole hemisphere)", [4]float64{-90, -10, 90, 10}, "", false},
	}
	for _, c := range cases {
		tl, fit := TileForBbox(c.viewport)
		if fit != c.wantFit {
			t.Errorf("%s: fit=%v, want %v (tile=%s published=%v)", c.name, fit, c.wantFit, tl.Key, tl.PublishedBbox)
		}
		if c.wantFit && tl.Key != c.wantKey {
			t.Errorf("%s: tile=%s, want %s", c.name, tl.Key, c.wantKey)
		}
	}
}

// TestCropPreservesValues builds a synthetic global WindRecord whose
// values encode (row, col) and verifies CropWindRecord pulls back the
// same cells (i.e. no off-by-one on the row/col mapping). Uses
// ECMWF-style origin (Lo1=180, La1=90) so the longitude-wrap math
// gets exercised — the bug we're guarding against is having
// CropWindRecord index the source array at (lon - 0) instead of
// (lon - srcLo1) mod 360 and silently returning the wrong cell.
func TestCropPreservesValues(t *testing.T) {
	const (
		nx = 1440
		ny = 721
		dx = 0.25
		dy = 0.25
	)
	data := make([]float64, nx*ny)
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			data[j*nx+i] = float64(j*10_000 + i) // unique value per cell
		}
	}
	src := WindRecord{
		Header: windHeader{
			Nx: nx, Ny: ny,
			Lo1: 180, La1: 90,
			Lo2: 179.75, La2: -90,
			Dx: dx, Dy: dy,
			GridDefinitionTemplateName: "regular_ll",
		},
		Data: data,
	}
	// Crop to N. Atlantic / E. coast — exercises a bbox where source
	// indices wrap past srcNx (lon=-80 → src col 400, lon=-30 → src
	// col 600 — both on the second half of the source array).
	bbox := [4]float64{-100, 20, -30, 60}
	out := CropWindRecord(src, bbox)
	wantNx := int(math.Ceil((bbox[2]-bbox[0])/dx)) + 1 // ~281
	wantNy := int(math.Ceil((bbox[3]-bbox[1])/dy)) + 1 // ~161
	if out.Header.Nx != wantNx || out.Header.Ny != wantNy {
		t.Fatalf("Nx,Ny = %d,%d want %d,%d", out.Header.Nx, out.Header.Ny, wantNx, wantNy)
	}
	if out.Header.Lo1 != -100 || out.Header.La1 != 60 {
		t.Errorf("Lo1,La1 = %v,%v want -100,60", out.Header.Lo1, out.Header.La1)
	}
	// Spot-check a known cell: (lat=40, lon=-70).
	// Target row j: latN=60 → tlat=60-j*0.25, so j s.t. tlat=40 → j=80.
	// Target col i: lonW=-100 → tlon=-100+i*0.25, so i s.t. tlon=-70 → i=120.
	// Source: tlat=40 → src row = (90-40)/0.25 = 200.
	//         tlon=-70, delta = -70-180 = -250, +360 = 110, src col = 110/0.25 = 440.
	got := out.Data[80*wantNx+120]
	want := float64(200*10_000 + 440)
	if got != want {
		t.Errorf("cell (lat=40,lon=-70) = %v, want %v", got, want)
	}
	// Spot-check (lat=60, lon=-100): the top-left corner.
	// src row = (90-60)/0.25 = 120; src col = ((-100-180) mod 360)/0.25 = 80/0.25 = 320.
	got = out.Data[0]
	want = float64(120*10_000 + 320)
	if got != want {
		t.Errorf("corner (lat=60,lon=-100) = %v, want %v", got, want)
	}
}
