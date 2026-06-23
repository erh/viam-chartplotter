package osmtiler

import "math"

// LowGeomMaxZoom is the highest XYZ zoom served from the pre-simplified geomLow
// geometry. The merged-tile overview band (z7..z11) renders land cover from a
// class-filtered, huge-bbox query; at those zooms full-resolution OSM polygons
// (big forest/landuse rings) make the query+decode+render slow enough to blow
// the render timeout (z7 ≈ 16s). geomLow is built at ~1px resolution for this
// zoom, so it's lossless on screen at z<=LowGeomMaxZoom while cutting the vertex
// count (and BSON transfer) by 10-100x. Mirrors mapdata/noaa's LowGeomMaxZoom.
const LowGeomMaxZoom = 11

// LowGeomTolerance is the Douglas–Peucker perpendicular-distance tolerance (in
// degrees) used to build geomLow: ~1 web-mercator pixel of longitude at
// LowGeomMaxZoom (360° spans 256·2^z pixels). Vertices closer than this to the
// line they sit on are invisible at that zoom and below, so dropping them is
// lossless on screen. A fixed longitude-degree tolerance is conservative
// (never over-simplifies) in latitude at higher latitudes where mercator
// stretches.
const LowGeomTolerance = 360.0 / float64(256*(1<<LowGeomMaxZoom))

// SimplifyLonLat runs Douglas–Peucker on a lon/lat polyline at tol degrees,
// keeping both endpoints, and returns the kept points plus whether any vertex
// was dropped. Iterative (explicit stack) rather than recursive so the
// 100k+-vertex rings overview features produce can't blow the goroutine stack.
func SimplifyLonLat(pts []LonLat, tol float64) ([]LonLat, bool) {
	n := len(pts)
	if n < 3 || tol <= 0 {
		return pts, false
	}
	keep := make([]bool, n)
	keep[0], keep[n-1] = true, true
	type seg struct{ lo, hi int }
	stack := []seg{{0, n - 1}}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.hi <= s.lo+1 {
			continue
		}
		ax, ay := pts[s.lo].Lon, pts[s.lo].Lat
		bx, by := pts[s.hi].Lon, pts[s.hi].Lat
		maxD, idx := -1.0, -1
		for i := s.lo + 1; i < s.hi; i++ {
			if d := perpDist(pts[i].Lon, pts[i].Lat, ax, ay, bx, by); d > maxD {
				maxD, idx = d, i
			}
		}
		if maxD > tol && idx > 0 {
			keep[idx] = true
			stack = append(stack, seg{s.lo, idx}, seg{idx, s.hi})
		}
	}
	out := make([]LonLat, 0, n)
	for i := 0; i < n; i++ {
		if keep[i] {
			out = append(out, pts[i])
		}
	}
	return out, len(out) < n
}

// perpDist is the perpendicular distance from point P to the line through A,B
// (or |PA| when A==B), in the same planar units as the inputs.
func perpDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	if dx == 0 && dy == 0 {
		return math.Hypot(px-ax, py-ay)
	}
	cross := math.Abs(dx*(ay-py) - dy*(ax-px))
	return cross / math.Hypot(dx, dy)
}
