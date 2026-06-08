package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erh/viam-chartplotter/mapdata/noaa"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/font/basicfont"

	"github.com/beetlebugorg/s57/pkg/s57"

	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

// ENCRenderer turns ENC cells on disk into XYZ PNG tiles in pure Go. It uses the
// catalog to find which cells overlap a tile, the cell store to locate the .000
// file on disk, github.com/beetlebugorg/s57 to parse, and fogleman/gg to draw.
//
// This is a deliberately minimal style — no S-52 — but readable enough to plot a
// course: water/land fills, coastline, depth contours, soundings, navaids.
type ENCRenderer struct {
	// noaaColl is the MongoDB collection of parsed ENC features that the
	// renderer reads from. Nil renders no ENC content (transparent). The
	// renderer is fully Mongo-backed — it never touches the disk ENC store.
	noaaColl *mongo.Collection
	// osm is the set of per-minZoom-bucket MongoDB collections the
	// /noaa-enc/osm-tile/ underlay queries. Nil disables the layer
	// entirely — the handler serves a transparent fallback.
	osm    *osmtiler.OSMCollections
	logger logging.Logger

	// noMongoBlankCount counts blank OSM tiles served because no
	// MongoDB collection is attached. Used to sample a warning at
	// 1/20 so misconfigured deployments get noticed without spamming.
	noMongoBlankCount atomic.Uint64
}

// drawPass orders the rendering of features so fills are below lines are below
// points, regardless of the order features come out of the spatial index.
type drawPass int

const (
	passAreas drawPass = iota
	passLines
	passPoints
)

// NewENCRenderer builds a Mongo-backed renderer. Attach data sources with
// SetNOAACollection (ENC features) and SetOSMCollections (OSM underlay).
func NewENCRenderer(logger logging.Logger) *ENCRenderer {
	return &ENCRenderer{logger: logger}
}

// SetOSMCollections attaches the per-bucket MongoDB collections that
// hold ingested OSM features. When set, RenderOSMTile fans a
// $geoIntersects query out across the buckets the tile zoom needs and
// draws the merged result. Nil disables the layer — the endpoint
// returns a transparent fallback PNG and never queries.
func (r *ENCRenderer) SetOSMCollections(c *osmtiler.OSMCollections) { r.osm = c }

// SetNOAACollection attaches the MongoDB collection of parsed ENC features
// that RenderTile reads from. Nil (the default) means the ENC layer renders
// transparent. Set from the noaa feature collection (noaa.OpenCollection).
func (r *ENCRenderer) SetNOAACollection(c *mongo.Collection) { r.noaaColl = c }

// Logger returns the renderer's logger (may be nil) so the HTTP handlers can
// log per-request timing breakdowns through the same sublogger.
func (r *ENCRenderer) Logger() logging.Logger { return r.logger }

// slowQueryThreshold: any MongoDB query slower than this is logged.
const slowQueryThreshold = 100 * time.Millisecond

// osmMergeMinZoom is the lowest zoom at which the merged tile pulls in the OSM
// underlay. Below it the OSM detail is invisible and the low-zoom OSM query is
// huge (z7 ≈ 90k features) and slow, so the merged tile is ENC-only there.
const osmMergeMinZoom = 12

// lowZoomBandCeilingZoom: at or below this zoom, queryTileFeatures restricts the
// NOAA query to coarse usage bands (<= lowZoomMaxUsageBand) so an overview tile
// fetches only the coastal/general/overview cells it actually paints (z7 ≈ 30k
// docs → ~760), instead of pulling every fine harbour cell and dropping it in
// memory. lowZoomMaxUsageBand = 3 keeps band 3 (coastal), which carries the
// overview DEPARE depth shading; band 2 alone leaves z7/z8 with no depth.
const (
	lowZoomBandCeilingZoom = 9
	lowZoomMaxUsageBand    = 3
)

// logSlowQuery logs a MongoDB query that exceeded slowQueryThreshold, with the
// collection, bbox, zoom, duration and row count, so slow DB queries are
// visible without profiling.
func (r *ENCRenderer) logSlowQuery(coll string, dur time.Duration, n, z int, minLon, minLat, maxLon, maxLat float64) {
	if r.logger == nil || dur < slowQueryThreshold {
		return
	}
	r.logger.Infof("slow mongo query: coll=%s z=%d bbox=[%.4f,%.4f,%.4f,%.4f] dur=%dms rows=%d",
		coll, z, minLon, minLat, maxLon, maxLat, dur.Milliseconds(), n)
}

// queryTileFeatures pulls every ENC feature whose geometry intersects the tile
// bbox and that renders at zoom z (minZoom <= z) from MongoDB, decoded into
// draw-ready features. Returns nil when no collection is attached.
func (r *ENCRenderer) queryTileFeatures(minLon, minLat, maxLon, maxLat float64, z int) ([]*mongoFeature, error) {
	if r.noaaColl == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// At overview zooms (z <= lowZoomBandCeilingZoom) a tile spans many fine
	// harbour cells (usage bands 4–6) whose detail is invisible at this scale
	// and is dropped by the in-memory cell-scale window anyway. Restrict the
	// query to the coarse bands (1=overview, 2=general, 3=coastal) so those tens
	// of thousands of fine-cell docs are never fetched. Band 3 (coastal) is kept
	// deliberately: it's where the overview DEPARE depth shading lives — gating
	// at band 2 leaves z7/z8 with almost no depth. Higher zooms keep the full
	// in-memory logic, where land/water base classes paint regardless of window.
	maxBand := 0
	var alwaysClasses []string
	if z <= lowZoomBandCeilingZoom {
		maxBand = lowZoomMaxUsageBand
		// Surface the coarse depth contours at overview zoom. They're stored at
		// minZoom≈11 (so the zoom filter drops them), but NOAA shows the major
		// fathom contours from z7 and the band ceiling keeps only coarse-cell
		// ones — without them the deep (white) water reads as "no depth data".
		alwaysClasses = []string{"DEPCNT"}
	}

	qStart := time.Now()
	docs, err := noaa.QueryBBoxBanded(ctx, r.noaaColl, minLon, minLat, maxLon, maxLat, z, maxBand, alwaysClasses, "")
	if err == nil && len(docs) == 0 && maxBand > 0 {
		// Sparse coverage at this band ceiling — retry unrestricted so the tile
		// still shows the best available (finer-than-expected) data, not blank.
		docs, err = noaa.QueryBBoxBanded(ctx, r.noaaColl, minLon, minLat, maxLon, maxLat, z, 0, alwaysClasses, "")
	}
	r.logSlowQuery("noaa", time.Since(qStart), len(docs), z, minLon, minLat, maxLon, maxLat)
	if err != nil {
		return nil, err
	}
	out := make([]*mongoFeature, 0, len(docs))
	for _, d := range docs {
		if mf, ok := featureFromDoc(d); ok {
			out = append(out, mf)
		}
	}
	return out, nil
}

// queryFeaturesAll pulls every ENC feature intersecting the bbox from MongoDB
// regardless of zoom (no minZoom filter), decoded to draw-ready features. Used
// by the navaid/structure/debug endpoints, which do their own class filtering.
func (r *ENCRenderer) queryFeaturesAll(minLon, minLat, maxLon, maxLat float64) ([]*mongoFeature, error) {
	if r.noaaColl == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	qStart := time.Now()
	docs, err := noaa.QueryBBox(ctx, r.noaaColl, minLon, minLat, maxLon, maxLat, -1, "")
	r.logSlowQuery("noaa", time.Since(qStart), len(docs), -1, minLon, minLat, maxLon, maxLat)
	if err != nil {
		return nil, err
	}
	out := make([]*mongoFeature, 0, len(docs))
	for _, d := range docs {
		if mf, ok := featureFromDoc(d); ok {
			out = append(out, mf)
		}
	}
	return out, nil
}

// DebugCellReport is the per-cell payload returned by DebugBBox.
type DebugCellReport struct {
	Name           string         `json:"name"`
	CScale         int            `json:"cscale"`
	BBox           [4]float64     `json:"bbox"` // [minLon, minLat, maxLon, maxLat]
	FeatureCount   int            `json:"feature_count"`
	ByGeometryType map[string]int `json:"by_geometry_type"`
	ByClass        map[string]int `json:"by_class"`
	// ClassesByGeom is class -> geomtype -> count, so we can spot e.g. "DEPARE
	// only ever shows up as Point" which would explain missing fills.
	ClassesByGeom map[string]map[string]int `json:"classes_by_geom"`
	// Features lists each feature whose geometry intersects the queried bbox,
	// with enough info to identify a misbehaving polygon: class, geom type,
	// vertex count, axis-aligned lon/lat bbox and span, and a few key
	// attributes (DRVAL1/DRVAL2, OBJNAM, COLOUR).
	Features   []DebugFeature `json:"features,omitempty"`
	ParseError string         `json:"parse_error,omitempty"`
}

// DebugFeature is a single feature snapshot for the inspector endpoint.
type DebugFeature struct {
	Class      string         `json:"class"`
	GeomType   string         `json:"geom_type"`
	Vertices   int            `json:"vertices"`
	BBox       [4]float64     `json:"bbox"` // [minLon, minLat, maxLon, maxLat]
	WidthDeg   float64        `json:"width_deg"`
	HeightDeg  float64        `json:"height_deg"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Sample     [][]float64    `json:"sample,omitempty"` // first/last few coords
}

// DebugBBox parses every cell overlapping the given lon/lat box and returns a
// per-cell breakdown of feature classes and geometry types. Geometry counts are
// taken from the cell as a whole. When the queried bbox is small (< ~0.05° on
// either side) per-feature details for features whose own bbox intersects the
// query are also included, so a suspect rendering artifact can be identified.
func (r *ENCRenderer) DebugBBox(minLon, minLat, maxLon, maxLat float64) ([]DebugCellReport, error) {
	feats, err := r.queryFeaturesAll(minLon, minLat, maxLon, maxLat)
	if err != nil {
		return nil, err
	}
	wantFeatures := (maxLon-minLon) < 0.05 && (maxLat-minLat) < 0.05

	// Group the Mongo features by cell so the report still reads per-cell.
	byCell := map[string]*DebugCellReport{}
	var order []string
	{
		for _, f := range feats {
			rep := byCell[f.cell]
			if rep == nil {
				rep = &DebugCellReport{
					Name:           f.cell,
					CScale:         f.scale,
					BBox:           [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)},
					ByGeometryType: map[string]int{},
					ByClass:        map[string]int{},
					ClassesByGeom:  map[string]map[string]int{},
				}
				byCell[f.cell] = rep
				order = append(order, f.cell)
			}
			if f.bbox[0] < rep.BBox[0] {
				rep.BBox[0] = f.bbox[0]
			}
			if f.bbox[1] < rep.BBox[1] {
				rep.BBox[1] = f.bbox[1]
			}
			if f.bbox[2] > rep.BBox[2] {
				rep.BBox[2] = f.bbox[2]
			}
			if f.bbox[3] > rep.BBox[3] {
				rep.BBox[3] = f.bbox[3]
			}
			class := f.ObjectClass()
			geomType := f.Geometry().Type.String()
			rep.FeatureCount++
			rep.ByGeometryType[geomType]++
			rep.ByClass[class]++
			if rep.ClassesByGeom[class] == nil {
				rep.ClassesByGeom[class] = map[string]int{}
			}
			rep.ClassesByGeom[class][geomType]++

			if !wantFeatures {
				continue
			}
			coords := f.Geometry().Coordinates
			if len(coords) == 0 {
				continue
			}
			fMinLon, fMaxLon := coords[0][0], coords[0][0]
			fMinLat, fMaxLat := coords[0][1], coords[0][1]
			for _, c := range coords {
				if len(c) < 2 {
					continue
				}
				if c[0] < fMinLon {
					fMinLon = c[0]
				}
				if c[0] > fMaxLon {
					fMaxLon = c[0]
				}
				if c[1] < fMinLat {
					fMinLat = c[1]
				}
				if c[1] > fMaxLat {
					fMaxLat = c[1]
				}
			}
			// Skip features that don't intersect the query bbox.
			if fMaxLon < minLon || fMinLon > maxLon || fMaxLat < minLat || fMinLat > maxLat {
				continue
			}
			df := DebugFeature{
				Class:     class,
				GeomType:  geomType,
				Vertices:  len(coords),
				BBox:      [4]float64{fMinLon, fMinLat, fMaxLon, fMaxLat},
				WidthDeg:  fMaxLon - fMinLon,
				HeightDeg: fMaxLat - fMinLat,
			}
			// Attach a few common attribute keys so we can distinguish
			// e.g. a DEPARE with DRVAL1=NaN from one with DRVAL1=0.
			df.Attributes = map[string]any{}
			for _, k := range []string{"DRVAL1", "DRVAL2", "VALSOU", "OBJNAM", "INFORM", "COLOUR"} {
				if v, ok := f.Attribute(k); ok {
					df.Attributes[k] = v
				}
			}
			// Sample first 3 + last 3 coords so a degenerate polygon's shape
			// is visible without dumping thousands of vertices.
			if len(coords) <= 6 {
				df.Sample = coords
			} else {
				df.Sample = append(df.Sample, coords[:3]...)
				df.Sample = append(df.Sample, coords[len(coords)-3:]...)
			}
			rep.Features = append(rep.Features, df)
		}
	}
	reports := make([]DebugCellReport, 0, len(order))
	for _, name := range order {
		reports = append(reports, *byCell[name])
	}
	return reports, nil
}

// Navaid is a single buoy/beacon/light/daymark extracted from the ENC cells
// in a bbox. Position is [lon, lat]. Properties carries every S-57 attribute
// the original feature had — stripped of internal lib types — so the frontend
// can format them as it sees fit.
type Navaid struct {
	Class      string         `json:"class"`
	Lon        float64        `json:"lon"`
	Lat        float64        `json:"lat"`
	Properties map[string]any `json:"properties"`
}

// isLandClass reports whether an S-57 object class is a land/built-up area
// fill — the polygons we drop under TransparentLand mode so the OSM
// basemap shows through. Coastline (COALNE) is intentionally NOT included
// here: it's a separate line feature and the chart's authoritative water/
// land boundary is still useful even when fill is off.
func isLandClass(class string) bool {
	switch class {
	case "LNDARE", "BUAARE", "BUISGL":
		return true
	}
	return false
}

// IsNavaidClass reports whether an S-57 object class is one of the navaid
// classes the navaids endpoint and the renderer's "skip navaids" mode care
// about. Centralised so the two places stay in sync.
func IsNavaidClass(class string) bool {
	switch class {
	case "BOYLAT", "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB",
		"BCNLAT", "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP",
		"LIGHTS", "DAYMAR":
		return true
	}
	return false
}

// IsStructureClass reports whether an S-57 object class is one of the
// overhead/above-water structures we lift into the interactive vector
// layer. Bridges and overhead cables/pipes carry chart-critical
// clearance attributes (VERCLR, HORCLR, etc.) that are far more
// useful in a hover popup than rasterised onto the tile PNG.
func IsStructureClass(class string) bool {
	switch class {
	case "BRIDGE", "CBLOHD", "PIPOHD", "CONVYR":
		return true
	}
	return false
}

// StructureAttributeKeys are the S-57 attributes worth surfacing on a
// structure hover popup. Includes the vertical/horizontal clearance
// fields plus identification and free-text remarks.
var StructureAttributeKeys = []string{
	"OBJNAM", // name
	"CATBRG", // category of bridge (fixed, opening, swing, lift, ...)
	"VERCLR", // vertical clearance (m)
	"VERCSA", // safe vertical clearance (m)
	"VERCCL", // closed-position vertical clearance (m)
	"VERCOP", // open-position vertical clearance (m)
	"HORCLR", // horizontal clearance (m)
	"HORACC", // horizontal accuracy (m)
	"VERACC", // vertical accuracy (m)
	"COLOUR",
	"NATCON", // nature of construction
	"PRODCT", // product (for pipes)
	"STATUS",
	"INFORM",
	"NINFOM",
	"CONRAD",
	"CONVIS",
}

// Structure is a single bridge / overhead-cable / overhead-pipe /
// conveyor extracted from the ENC cells in a bbox. Geometry is emitted
// in GeoJSON-compatible form so the frontend's GeoJSON format reader
// consumes the response directly.
type Structure struct {
	Class      string         `json:"class"`
	Geometry   StructureGeom  `json:"geometry"`
	Properties map[string]any `json:"properties"`
}

// StructureGeom mirrors the GeoJSON geometry object: Type is "Point",
// "LineString", or "Polygon"; Coordinates' shape follows GeoJSON.
type StructureGeom struct {
	Type        string `json:"type"`
	Coordinates any    `json:"coordinates"`
}

// Structures returns deduped structure features (bridges, overhead
// cables, overhead pipes, conveyors) whose footprint overlaps the
// given lon/lat box. The same physical bridge often appears in
// multiple overlapping cells with slightly different segmentation;
// we dedup on class + name (when present) + bbox-rounded centroid so
// those coalesce to a single feature. The richest attribute bag wins
// ties.
func (r *ENCRenderer) Structures(minLon, minLat, maxLon, maxLat float64) ([]Structure, error) {
	feats, err := r.queryFeaturesAll(minLon, minLat, maxLon, maxLat)
	if err != nil {
		return nil, err
	}

	// dedup grid: ~10 m at mid-latitudes — wider than typical
	// segmentation drift across overlapping cells but tighter than two
	// genuinely distinct bridges in the same harbour.
	const dedupQ = 1e4
	type key struct {
		class string
		name  string
		lonQ  int64
		latQ  int64
	}
	seen := make(map[key]Structure)

	{
		for _, f := range feats {
			class := f.ObjectClass()
			if !IsStructureClass(class) {
				continue
			}
			geom := f.Geometry()
			if len(geom.Coordinates) == 0 {
				continue
			}
			// At least one vertex must land inside the bbox so the
			// feature is actually visible at this zoom — without this
			// guard a single offshore-spanning cable in the chart
			// shows up on every harbour tile.
			inside := false
			for _, c := range geom.Coordinates {
				if len(c) >= 2 && c[0] >= minLon && c[0] <= maxLon &&
					c[1] >= minLat && c[1] <= maxLat {
					inside = true
					break
				}
			}
			if !inside {
				continue
			}
			props := map[string]any{}
			for _, k := range StructureAttributeKeys {
				if v, ok := f.Attribute(k); ok {
					props[k] = v
				}
			}

			// emit stores one structure geometry into the dedup map, keyed
			// on its OWN centroid so distinct sub-features (e.g. the two
			// deck edges of a long bridge recovered below) get separate
			// keys instead of colliding on a shared midpoint. Props are
			// copied per geometry so the downstream hideIcon flag, applied
			// per-Structure, doesn't leak between a bridge's deck edges.
			emit := func(sg StructureGeom, coords [][]float64) {
				var sumLon, sumLat float64
				var n int
				for _, c := range coords {
					if len(c) >= 2 {
						sumLon += c[0]
						sumLat += c[1]
						n++
					}
				}
				if n == 0 {
					return
				}
				cLon := sumLon / float64(n)
				cLat := sumLat / float64(n)
				name := ""
				if v, ok := props["OBJNAM"]; ok {
					if s, ok := v.(string); ok {
						name = s
					}
				}
				k := key{
					class: class,
					name:  name,
					lonQ:  int64(cLon * dedupQ),
					latQ:  int64(cLat * dedupQ),
				}
				p := make(map[string]any, len(props))
				for kk, vv := range props {
					p[kk] = vv
				}
				s := Structure{Class: class, Geometry: sg, Properties: p}
				if existing, ok := seen[k]; !ok || len(p) > len(existing.Properties) {
					seen[k] = s
				}
			}

			// Phantom geometry: the s57 lib sometimes hands a structure
			// back as flat-concatenated raw edges with km-scale diagonals
			// between disconnected sub-features (see structurePhantomJumpM).
			// We used to drop the whole feature, which for a long bridge
			// left only the short approach fragments so the bridge never
			// crossed the water.
			if hasPhantomEdge(geom.Coordinates, structurePhantomJumpM) {
				runs, _ := splitPathOnLongJumps(geom.Coordinates, structurePhantomJumpM)
				var maxRun float64
				for _, r := range runs {
					if l := pathLengthM(r); l > maxRun {
						maxRun = l
					}
				}
				span, a, b := farthestPairM(geom.Coordinates)
				// A bridge's deck is its long axis. When the split preserves
				// most of that axis as a clean run (the deck was densely
				// sampled — e.g. the Manhattan Bridge), stroke the runs so
				// the line follows the real deck. When it doesn't — the deck
				// fragments into sub-300 m stubs (the Brooklyn Bridge), or
				// the whole span is a single 2-point edge (overview cells) —
				// the runs miss the crossing entirely. Fall back to a
				// straight line between the two farthest vertices: the deck
				// axis, which still spans the channel like NOAA draws it. The
				// span cap rejects pathological cross-feature concatenations
				// (a diagonal to a genuinely unrelated structure). Bridges
				// only: for cables/pipes a feature can hold several distinct
				// spans and a farthest-pair line would wrongly join them.
				//
				// The vertex floor is what keeps overview cells from
				// littering the chart: a 1:350k cell encodes a bridge as a
				// bare 2–3 point centerline whose single edge is itself a
				// >300 m "jump", so it has no clean run and trips the
				// fallback — but those coarse stubs are misplaced relative
				// to the detailed-cell bridge and paint as random diagonals
				// across the city. A genuinely-fragmented detailed footprint
				// (the Brooklyn Bridge) carries 8+ vertices; require that so
				// the coarse stubs fall through to the (empty) run path and
				// drop, exactly as they did before bridges were recovered.
				if class == "BRIDGE" && len(geom.Coordinates) >= phantomMinVertices &&
					maxRun < 0.5*span && span > 0 && span <= phantomSpanCapM {
					line := [][]float64{a, b}
					emit(StructureGeom{Type: "LineString", Coordinates: line}, line)
				} else {
					for _, run := range runs {
						emit(StructureGeom{Type: "LineString", Coordinates: run}, run)
					}
				}
				continue
			}
			switch geom.Type {
			case s57.GeometryTypePoint:
				emit(StructureGeom{Type: "Point", Coordinates: geom.Coordinates[0]}, geom.Coordinates)
			case s57.GeometryTypeLineString:
				emit(StructureGeom{Type: "LineString", Coordinates: geom.Coordinates}, geom.Coordinates)
			case s57.GeometryTypePolygon:
				// Wrap the single ring as GeoJSON Polygon coordinates
				// expect: [outer-ring, ...holes]. Holes aren't surfaced
				// by the s57 library at this level so we always emit a
				// one-ring polygon.
				emit(StructureGeom{Type: "Polygon", Coordinates: [][][]float64{geom.Coordinates}}, geom.Coordinates)
			default:
				continue
			}
		}
	}

	out := make([]Structure, 0, len(seen))
	// byName tracks which entry currently "owns" the hover icon for a
	// given (class, OBJNAM). Same-named bridges across overlapping cells
	// (e.g. "Henry Hudson Bridge" encoded once per cell with materially
	// different vertex sets, beyond the grid-dedup tolerance above) are
	// all kept in the output so each cell's line trace still draws on
	// the map; only one of them gets the icon — the rest are flagged
	// hideIcon so the frontend skips the icon + hover popup.
	byName := make(map[string]int, len(seen))
	for _, s := range seen {
		uninformative := isUninformativeStructure(s)
		if uninformative {
			// Uninformative bridges: keep the trace, suppress the icon
			// and hover popup. Don't participate in same-name dedup;
			// empty/generic names would collapse distinct bridges.
			s.Properties["uninformative"] = true
			s.Properties["hideIcon"] = true
			out = append(out, s)
			continue
		}
		out = append(out, s)
		name := strFromProp(s.Properties, "OBJNAM")
		if name == "" {
			continue
		}
		idx := len(out) - 1
		nameKey := strings.ToLower(s.Class) + "|" + name
		ownerIdx, owned := byName[nameKey]
		if !owned {
			byName[nameKey] = idx
			continue
		}
		// Already have an owner for this name. Keep the entry with the
		// larger attribute bag as the icon-owner — the tooltip is only
		// as informative as what we send for the icon-bearing feature.
		if len(out[idx].Properties) > len(out[ownerIdx].Properties) {
			out[ownerIdx].Properties["hideIcon"] = true
			byName[nameKey] = idx
		} else {
			out[idx].Properties["hideIcon"] = true
		}
	}
	return out, nil
}

// genericBridgeNames are OBJNAM values that add nothing beyond what the class
// label "Bridge" already conveys — used by isUninformativeStructure to decide
// whether a bridge feature deserves an interactive hover target. Match is
// case-insensitive and whitespace-trimmed.
var genericBridgeNames = map[string]struct{}{
	"bridge":            {},
	"footbridge":        {},
	"foot bridge":       {},
	"pedestrian bridge": {},
	"railway bridge":    {},
	"rail bridge":       {},
	"rr bridge":         {},
	"r.r. bridge":       {},
	"road bridge":       {},
	"highway bridge":    {},
	"hwy bridge":        {},
}

// isUninformativeStructure reports whether a BRIDGE feature would render as
// nothing but boilerplate ("Bridge / Fixed", "Bridge / Railway Bridge / Fixed")
// — no clearances, no remarks, no distinctive name, and either no CATBRG or
// CATBRG=Fixed. Any clearance value, remark, non-Fixed category, or unique
// name keeps the feature.
func isUninformativeStructure(s Structure) bool {
	if s.Class != "BRIDGE" {
		return false
	}
	p := s.Properties
	name := ""
	if v, ok := p["OBJNAM"]; ok {
		if str, ok := v.(string); ok {
			name = strings.ToLower(strings.TrimSpace(str))
		}
	}
	// Footbridges (CATBRG=9 or named as such) are pedestrian-only and
	// carry no navigational meaning for a boat — suppress the info icon
	// regardless of what other attributes they happen to have. The tile
	// still renders the bridge geometry underneath.
	if v, ok := p["CATBRG"]; ok && v != nil {
		if n, ok := toFloat(v); ok && n == 9 {
			return true
		}
	}
	if strings.Contains(name, "footbridge") || strings.Contains(name, "foot bridge") {
		return true
	}
	for _, k := range []string{"INFORM", "NINFOM", "VERCLR", "VERCCL", "VERCOP", "VERCSA", "HORCLR"} {
		if v, ok := p[k]; ok && v != nil && fmt.Sprintf("%v", v) != "" {
			return false
		}
	}
	// CATBRG: anything other than Fixed (1) is itself informative.
	if v, ok := p["CATBRG"]; ok && v != nil {
		if n, ok := toFloat(v); ok && n != 1 {
			return false
		}
	}
	if name == "" {
		return true
	}
	_, generic := genericBridgeNames[name]
	return generic
}

func strFromProp(p map[string]any, k string) string {
	v, ok := p[k]
	if !ok || v == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v)))
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case string:
		var f float64
		_, err := fmt.Sscanf(n, "%g", &f)
		return f, err == nil
	}
	return 0, false
}

// NavaidAttributeKeys are the S-57 attributes worth surfacing on a navaid
// hover popup. The list is conservative — chart-critical attrs only — so the
// JSON payload stays small and the popup doesn't drown the user in metadata.
var NavaidAttributeKeys = []string{
	"OBJNAM", // common name (e.g. "Buoy 5", "Frying Pan Shoal Light")
	"COLOUR", // S-57 colour code(s) — comma-separated string
	"COLPAT", // colour pattern (horizontal/vertical stripes, etc.)
	"BOYSHP", // buoy shape enum (1=conical, 2=can, 3=spherical, 4=pillar, …)
	"BCNSHP", // beacon shape
	"CATLAM", // category of lateral mark (port/starboard etc.)
	"CATCAM", // category of cardinal mark (N/E/S/W)
	"CATSPM", // category of special purpose mark
	"LITCHR", // light character (1=Fixed, 2=Flashing, …)
	"SIGGRP", // signal group (e.g. "(2+1)")
	"SIGPER", // signal period (s)
	"SIGSEQ", // signal sequence
	"HEIGHT", // height in metres
	"VALNMR", // nominal range in nautical miles
	"SECTR1", // sector start bearing
	"SECTR2", // sector end bearing
	"ORIENT", // orientation (degrees)
	"INFORM", // free-text remarks
	"NINFOM", // ditto, national language
	"NATCON", // nature of construction
	"CONRAD", // conspicuous, radar
	"CONVIS", // conspicuous, visual
	"STATUS", // status
}

// Navaids returns deduped navaid features (buoys, beacons, lights, daymarks)
// whose position falls inside the given lon/lat box. Cell scale is not
// filtered: the same physical buoy may appear in several overlapping cells
// (overview + harbour), so we coalesce duplicates by class + rounded
// coordinate. The most attribute-rich appearance wins ties.
//
// Standalone LIGHTS features are spatially joined onto a co-located buoy or
// beacon (within ~15 m); when matched, the light's properties are merged
// into the structure's bag under "LIGHT_*" prefixed keys plus a "lighted"
// flag, and the LIGHTS feature is dropped from the output. Lights with no
// nearby structure (sector lights, lighthouses) are returned standalone.
func (r *ENCRenderer) Navaids(minLon, minLat, maxLon, maxLat float64) ([]Navaid, error) {
	feats, err := r.queryFeaturesAll(minLon, minLat, maxLon, maxLat)
	if err != nil {
		return nil, err
	}

	type key struct {
		class string
		lonQ  int64
		latQ  int64
	}
	// Round to ~1 metre (~5e-6 deg lat) so the same buoy from two cells
	// dedupes even if their coords differ by sub-metre rounding.
	const q = 1e5
	seen := make(map[key]Navaid)
	var lights []Navaid

	{
		for _, f := range feats {
			class := f.ObjectClass()
			if !IsNavaidClass(class) {
				continue
			}
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypePoint || len(geom.Coordinates) == 0 {
				continue
			}
			c := geom.Coordinates[0]
			if len(c) < 2 {
				continue
			}
			lon, lat := c[0], c[1]
			if lon < minLon || lon > maxLon || lat < minLat || lat > maxLat {
				continue
			}
			props := map[string]any{}
			for _, k := range NavaidAttributeKeys {
				if v, ok := f.Attribute(k); ok {
					props[k] = v
				}
			}
			n := Navaid{Class: class, Lon: lon, Lat: lat, Properties: props}
			if class == "LIGHTS" {
				// Defer until after buoys/beacons are gathered so we can
				// try to attach each light to a structure.
				lights = append(lights, n)
				continue
			}
			k := key{class: class, lonQ: int64(lon * q), latQ: int64(lat * q)}
			if existing, ok := seen[k]; !ok || len(props) > len(existing.Properties) {
				seen[k] = n
			}
		}
	}

	// Spatial join: try to attach each LIGHTS feature to a co-located
	// buoy/beacon. ~15 m tolerance: tighter than a chart's typical
	// position uncertainty but loose enough that sub-cell rounding
	// differences between the BOY/BCN feature and its sibling LIGHTS
	// don't miss a real match. Coordinates are in degrees; convert the
	// tolerance to a per-degree budget at this latitude so the test is
	// roughly isotropic away from the equator.
	const tolMetres = 15.0
	out := make([]Navaid, 0, len(seen)+len(lights))
	// Build a flat slice we can index for the join; map iteration order
	// is non-deterministic but the indices are local to this function.
	structures := make([]Navaid, 0, len(seen))
	for _, n := range seen {
		structures = append(structures, n)
	}
	attached := make([]bool, len(structures))
	for _, light := range lights {
		latRad := light.Lat * math.Pi / 180
		mPerDegLat := 111132.92 - 559.82*math.Cos(2*latRad)
		mPerDegLon := 111412.84 * math.Cos(latRad)
		bestIdx := -1
		bestSq := math.MaxFloat64
		for i, s := range structures {
			dN := (s.Lat - light.Lat) * mPerDegLat
			dE := (s.Lon - light.Lon) * mPerDegLon
			d2 := dN*dN + dE*dE
			if d2 < bestSq {
				bestSq = d2
				bestIdx = i
			}
		}
		if bestIdx >= 0 && bestSq <= tolMetres*tolMetres {
			s := structures[bestIdx]
			s.Properties["lighted"] = true
			for k, v := range light.Properties {
				s.Properties["LIGHT_"+k] = v
			}
			structures[bestIdx] = s
			attached[bestIdx] = true
		} else {
			// Standalone light (lighthouse, sector light, etc.).
			out = append(out, light)
		}
	}
	for _, s := range structures {
		out = append(out, s)
	}
	_ = attached // kept for clarity; structures slice already updated
	return out, nil
}

// RenderTile draws a 256x256 PNG for the given XYZ tile. If no cells overlap, a
// transparent tile is returned so the layer composes cleanly with the basemap.
// safeDepthM is the boat's safety contour in metres; depth-area shading uses a
// gradient from coral at safeDepthM to white at 2×safeDepthM.
// RenderStyle selects how the tile is rendered.
//
// StyleWMS aims for the closest possible visual match to NOAA's WMS chart
// service — uniform depth-contour weights, single-tone soundings, no
// topmarks. Designed for users who want our renderer to compose seamlessly
// with NOAA-style charts (for the /compare endpoint, or so users can flip
// between cached tiles and live WMS without a visual jolt).
//
// StyleECDIS adds the S-52 conditional-symbology niceties — bold safety
// contour (DEPCNT02), two-tone soundings (SOUNDG02), TOPMAR rendering, etc.
// Reads more like a real ECDIS display but won't pixel-match NOAA WMS.
type RenderStyle int

const (
	StyleWMS RenderStyle = iota
	StyleECDIS
)

// ENCRenderRulesVersion is baked into every ENC tile cache key. Bump it
// whenever a code change in this file (or anything it transitively renders
// through) alters the pixels for an unchanged URL — e.g. new S-52 symbology,
// a depth-shading tweak, navaid icon redesign, etc. URL params already shard
// the cache by style/safe-depth/skip flags; this version covers the things
// the URL doesn't say. After bumping, old vN directories are inert and can
// be `rm -rf`'d at the operator's leisure.
// v5: Mongo-backed render (per-feature scale window + coverage mask, land-
// before-water area ordering); invalidates v4 tiles rendered during the
// initial cutover that showed land fills over water.
// v6: dropped the cell-boundary-clip guard that was wrongly suppressing
// coastal/approach-cell LNDARE, leaving land white at z<=11.
// v7: merged tile uses the ENC tan-land base with OSM drawn on top, so land
// is never white where OSM lacks low-zoom data.
// v8: single coarse→fine area pass (finest cell wins) so fine barrier-
// island LNDARE paints over coarse shallow DEPARE (tan island, not blue).
const ENCRenderRulesVersion = 8

// OSMRenderRulesVersion is the same idea, scoped to the OSM raster pipeline
// (RenderOSMTile via osmtiler). Bump on any change to the rasteriser that
// should invalidate the on-disk cache. Independent from ENC so an ENC bump
// doesn't rebuild OSM tiles (and vice versa).
//
// Also mirror this value in src/marineMap.svelte's OSM_RENDER_VERSION so
// the frontend URL pattern bumps `?osmv=` and busts browser caches on the
// next page load — otherwise a Go-only bump leaves stale tiles cached
// client-side for up to a day.
// v15: OSM underlay renders over a transparent base in the merged tile (no
// beige land base bleeding through the chart's transparent deep-water).
const OSMRenderRulesVersion = 15

func (s RenderStyle) String() string {
	if s == StyleECDIS {
		return "ecdis"
	}
	return "wms"
}

// ParseRenderStyle accepts the string from a query parameter or config and
// returns the corresponding RenderStyle. Anything not "ecdis" maps to WMS.
func ParseRenderStyle(s string) RenderStyle {
	if strings.EqualFold(s, "ecdis") {
		return StyleECDIS
	}
	return StyleWMS
}

// RenderOptions bundles per-tile render knobs that aren't part of the
// (z,x,y) coordinate. SkipNavaids drops buoys/beacons/lights/daymarks from
// the tile so the frontend can render them as interactive vector features
// in a separate OL layer. TransparentLand drops LNDARE/BUAARE/BUISGL fills
// so whatever's underneath shows through where the chart says "land" —
// e.g. an OSM raster basemap or our /noaa-enc/osm-tile vector layer.
// SkipClasses is an arbitrary set of S-57 object classes to drop entirely
// from the render — debug-only, lets us bisect "what's painting that
// weird artefact" without chasing it through the styling logic.
type RenderOptions struct {
	SafeDepthM      float64
	Style           RenderStyle
	SkipNavaids     bool
	TransparentLand bool
	SkipClasses     map[string]bool

	// Merge-phase gates (internal): the merged tile renders ENC in two passes
	// from one query so OSM can be sandwiched between them — area fills below
	// the OSM ink, then lines/points/labels on top of it, so OSM landuse never
	// covers ENC chart labels. Exactly one is set per drawENCTile call during a
	// merge; both false renders a complete ENC tile (the standalone path).
	areasOnly   bool // paint only area fills
	overlayOnly bool // paint only lines, points and labels
}

// Zoom thresholds the frontend uses to switch ENC tile params (mirrors
// VECTOR_TILE_NAVAID_MIN_Z / VECTOR_TILE_STRUCTURE_MIN_Z in src/marineMap.svelte).
const (
	browserNavaidMinZoom    = 12 // at/above: navaids come from a vector layer, not the raster
	browserStructureMinZoom = 14 // at/above: bridges/cables come from a vector layer too
)

// BrowserMergedOptions returns the RenderOptions the live frontend requests for
// the ENC layer at zoom z — so a merged tile (OSM under ENC) renders exactly
// what the app composites. Mirrors the overview/mid/detail param sets in
// src/marineMap.svelte:
//
//	z < 12  : ECDIS style, land transparent (OSM shows through), navaids in raster
//	12..13  : WMS style, land transparent, navaids dropped (vector layer)
//	z >= 14 : + skip BRIDGE/CBLOHD/PIPOHD/CONVYR (structure vector layer)
//
// Land is always transparent (landfill=0) so the OSM underlay's streets/
// buildings show on land — the whole point of the merge.
func BrowserMergedOptions(z int, safeDepthM float64) RenderOptions {
	opts := RenderOptions{SafeDepthM: safeDepthM, TransparentLand: true}
	if z < browserNavaidMinZoom {
		opts.Style = StyleECDIS
		return opts
	}
	opts.Style = StyleWMS
	opts.SkipNavaids = true
	if z >= browserStructureMinZoom {
		opts.SkipClasses = map[string]bool{
			"BRIDGE": true, "CBLOHD": true, "PIPOHD": true, "CONVYR": true,
		}
	}
	return opts
}

// TileFeatureReport summarises what the renderer pulls from MongoDB for a
// single tile — the ground truth behind a tile that renders empty or wrong.
type TileFeatureReport struct {
	Z        int            `json:"z"`
	X        int            `json:"x"`
	Y        int            `json:"y"`
	BBox     [4]float64     `json:"bbox"` // [minLon, minLat, maxLon, maxLat]
	Features int            `json:"features"`
	QueryMS  float64        `json:"query_ms"`
	ByClass  map[string]int `json:"by_class"`
	ByCell   map[string]int `json:"by_cell"`
	ByScale  map[int]int    `json:"by_scale"`
	ByKind   map[string]int `json:"by_kind"`
}

// TileFeatureReport runs the exact same Mongo query RenderTile uses and reports
// the result broken down by object class, cell, compilation scale and geometry
// kind. Serve it from a debug endpoint to answer "why does this tile look
// wrong / empty?" without guessing.
func (r *ENCRenderer) TileFeatureReport(z, x, y int) (TileFeatureReport, error) {
	rep := TileFeatureReport{
		Z: z, X: x, Y: y,
		ByClass: map[string]int{},
		ByCell:  map[string]int{},
		ByScale: map[int]int{},
		ByKind:  map[string]int{},
	}
	txmin, tymin, txmax, tymax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(txmin, tymax)
	maxLon, minLat := mercToLonLat(txmax, tymin)
	rep.BBox = [4]float64{minLon, minLat, maxLon, maxLat}
	start := time.Now()
	feats, err := r.queryTileFeatures(minLon, minLat, maxLon, maxLat, z)
	rep.QueryMS = msSince(start)
	if err != nil {
		return rep, err
	}
	rep.Features = len(feats)
	for _, f := range feats {
		rep.ByClass[f.class]++
		if f.cell != "" {
			rep.ByCell[f.cell]++
		}
		rep.ByScale[f.scale]++
		switch f.geom.Type {
		case s57.GeometryTypePoint:
			rep.ByKind["point"]++
		case s57.GeometryTypeLineString:
			rep.ByKind["line"]++
		case s57.GeometryTypePolygon:
			rep.ByKind["polygon"]++
		}
	}
	return rep, nil
}

// tileTiming breaks down where RenderTile spent its wall-clock, so a slow tile
// can be attributed to the Mongo query vs the draw work.
type tileTiming struct {
	QueryMS  float64 // Mongo $geoIntersects query + decode
	DrawMS   float64 // rasterization (all passes + labels)
	Features int     // features returned by the query
}

func (r *ENCRenderer) RenderTile(z, x, y int, opts RenderOptions) ([]byte, tileTiming, error) {
	var timing tileTiming

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	if opts.SkipClasses != nil && opts.SkipClasses["*"] {
		b, e := encodePNG(gg.NewContext(256, 256))
		return b, timing, e
	}

	// Pull every ENC feature whose geometry intersects this tile and that
	// renders at this zoom (minZoom <= z) from MongoDB in one query. minZoom
	// (per S-57 object class) does the scale-dependent class thinning; the
	// cell compilation scale on each feature drives the cell-scale window and
	// coverage mask below — the per-feature equivalents of the old per-cell
	// machinery.
	qStart := time.Now()
	features, err := r.queryTileFeatures(minLon, minLat, maxLon, maxLat, z)
	timing.QueryMS = msSince(qStart)
	timing.Features = len(features)
	if err != nil {
		return nil, timing, err
	}
	sortFeaturesForPaint(features)

	b, drawMS, err := r.drawENCTile(features, z, x, y, opts)
	timing.DrawMS = drawMS
	if r.logger != nil && (timing.QueryMS+timing.DrawMS) > 200 {
		r.logger.Infof("slow enc tile z=%d x=%d y=%d db=%.0fms draw=%.0fms feats=%d",
			z, x, y, timing.QueryMS, timing.DrawMS, timing.Features)
	}
	return b, timing, err
}

// sortFeaturesForPaint orders features coarsest-cell first so finer-cell
// polygons paint last and win. Ties (equal scale) break on the doc _id so the
// paint order is fully deterministic — independent of the order MongoDB returns
// documents in (which varies with the chosen index, e.g. band_geo vs
// geo_minZoom_class). Without the tiebreak, the same tile could differ by a
// few boundary pixels run-to-run as the query plan changed.
func sortFeaturesForPaint(features []*mongoFeature) {
	sort.Slice(features, func(i, j int) bool {
		if features[i].scale != features[j].scale {
			return features[i].scale > features[j].scale
		}
		return features[i].id < features[j].id
	})
}

// drawENCTile paints already-queried, already-sorted (coarsest-first) ENC
// features onto a 256×256 tile and returns the PNG plus draw-milliseconds. The
// query lives in RenderTile; splitting the draw out lets the merged tile paint
// ENC in two phases (areas, then overlay) around the OSM layer from one query.
// opts.areasOnly / opts.overlayOnly gate which passes run (both false = full).
func (r *ENCRenderer) drawENCTile(features []*mongoFeature, z, x, y int, opts RenderOptions) ([]byte, float64, error) {
	safeDepthM := opts.SafeDepthM
	style := opts.Style
	skipNavaids := opts.SkipNavaids
	transparentLand := opts.TransparentLand

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	dc := gg.NewContext(256, 256)
	// Transparent background so the OSM/seachart base layers below show through
	// where we have no chart coverage.

	project := func(lon, lat float64) (float64, float64) {
		mx, my := lonLatToMerc(lon, lat)
		px := (mx - tileXmin) / (tileXmax - tileXmin) * 256
		py := (tileYmax - my) / (tileYmax - tileYmin) * 256
		return px, py
	}
	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}
	scale := zoomSymbolScale(z)

	drawStart := time.Now()

	// Cell-scale window (per-feature equivalent of the old cellScaleRangeFor
	// cell filter): drops coarse approach/overview cells at chart-detail zoom
	// so their imprecise coastlines/soundings don't clutter or paint over the
	// finer harbour cell. Applied to "other" area fills, lines and points. If
	// the window would exclude everything (sparse coverage), it's disabled so
	// the tile still shows the best available data.
	midLat := (minLat + maxLat) / 2
	minScale, maxScale := cellScaleRangeFor(z, midLat)
	inWindow := func(f *mongoFeature) bool {
		if f.scale <= 0 {
			return true
		}
		if minScale > 0 && f.scale < minScale {
			return false
		}
		if maxScale > 0 && f.scale > maxScale {
			return false
		}
		return true
	}
	useWindow := false
	for _, f := range features {
		if inWindow(f) {
			useWindow = true
			break
		}
	}
	windowOK := func(f *mongoFeature) bool { return !useWindow || inWindow(f) }

	// Coverage handling: rely on the coarse->fine sort (finer cells paint
	// last and win), land-before-water ordering (water always covers land),
	// and the per-polygon oversized/degenerate guards in drawFeature. The old
	// cell-extent coverage mask is intentionally NOT applied — approximating a
	// cell's extent from feature bboxes produced rectangular tan patches and
	// half-filled land.

	skip := func(class string) bool {
		if skipNavaids && IsNavaidClass(class) {
			return true
		}
		if opts.SkipClasses != nil && opts.SkipClasses[class] {
			return true
		}
		return false
	}

	// --- area pass: a single coarsest→finest pass (features are sorted that
	// way) so the finest cell's polygon wins per pixel. This is what makes a
	// fine-cell barrier-island LNDARE paint over a coarse-cell shallow DEPARE
	// (tan island, not blue) AND a fine-cell DEPARE paint over a coarse-cell
	// LNDARE (blue water, not tan). Drawing all land then all water instead
	// would let coarse water blue-out fine islands.
	if !opts.overlayOnly {
		for _, f := range features {
			class := f.class
			if skip(class) {
				continue
			}
			if isLandClass(class) {
				if transparentLand {
					continue
				}
				// coarse-zoom seam guard: only continent-scale land fills at z<10.
				if z < 10 && f.scale > 0 && f.scale < 800_000 {
					continue
				}
			} else if !isWaterBaseClass(class) {
				// "Other" area fills (RESARE, anchorages, …) are scale-windowed;
				// the land/water base classes always paint (finest-wins handles
				// any overlap).
				if !windowOK(f) {
					continue
				}
			}
			drawFeature(dc, f, passAreas, project, scale, safeDepthM, bbox, z, style)
		}
	}

	if opts.areasOnly {
		timing := msSince(drawStart)
		b, e := encodePNG(dc)
		return b, timing, e
	}

	// --- line pass --- (scale-windowed, but channel/fairway lines always
	// show from z>=9 regardless of window — they're the most navigationally
	// important linework and live on fine harbour cells the window excludes).
	// Depth contours (DEPCNT) also bypass the window: they're the depth signal
	// at overview zoom and the coarse-cell ones the window would drop are
	// exactly the ones we want to keep (see queryTileFeatures alwaysClasses).
	for _, f := range features {
		if skip(f.class) {
			continue
		}
		if transparentLand && isLandClass(f.class) {
			continue
		}
		if !windowOK(f) && !(z >= 9 && isChannelClass(f.class)) && f.class != "DEPCNT" {
			continue
		}
		drawFeature(dc, f, passLines, project, scale, safeDepthM, bbox, z, style)
	}

	// --- point pass --- (scale-windowed).
	for _, f := range features {
		if skip(f.class) {
			continue
		}
		if transparentLand && isLandClass(f.class) {
			continue
		}
		if !windowOK(f) {
			continue
		}
		drawFeature(dc, f, passPoints, project, scale, safeDepthM, bbox, z, style)
	}
	drawMS := msSince(drawStart)

	// Place-name labels at overview zooms (z >= 10). Bigger features get
	// priority: we collect all viable candidates, dedupe by name keeping
	// the largest polygon, sort largest-first, then place greedily with
	// collision detection so a tile full of named marshes/coves doesn't
	// turn into stacked unreadable text.
	if z >= 10 {
		type labelCand struct {
			name              string
			px, py            float64
			labelScale        float64
			halfW, halfH, pad float64
			area              float64
		}
		bestByName := map[string]labelCand{}
		for _, f := range features {
			class := f.class
			switch class {
			case "LNDARE", "LNDRGN", "BUAARE", "SEAARE", "ADMARE", "BUISGL":
			default:
				continue
			}
			if opts.SkipClasses != nil && opts.SkipClasses[class] {
				continue
			}
			v, ok := f.Attribute("OBJNAM")
			if !ok {
				continue
			}
			name, _ := v.(string)
			if name == "" {
				continue
			}
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypePolygon || len(geom.Coordinates) < 3 {
				continue
			}
			if isOversizedPolygon(geom.Coordinates, bbox, 4) {
				continue
			}
			cx, cy := polygonCentroid(geom.Coordinates)
			px, py := project(cx, cy)

			ls := scale
			switch {
			case z <= 10:
				ls = scale * 1.7
			case z == 11:
				ls = scale * 1.5
			case z == 12:
				ls = scale * 1.25
			}
			halfW := float64(len(name)) * 7 * ls / 2
			halfH := 6.5 * ls
			if px-halfW < 2 || px+halfW > 254 || py-halfH < 2 || py+halfH > 254 {
				continue
			}

			// Polygon area as importance proxy. Use the dominant ring.
			rings := splitRings(geom.Coordinates)
			if len(rings) == 0 {
				rings = [][][]float64{geom.Coordinates}
			}
			var area float64
			for _, ring := range rings {
				a := math.Abs(ringSignedArea(ring))
				if a > area {
					area = a
				}
			}

			cand := labelCand{name, px, py, ls, halfW, halfH, 2.0, area}
			if existing, dup := bestByName[name]; !dup || cand.area > existing.area {
				bestByName[name] = cand
			}
		}

		cands := make([]labelCand, 0, len(bestByName))
		for _, c := range bestByName {
			cands = append(cands, c)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].area > cands[j].area })

		type rect struct{ minX, minY, maxX, maxY float64 }
		var placed []rect
		for _, c := range cands {
			rc := rect{
				c.px - c.halfW - c.pad,
				c.py - c.halfH - c.pad,
				c.px + c.halfW + c.pad,
				c.py + c.halfH + c.pad,
			}
			overlap := false
			for _, p := range placed {
				if !(rc.maxX < p.minX || rc.minX > p.maxX || rc.maxY < p.minY || rc.minY > p.maxY) {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			placed = append(placed, rc)
			dc.SetFontFace(basicfont.Face7x13)
			dc.SetColor(s52CHBLK)
			dc.Push()
			dc.ScaleAbout(c.labelScale, c.labelScale, c.px, c.py)
			dc.DrawStringAnchored(c.name, c.px, c.py, 0.5, 0.5)
			dc.Pop()
		}
	}

	b, e := encodePNG(dc)
	return b, drawMS, e
}

// msSince returns elapsed milliseconds since t as a float (for timing logs).
func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

// isWaterBaseClass reports the area-fill classes that constitute water/depth
// shading; these always paint after land so open water never reads as land.
func isWaterBaseClass(class string) bool {
	switch class {
	case "DEPARE", "DRGARE", "LOKBSN", "UNSARE":
		return true
	}
	return false
}

// isChannelClass reports the navigationally-critical channel/fairway linework
// that must show from z>=9 even when the cell-scale window would exclude its
// (fine, harbour-cell) source.
func isChannelClass(class string) bool {
	switch class {
	case "FAIRWY", "RECTRC", "NAVLNE", "DWRTPT", "TWRTPT":
		return true
	}
	return false
}

func encodePNG(dc *gg.Context) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderOSMTile draws our self-hosted OSM raster for the given XYZ
// tile by $geoIntersects-querying the OSM MongoDB collection and
// painting the returned features. Water is omitted by design — the
// chart's depth shading provides it through the chart-water mask.
//
// The second return value is true when a feature-backed render
// happened, false when this is the transparent fallback (no Mongo
// collection attached, query failed). The handler uses this to
// decide between long-cache (real render) and no-cache (fallback).
func (r *ENCRenderer) RenderOSMTile(z, x, y int) ([]byte, bool, error) {
	return r.renderOSMTile(z, x, y, false)
}

// renderOSMTile renders the OSM layer. transparentBase=true (used by the merged
// tile) renders over a transparent background so the beige land base can't show
// through the ENC chart's transparent deep-water as "yellow water"; the
// standalone /noaa-enc/osm-tile endpoint uses false (opaque land base).
func (r *ENCRenderer) renderOSMTile(z, x, y int, transparentBase bool) ([]byte, bool, error) {
	t0 := time.Now()
	if r.osm == nil {
		if n := r.noMongoBlankCount.Add(1); n%20 == 1 && r.logger != nil {
			r.logger.Warnf("osm-tile served blank (no mongo collection attached); %d such tiles so far", n)
		}
	}
	if r.osm != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		qStart := time.Now()
		features, _, err := osmtiler.FetchTileFeaturesMulti(ctx, r.osm, z, x, y, osmtiler.QueryOptions{
			IncludeMinZoom: true,
			ZoomOverride:   -1,
			PadBuffer:      true,
		})
		if d := time.Since(qStart); d >= slowQueryThreshold && r.logger != nil {
			r.logger.Infof("slow mongo query: coll=osm_* z=%d x=%d y=%d dur=%dms rows=%d",
				z, x, y, d.Milliseconds(), len(features))
		}
		if err != nil {
			if r.logger != nil {
				r.logger.Warnf("osm-tile query z=%d x=%d y=%d: %v", z, x, y, err)
			}
		} else {
			var data []byte
			if transparentBase {
				data, err = osmtiler.RenderTileFromFeaturesTransparent(features, z, x, y)
			} else {
				data, err = osmtiler.RenderTileFromFeatures(features, z, x, y)
			}
			if err == nil {
				if r.logger != nil && time.Since(t0) > 200*time.Millisecond {
					r.logger.Infof("osm-tile rendered z=%d x=%d y=%d in %s (%d bytes, %d features)",
						z, x, y, time.Since(t0).Round(time.Millisecond), len(data), len(features))
				}
				return data, true, nil
			}
			if r.logger != nil {
				r.logger.Warnf("osm-tile render z=%d x=%d y=%d: %v", z, x, y, err)
			}
		}
	}
	dc := gg.NewContext(256, 256)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), false, nil
}

// mergeTiming breaks down a merged-tile render: the OSM underlay, the ENC
// chart (with its own query/draw split), and the PNG composite.
type mergeTiming struct {
	OSMMS       float64
	ENC         tileTiming
	CompositeMS float64
}

// RenderMergedTile composites the ENC chart and the OSM detail into one PNG.
// The ENC chart is the BASE with land fill ON (tan land, depth-shaded water,
// coastline, navaids) so land is never white where OSM lacks data; the OSM
// layer (streets/buildings/landuse, water omitted) is drawn OVER it so street
// context shows on land while ENC owns land/water colour. The bool reports
// whether the OSM layer was feature-backed; the timing is for attribution.
func (r *ENCRenderer) RenderMergedTile(z, x, y int, opts RenderOptions) ([]byte, bool, mergeTiming, error) {
	var mt mergeTiming

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	// One ENC query feeds both ENC phases below (fills, then overlay).
	qStart := time.Now()
	features, err := r.queryTileFeatures(minLon, minLat, maxLon, maxLat, z)
	mt.ENC.QueryMS = msSince(qStart)
	mt.ENC.Features = len(features)
	if err != nil {
		return nil, false, mt, err
	}
	sortFeaturesForPaint(features)

	// ENC base: force land fill on regardless of the caller's TransparentLand —
	// in the merge the ENC tan land is the authoritative land base (OSM draws
	// on top of it), so "landfill=0" must not leave land white.
	encOpts := opts
	encOpts.TransparentLand = false

	// Below osmMergeMinZoom, skip OSM entirely: street/building detail is
	// invisible at overview scale, and the low-zoom OSM query is enormous
	// (z7 ≈ 90k features, z8 ≈ 75k) — it dominates the render and times out.
	// The chart (ENC) alone is the right overview tile, and it's fast — render
	// it complete (areas + overlay) in one pass, no compositing.
	if z < osmMergeMinZoom {
		encPNG, drawMS, derr := r.drawENCTile(features, z, x, y, encOpts)
		mt.ENC.DrawMS = drawMS
		return encPNG, false, mt, derr
	}

	// Three-layer composite so OSM landuse never paints over ENC labels:
	//   1. ENC area fills (land tan base + water/depth shading), OSM-less
	//   2. OSM ink (roads, buildings, landuse) — transparent base, no water
	//   3. ENC lines + points + labels, on top of OSM
	fillsOpts := encOpts
	fillsOpts.areasOnly = true
	fillsPNG, fillsMS, err := r.drawENCTile(features, z, x, y, fillsOpts)
	if err != nil {
		return nil, false, mt, err
	}
	overlayOpts := encOpts
	overlayOpts.overlayOnly = true
	overlayPNG, overlayMS, err := r.drawENCTile(features, z, x, y, overlayOpts)
	if err != nil {
		return nil, false, mt, err
	}
	mt.ENC.DrawMS = fillsMS + overlayMS

	osmStart := time.Now()
	// Transparent base so OSM contributes only its land ink (roads, buildings,
	// landuse); it draws no water, so ENC's water shows through underneath.
	osmPNG, osmRendered, _ := r.renderOSMTile(z, x, y, true)
	mt.OSMMS = msSince(osmStart)

	cStart := time.Now()
	base, err := compositeOver(fillsPNG, osmPNG) // OSM over ENC fills
	if err != nil {
		return nil, osmRendered, mt, err
	}
	merged, err := compositeOver(base, overlayPNG) // ENC lines/labels on top
	mt.CompositeMS = msSince(cStart)
	if err != nil {
		return nil, osmRendered, mt, err
	}
	if r.logger != nil {
		total := mt.OSMMS + mt.ENC.QueryMS + mt.ENC.DrawMS + mt.CompositeMS
		if total > 300 {
			r.logger.Infof("slow merged tile z=%d x=%d y=%d total=%.0fms (enc-db=%.0f enc-draw=%.0f osm=%.0f composite=%.0f feats=%d)",
				z, x, y, total, mt.ENC.QueryMS, mt.ENC.DrawMS, mt.OSMMS, mt.CompositeMS, mt.ENC.Features)
		}
	}
	return merged, osmRendered, mt, nil
}

// compositeOver draws the `over` PNG on top of the `under` PNG (both 256x256,
// transparent where empty) and returns the combined PNG. Either input may be
// nil/empty, in which case the other is returned as-is.
func compositeOver(under, over []byte) ([]byte, error) {
	underImg, uErr := decodePNG(under)
	overImg, oErr := decodePNG(over)
	switch {
	case uErr != nil && oErr != nil:
		return nil, fmt.Errorf("composite: both layers undecodable: under=%v over=%v", uErr, oErr)
	case uErr != nil:
		return over, nil
	case oErr != nil:
		return under, nil
	}
	b := underImg.Bounds()
	canvas := image.NewRGBA(b)
	draw.Draw(canvas, b, underImg, b.Min, draw.Src)
	draw.Draw(canvas, overImg.Bounds(), overImg, overImg.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodePNG(b []byte) (image.Image, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty png")
	}
	return png.Decode(bytes.NewReader(b))
}

// splitRings reconstructs ring boundaries in an S-57 polygon coordinate array.
// The s57 library concatenates every ring (outer + holes, or multiple disjoint
// outer rings) into one flat [][]float64 with no separator — see
// internal/parser/geometry.go in beetlebugorg/s57 — and each constituent ring
// is self-closed (first vertex == last vertex). Callers that don't split end
// up drawing a stray diagonal LineTo from one ring's last vertex to the next
// ring's first vertex, which paints as a triangular wedge inside the polygon.
//
// We detect a ring boundary by watching for a vertex equal to the current
// ring's start: that's the closing vertex, and the next vertex begins a new
// ring. A trailing fragment with no closure (malformed input) is returned as
// its own ring rather than dropped, so we still paint something.
func splitRings(coords [][]float64) [][][]float64 {
	var rings [][][]float64
	if len(coords) == 0 {
		return rings
	}
	start := 0
	for i := 1; i < len(coords); i++ {
		if len(coords[i]) < 2 || len(coords[start]) < 2 {
			continue
		}
		if coords[i][0] == coords[start][0] && coords[i][1] == coords[start][1] {
			rings = append(rings, coords[start:i+1])
			start = i + 1
			i = start // loop's i++ will advance past the new ring's start vertex
		}
	}
	if start < len(coords) {
		rings = append(rings, coords[start:])
	}
	return rings
}

// structurePhantomJumpM is the consecutive-vertex distance above which a
// BRIDGE/CBLOHD/PIPOHD/CONVYR/CAUSWY polygon or linestring is treated as the
// s57 library's edge-concatenation fallback rather than a coherent feature.
// When polygon topology fails to resolve, that library dumps each edge's
// raw coords end-to-end with no shared-node bridging — see geometry.go's
// constructPolygonGeometry fallback path — producing a phantom-diagonal
// zigzag that splitRings can't unpack because the synthetic "ring" still
// self-closes vertex-equality at start. Real bridge/cable footprints have
// vertex spacing under ~100 m even at 1:80k cells, so 300 m is unambiguous.
const structurePhantomJumpM = 300.0

// phantomMinVertices is the smallest vertex count a phantom BRIDGE feature
// must have for the farthest-pair span fallback (see Structures). Detailed-cell
// bridge footprints that fragment carry 8+ vertices; coarse overview-cell
// centerlines are 2–3 points and below this floor, so they drop instead of
// painting misplaced diagonals.
const phantomMinVertices = 6

// phantomSpanCapM bounds the farthest-pair fallback used to recover a long
// bridge's spanning deck from a phantom feature (see Structures). No single
// BRIDGE object in NOAA ENCs spans more than ~2 km; a farthest-pair distance
// beyond this means the phantom concatenated genuinely unrelated features, so
// we decline to draw a diagonal joining them.
const phantomSpanCapM = 3000.0

// pathLengthM sums the ground distance along a coord run in metres.
func pathLengthM(coords [][]float64) float64 {
	var sum float64
	for i := 1; i < len(coords); i++ {
		if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
			continue
		}
		sum += haversineMeters(coords[i-1][1], coords[i-1][0], coords[i][1], coords[i][0])
	}
	return sum
}

// farthestPairM returns the greatest ground distance between any two vertices
// and the pair itself. For a long thin structure (a bridge deck) the farthest
// pair are its two ends, so the segment a→b approximates the deck axis even
// when the vertex ordering is scrambled by the s57 phantom-edge fallback.
// O(n^2); structure features carry only a handful of vertices.
func farthestPairM(coords [][]float64) (dist float64, a, b []float64) {
	for i := 0; i < len(coords); i++ {
		if len(coords[i]) < 2 {
			continue
		}
		for j := i + 1; j < len(coords); j++ {
			if len(coords[j]) < 2 {
				continue
			}
			d := haversineMeters(coords[i][1], coords[i][0], coords[j][1], coords[j][0])
			if d > dist {
				dist, a, b = d, coords[i], coords[j]
			}
		}
	}
	return dist, a, b
}

// classNeedsPhantomEdgeRepair reports whether splitPathOnLongJumps /
// hasPhantomEdge should be applied to features of this class. The phantom-
// jump filter exists to repair the s57 lib's edge-concatenation fallback
// (see structurePhantomJumpM) — a bug that's only been observed on bridge /
// overhead-cable / overhead-pipe / conveyor / causeway features. Applying
// the filter universally silently drops legitimate sparse-vertex linework
// on overview cells: e.g. a 1:1.2M COALNE or DEPCNT can carry consecutive
// vertices ~1 km apart by design, and the 300 m guard splits every edge
// into single-vertex runs that get discarded.
func classNeedsPhantomEdgeRepair(class string) bool {
	switch class {
	case "BRIDGE", "CBLOHD", "PIPOHD", "CONVYR", "CAUSWY":
		return true
	}
	return false
}

// overviewClipEdgeM is the per-edge threshold above which a polygon's
// max-edge counts as "cell-boundary-scale": s57 polygon rings that thread
// along a cell edge to close the polygon clip carry one or more multi-km
// straight segments. Real navigation features inside a single ENC cell
// stay well under this even at coastline detail, so a polygon with an
// edge this big is almost certainly an overview-cell ring whose detail
// finer cells should supply.
const overviewClipEdgeM = 5000.0

// hasCellBoundaryClipEdge reports whether any consecutive pair of vertices in
// coords share an exact lon (vertical cell-boundary edge) or exact lat
// (horizontal) and are at least minMeters apart. The signature of an
// overview-cell polygon whose outer ring was clipped to the ENC cell's
// bounding box: the ring threads along the boundary at constant lat or lon
// for kilometres. Real geographic features almost never produce two
// consecutive vertices with identical lat-or-lon to 6+ decimal places, so
// this is a much sharper detector than raw edge length — distinguishes
// "the polygon is honestly long" from "the polygon is honestly long AND
// includes synthetic cell-clip segments finer cells should supersede".
func hasCellBoundaryClipEdge(coords [][]float64, minMeters float64) bool {
	if len(coords) < 2 {
		return false
	}
	minSq := minMeters * minMeters
	for i := 1; i < len(coords); i++ {
		if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
			continue
		}
		sameLon := coords[i][0] == coords[i-1][0]
		sameLat := coords[i][1] == coords[i-1][1]
		if !sameLon && !sameLat {
			continue
		}
		midLat := (coords[i][1] + coords[i-1][1]) / 2
		const mPerDegLat = 111_320.0
		mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
		dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
		dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
		if dlon*dlon+dlat*dlat > minSq {
			return true
		}
	}
	return false
}

// hasPhantomEdge reports whether any consecutive pair of vertices in coords
// exceeds maxMeters of ground distance. Used to decide whether to apply
// phantom-segment rendering paths (split-and-stroke for line features, skip
// for fill polygons whose ring topology is too broken to repair).
func hasPhantomEdge(coords [][]float64, maxMeters float64) bool {
	if len(coords) < 2 {
		return false
	}
	maxSq := maxMeters * maxMeters
	for i := 1; i < len(coords); i++ {
		if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
			continue
		}
		midLat := (coords[i][1] + coords[i-1][1]) / 2
		const mPerDegLat = 111_320.0
		mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
		dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
		dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
		if dlon*dlon+dlat*dlat > maxSq {
			return true
		}
	}
	return false
}

// splitPathOnLongJumps walks a flat coord list, returning sub-runs split at
// every pair of consecutive vertices whose ground-distance exceeds maxMeters
// plus a flag indicating whether any such jump was found. Used to repair
// s57-lib edge concatenations that left phantom diagonals between
// disconnected sub-features (see structurePhantomJumpM). Runs shorter than 2
// vertices are dropped so the caller can stroke each as an open subpath
// without spurious points; the split flag lets the caller distinguish "no
// phantom jumps, draw normally" from "every fragment was too short to keep,
// don't draw at all".
func splitPathOnLongJumps(coords [][]float64, maxMeters float64) (runs [][][]float64, split bool) {
	if len(coords) == 0 {
		return nil, false
	}
	maxSq := maxMeters * maxMeters
	start := 0
	for i := 1; i < len(coords); i++ {
		if len(coords[i]) < 2 || len(coords[i-1]) < 2 {
			continue
		}
		midLat := (coords[i][1] + coords[i-1][1]) / 2
		const mPerDegLat = 111_320.0
		mPerDegLon := mPerDegLat * math.Cos(midLat*math.Pi/180)
		dlon := (coords[i][0] - coords[i-1][0]) * mPerDegLon
		dlat := (coords[i][1] - coords[i-1][1]) * mPerDegLat
		if dlon*dlon+dlat*dlat > maxSq {
			split = true
			if i-start >= 2 {
				runs = append(runs, coords[start:i])
			}
			start = i
		}
	}
	if len(coords)-start >= 2 {
		runs = append(runs, coords[start:])
	}
	return runs, split
}

// strokeOpenSubpaths traces each run as an open (non-closing) subpath onto dc
// and emits a single Stroke. Companion to splitPathOnLongJumps for malformed
// structure features where we want to draw the constituent edge fragments
// but not paint a closing line that would re-introduce a phantom diagonal.
func strokeOpenSubpaths(dc *gg.Context, runs [][][]float64, project func(lon, lat float64) (float64, float64), stroke color.Color, width float64) {
	if len(runs) == 0 {
		return
	}
	for _, run := range runs {
		started := false
		dc.NewSubPath()
		for _, c := range run {
			if len(c) < 2 {
				continue
			}
			px, py := project(c[0], c[1])
			if !started {
				dc.MoveTo(px, py)
				started = true
			} else {
				dc.LineTo(px, py)
			}
		}
	}
	dc.SetColor(stroke)
	dc.SetLineWidth(width)
	dc.Stroke()
}

// ringSignedArea returns the signed shoelace area of a single ring (positive
// for CCW, negative for CW under image y-down). Used to pick the dominant ring
// for centroid seeding.
func ringSignedArea(ring [][]float64) float64 {
	var sum float64
	for i := range ring {
		if len(ring[i]) < 2 {
			continue
		}
		j := (i + 1) % len(ring)
		if len(ring[j]) < 2 {
			continue
		}
		sum += ring[i][0]*ring[j][1] - ring[j][0]*ring[i][1]
	}
	return sum / 2
}

// polygonCentroid returns the area-weighted centroid of a polygon. For
// multi-ring polygons it returns the centroid of the largest-area ring, which
// is what we want when seeding a single representative point (e.g. for IDW).
// Falls back to the simple coordinate mean if all rings are degenerate.
func polygonCentroid(coords [][]float64) (float64, float64) {
	if len(coords) == 0 {
		return 0, 0
	}
	rings := splitRings(coords)
	if len(rings) == 0 {
		rings = [][][]float64{coords}
	}
	var best [][]float64
	bestArea := -1.0
	for _, ring := range rings {
		a := math.Abs(ringSignedArea(ring))
		if a > bestArea {
			bestArea = a
			best = ring
		}
	}
	if best == nil {
		best = coords
	}
	var sumX, sumY, sumA float64
	for i := range best {
		if len(best[i]) < 2 {
			continue
		}
		j := (i + 1) % len(best)
		if len(best[j]) < 2 {
			continue
		}
		x0, y0 := best[i][0], best[i][1]
		x1, y1 := best[j][0], best[j][1]
		cross := x0*y1 - x1*y0
		sumA += cross
		sumX += (x0 + x1) * cross
		sumY += (y0 + y1) * cross
	}
	if math.Abs(sumA) < 1e-12 {
		// Degenerate ring; fall back to vertex mean over all coords.
		var mx, my float64
		var n int
		for _, c := range coords {
			if len(c) < 2 {
				continue
			}
			mx += c[0]
			my += c[1]
			n++
		}
		if n == 0 {
			return 0, 0
		}
		return mx / float64(n), my / float64(n)
	}
	a := sumA / 2
	return sumX / (6 * a), sumY / (6 * a)
}

// tracePolygonPath walks every ring of a flattened multi-ring polygon onto dc,
// emitting each ring as its own subpath so the renderer never draws a stray
// connecting edge between rings. Caller is responsible for Fill/Stroke.
func tracePolygonPath(dc *gg.Context, coords [][]float64, project func(lon, lat float64) (float64, float64)) {
	rings := splitRings(coords)
	if len(rings) == 0 {
		return
	}
	for _, ring := range rings {
		started := false
		dc.NewSubPath()
		for _, c := range ring {
			if len(c) < 2 {
				continue
			}
			px, py := project(c[0], c[1])
			if !started {
				dc.MoveTo(px, py)
				started = true
			} else {
				dc.LineTo(px, py)
			}
		}
		dc.ClosePath()
	}
}

// isOversizedPolygon returns true if the polygon's lon/lat bbox is more than
// `maxFactor`× the tile bbox in either direction. NOAA overview cells carry
// continent-sized rings — a single LNDARE that covers the whole SE US, a
// CTNARE that covers half the Atlantic — and rendering them at chart-detail
// zoom paints huge tan/grey areas over genuine water. Intermediate-scale
// cells carry similar oversized rings (Florida-coast LNDARE, ~1° wide) which
// also blot out marina basins where finer cells provide the proper detail.
//
// The threshold is relative to the tile size so the same heuristic works at
// all zooms: at z=16 a 0.3° polygon is way too coarse, but at z=10 a 0.3°
// polygon is the right level of detail. At zooms where overview cells are
// the *only* coverage the tile is so big the polygon doesn't qualify as
// oversized and we'll still render it.
func isOversizedPolygon(coords [][]float64, tileBbox s57.Bounds, maxFactor float64) bool {
	if len(coords) < 3 {
		return false
	}
	minX, maxX := coords[0][0], coords[0][0]
	minY, maxY := coords[0][1], coords[0][1]
	for _, c := range coords {
		if len(c) < 2 {
			continue
		}
		if c[0] < minX {
			minX = c[0]
		}
		if c[0] > maxX {
			maxX = c[0]
		}
		if c[1] < minY {
			minY = c[1]
		}
		if c[1] > maxY {
			maxY = c[1]
		}
	}
	tileW := tileBbox.MaxLon - tileBbox.MinLon
	tileH := tileBbox.MaxLat - tileBbox.MinLat
	return (maxX-minX) > tileW*maxFactor || (maxY-minY) > tileH*maxFactor
}

// isDegeneratePixelPolygon returns true if the polygon's projected pixel bbox
// is too narrow in either direction to represent real chart geometry. Used to
// suppress thin diagonal "slash" artifacts where the s57 lib produced a ring
// with only a few near-collinear points.
func isDegeneratePixelPolygon(coords [][]float64, project func(lon, lat float64) (float64, float64)) bool {
	if len(coords) < 3 {
		return true
	}
	const minPx = 3.0
	first := true
	var minX, maxX, minY, maxY float64
	for _, c := range coords {
		if len(c) < 2 {
			continue
		}
		px, py := project(c[0], c[1])
		if first {
			minX, maxX = px, px
			minY, maxY = py, py
			first = false
			continue
		}
		if px < minX {
			minX = px
		}
		if px > maxX {
			maxX = px
		}
		if py < minY {
			minY = py
		}
		if py > maxY {
			maxY = py
		}
	}
	if first {
		return true
	}
	return (maxX-minX) < minPx || (maxY-minY) < minPx
}

// cellScaleRangeFor returns the CScale window we want to render at the given
// map zoom: [minScale, maxScale]. Cells outside this window are filtered out.
//
//   - minScale: 1/2 of the display scale — drops cells finer than the display
//     can sensibly show. At z=11 this drops 1:80 000 Approach cells whose
//     coastlines have thousands of vertices that turn into solid black
//     squiggles when smashed into a few pixels.
//   - maxScale: 8× the display scale — drops continent-sized overview cells
//     at high zooms. Without this, an LNDARE from a 1:1 200 000 cell paints
//     yellow over a z=16 harbour tile that the local Berthing cell would
//     have outlined more precisely.
//
// Computed for `latDeg` so high-latitude tiles use the right mercator scale.
func cellScaleRangeFor(z int, latDeg float64) (int, int) {
	const screenMetresPerPx = 0.000264 // 96 DPI ≈ 0.000264 m / px
	cosLat := math.Cos(latDeg * math.Pi / 180)
	if cosLat < 0.1 {
		cosLat = 0.1
	}
	groundResPerPx := 156543.04 * cosLat / math.Pow(2, float64(z))
	displayScale := groundResPerPx / screenMetresPerPx
	// Lower bound: zoom-tuned divisor on the display scale:
	//   z≤9   → 8.0 (need 1:200 k+ cells; otherwise the overview is too
	//           sparse to read as a chart at all — NOAA shows full chart
	//           detail at z=9 even though our pixels are huge)
	//   z=10+ → 2.0 (chart-detail; balances coastline cleanness vs feature
	//           density)
	div := 2.0
	switch {
	case z <= 6:
		// Floor must drop low enough to admit the 1:675k overview cell
		// (US2EC04M-style band-2 coverage). z=6 displayScale ≈ 7.2M, so
		// div=12 yields a floor around 600k. Band-2 cells are still
		// continent-scale so they tile cleanly — no seam risk.
		div = 12.0
	case z <= 9:
		div = 8.0
	case z == 12:
		// displayScale/2 floor at z=12 lands above 1:20 000, dropping
		// Harbor-scale cells with detailed channel DEPARE polygons —
		// Reynolds Channel and similar reads as shaded because only the
		// coarser Approach cell's wide-depth-range polygon paints here.
		// div=3 pulls 1:20k cells in.
		div = 3.0
	case z == 13:
		// Same issue, but at z=13 we also want NOAA's 1:12 000 city-harbor
		// cells (Charleston US5CHSDD/DE etc.) to load — their narrow-band
		// DEPARE polygons are what makes deep channels render as DEPDW
		// (white) instead of getting blanketed by the 1:45k Approach
		// cell's single "0–6 ft" polygon. 1:12 000 requires div≥5.
		div = 5.0
	case z == 14:
		// At z=14 displayScale ≈ 30 000, so div=2 floors minScale at
		// ~15k — excludes the 1:12 000 city-harbor cells whose
		// narrow-band polygons are needed for proper channel shading.
		// div=3 lowers minScale to ~10k, pulling them in.
		div = 3.0
	}
	min := int(displayScale / div)
	if min < 0 {
		min = 0
	}
	// Upper bound: drop continent-sized overview cells from the regular
	// rendering pass at z≥12 (8× display scale). Their giant DEPARE rings
	// paint blanket water over finer-cell harbour detail — e.g. at z=12 a
	// 1:1 200 000 "0-18 m" polygon covering the whole East Coast otherwise
	// paints every Chesapeake-area tile DEPDW white. LNDARE/BUAARE
	// coverage from the same overview cell still reaches the tile via the
	// all-cells area-fill base pass.
	//
	// At z≥14 we tighten further (×4) so coastal cells (~1:80–150 k) are
	// also excluded from DEPARE rendering. Their wide-range "0–6 ft"
	// polygon otherwise paints blanket DEPMS over Charleston-style
	// harbours where harbour cells have varied narrow-band polygons that
	// should mostly read as DEPDW (white) — matches the z=15 cell-window
	// shape and gives consistent z=14/z=15 shading.
	//
	// Below z=12 we don't cap — at z=10/11 those overview cells are the
	// primary source of any land or water coverage at all.
	max := 0
	switch {
	case z >= 14:
		max = int(displayScale * 4)
	case z >= 12:
		max = int(displayScale * 8)
	}
	if max < 0 {
		max = 0
	}
	return min, max
}

// zoomSymbolScale returns a multiplier applied to stroke widths and point
// symbol sizes. Goal: symbols stay readable when zoomed in but don't blow up
// and crash into each other. Conservative — anything over ~2× starts looking
// cartoonish next to the underlying chart density.
func zoomSymbolScale(z int) float64 {
	switch {
	case z >= 18:
		return 2.2
	case z == 17:
		return 1.8
	case z == 16:
		return 1.5
	case z == 15:
		return 1.2
	case z == 14:
		return 1.1
	default:
		return 1.0
	}
}

// drawFeature dispatches to the right pass based on the feature's object class
// and geometry. Anything we don't recognise is silently skipped. `scale` is a
// zoom-derived multiplier applied to stroke widths and symbol sizes.
// `safeDepthM` controls DEPARE depth-band colouring. `tileBbox` is used to
// reject polygons that are way larger than the tile (overview-cell rings).
func drawFeature(dc *gg.Context, f encFeature, pass drawPass, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, tileBbox s57.Bounds, z int, style RenderStyle) {
	geom := f.Geometry()
	if len(geom.Coordinates) == 0 {
		return
	}
	class := f.ObjectClass()
	if z < noaa.MinZoomForObjectClass(class) {
		return
	}

	// Place-name labels for major area features are handled by the
	// tile-level dedup pass in RenderTile (z≥10), which picks the best
	// instance per name across all overlapping cells. We used to also
	// paint them per-cell here at z≥13, but that double-rendered every
	// label once that dedup pass kicked in.
	if pass == passPoints {
		switch class {
		case "LNDARE", "LNDRGN", "BUAARE", "SEAARE", "ADMARE", "BUISGL":
			return
		}
	}

	switch geom.Type {
	case s57.GeometryTypePolygon:
		// Polygons paint in BOTH passAreas (fill) and passLines (ring stroke).
		// Filling some classes is wrong (FAIRWY, ACHARE, DOCARE, ...) but their
		// boundary is what charts actually show, so stroke the ring whenever
		// lineStroke has an entry. This also covers fill+stroke on classes
		// where both are styled.
		fill := areaFill(class, f, safeDepthM, z)
		stroke, width := lineStroke(class, f, safeDepthM, style, z)

		// Drop polygons whose projected pixel bbox is degenerate (< 3 px in
		// either direction). The s57 lib occasionally produces thin/collinear
		// rings — typically as DEPARE — that paint as an unsightly diagonal
		// sliver in open water. Real DEPARE polygons cover meaningful area; a
		// 2- or 3-vertex sliver represents nothing the user should see.
		if isDegeneratePixelPolygon(geom.Coordinates, project) {
			return
		}
		// Drop continent-sized rings from overview cells (e.g. an LNDARE that
		// covers the whole SE US): they paint tan over real water at chart
		// zoom. See isOversizedPolygon for why this is safe.
		// 200× tile passes regional overview-cell polygons (barrier-island
		// strings, big harbour LNDARE) while still rejecting continent-
		// sized rings that would paint blanket land over open ocean.
		if isOversizedPolygon(geom.Coordinates, tileBbox, 200) {
			return
		}
		switch pass {
		case passAreas:
			if fill == nil {
				return
			}
			// Compact built-up-area polygons (BUAARE / BUISGL) and bridge
			// footprints occasionally come back from the s57 lib as
			// concatenated raw edges when polygon topology resolution
			// falls through — the resulting "polygon" self-crosses, and
			// the even-odd fill paints X-shaped wedges across the chart.
			// Can't repair the topology from a flat coord list, so skip
			// the fill entirely when an unmistakable phantom edge is
			// present. Scope to small compact-area classes so legitimate
			// long-edged depth/coastline polygons aren't suppressed.
			switch class {
			case "BUAARE", "BUISGL":
				if hasPhantomEdge(geom.Coordinates, structurePhantomJumpM) {
					return
				}
			}
			// NOTE: the old cell-boundary-clip guard (drop polygons whose
			// boundary runs along the ENC cell rectangle) is intentionally
			// gone. It was written for the disk renderer's per-cell painting
			// and, in the Mongo pipeline, wrongly dropped legitimate coastal/
			// approach-cell LNDARE — leaving land white at z<=11. Continent-
			// scale overview rings (the real wedge culprits) are still caught
			// by isOversizedPolygon(…, 200) above; the coarse→fine paint order
			// and per-feature minZoom handle the rest. Verified wedge-free at
			// z=7..16 against NOAA WMS.
			tracePolygonPath(dc, geom.Coordinates, project)
			dc.Push()
			dc.SetFillRuleEvenOdd()
			dc.SetColor(fill)
			dc.Fill()
			dc.Pop()
		case passLines:
			if stroke == nil {
				return
			}
			// Phantom-segment guard for the structure classes whose s57-lib
			// output sometimes concatenates raw edges with multi-km jumps
			// between disconnected sub-features. Scoped narrowly because
			// overview-cell coastline polygons legitimately carry vertex
			// spacings well above structurePhantomJumpM.
			if classNeedsPhantomEdgeRepair(class) {
				if runs, split := splitPathOnLongJumps(geom.Coordinates, structurePhantomJumpM); split {
					if len(runs) > 0 {
						strokeOpenSubpaths(dc, runs, project, stroke, width*scale)
					}
					return
				}
			}
			tracePolygonPath(dc, geom.Coordinates, project)
			dc.SetColor(stroke)
			dc.SetLineWidth(width * scale)
			dc.Stroke()
		default:
			return
		}

	case s57.GeometryTypeLineString:
		// DEPCNT depth contours get their VALDCO labeled in the points
		// pass so the text paints on top of any line that crosses it.
		// The 60-px arc-length guard inside drawContourLabel keeps
		// short fragments from cluttering — at z=10 that's ~5 nm of
		// contour minimum, which leaves the labels meaningful without
		// spamming.
		if pass == passPoints && class == "DEPCNT" && z >= 10 {
			drawContourLabel(dc, f, project, scale, z)
			return
		}
		if pass != passLines {
			return
		}
		stroke, width := lineStroke(class, f, safeDepthM, style, z)
		if stroke == nil {
			return
		}
		// Phantom-segment guard, scoped to the structure classes that need it
		// — see classNeedsPhantomEdgeRepair. Generic line classes (COALNE,
		// DEPCNT, RIVERS, ...) can have legitimate >300 m vertex gaps in
		// overview cells and must NOT be split here.
		if classNeedsPhantomEdgeRepair(class) {
			if runs, split := splitPathOnLongJumps(geom.Coordinates, structurePhantomJumpM); split {
				if len(runs) > 0 {
					strokeOpenSubpaths(dc, runs, project, stroke, width*scale)
				}
				return
			}
		}
		dc.NewSubPath()
		for i, c := range geom.Coordinates {
			if len(c) < 2 {
				continue
			}
			px, py := project(c[0], c[1])
			if i == 0 {
				dc.MoveTo(px, py)
			} else {
				dc.LineTo(px, py)
			}
		}
		dc.SetColor(stroke)
		dc.SetLineWidth(width * scale)
		dc.Stroke()

	case s57.GeometryTypePoint:
		if pass != passPoints {
			return
		}
		drawPoint(dc, class, f, project, scale, safeDepthM, style, z)
	}
}

// drawNavaidLabel paints the navaid's OBJNAM (e.g. "G "5"", "RG TC") next
// to the symbol. NOAA renders short buoy/beacon identifiers like that
// inline; we skip long descriptive names to avoid cluttering tiles.
func drawNavaidLabel(dc *gg.Context, f encFeature, px, py, scale float64) {
	v, ok := f.Attribute("OBJNAM")
	if !ok {
		return
	}
	name, _ := v.(string)
	if name == "" {
		return
	}
	// Skip long descriptive names — NOAA only labels short identifiers
	// (e.g. "G "5"", "RG TC"). 12 chars is a generous upper bound that
	// keeps short codes and rejects "Beaufort Harbor Channel Light".
	if len(name) > 12 {
		return
	}
	dc.SetFontFace(basicfont.Face7x13)
	dc.SetColor(s52CHBLK)
	tx := px + 5*scale
	ty := py
	dc.Push()
	dc.ScaleAbout(scale*0.7, scale*0.7, tx, ty)
	dc.DrawStringAnchored(name, tx, ty, 0, 0.5)
	dc.Pop()
}

// drawLightLabel paints the abbreviated S-52 light description next to the
// flare, e.g. "F G 65ft" — colour + character + height. NOAA renders these
// inline; matching them is the single biggest visible gap at z=13–15.
func drawLightLabel(dc *gg.Context, f encFeature, px, py, scale float64) {
	parts := []string{}
	// Character (LITCHR = 1..28 in S-57). 1=Fixed, 2=Flashing, 4=Quick, etc.
	switch intAttr(f, "LITCHR") {
	case 1:
		parts = append(parts, "F")
	case 2, 3:
		parts = append(parts, "Fl")
	case 4, 5, 6:
		parts = append(parts, "Q")
	case 7, 8:
		parts = append(parts, "Iso")
	case 9, 10:
		parts = append(parts, "Oc")
	case 11:
		parts = append(parts, "Mo")
	case 12, 13:
		parts = append(parts, "FFl")
	}
	// Colour code(s) — first letter only (R/G/W/Y).
	if v, ok := f.Attribute("COLOUR"); ok {
		if s, _ := v.(string); s != "" {
			letters := ""
			for _, c := range strings.Split(s, ",") {
				switch strings.TrimSpace(c) {
				case "1":
					letters += "W"
				case "3":
					letters += "R"
				case "4":
					letters += "G"
				case "6":
					letters += "Y"
				}
			}
			if letters != "" {
				parts = append(parts, letters)
			}
		}
	}
	// Height in feet. HEIGHT is in metres in S-57; convert.
	if v, ok := f.Attribute("HEIGHT"); ok {
		hM := numAttr(v)
		if !math.IsNaN(hM) && hM > 0 {
			parts = append(parts, fmt.Sprintf("%dft", int(math.Round(hM*feetPerMetre))))
		}
	}
	if len(parts) == 0 {
		return
	}
	label := strings.Join(parts, " ")
	dc.SetFontFace(basicfont.Face7x13)
	dc.SetColor(s52CHMGD)
	tx := px + 4*scale
	ty := py - 4*scale
	dc.Push()
	dc.ScaleAbout(scale*0.65, scale*0.65, tx, ty)
	dc.DrawStringAnchored(label, tx, ty, 0, 0.5)
	dc.Pop()
}

// drawContourLabel paints the depth value on a DEPCNT line at the line's
// arc-length midpoint, in feet, in the contour stroke color. Short-fragment
// guard skips labeling tiny pieces that would just clutter the tile, but
// long contours that get split across multiple features still get one label
// each — that matches NOAA's repeated labeling of long contour stretches.
func drawContourLabel(dc *gg.Context, f encFeature, project func(lon, lat float64) (float64, float64), scale float64, z int) {
	v, ok := f.Attribute("VALDCO")
	if !ok {
		return
	}
	val := numAttr(v)
	if math.IsNaN(val) || val <= 0 {
		return
	}
	coords := f.Geometry().Coordinates
	if len(coords) < 2 {
		return
	}

	// Project all vertices once and walk arc length to find the midpoint.
	type pt struct{ x, y float64 }
	pts := make([]pt, 0, len(coords))
	for _, c := range coords {
		if len(c) < 2 {
			continue
		}
		x, y := project(c[0], c[1])
		pts = append(pts, pt{x, y})
	}
	if len(pts) < 2 {
		return
	}
	var total float64
	segLens := make([]float64, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		dx := pts[i].x - pts[i-1].x
		dy := pts[i].y - pts[i-1].y
		segLens[i-1] = math.Sqrt(dx*dx + dy*dy)
		total += segLens[i-1]
	}
	// Skip stubby fragments — they'd produce a label the user can't tie
	// back to a recognizable line.
	if total < 60 {
		return
	}

	// Walk to the arc-length midpoint and interpolate within that segment.
	half := total / 2
	var px, py float64
	acc := 0.0
	for i, segLen := range segLens {
		if acc+segLen >= half {
			t := (half - acc) / segLen
			px = pts[i].x + t*(pts[i+1].x-pts[i].x)
			py = pts[i].y + t*(pts[i+1].y-pts[i].y)
			break
		}
		acc += segLen
	}

	// Off-tile / edge-clipped guard.
	if px < 6 || px > 250 || py < 6 || py > 250 {
		return
	}

	ft := val * feetPerMetre
	label := fmt.Sprintf("%d", int(math.Round(ft)))

	// Bump font at low zoom so the integer reads on a sparse overview tile.
	// Slightly less aggressive than place-name labels — depth values are
	// short (1-3 chars) so they stay readable at smaller scales than a
	// 14-char place name.
	labelScale := scale
	switch {
	case z <= 10:
		labelScale = scale * 1.5
	case z == 11:
		labelScale = scale * 1.3
	case z == 12:
		labelScale = scale * 1.15
	}

	dc.SetFontFace(basicfont.Face7x13)
	dc.SetColor(s52DEPCN)
	dc.Push()
	dc.ScaleAbout(labelScale, labelScale, px, py)
	dc.DrawStringAnchored(label, px, py, 0.5, 0.5)
	dc.Pop()
}

// S-52 day-bright palette. RGB values were sampled directly from cached NOAA
// WMS tiles so our renders blend cleanly against the WMS reference layer.
// The compare endpoint + TestCompareWithWMS test surface any palette drift
// as it happens.
var (
	// Water fills, four-band scheme keyed off the boat's safety contour.
	// All four shades appear in the NOAA WMS sample data: AFCDE1 (below
	// safety), D1DDEF / DDEAF7 (transition zone past the safety contour),
	// pure white (well past safety / open ocean). DDEAF7 in particular is
	// the dominant colour in offshore tiles — going 2-band forced us to
	// paint those tiles either fully white or fully AFCDE1, neither right.
	s52DEPDW = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF} // safe water (≥ 4× safety)
	s52DEPMD = color.RGBA{0xDD, 0xEA, 0xF7, 0xFF} // medium-deep (2× to 4× safety)
	s52DEPMS = color.RGBA{0xD1, 0xDD, 0xEF, 0xFF} // medium-shallow (safety to 2× safety)
	s52DEPVS = color.RGBA{0xAF, 0xCD, 0xE1, 0xFF} // unsafe (< safety)
	s52DEPIT = color.RGBA{0xD6, 0xDB, 0xC9, 0xFF} // intertidal / drying — pale tan-green

	// Land + coast.
	s52LANDA = color.RGBA{0xF4, 0xE8, 0xC1, 0xFF} // land area (warm pale yellow)
	s52CSTLN = color.RGBA{0x00, 0x00, 0x00, 0xFF} // coastline (black)

	// Generic chart colours.
	s52CHBLK = color.RGBA{0x00, 0x00, 0x00, 0xFF}
	s52CHGRD = color.RGBA{0x72, 0x72, 0x72, 0xFF} // grey (medium)
	s52CHGRF = color.RGBA{0xA2, 0xA2, 0xA2, 0xFF} // grey (faint)
	s52CHWHT = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	s52CHRED = color.RGBA{0xDC, 0x14, 0x14, 0xFF}
	s52CHGRN = color.RGBA{0x14, 0xB4, 0x14, 0xFF}
	s52CHYLW = color.RGBA{0xEF, 0xE7, 0x39, 0xFF} // bright chart yellow (navaid accents)
	s52CHMGD = color.RGBA{0xDB, 0x49, 0x96, 0xFF} // chart magenta — pink, NOT pure magenta
	s52CHBRN = color.RGBA{0x82, 0x5A, 0x23, 0xFF}

	// Soundings + depth contours.
	s52SNDG1 = color.RGBA{0x72, 0x72, 0x72, 0xFF} // sounding label (deeper than safety)
	s52SNDG2 = color.RGBA{0x00, 0x00, 0x00, 0xFF} // sounding label (shoaler than safety, bolder)
	s52DEPCN = color.RGBA{0x83, 0x99, 0xA8, 0xFF} // depth contour line (subtle steel blue)
	s52DEPSC = color.RGBA{0x57, 0x66, 0x70, 0xFF} // safety contour line (bolder)

	// Light/buoy accents.
	s52LITYW = color.RGBA{0xEF, 0xE7, 0x39, 0xFF} // yellow light flare
)

// areaFill returns the fill colour for a polygon feature, or nil to skip.
// safeDepthM keys the DEPARE four-colour band scheme.
//
// We fill the area classes that S-52 colours: depth areas (banded by depth),
// drying (intertidal) areas, land, dredged areas, lock basins. Outline-only
// classes (fairways, anchorages, harbour areas, docks, pontoons, pipeline
// areas) are handled in lineStroke.
func areaFill(class string, f encFeature, safeDepthM float64, z int) color.Color {
	switch class {
	case "DEPARE":
		min, max := depthRange(f)
		if !math.IsNaN(min) && min < 0 {
			return s52DEPIT
		}
		// Key on the polygon's midpoint depth so a "6.6–16.4 ft" channel
		// polygon (DRVAL1=2 m, DRVAL2=5 m, mid=3.5 m) lands in the band
		// that matches a 10 ft depth — DEPMD for default draft — instead
		// of being collapsed to DEPDW by a DRVAL2-only key. Midpoint also
		// reads sensibly for wide-range overview polygons (0–18 m mid=9 m
		// → DEPDW, correctly painting open ocean white).
		var key float64
		switch {
		case !math.IsNaN(min) && !math.IsNaN(max):
			key = (min + max) / 2
		case !math.IsNaN(max):
			key = max
		case !math.IsNaN(min):
			key = min
		default:
			return s52DEPDW
		}
		return depthFill(key, safeDepthM, z)
	case "DRGARE":
		// Dredged area: maintained safe-depth channel. NOAA's WMS paints
		// these as DEPDW (white) — the channel is "above safety" by virtue
		// of being dredged regardless of the surrounding shallow water's
		// DRVAL1.
		_ = z
		return s52DEPDW
	case "LNDARE":
		return s52LANDA
	case "BUAARE", "BUISGL":
		// Built-up area / single conspicuous building: NOAA paints both
		// with the same saturated yellow ochre, distinct from regular
		// LANDA so harbour structures stand out.
		return color.RGBA{0xEF, 0xD8, 0xA3, 0xFF}
	case "LOKBSN":
		return s52DEPVS
	case "UNSARE":
		// Unsurveyed area: pale grey, mostly transparent. Use NRGBA
		// (non-premultiplied) because color.RGBA is alpha-premultiplied
		// and 0xE0 > 0xC0 would be an invalid premultiplied value —
		// gg's blender renders that as black when multiple polygons
		// stack (e.g. dense harbour-cell UNSARE in NY harbor).
		return color.NRGBA{0xE0, 0xE0, 0xE0, 0xC0}
	}
	return nil
}

// s52ShallowContourM is the DEPVS/DEPMS boundary at z≥12. At 1 m, a
// "0–6 ft" NOAA harbour-cell polygon (DRVAL2 ≈ 1.83 m) lands in DEPMS
// (lighter blue) instead of DEPVS (saturated). DEPVS is reserved for
// drying-edge polygons and the z≤11 shallow-edge override.
//
// Everything else is keyed off the boat's `draft` (in metres):
//
//	z≥12  DEPVS   < 1 m
//	      DEPMS   1 m … draft
//	      DEPMD   draft … 2×draft
//	      DEPDW   ≥ 2×draft   (safe water, white)
//	z≤11  DEPVS   < 2×draft
//	      DEPDW   ≥ 2×draft
const s52ShallowContourM = 1.0

// depthFill returns the water fill for a DEPARE polygon, keyed off the
// boat's draft (metres).
//
//	z≥12  DEPVS   0   … 1 m         (saturated; very shallow / drying-ish)
//	      DEPMS   1 m … draft       (below the boat's draft — warning)
//	      DEPMD   draft … 2×draft   (just above draft — caution)
//	      DEPDW   ≥ 2×draft         (safe water, white)
//	z≤11  DEPVS   0   … 2×draft     (anything you can't safely cross)
//	      DEPDW   ≥ 2×draft         (safe water, white)
//
// The z≤11 collapse keeps the coarse-zoom palette readable when polygons
// shrink to a handful of pixels.
func depthFill(depthM, draftM float64, z int) color.Color {
	if draftM <= 0 {
		draftM = 6.0 / feetPerMetre // default 6 ft
	}
	// Don't let draft underflow our DEPVS band — clamp so DEPMS stays
	// non-empty at z≥12.
	if draftM < s52ShallowContourM {
		draftM = s52ShallowContourM
	}
	deep := 2 * draftM
	if depthM < 0 {
		return s52DEPIT
	}
	if z <= 11 {
		if depthM < deep {
			return s52DEPVS
		}
		return s52DEPDW
	}
	if depthM < s52ShallowContourM {
		return s52DEPVS
	}
	if depthM < draftM {
		return s52DEPMS
	}
	if depthM < deep {
		return s52DEPMD
	}
	return s52DEPDW
}

// depthRange returns DRVAL1/DRVAL2 from the feature, leaving NaN when the
// attribute is genuinely missing. Callers should treat NaN as "unknown" — not
// the same as "0", which on a chart means a drying area.
func depthRange(f encFeature) (min, max float64) {
	min = math.NaN()
	max = math.NaN()
	if v, ok := f.Attribute("DRVAL1"); ok {
		min = numAttr(v)
	}
	if v, ok := f.Attribute("DRVAL2"); ok {
		max = numAttr(v)
	}
	return min, max
}

func lineStroke(class string, f encFeature, safeDepthM float64, style RenderStyle, z int) (color.Color, float64) {
	switch class {
	case "COALNE", "SLCONS":
		// Coastline / shoreline construction: solid black, full weight.
		return s52CSTLN, 1.4
	case "DEPCNT":
		// S-52 DEPCNT02: the contour matching the boat's safety depth gets
		// rendered in DEPSC (bolder) so it stands out. Only applied in
		// ECDIS mode — NOAA WMS uses uniform contour weights.
		if style == StyleECDIS {
			if v, ok := f.Attribute("VALDCO"); ok {
				val := numAttr(v)
				if !math.IsNaN(val) && math.Abs(val-safeDepthM) < 0.5 {
					return s52DEPSC, 1.4
				}
			}
		}
		return s52DEPCN, 0.6
	case "NAVLNE", "RECTRC", "FAIRWY", "ACHARE", "DWRTPT", "TWRTPT", "RESARE":
		// Channel limit / recommended track / fairway / anchorage / deep-
		// water route / restricted area — magenta boundary line.
		// At overview zoom these are the most navigationally meaningful
		// features on screen (NOAA WMS makes them dominant); bump weight
		// so they don't disappear when zoomSymbolScale is still 1.0.
		w := 0.8
		switch {
		case z <= 10:
			w = 1.6
		case z == 11:
			w = 1.3
		case z == 12:
			w = 1.0
		}
		return s52CHMGD, w
	case "RIVERS":
		return color.RGBA{0x7F, 0xB0, 0xCB, 0xFF}, 0.8
	case "BRIDGE", "CAUSWY":
		return s52CHGRD, 1.2
	case "PIPSOL", "CBLSUB", "CBLOHD":
		// Pipelines / cables: brownish dashed in S-52; we draw solid for now.
		return color.RGBA{0x82, 0x32, 0x32, 0x99}, 0.7
	case "DAMCON":
		return s52CHGRD, 1.0
	case "PONTON":
		return s52CHGRD, 0.8
	case "DOCARE", "HRBFAC", "HRBARE", "PIPARE":
		return color.RGBA{0x66, 0x66, 0x66, 0x99}, 0.6
	}
	return nil, 0
}

// drawPoint renders point/multi-point features (buoys, beacons, lights,
// hazards, soundings). Shapes follow S-52 conventions where it matters most
// for navigation: BOYSHP/BCNSHP drives the silhouette, COLOUR drives the fill
// (and a second colour stripes for safe-water/isolated-danger marks). The z
// parameter lets per-class handlers adjust behaviour at overview zoom — e.g.
// suppressing soundings, which become visual noise at z <= 11.
func drawPoint(dc *gg.Context, class string, f encFeature, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, style RenderStyle, z int) {
	coords := f.Geometry().Coordinates
	at := func(c []float64) (float64, float64) { return project(c[0], c[1]) }

	first := func(draw func(px, py float64)) {
		if len(coords) == 0 || len(coords[0]) < 2 {
			return
		}
		px, py := at(coords[0])
		draw(px, py)
	}

	// Buoys/beacons/lights are chart-critical and were rendering small
	// enough to be hard to read at coastal zoom. Bump their effective
	// scale uniformly so the symbol grows but the zoom-derived growth
	// curve still applies on top.
	const navaidSizeBoost = 1.4
	navaidScale := scale * navaidSizeBoost

	switch class {
	case "BOYLAT", "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB":
		first(func(px, py float64) {
			drawBuoy(dc, f, px, py, navaidScale)
			drawNavaidLabel(dc, f, px, py, navaidScale)
		})
	case "BCNLAT", "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		first(func(px, py float64) {
			drawBeacon(dc, f, px, py, navaidScale)
			drawNavaidLabel(dc, f, px, py, navaidScale)
		})
	case "LIGHTS":
		first(func(px, py float64) {
			drawLight(dc, f, px, py, navaidScale)
			drawLightLabel(dc, f, px, py, navaidScale)
		})
	case "WRECKS", "OBSTRN":
		// Wrecks / obstructions: red-magenta cross with sounding-style hash
		// matching S-52's symbol. Bold so it pops over depth fills.
		arm := 2.5 * scale
		first(func(px, py float64) {
			dc.SetColor(s52CHMGD)
			dc.SetLineWidth(1.2 * scale)
			dc.DrawLine(px-arm, py-arm, px+arm, py+arm)
			dc.DrawLine(px-arm, py+arm, px+arm, py-arm)
			dc.Stroke()
		})
	case "UWTROC":
		// Underwater rock — small "+" so dense fields don't overpower the chart.
		arm := 1.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0x82, 0x32, 0x32, 0xAA})
			dc.SetLineWidth(0.6 * scale)
			dc.DrawLine(px-arm, py, px+arm, py)
			dc.DrawLine(px, py-arm, px, py+arm)
			dc.Stroke()
		})
	case "MORFAC", "PILPNT", "MOORNG":
		// Mooring/dolphin/pile point — small black square.
		half := 1.5 * scale
		first(func(px, py float64) {
			dc.SetColor(s52CHBLK)
			dc.DrawRectangle(px-half, py-half, 2*half, 2*half)
			dc.Fill()
		})
	case "ACHBRT":
		// Anchorage berth — magenta open circle (S-52 anchorage symbol).
		r := 2.5 * scale
		first(func(px, py float64) {
			dc.SetColor(s52CHMGD)
			dc.SetLineWidth(0.8 * scale)
			dc.DrawCircle(px, py, r)
			dc.Stroke()
		})
	case "SOUNDG":
		// At overview zoom, soundings are visual noise: dense fields of
		// numbers crowd out the bigger-picture features (channels, place
		// names, coastline) the user is actually looking for. NOAA WMS
		// thins these out the same way; suppressing them entirely below
		// z=12 is a coarser approximation that's also simpler.
		if z <= 11 {
			return
		}
		drawSoundings(dc, coords, project, scale, safeDepthM, style)
	case "TOPMAR":
		// Topmarks only render in ECDIS mode — NOAA WMS doesn't draw them
		// as separate symbols (they're baked into the buoy/beacon shape
		// when relevant), so adding them in WMS mode just adds noise.
		if style == StyleECDIS {
			first(func(px, py float64) { drawTopmark(dc, f, px, py, scale) })
		}
	}
}

// drawTopmark renders an S-57 TOPMAR feature: a small shape sitting at the
// position of its parent buoy/beacon, with shape determined by TOPSHP. We
// implement the most common topmark shapes — cone (1, 2), sphere (3, 4),
// X (7), upright cross (8), 2-cone (13, 14, 15, 16). Anything else falls
// back to a plain dot. Drawn in chart black so the silhouette reads against
// any underlying buoy colour.
func drawTopmark(dc *gg.Context, f encFeature, px, py, scale float64) {
	shape := intAttr(f, "TOPSHP")
	r := 2.0 * scale
	// Anchor topmark just above the position so it doesn't overlap a
	// nearby buoy/beacon symbol (S-52 places topmarks above the structure).
	cy := py - 4.5*scale
	dc.SetColor(s52CHBLK)
	dc.SetLineWidth(0.6 * scale)
	switch shape {
	case 1: // cone, point up
		drawTriangleStroke(dc, px, cy, r, true)
	case 2: // cone, point down
		drawTriangleStroke(dc, px, cy, r, false)
	case 3: // sphere
		dc.DrawCircle(px, cy, r)
		dc.Fill()
	case 4: // 2 spheres, vertical
		dc.DrawCircle(px, cy-r*1.1, r*0.8)
		dc.Fill()
		dc.DrawCircle(px, cy+r*0.7, r*0.8)
		dc.Fill()
	case 7: // X-shape
		dc.DrawLine(px-r, cy-r, px+r, cy+r)
		dc.DrawLine(px-r, cy+r, px+r, cy-r)
		dc.Stroke()
	case 8: // upright cross
		dc.DrawLine(px-r, cy, px+r, cy)
		dc.DrawLine(px, cy-r, px, cy+r)
		dc.Stroke()
	case 13: // 2 cones, point up + point up
		drawTriangleStroke(dc, px, cy-r*0.5, r*0.7, true)
		drawTriangleStroke(dc, px, cy+r*0.7, r*0.7, true)
	case 14: // 2 cones, point down + point down
		drawTriangleStroke(dc, px, cy-r*0.5, r*0.7, false)
		drawTriangleStroke(dc, px, cy+r*0.7, r*0.7, false)
	case 15: // 2 cones, point down + point up
		drawTriangleStroke(dc, px, cy-r*0.5, r*0.7, false)
		drawTriangleStroke(dc, px, cy+r*0.7, r*0.7, true)
	case 16: // 2 cones, point up + point down
		drawTriangleStroke(dc, px, cy-r*0.5, r*0.7, true)
		drawTriangleStroke(dc, px, cy+r*0.7, r*0.7, false)
	default:
		dc.DrawCircle(px, cy, r*0.6)
		dc.Fill()
	}
}

// drawTriangleStroke fills a triangle with the current colour, optionally
// pointing up. Used for cone-style topmarks.
func drawTriangleStroke(dc *gg.Context, px, cy, r float64, up bool) {
	if up {
		dc.MoveTo(px, cy-r)
		dc.LineTo(px+r*0.866, cy+r*0.5)
		dc.LineTo(px-r*0.866, cy+r*0.5)
	} else {
		dc.MoveTo(px, cy+r)
		dc.LineTo(px+r*0.866, cy-r*0.5)
		dc.LineTo(px-r*0.866, cy-r*0.5)
	}
	dc.ClosePath()
	dc.Fill()
}

// drawBuoy paints a buoy at (px, py) shaped per BOYSHP and coloured per COLOUR.
// Multi-colour buoys (e.g. safe-water red+white) get horizontal stripes via a
// second-colour cap. Each buoy gets a thin black outline so it stays visible
// against any depth band.
func drawBuoy(dc *gg.Context, f encFeature, px, py, scale float64) {
	colors := buoyColours(f)
	primary := colors[0]
	secondary := colors[0]
	if len(colors) > 1 {
		secondary = colors[1]
	}
	shape := intAttr(f, "BOYSHP")
	r := 2.6 * scale
	switch shape {
	case 1: // conical (point up)
		drawTriangleUp(dc, px, py, r, primary, s52CHBLK, scale)
	case 2: // can / cylindrical
		w := 2.0 * scale
		h := 3.4 * scale
		drawStripedRect(dc, px-w, py-h/2, 2*w, h, primary, secondary, s52CHBLK, scale)
	case 3: // spherical
		drawFilledCircle(dc, px, py, r, primary, secondary, s52CHBLK, scale)
	case 4: // pillar
		w := 1.6 * scale
		h := 4.2 * scale
		drawStripedRect(dc, px-w, py-h/2, 2*w, h, primary, secondary, s52CHBLK, scale)
	case 5: // spar
		w := 0.9 * scale
		h := 4.6 * scale
		drawStripedRect(dc, px-w, py-h/2, 2*w, h, primary, secondary, s52CHBLK, scale)
	case 6: // barrel
		w := 3.2 * scale
		h := 2.0 * scale
		drawStripedRect(dc, px-w/2, py-h/2, w, h, primary, secondary, s52CHBLK, scale)
	default:
		// Unknown shape — pillar is the most common default in NOAA cells.
		w := 1.6 * scale
		h := 4.0 * scale
		drawStripedRect(dc, px-w, py-h/2, 2*w, h, primary, secondary, s52CHBLK, scale)
	}
}

// drawBeacon paints a beacon: a thin vertical pillar (the "stick") with a
// shape-specific topmark. Distinguishes from buoys so navigators see the
// fixed-vs-floating difference at a glance.
func drawBeacon(dc *gg.Context, f encFeature, px, py, scale float64) {
	colors := buoyColours(f)
	primary := colors[0]
	// Stick: thin vertical line below the topmark.
	stickH := 4.0 * scale
	dc.SetColor(s52CHBLK)
	dc.SetLineWidth(0.8 * scale)
	dc.DrawLine(px, py, px, py+stickH)
	dc.Stroke()
	// Topmark: small filled square at the head.
	half := 1.6 * scale
	dc.SetColor(primary)
	dc.DrawRectangle(px-half, py-half*2, 2*half, 2*half)
	dc.Fill()
	dc.SetColor(s52CHBLK)
	dc.SetLineWidth(0.5 * scale)
	dc.DrawRectangle(px-half, py-half*2, 2*half, 2*half)
	dc.Stroke()
}

// drawLight paints a yellow flare with a magenta accent. S-52's light symbol
// is a small magenta dot with a yellow flare extending toward the bearing of
// the strongest sector — we just render a yellow flare pointing up-right which
// reads as "light here" without needing the full sector geometry.
func drawLight(dc *gg.Context, f encFeature, px, py, scale float64) {
	_ = f
	// Yellow flare: small triangle pointing up-right from the position.
	flare := 4.0 * scale
	dc.SetColor(s52LITYW)
	dc.MoveTo(px, py)
	dc.LineTo(px+flare, py-flare*0.4)
	dc.LineTo(px+flare*0.4, py-flare)
	dc.ClosePath()
	dc.Fill()
	dc.SetColor(s52CHMGD)
	dc.SetLineWidth(0.6 * scale)
	dc.MoveTo(px, py)
	dc.LineTo(px+flare, py-flare*0.4)
	dc.LineTo(px+flare*0.4, py-flare)
	dc.ClosePath()
	dc.Stroke()
	// Magenta dot at the position itself.
	dc.SetColor(s52CHMGD)
	dc.DrawCircle(px, py, 1.0*scale)
	dc.Fill()
}

func drawTriangleUp(dc *gg.Context, px, py, r float64, fill, outline color.Color, scale float64) {
	dc.MoveTo(px, py-r)
	dc.LineTo(px+r*0.866, py+r*0.5)
	dc.LineTo(px-r*0.866, py+r*0.5)
	dc.ClosePath()
	dc.SetColor(fill)
	dc.Fill()
	dc.MoveTo(px, py-r)
	dc.LineTo(px+r*0.866, py+r*0.5)
	dc.LineTo(px-r*0.866, py+r*0.5)
	dc.ClosePath()
	dc.SetColor(outline)
	dc.SetLineWidth(0.5 * scale)
	dc.Stroke()
}

func drawFilledCircle(dc *gg.Context, px, py, r float64, fill, accent, outline color.Color, scale float64) {
	dc.SetColor(fill)
	dc.DrawCircle(px, py, r)
	dc.Fill()
	if accent != fill {
		// Half-fill the bottom half with the accent colour for two-tone
		// spheres (isolated-danger black+red, etc.).
		dc.Push()
		dc.DrawRectangle(px-r, py, 2*r, r)
		dc.Clip()
		dc.SetColor(accent)
		dc.DrawCircle(px, py, r)
		dc.Fill()
		dc.Pop()
	}
	dc.SetColor(outline)
	dc.SetLineWidth(0.5 * scale)
	dc.DrawCircle(px, py, r)
	dc.Stroke()
}

// drawStripedRect fills a rectangle and, if a second colour is given, paints
// the bottom half in that colour — which gives lateral marks their two-tone
// look (red/white safe-water buoys, etc.) without needing per-pixel patterns.
func drawStripedRect(dc *gg.Context, x, y, w, h float64, primary, secondary, outline color.Color, scale float64) {
	dc.SetColor(primary)
	dc.DrawRectangle(x, y, w, h)
	dc.Fill()
	if secondary != primary {
		dc.SetColor(secondary)
		dc.DrawRectangle(x, y+h/2, w, h/2)
		dc.Fill()
	}
	dc.SetColor(outline)
	dc.SetLineWidth(0.5 * scale)
	dc.DrawRectangle(x, y, w, h)
	dc.Stroke()
}

// drawSoundings implements (a simplified) S-52 SOUNDG02:
//   - colour SNDG2 (bolder) for soundings shoaler than the safety contour,
//     SNDG1 (lighter) for safe soundings
//   - two-tone formatting: whole feet at full size, tenths rendered in a
//     subscript half size, slightly to the lower-right
//   - falls back to a small dot when the Z coordinate is missing
func drawSoundings(dc *gg.Context, coords [][]float64, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, style RenderStyle) {
	at := func(c []float64) (float64, float64) { return project(c[0], c[1]) }
	dc.SetFontFace(basicfont.Face7x13)
	intScale := 0.55 * scale
	subScale := 0.36 * scale
	dotR := 0.7 * scale
	for _, c := range coords {
		if len(c) < 2 {
			continue
		}
		px, py := at(c)
		if len(c) < 3 {
			dc.SetColor(s52SNDG1)
			dc.DrawCircle(px, py, dotR)
			dc.Fill()
			continue
		}
		depthM := c[2]
		if math.IsNaN(depthM) || depthM < 0 {
			dc.SetColor(s52SNDG1)
			dc.DrawCircle(px, py, dotR)
			dc.Fill()
			continue
		}
		// Bolder colour when shoaler than safety so a sailor scanning the
		// chart notices the dangerous depths first. Only in ECDIS mode —
		// NOAA WMS draws every sounding the same shade.
		col := s52SNDG1
		if style == StyleECDIS && depthM < safeDepthM {
			col = s52SNDG2
		}
		dc.SetColor(col)
		depthFt := depthM * feetPerMetre
		if style == StyleWMS {
			// Single-tone: round to whole feet, draw centred. Matches NOAA
			// WMS which doesn't show tenths at compare-relevant zooms.
			label := fmt.Sprintf("%d", int(math.Round(depthFt)))
			dc.Push()
			dc.ScaleAbout(intScale, intScale, px, py)
			dc.DrawStringAnchored(label, px, py, 0.5, 0.5)
			dc.Pop()
			continue
		}
		// ECDIS two-tone (SOUNDG02): round to nearest tenth, then split
		// into integer + tenth so the tenth can render as a subscript.
		tenths := math.Round(depthFt * 10)
		intPart := int(tenths) / 10
		decPart := int(tenths) % 10
		intStr := fmt.Sprintf("%d", intPart)
		dc.Push()
		dc.ScaleAbout(intScale, intScale, px, py)
		dc.DrawStringAnchored(intStr, px, py, 0.5, 0.5)
		dc.Pop()
		if decPart > 0 {
			intHalfW := float64(len(intStr)) * 7 / 2 * intScale
			subX := px + intHalfW + 0.6*scale
			subY := py + 2.2*scale
			dc.Push()
			dc.ScaleAbout(subScale, subScale, subX, subY)
			dc.DrawStringAnchored(fmt.Sprintf("%d", decPart), subX, subY, 0, 0.5)
			dc.Pop()
		}
	}
}

// s57ColourCode maps an S-57 COLOUR code (1..13) to its S-52 RGB.
func s57ColourCode(code string) color.RGBA {
	switch strings.TrimSpace(code) {
	case "1": // white
		return s52CHWHT
	case "2": // black
		return s52CHBLK
	case "3": // red
		return s52CHRED
	case "4": // green
		return s52CHGRN
	case "5": // blue
		return color.RGBA{0x14, 0x46, 0xCC, 0xFF}
	case "6": // yellow
		return s52CHYLW
	case "7": // grey
		return s52CHGRD
	case "8": // brown
		return s52CHBRN
	case "9": // amber
		return color.RGBA{0xFF, 0xA5, 0x00, 0xFF}
	case "10": // violet
		return color.RGBA{0x82, 0x46, 0xC8, 0xFF}
	case "11": // orange
		return color.RGBA{0xFF, 0x6E, 0x00, 0xFF}
	case "12": // magenta
		return s52CHMGD
	case "13": // pink
		return color.RGBA{0xFF, 0xB4, 0xD2, 0xFF}
	}
	return s52CHGRF
}

// buoyColours returns the COLOUR attribute as an ordered list of S-52 RGB
// colours. NOAA cells store COLOUR as a comma-separated string of S-57 codes
// (e.g. "3,1" for red+white safe-water marks). Always returns at least one
// colour (grey fallback) so callers never have to bounds-check.
func buoyColours(f encFeature) []color.RGBA {
	v, ok := f.Attribute("COLOUR")
	if !ok {
		return []color.RGBA{s52CHGRF}
	}
	s, _ := v.(string)
	if s == "" {
		return []color.RGBA{s52CHGRF}
	}
	parts := strings.Split(s, ",")
	out := make([]color.RGBA, 0, len(parts))
	for _, p := range parts {
		out = append(out, s57ColourCode(p))
	}
	if len(out) == 0 {
		return []color.RGBA{s52CHGRF}
	}
	return out
}

// intAttr returns the integer-valued S-57 attribute, or 0 if missing or not
// representable as an integer. Used for enumerated attributes like BOYSHP.
func intAttr(f encFeature, key string) int {
	v, ok := f.Attribute(key)
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	case string:
		var i int
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%d", &i)
		return i
	}
	return 0
}

func numAttr(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		var f float64
		_, _ = fmt.Sscanf(n, "%f", &f)
		return f
	}
	return math.NaN()
}

// mercToLonLat converts EPSG:3857 metres to WGS84 lon/lat degrees.
func mercToLonLat(x, y float64) (lon, lat float64) {
	lon = x / mercatorMax * 180.0
	lat = math.Atan(math.Sinh(y/mercatorMax*math.Pi)) * 180.0 / math.Pi
	return
}

// lonLatToMerc is the inverse projection used to map feature coords into the
// tile's mercator pixel space.
func lonLatToMerc(lon, lat float64) (x, y float64) {
	x = lon / 180.0 * mercatorMax
	rad := lat * math.Pi / 180.0
	y = math.Log(math.Tan(math.Pi/4+rad/2)) / math.Pi * mercatorMax
	return
}
