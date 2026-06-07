package render

import (
	"go.mongodb.org/mongo-driver/bson"

	"github.com/beetlebugorg/s57/pkg/s57"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// encFeature is the minimal view of an ENC feature the draw path needs. Both
// *s57.Feature (disk parse, retired) and *mongoFeature (decoded from the noaa
// Mongo collection) satisfy it, so the draw helpers don't care where a feature
// came from. The four methods match *s57.Feature's signatures verbatim.
type encFeature interface {
	ObjectClass() string
	Geometry() s57.Geometry
	Attribute(name string) (any, bool)
	Attributes() map[string]any
}

// mongoFeature is an encFeature decoded from a noaa.FeatureDoc. It carries the
// cell compilation scale so the renderer can paint coarse cells before fine
// ones (finest wins), reproducing the old per-cell coarse→fine layering.
type mongoFeature struct {
	class string
	geom  s57.Geometry
	attrs map[string]any
	scale int
	cell  string     // ENC cell name — for the coverage mask
	bbox  [4]float64 // [minLon, minLat, maxLon, maxLat] — for the coverage mask
}

func (m *mongoFeature) ObjectClass() string        { return m.class }
func (m *mongoFeature) Geometry() s57.Geometry     { return m.geom }
func (m *mongoFeature) Attributes() map[string]any { return m.attrs }
func (m *mongoFeature) Attribute(k string) (any, bool) {
	v, ok := m.attrs[k]
	return v, ok
}

// featureFromDoc converts a noaa.FeatureDoc (as read from Mongo, where the
// GeoJSON geometry arrives as bson.D/bson.A) into a draw-ready *mongoFeature.
// Returns ok=false for empty/unsupported geometry. This is the inverse of
// mapdata/noaa.geoJSONGeometry, flattening GeoJSON back to s57.Geometry's
// concatenated [][]float64 coordinate convention.
//
// SOUNDG and other multi-point features are stored as GeoJSON MultiPoint;
// these map to s57.GeometryTypePoint with N coordinates, matching how the s57
// lib presented them to the draw code (Point geometry, many coords).
func featureFromDoc(d noaa.FeatureDoc) (*mongoFeature, bool) {
	gm, ok := asMap(d.Geometry)
	if !ok {
		return nil, false
	}
	gtype, _ := gm["type"].(string)
	coords, _ := gm["coordinates"]

	var g s57.Geometry
	switch gtype {
	case "Point":
		ll, ok := coordPair(coords)
		if !ok {
			return nil, false
		}
		g = s57.Geometry{Type: s57.GeometryTypePoint, Coordinates: [][]float64{ll}}
	case "MultiPoint":
		pts := coordList(coords)
		if len(pts) == 0 {
			return nil, false
		}
		g = s57.Geometry{Type: s57.GeometryTypePoint, Coordinates: pts}
	case "LineString":
		line := coordList(coords)
		if len(line) < 2 {
			return nil, false
		}
		g = s57.Geometry{Type: s57.GeometryTypeLineString, Coordinates: line}
	case "Polygon":
		// parse.go stores GeoJSON rings (outer + any holes/parts), each
		// self-closed. The draw path wants the s57 flat concatenated-ring
		// convention and re-splits via splitRings (even-odd fill handles
		// holes), so concatenate every ring back together.
		flat := concatRings(asArr(coords))
		if len(flat) < 3 {
			return nil, false
		}
		g = s57.Geometry{Type: s57.GeometryTypePolygon, Coordinates: flat}
	case "MultiPolygon":
		var flat [][]float64
		for _, poly := range asArr(coords) {
			flat = append(flat, concatRings(asArr(poly))...)
		}
		if len(flat) < 3 {
			return nil, false
		}
		g = s57.Geometry{Type: s57.GeometryTypePolygon, Coordinates: flat}
	default:
		return nil, false
	}

	attrs := d.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	return &mongoFeature{
		class: d.ObjectClass,
		geom:  g,
		attrs: attrs,
		scale: d.Scale,
		cell:  d.Cell,
		bbox:  d.BBox,
	}, true
}

// ---- BSON shape helpers ----------------------------------------------------
//
// When a document is decoded into a struct field of type `any`, the mongo
// driver yields bson.D for sub-documents and bson.A for arrays, with numbers
// as float64/int32/int64. These helpers normalise those shapes.

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case bson.D:
		out := make(map[string]any, len(m))
		for _, e := range m {
			out[e.Key] = e.Value
		}
		return out, true
	case bson.M:
		return m, true
	case map[string]any:
		return m, true
	}
	return nil, false
}

func asArr(v any) []any {
	switch a := v.(type) {
	case bson.A:
		return a
	case []any:
		return a
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// coordPair reads a single [lon, lat] coordinate.
func coordPair(v any) ([]float64, bool) {
	a := asArr(v)
	if len(a) < 2 {
		return nil, false
	}
	lon, lonOK := asFloat(a[0])
	lat, latOK := asFloat(a[1])
	if !lonOK || !latOK {
		return nil, false
	}
	return []float64{lon, lat}, true
}

// concatRings flattens a GeoJSON ring array (each ring a [][lon,lat]) into one
// concatenated [][]float64, the s57 self-closed-ring convention the draw path's
// splitRings expects.
func concatRings(rings []any) [][]float64 {
	var out [][]float64
	for _, r := range rings {
		out = append(out, coordList(r)...)
	}
	return out
}

// coordList reads an array of [lon, lat] coordinates, dropping malformed ones.
func coordList(v any) [][]float64 {
	a := asArr(v)
	if len(a) == 0 {
		return nil
	}
	out := make([][]float64, 0, len(a))
	for _, e := range a {
		if ll, ok := coordPair(e); ok {
			out = append(out, ll)
		}
	}
	return out
}
