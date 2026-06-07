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
	Attributes  map[string]any `bson:"attributes,omitempty"`
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
			MinZoom:     MinZoomForObjectClass(f.ObjectClass()),
			BBox:        bbox,
			Geometry:    geom,
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
		ring := make([][]float64, 0, len(g.Coordinates)+1)
		for _, c := range g.Coordinates {
			if len(c) < 2 {
				continue
			}
			accum(c[0], c[1])
			ring = append(ring, []float64{c[0], c[1]})
		}
		if len(ring) < 3 {
			return nil, "", [4]float64{}, false
		}
		// GeoJSON polygons must be explicitly closed (first == last).
		first, last := ring[0], ring[len(ring)-1]
		if first[0] != last[0] || first[1] != last[1] {
			ring = append(ring, []float64{first[0], first[1]})
		}
		return map[string]any{
			"type":        "Polygon",
			"coordinates": [][][]float64{ring},
		}, "polygon", bbox, true
	}
	return nil, "", [4]float64{}, false
}
