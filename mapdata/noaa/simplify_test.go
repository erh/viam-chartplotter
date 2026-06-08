package noaa

import "testing"

// denseLine builds a polyline from a→b with n intermediate points that all lie
// (almost) on the straight segment, plus a perpendicular jog in the middle so
// at least one vertex must survive simplification.
func denseLine(n int) [][]float64 {
	pts := make([][]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		lon := -70.0 + t // 1° span
		lat := 40.0
		if i == n/2 {
			lat += 0.5 // big jog: must be kept
		}
		pts = append(pts, []float64{lon, lat})
	}
	return pts
}

func TestSimplifyLineDropsCollinear(t *testing.T) {
	pts := denseLine(1000)
	out := simplifyLine(pts, lowGeomTolerance)
	if len(out) >= len(pts) {
		t.Fatalf("expected reduction, got %d from %d", len(out), len(pts))
	}
	// Endpoints preserved.
	if out[0][0] != pts[0][0] || out[len(out)-1][0] != pts[len(pts)-1][0] {
		t.Fatalf("endpoints not preserved: %v..%v", out[0], out[len(out)-1])
	}
	// The perpendicular jog (the only off-line vertex) survives.
	keptJog := false
	for _, p := range out {
		if p[1] > 40.4 {
			keptJog = true
		}
	}
	if !keptJog {
		t.Fatalf("simplification dropped the significant jog vertex")
	}
	// A 1000-segment near-straight line should collapse to a handful of points.
	if len(out) > 10 {
		t.Fatalf("expected aggressive reduction, kept %d points", len(out))
	}
}

func TestSimplifyGeometryLineString(t *testing.T) {
	geom := map[string]any{"type": "LineString", "coordinates": denseLine(500)}
	low := simplifyGeometry(geom, lowGeomTolerance)
	if low == nil {
		t.Fatal("expected a simplified LineString, got nil")
	}
	m := low.(map[string]any)
	if m["type"] != "LineString" {
		t.Fatalf("type = %v", m["type"])
	}
	if got := len(m["coordinates"].([][]float64)); got >= 500 {
		t.Fatalf("no reduction: %d", got)
	}
}

func TestSimplifyGeometryPolygonKeepsClosure(t *testing.T) {
	// A dense square ring (many points per edge), closed.
	var ring [][]float64
	add := func(lon, lat float64) { ring = append(ring, []float64{lon, lat}) }
	for i := 0; i <= 100; i++ {
		add(-70.0+float64(i)/100.0, 40.0)
	}
	for i := 0; i <= 100; i++ {
		add(-69.0, 40.0+float64(i)/100.0)
	}
	for i := 0; i <= 100; i++ {
		add(-69.0-float64(i)/100.0, 41.0)
	}
	for i := 0; i <= 100; i++ {
		add(-70.0, 41.0-float64(i)/100.0)
	}
	ring = closeRing(ring)
	geom := map[string]any{"type": "Polygon", "coordinates": [][][]float64{ring}}

	low := simplifyGeometry(geom, lowGeomTolerance)
	if low == nil {
		t.Fatal("expected a simplified Polygon, got nil")
	}
	out := low.(map[string]any)["coordinates"].([][][]float64)
	r := out[0]
	if len(r) >= len(ring) {
		t.Fatalf("no reduction: %d from %d", len(r), len(ring))
	}
	if r[0][0] != r[len(r)-1][0] || r[0][1] != r[len(r)-1][1] {
		t.Fatalf("ring not closed after simplify: %v..%v", r[0], r[len(r)-1])
	}
	if len(r) < 4 {
		t.Fatalf("ring degenerated to %d points", len(r))
	}
}

func TestSimplifyGeometryPointReturnsNil(t *testing.T) {
	geom := map[string]any{"type": "Point", "coordinates": []float64{-70, 40}}
	if low := simplifyGeometry(geom, lowGeomTolerance); low != nil {
		t.Fatalf("points should not be simplified, got %v", low)
	}
}
