package osmtiler

import "testing"

func TestSimplifyLonLatDropsCollinear(t *testing.T) {
	// A straight east-west line densely sampled: every interior point is
	// collinear, so DP should collapse it to the two endpoints.
	pts := make([]LonLat, 0, 101)
	for i := 0; i <= 100; i++ {
		pts = append(pts, LonLat{Lon: float64(i) * 0.01, Lat: 10})
	}
	out, reduced := SimplifyLonLat(pts, LowGeomTolerance)
	if !reduced {
		t.Fatalf("expected reduction of a collinear line")
	}
	if len(out) != 2 {
		t.Fatalf("collinear line should collapse to 2 points, got %d", len(out))
	}
	if out[0] != pts[0] || out[1] != pts[len(pts)-1] {
		t.Fatalf("endpoints not preserved: got %v..%v", out[0], out[1])
	}
}

func TestSimplifyLonLatKeepsSharpCorners(t *testing.T) {
	// A square ring (closed). Tolerance is sub-pixel, far smaller than the
	// square's sides, so all four corners (plus the closing vertex) survive.
	ring := []LonLat{
		{Lon: 0, Lat: 0},
		{Lon: 1, Lat: 0},
		{Lon: 1, Lat: 1},
		{Lon: 0, Lat: 1},
		{Lon: 0, Lat: 0},
	}
	out, reduced := SimplifyLonLat(ring, LowGeomTolerance)
	if reduced {
		t.Fatalf("a minimal square ring should not reduce; got %d points", len(out))
	}
	if len(out) != len(ring) {
		t.Fatalf("expected %d points, got %d", len(ring), len(out))
	}
}

func TestSimplifyLonLatShortInputUnchanged(t *testing.T) {
	pts := []LonLat{{Lon: 0, Lat: 0}, {Lon: 1, Lat: 1}}
	out, reduced := SimplifyLonLat(pts, LowGeomTolerance)
	if reduced || len(out) != 2 {
		t.Fatalf("2-point line must pass through unchanged, got reduced=%v len=%d", reduced, len(out))
	}
}
