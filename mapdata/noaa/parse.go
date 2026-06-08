package noaa

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/beetlebugorg/s57/pkg/s57"
)

// FeatureDoc is the BSON shape of one S-57 feature stored in the `noaa`
// collection. It mirrors the osmtiler feature document so the two layers
// query the same way: a 2dsphere `geometry` for $geoIntersects, a `minZoom`
// for scale-dependent thinning, and a `bbox` for cheap overlap math.
//
// The _id is composite (cell:objectClass:featureID) so re-ingesting a cell
// upserts its features in place rather than duplicating them.
type FeatureDoc struct {
	ID          string         `bson:"_id"`
	Cell        string         `bson:"cell"`           // ENC cell name, e.g. "US5NYCDM"
	Scale       int            `bson:"scale"`          // cell compilation scale (CSCL/cscale)
	UsageBand   int            `bson:"usageBand"`      // S-57 navigational purpose 1..6
	ObjectClass string         `bson:"objectClass"`    // S-57 acronym, e.g. "DEPARE", "LIGHTS"
	FeatureID   int64          `bson:"featureID"`      // s57 Feature.ID(), unique within cell
	Kind        string         `bson:"kind"`           // "point", "line", "polygon"
	Name        string         `bson:"name,omitempty"` // OBJNAM if present
	MinZoom     int            `bson:"minZoom"`        // lowest XYZ zoom this feature renders at
	BBox        [4]float64     `bson:"bbox"`           // [minLon, minLat, maxLon, maxLat]
	Geometry    any            `bson:"geometry"`       // GeoJSON Point/MultiPoint/LineString/Polygon
	// GeomLow is the geometry pre-simplified (Douglas–Peucker) to ~1px at
	// LowGeomMaxZoom, for line/polygon features whose simplification actually
	// removes vertices. The tile-query path serves it at overview/mid zooms so
	// the giant full-resolution coastlines/contours don't cross the wire when
	// their sub-pixel detail is invisible. Absent (nil) for points and for any
	// feature already coarse enough that simplification wouldn't help — callers
	// coalesce to Geometry in that case. Never indexed, so it needn't satisfy
	// the 2dsphere right-hand rule.
	GeomLow    any            `bson:"geomLow,omitempty"`
	Attributes map[string]any `bson:"attributes,omitempty"`
}

// CellMeta is the metadata stored in the noaa collection under
// "_meta:<cell>" so a re-ingest can skip a cell whose edition+update are
// already loaded.
type CellMeta struct {
	ID           string `bson:"_id"`
	Cell         string `bson:"cell"`
	Edition      string `bson:"edition"`
	UpdateNumber string `bson:"updateNumber"`
	FeatureCount int    `bson:"featureCount"`
}

// maxPolygonVertices caps a single polygon ring so one pathological
// overview-cell coastline can't blow past MongoDB's 16 MB document limit.
// Real ENC cell features are far below this; anything larger is skipped
// with a count returned to the caller.
const maxPolygonVertices = 250_000

// CellNameFromPath derives the ENC cell name from a .000 file path
// ("/x/US5NYCDM.000" -> "US5NYCDM").
func CellNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ParseResult bundles the documents produced from a cell plus the chart-level
// metadata needed for dedup and the count of features skipped for bad geometry.
type ParseResult struct {
	Docs    []FeatureDoc
	Meta    CellMeta
	Skipped int
}

// ParseCell parses a single S-57 .000 file into feature documents ready to
// upsert into the noaa collection. cellName is the ENC cell name (e.g.
// "US5NYCDM"); if empty it's derived from the file path.
func ParseCell(cellName, path string) (ParseResult, error) {
	if cellName == "" {
		cellName = CellNameFromPath(path)
	}
	parser := s57.NewParser()
	chart, err := parser.Parse(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("parse %s: %w", cellName, err)
	}

	scale := int(chart.CompilationScale())
	usage := int(chart.UsageBand())
	feats := chart.Features()

	res := ParseResult{
		Docs: make([]FeatureDoc, 0, len(feats)),
		Meta: CellMeta{
			ID:           "_meta:" + cellName,
			Cell:         cellName,
			Edition:      chart.Edition(),
			UpdateNumber: chart.UpdateNumber(),
		},
	}

	for i := range feats {
		f := &feats[i]
		geom, kind, bbox, ok := geoJSONGeometry(f.Geometry())
		if !ok {
			res.Skipped++
			continue
		}
		doc := FeatureDoc{
			ID:          fmt.Sprintf("%s:%s:%d", cellName, f.ObjectClass(), f.ID()),
			Cell:        cellName,
			Scale:       scale,
			UsageBand:   usage,
			ObjectClass: f.ObjectClass(),
			FeatureID:   f.ID(),
			Kind:        kind,
			Name:        objectName(f),
			MinZoom:     MinZoomForFeature(f.ObjectClass(), f.Attributes()),
			BBox:        bbox,
			Geometry:    geom,
			GeomLow:     simplifyGeometry(geom, lowGeomTolerance),
			Attributes:  f.Attributes(),
		}
		res.Docs = append(res.Docs, doc)
	}
	res.Meta.FeatureCount = len(res.Docs)
	return res, nil
}

// objectName pulls a human-readable name from OBJNAM if present.
func objectName(f *s57.Feature) string {
	if v, ok := f.Attribute("OBJNAM"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// geoJSONGeometry converts an s57.Geometry into a GeoJSON document suitable
// for a 2dsphere index, returning the geometry, its kind ("point"/"line"/
// "polygon"), the lon/lat bbox, and ok=false if the geometry is unusable
// (empty, degenerate, or too large).
//
// Coordinates are flattened to 2D [lon, lat]; S-57 SOUNDG depth (z) values
// live in the feature's DEPTHS attribute, not the geometry.
func geoJSONGeometry(g s57.Geometry) (any, string, [4]float64, bool) {
	if len(g.Coordinates) == 0 {
		return nil, "", [4]float64{}, false
	}
	bbox := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	accum := func(lon, lat float64) {
		if lon < bbox[0] {
			bbox[0] = lon
		}
		if lat < bbox[1] {
			bbox[1] = lat
		}
		if lon > bbox[2] {
			bbox[2] = lon
		}
		if lat > bbox[3] {
			bbox[3] = lat
		}
	}

	switch g.Type {
	case s57.GeometryTypePoint:
		if len(g.Coordinates) == 1 {
			c := g.Coordinates[0]
			if len(c) < 2 {
				return nil, "", [4]float64{}, false
			}
			accum(c[0], c[1])
			return map[string]any{
				"type":        "Point",
				"coordinates": []float64{c[0], c[1]},
			}, "point", bbox, true
		}
		pts := make([][]float64, 0, len(g.Coordinates))
		for _, c := range g.Coordinates {
			if len(c) < 2 {
				continue
			}
			accum(c[0], c[1])
			pts = append(pts, []float64{c[0], c[1]})
		}
		if len(pts) == 0 {
			return nil, "", [4]float64{}, false
		}
		return map[string]any{
			"type":        "MultiPoint",
			"coordinates": pts,
		}, "point", bbox, true

	case s57.GeometryTypeLineString:
		line := make([][]float64, 0, len(g.Coordinates))
		for _, c := range g.Coordinates {
			if len(c) < 2 {
				continue
			}
			accum(c[0], c[1])
			line = append(line, []float64{c[0], c[1]})
		}
		if len(line) < 2 {
			return nil, "", [4]float64{}, false
		}
		return map[string]any{
			"type":        "LineString",
			"coordinates": line,
		}, "line", bbox, true

	case s57.GeometryTypePolygon:
		if len(g.Coordinates) > maxPolygonVertices {
			return nil, "", [4]float64{}, false
		}
		pts := make([][]float64, 0, len(g.Coordinates))
		for _, c := range g.Coordinates {
			if len(c) < 2 {
				continue
			}
			accum(c[0], c[1])
			pts = append(pts, []float64{c[0], c[1]})
		}
		// The s57 lib concatenates every ring (outer + holes, or multiple
		// disjoint outer rings) into one flat list, each ring self-closed.
		// Storing that as a single GeoJSON ring makes a self-intersecting
		// loop the 2dsphere index rejects ("Can't extract geo keys"), which
		// silently dropped the big mainland LNDARE / harbour DEPARE polygons.
		// Split into proper rings, orient each CCW (MongoDB's right-hand
		// rule), and emit MultiPolygon (one polygon per ring) when there's
		// more than one. The renderer re-concatenates + even-odd fills, so
		// holes still render correctly.
		var rings [][][]float64
		for _, r := range splitS57Rings(pts) {
			r = closeRing(r)
			if len(r) < 4 {
				continue // need >= 3 distinct vertices + closing point
			}
			orientCCW(r)
			rings = append(rings, r)
		}
		switch len(rings) {
		case 0:
			return nil, "", [4]float64{}, false
		case 1:
			return map[string]any{
				"type":        "Polygon",
				"coordinates": [][][]float64{rings[0]},
			}, "polygon", bbox, true
		default:
			mp := make([][][][]float64, 0, len(rings))
			for _, r := range rings {
				mp = append(mp, [][][]float64{r})
			}
			return map[string]any{
				"type":        "MultiPolygon",
				"coordinates": mp,
			}, "polygon", bbox, true
		}
	}
	return nil, "", [4]float64{}, false
}

// splitS57Rings splits the s57 concatenated-ring coordinate convention (each
// constituent ring self-closed: first vertex == last) into separate rings. A
// trailing unclosed fragment is returned as its own ring.
func splitS57Rings(coords [][]float64) [][][]float64 {
	var rings [][][]float64
	if len(coords) == 0 {
		return rings
	}
	start := 0
	for i := 1; i < len(coords); i++ {
		if coords[i][0] == coords[start][0] && coords[i][1] == coords[start][1] {
			rings = append(rings, coords[start:i+1])
			start = i + 1
			if start >= len(coords) {
				break
			}
		}
	}
	if start < len(coords) {
		rings = append(rings, coords[start:])
	}
	return rings
}

// closeRing ensures a ring's first and last vertices coincide.
func closeRing(r [][]float64) [][]float64 {
	if len(r) < 3 {
		return r
	}
	if r[0][0] != r[len(r)-1][0] || r[0][1] != r[len(r)-1][1] {
		r = append(r, []float64{r[0][0], r[0][1]})
	}
	return r
}

// ringSignedArea is the shoelace signed area in lon/lat; positive = CCW.
func ringSignedArea(r [][]float64) float64 {
	var a float64
	for i := 0; i+1 < len(r); i++ {
		a += r[i][0]*r[i+1][1] - r[i+1][0]*r[i][1]
	}
	return a / 2
}

// orientCCW reverses a ring in place if it's clockwise, so MongoDB's 2dsphere
// (right-hand rule: exterior rings CCW) indexes it as the enclosed area rather
// than its complement.
func orientCCW(r [][]float64) {
	if ringSignedArea(r) < 0 {
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
	}
}

// lowGeomTolerance is the Douglas–Peucker perpendicular-distance tolerance (in
// degrees) used to build FeatureDoc.GeomLow. It's ~1 web-mercator pixel of
// longitude at LowGeomMaxZoom: 360° spans 256·2^z pixels, so one pixel is
// 360/(256·2^z) degrees. Vertices closer than this to the line they sit on are
// invisible at that zoom and below, so dropping them is lossless on screen.
// (A fixed longitude-degree tolerance is conservative — never over-simplifies —
// in the latitude direction at higher latitudes, where mercator stretches.)
const lowGeomTolerance = 360.0 / float64(256*(1<<LowGeomMaxZoom))

// simplifyGeometry returns a GeoJSON geometry with line/polygon vertices thinned
// by Douglas–Peucker at tol (degrees), or nil when simplification wouldn't help:
// points (no vertices to drop) and already-coarse features whose vertex count is
// unchanged. Returning nil keeps the stored doc small — the query path coalesces
// a missing GeomLow back to the full Geometry.
func simplifyGeometry(geom any, tol float64) any {
	m, ok := geom.(map[string]any)
	if !ok {
		return nil
	}
	switch m["type"] {
	case "LineString":
		coords, _ := m["coordinates"].([][]float64)
		s := simplifyLine(coords, tol)
		if len(s) < 2 || len(s) >= len(coords) {
			return nil
		}
		return map[string]any{"type": "LineString", "coordinates": s}
	case "Polygon":
		rings, _ := m["coordinates"].([][][]float64)
		out, reduced := simplifyRingSet(rings, tol)
		if !reduced {
			return nil
		}
		return map[string]any{"type": "Polygon", "coordinates": out}
	case "MultiPolygon":
		polys, _ := m["coordinates"].([][][][]float64)
		out := make([][][][]float64, 0, len(polys))
		reduced := false
		for _, poly := range polys {
			rs, r := simplifyRingSet(poly, tol)
			out = append(out, rs)
			reduced = reduced || r
		}
		if !reduced {
			return nil
		}
		return map[string]any{"type": "MultiPolygon", "coordinates": out}
	}
	return nil
}

// simplifyRingSet simplifies each ring in a polygon, reporting whether any ring
// actually lost vertices. A ring that would collapse below a valid quad
// (< 4 points incl. closure) is kept at full resolution rather than degenerated.
func simplifyRingSet(rings [][][]float64, tol float64) ([][][]float64, bool) {
	out := make([][][]float64, 0, len(rings))
	reduced := false
	for _, r := range rings {
		s := simplifyLine(r, tol)
		if len(s) < 4 {
			s = r // don't degenerate a ring below a closed triangle
		}
		if len(s) < len(r) {
			reduced = true
		}
		out = append(out, s)
	}
	return out, reduced
}

// simplifyLine runs Douglas–Peucker on a polyline (lon/lat degrees), keeping both
// endpoints, and returns the kept points. It's iterative (explicit stack) rather
// than recursive so the 100k+-vertex rings overview cells produce can't blow the
// goroutine stack.
func simplifyLine(pts [][]float64, tol float64) [][]float64 {
	n := len(pts)
	if n < 3 || tol <= 0 {
		return pts
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
		ax, ay := pts[s.lo][0], pts[s.lo][1]
		bx, by := pts[s.hi][0], pts[s.hi][1]
		maxD, idx := -1.0, -1
		for i := s.lo + 1; i < s.hi; i++ {
			if d := perpDist(pts[i][0], pts[i][1], ax, ay, bx, by); d > maxD {
				maxD, idx = d, i
			}
		}
		if maxD > tol && idx > 0 {
			keep[idx] = true
			stack = append(stack, seg{s.lo, idx}, seg{idx, s.hi})
		}
	}
	out := make([][]float64, 0, n)
	for i := 0; i < n; i++ {
		if keep[i] {
			out = append(out, pts[i])
		}
	}
	return out
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
