package osmtiler

import (
	"context"
	"fmt"
	"math"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// tileQueryProjection lists fields the cursor SHOULDN'T return — the
// upsert-only identity columns the runtime renderer never touches.
// Implicit-include semantics keep every other field in the doc
// (class, kind, geometry, bbox, name, ref, roadKind, minZoom,
// minLabelZoom) without us having to re-list them on every ingest
// schema tweak.
var tileQueryProjection = bson.M{
	"_id":       0,
	"region":    0,
	"osmType":   0,
	"osmID":     0,
	"ringIndex": 0,
}

// RenderZoomOffset is the default gap between the tile's own zoom
// and the maxZoom of features the renderer asks for. Higher offset
// = coarser, less cluttered tiles; the renderer at z=14 shows the
// feature set that would be drawn at z=11 instead. Tunes how "busy"
// every tile feels without changing the tile's pixel coordinates.
const RenderZoomOffset = 3

// QueryOptions bundles the optional filter knobs for a tile query.
// The zero value (with IncludeMinZoom=true) gives you "features at
// z - RenderZoomOffset or lower" — the same view the live renderer
// uses on every request.
type QueryOptions struct {
	// IncludeMinZoom, when true, adds {minZoom: {$lte: effectiveZoom}}.
	// effectiveZoom is ZoomOverride if it's ≥ 0; otherwise it's the
	// tile's own z minus RenderZoomOffset (clamped to 0). Without
	// this filter a low-zoom tile would pull back every residential
	// street in the bbox.
	IncludeMinZoom bool

	// ZoomOverride forces a specific zoom into the minZoom filter,
	// bypassing the RenderZoomOffset default. Negative means "use
	// the offset-applied default."
	ZoomOverride int

	// Region, if non-empty, restricts to docs whose region field
	// matches. Useful for inspection / dedup queries; in normal
	// rendering we want every region that overlaps so border
	// coverage works.
	Region string

	// Class, if non-empty, restricts to docs whose class field matches.
	// Use for class-specific render paths or debugging.
	Class string

	// PadBuffer, when true, expands the bbox by LabelBuffer pixels'
	// worth of degrees on each side so the renderer's cross-tile
	// label overdraw has the features it needs. Leave false for
	// plain counting / inspection queries.
	PadBuffer bool
}

// BuildTileFilter constructs the bson.M filter for the standard
// $geoIntersects-by-tile-bbox query, with the optional scalar
// predicates from QueryOptions applied on top.
func BuildTileFilter(z, x, y int, opts QueryOptions) bson.M {
	minLon, minLat, maxLon, maxLat := TileBoundsLonLat(z, x, y)
	if opts.PadBuffer {
		bufDeg := float64(LabelBuffer) / float64(TileSize) * 360.0 / math.Exp2(float64(z))
		minLon -= bufDeg
		maxLon += bufDeg
		minLat -= bufDeg
		maxLat += bufDeg
	}
	polygon := bson.M{
		"type": "Polygon",
		"coordinates": [][][]float64{{
			{minLon, minLat},
			{maxLon, minLat},
			{maxLon, maxLat},
			{minLon, maxLat},
			{minLon, minLat},
		}},
	}
	filter := bson.M{
		"geometry": bson.M{"$geoIntersects": bson.M{"$geometry": polygon}},
	}
	zoom := z - RenderZoomOffset
	if zoom < 0 {
		zoom = 0
	}
	if opts.ZoomOverride >= 0 {
		zoom = opts.ZoomOverride
	}
	if opts.IncludeMinZoom {
		filter["minZoom"] = bson.M{"$lte": zoom}
	}
	if opts.Region != "" {
		filter["region"] = opts.Region
	}
	if opts.Class != "" {
		filter["class"] = opts.Class
	}
	return filter
}

// FetchStats is per-query bookkeeping returned alongside the decoded
// features. Useful for log lines / dev tooling that want to surface
// "this tile pulled N bytes off the wire."
type FetchStats struct {
	Docs       int   // documents returned by the cursor
	BytesRead  int64 // total BSON bytes read for those documents
	DecodeFail int   // docs we couldn't decode and skipped
}

// FetchTileFeatures runs the standard tile-bbox query and decodes
// every matching document into a Feature ready to pass to
// RenderTileFromFeatures. Returns a non-nil slice of length zero if
// the tile has no matches; the renderer is happy to draw an empty
// tile (yellow base only, no features).
//
// The FetchStats return summarises wire-level work done — handy for
// surfacing per-tile data-transfer cost in dev tooling.
func FetchTileFeatures(ctx context.Context, coll *mongo.Collection, z, x, y int, opts QueryOptions) ([]Feature, FetchStats, error) {
	if coll == nil {
		return nil, FetchStats{}, fmt.Errorf("osmtiler: nil mongo collection")
	}
	filter := BuildTileFilter(z, x, y, opts)
	cur, err := coll.Find(ctx, filter, options.Find().SetProjection(tileQueryProjection))
	if err != nil {
		return nil, FetchStats{}, fmt.Errorf("find: %w", err)
	}
	defer cur.Close(ctx)

	var stats FetchStats
	features := make([]Feature, 0, 64)
	for cur.Next(ctx) {
		stats.Docs++
		stats.BytesRead += int64(len(cur.Current))
		feat, err := DecodeFeature(cur.Current)
		if err != nil {
			// Skip individual decode failures so one bad doc can't
			// black out an entire tile; the rendered tile is the
			// useful artefact and missing a single building rarely
			// matters.
			stats.DecodeFail++
			continue
		}
		features = append(features, feat)
	}
	if err := cur.Err(); err != nil {
		return nil, stats, fmt.Errorf("cursor: %w", err)
	}
	return features, stats, nil
}

// DecodeFeature turns a raw BSON feature document (the shape written
// by cmd/osmtools ingest) into the in-memory Feature the renderer
// expects. Exported so cmd/osmtools gentile and the runtime renderer
// can share the same conversion path.
func DecodeFeature(raw bson.Raw) (Feature, error) {
	var d struct {
		Class        string    `bson:"class"`
		Kind         string    `bson:"kind"`
		Name         string    `bson:"name"`
		Ref          string    `bson:"ref"`
		RoadKind     string    `bson:"roadKind"`
		MinZoom      int       `bson:"minZoom"`
		MinLabelZoom int       `bson:"minLabelZoom"`
		BBox         []float64 `bson:"bbox"`
		Geometry     struct {
			Type        string `bson:"type"`
			Coordinates bson.A `bson:"coordinates"`
		} `bson:"geometry"`
	}
	if err := bson.Unmarshal(raw, &d); err != nil {
		return Feature{}, fmt.Errorf("unmarshal: %w", err)
	}
	coords, err := coordsFromGeoJSON(d.Geometry.Type, d.Geometry.Coordinates)
	if err != nil {
		return Feature{}, err
	}
	feat := Feature{
		Class:        ClassFromString(d.Class),
		Kind:         GeomKindFromString(d.Kind),
		Coords:       coords,
		Name:         d.Name,
		Ref:          d.Ref,
		MinZoom:      uint8(d.MinZoom),
		MinLabelZoom: uint8(d.MinLabelZoom),
		RoadKind:     RoadKindFromString(d.RoadKind),
	}
	if len(d.BBox) == 4 {
		feat.MinLon = d.BBox[0]
		feat.MinLat = d.BBox[1]
		feat.MaxLon = d.BBox[2]
		feat.MaxLat = d.BBox[3]
	}
	return feat, nil
}

func coordsFromGeoJSON(typ string, raw bson.A) ([]LonLat, error) {
	switch typ {
	case "Point":
		ll, err := xyFromArray(raw)
		if err != nil {
			return nil, fmt.Errorf("Point: %w", err)
		}
		return []LonLat{ll}, nil
	case "LineString":
		return ringFromArray(raw, "LineString")
	case "Polygon":
		if len(raw) == 0 {
			return nil, nil
		}
		outer, ok := raw[0].(bson.A)
		if !ok {
			return nil, fmt.Errorf("Polygon: outer ring not an array")
		}
		return ringFromArray(outer, "Polygon outer")
	}
	return nil, fmt.Errorf("unsupported geometry type %q", typ)
}

func ringFromArray(arr bson.A, what string) ([]LonLat, error) {
	out := make([]LonLat, 0, len(arr))
	for i, v := range arr {
		pa, ok := v.(bson.A)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: not a coord array", what, i)
		}
		ll, err := xyFromArray(pa)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", what, i, err)
		}
		out = append(out, ll)
	}
	return out, nil
}

func xyFromArray(arr bson.A) (LonLat, error) {
	if len(arr) < 2 {
		return LonLat{}, fmt.Errorf("expected [lon, lat], got %d elements", len(arr))
	}
	lon, lonOK := arr[0].(float64)
	lat, latOK := arr[1].(float64)
	if !lonOK || !latOK {
		return LonLat{}, fmt.Errorf("expected [float64, float64], got %T, %T", arr[0], arr[1])
	}
	return LonLat{Lon: lon, Lat: lat}, nil
}

// ClassFromString is the inverse of Class.String(). Lives next to
// DecodeFeature so callers parsing the on-disk format have a single
// place to look.
func ClassFromString(s string) Class {
	switch s {
	case "road":
		return ClassRoad
	case "building":
		return ClassBuilding
	case "landuse":
		return ClassLanduse
	case "leisure":
		return ClassLeisure
	case "natural":
		return ClassNatural
	case "place":
		return ClassPlace
	case "poi":
		return ClassPOI
	case "admin":
		return ClassAdmin
	case "railway":
		return ClassRailway
	case "aeroway":
		return ClassAeroway
	}
	return ClassSkip
}

// GeomKindFromString is the inverse of the lowercase strings the
// ingest tool writes ("point", "line", "polygon").
func GeomKindFromString(s string) GeomKind {
	switch s {
	case "point":
		return GeomPoint
	case "line":
		return GeomLine
	case "polygon":
		return GeomPolygon
	}
	return GeomPoint
}

// RoadKindFromString is the inverse of the lowercase strings the
// ingest tool writes for road sub-classes.
func RoadKindFromString(s string) RoadKind {
	switch s {
	case "motorway":
		return RoadMotorway
	case "trunk":
		return RoadTrunk
	case "primary":
		return RoadPrimary
	case "secondary":
		return RoadSecondary
	case "tertiary":
		return RoadTertiary
	case "residential":
		return RoadResidential
	case "service":
		return RoadService
	case "path":
		return RoadPath
	}
	return RoadUnknown
}
