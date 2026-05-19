package vc

import (
	"encoding/json"
	"math"
	"testing"
)

// TestMarchCellEmpty: cells whose corners all lie on the same side of
// the level emit no segments.
func TestMarchCellEmpty(t *testing.T) {
	// All four corners below the level.
	if segs := marchCell(1000, 990, 992, 991, 993, 0, 1, 1, 0); segs != nil {
		t.Fatalf("expected no segments when all corners below level, got %v", segs)
	}
	// All four corners above.
	if segs := marchCell(1000, 1010, 1012, 1011, 1013, 0, 1, 1, 0); segs != nil {
		t.Fatalf("expected no segments when all corners above level, got %v", segs)
	}
}

// TestMarchCellSingleCrossing exercises one of the simple cases: top-
// left corner above, three others below. The contour should cut the
// top edge and the left edge of the cell, both halfway between corners
// since the values are symmetric around the level.
func TestMarchCellSingleCrossing(t *testing.T) {
	segs := marchCell(1000, 1004, 996, 996, 996, 10, 11, 5, 4)
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d: %v", len(segs), segs)
	}
	s := segs[0]
	// Edge T crossing: tl=1004, tr=996, level=1000 → midpoint at lon=10.5.
	// Edge L crossing: tl=1004, bl=996, level=1000 → midpoint at lat=4.5.
	const eps = 1e-9
	gotLon := s[0]
	gotLat := s[3]
	if math.Abs(gotLon-10.5) > eps {
		t.Errorf("top-edge crossing lon: want 10.5, got %v", gotLon)
	}
	if math.Abs(gotLat-4.5) > eps {
		t.Errorf("left-edge crossing lat: want 4.5, got %v", gotLat)
	}
}

// TestMarchCellOppositeCorners exercises case 6/9 — a straight contour
// passing through the top and bottom edges (left corners below, right
// corners above).
func TestMarchCellOppositeCorners(t *testing.T) {
	segs := marchCell(1000, 996, 1004, 996, 1004, 10, 11, 5, 4)
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	// Top edge crosses at lon=10.5 (between tl=996 and tr=1004), bottom
	// edge crosses at lon=10.5 (between bl=996 and br=1004).
	s := segs[0]
	const eps = 1e-9
	if math.Abs(s[0]-10.5) > eps || math.Abs(s[2]-10.5) > eps {
		t.Errorf("expected both endpoints at lon=10.5, got %v %v", s[0], s[2])
	}
	if math.Abs(s[1]-5) > eps || math.Abs(s[3]-4) > eps {
		t.Errorf("expected endpoints at lat=5 and lat=4, got %v %v", s[1], s[3])
	}
}

// TestMarchCellSaddle: the standard ambiguous case where two diagonally-
// opposite corners are above the level and the other two are below. We
// resolve using the cell mean; with symmetric corners the mean equals
// the level and we pick the "above" branch (mean > level is false →
// "below" branch).
func TestMarchCellSaddle(t *testing.T) {
	// tl & br above (1004), tr & bl below (996). Case index = 8|2 = 10.
	segs := marchCell(1000, 1004, 996, 996, 1004, 10, 11, 5, 4)
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments for saddle, got %d", len(segs))
	}
}

// TestContourLatLonGridShape: feed a simple linear pressure ramp and
// confirm the output FeatureCollection has features at the expected
// pressure levels.
func TestContourLatLonGridShape(t *testing.T) {
	// 10×10 grid, PRMSL ramping from 99000 Pa in the west to 101000 Pa
	// in the east. We expect contours at 992, 996, 1000, 1004, 1008 hPa
	// — only 996, 1000, 1004 fall inside [990, 1010] hPa.
	nx, ny := 10, 10
	data := make([]float64, nx*ny)
	for iy := 0; iy < ny; iy++ {
		for ix := 0; ix < nx; ix++ {
			frac := float64(ix) / float64(nx-1)
			data[iy*nx+ix] = 99000 + frac*2000 // 990..1010 hPa
		}
	}
	rec := &windRecord{
		Header: windHeader{
			Nx:  nx,
			Ny:  ny,
			Lo1: 0,
			La1: 10,
			Dx:  1,
			Dy:  1,
		},
		Data: data,
	}
	feats := contourLatLonGrid(rec)
	if len(feats) == 0 {
		t.Fatal("expected at least one isobar feature, got 0")
	}
	// Levels present should be 992, 996, 1000, 1004, 1008 — all within
	// our ramp range.
	seen := map[int]bool{}
	for _, f := range feats {
		seen[f.Properties.HPa] = true
	}
	for _, want := range []int{992, 996, 1000, 1004, 1008} {
		if !seen[want] {
			t.Errorf("expected isobar at %d hPa, missing", want)
		}
	}
	// We should NOT see contours outside the ramp range.
	for _, unwanted := range []int{984, 1012} {
		if seen[unwanted] {
			t.Errorf("unexpected isobar at %d hPa for pressure range 990..1010", unwanted)
		}
	}
}

// TestContourLatLonGridDateline guards against a previous bug where
// cells whose left edge sat at exactly 180° produced segments with one
// endpoint at +180 and the other near -179, painting a 360°-wide
// horizontal stripe. The fix is to shift the whole cell west when the
// left edge is at or past 180, before interpolating segment endpoints.
//
// Build a GFS-shaped grid (Lo1=0, Dx=0.25) that's just wide enough to
// straddle the dateline (10 cells centred at 180), with a pressure
// ramp that's guaranteed to produce contour crossings. Any output
// segment with |lon1 - lon2| > Dx is a regression.
func TestContourLatLonGridDateline(t *testing.T) {
	// 11 grid points × 11 grid points = 10 × 10 cells, centred so the
	// columns span lon=178.75 to lon=181.25 (= -178.75 after shift).
	nx, ny := 11, 11
	lo1 := 178.75
	dx := 0.25
	data := make([]float64, nx*ny)
	// North-south pressure ramp so every cell row produces contours.
	for iy := 0; iy < ny; iy++ {
		frac := float64(iy) / float64(ny-1)
		row := 99000 + frac*2000
		for ix := 0; ix < nx; ix++ {
			data[iy*nx+ix] = row
		}
	}
	rec := &windRecord{
		Header: windHeader{Nx: nx, Ny: ny, Lo1: lo1, La1: 5, Dx: dx, Dy: 0.25},
		Data:   data,
	}
	feats := contourLatLonGrid(rec)
	if len(feats) == 0 {
		t.Fatal("expected isobar segments across the dateline grid, got none")
	}
	const maxSegLon = 1.0 // generous: real segments are <= Dx wide
	for i, f := range feats {
		c := f.Geometry.Coordinates
		if len(c) != 2 {
			t.Fatalf("feat %d: expected 2 coords, got %d", i, len(c))
		}
		dlon := math.Abs(c[0][0] - c[1][0])
		if dlon > maxSegLon {
			t.Fatalf("feat %d spans %.3f° of longitude — dateline wrap regressed (coords %v)", i, dlon, c)
		}
	}
}

// TestDecodeGFSIsobarsRoundtrip serialises an empty pressure field
// through decodeGFSIsobars (via contourLatLonGrid + json.Marshal) and
// confirms the resulting JSON parses back as a FeatureCollection.
func TestGeoJSONMarshalShape(t *testing.T) {
	rec := &windRecord{
		Header: windHeader{Nx: 3, Ny: 3, Lo1: 0, La1: 2, Dx: 1, Dy: 1, RefTime: "2024-01-01T00:00:00.000Z", ForecastTime: 6},
		Data:   []float64{99000, 99500, 100000, 99500, 100000, 100500, 100000, 100500, 101000},
	}
	feats := contourLatLonGrid(rec)
	fc := geoJSONFeatureCollection{
		Type:     "FeatureCollection",
		Features: feats,
		Meta:     &isobarMeta{RefTime: rec.Header.RefTime, ForecastTime: rec.Header.ForecastTime, StepHPa: isobarStepHPa},
	}
	body, err := json.Marshal(fc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["type"] != "FeatureCollection" {
		t.Errorf("type: want FeatureCollection, got %v", out["type"])
	}
	meta, ok := out["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing or wrong type")
	}
	if meta["refTime"] != "2024-01-01T00:00:00.000Z" {
		t.Errorf("refTime mismatch: %v", meta["refTime"])
	}
}
