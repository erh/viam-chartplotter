package vc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/font/basicfont"

	"github.com/beetlebugorg/s57/pkg/s57"

	"github.com/erh/viam-chartplotter/osmtiler"
)

// ENCRenderer turns ENC cells on disk into XYZ PNG tiles in pure Go. It uses the
// catalog to find which cells overlap a tile, the cell store to locate the .000
// file on disk, github.com/beetlebugorg/s57 to parse, and fogleman/gg to draw.
//
// This is a deliberately minimal style — no S-52 — but readable enough to plot a
// course: water/land fills, coastline, depth contours, soundings, navaids.
type ENCRenderer struct {
	catalog *ENCCatalog
	store   *ENCStore
	// osm resolves a lon/lat to a (possibly still-downloading) parsed
	// OSM FeatureSet for the /noaa-enc/osm-tile/ underlay. Nil disables
	// the layer entirely.
	osm    *osmtiler.RegionManager
	logger logging.Logger

	mu     sync.Mutex
	charts map[string]*chartEntry
}

type chartEntry struct {
	chart *s57.Chart
	mtime int64
	path  string
}

// drawPass orders the rendering of features so fills are below lines are below
// points, regardless of the order features come out of the spatial index.
type drawPass int

const (
	passAreas drawPass = iota
	passLines
	passPoints
)

func NewENCRenderer(catalog *ENCCatalog, store *ENCStore, logger logging.Logger) *ENCRenderer {
	return &ENCRenderer{
		catalog: catalog,
		store:   store,
		logger:  logger,
		charts:  map[string]*chartEntry{},
	}
}

// SetOSMRegionManager attaches the region manager that backs the
// /noaa-enc/osm-tile/ endpoint. When set, RenderOSMTile looks up a
// region for the requested tile and either renders from its parsed
// FeatureSet or kicks off a background download/parse if the region
// is known-but-not-yet-loaded. Optional — when nil, the endpoint
// serves blank.
func (r *ENCRenderer) SetOSMRegionManager(m *osmtiler.RegionManager) { r.osm = m }

// OSMRegionManager exposes the attached manager (or nil) so the
// status endpoint can read its load epoch.
func (r *ENCRenderer) OSMRegionManager() *osmtiler.RegionManager { return r.osm }

// chartFor returns the parsed chart for a cell, parsing once and reusing the
// result until the on-disk .000 file's mtime changes.
func (r *ENCRenderer) chartFor(name string) (*s57.Chart, error) {
	path := r.store.S57Path(name)
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mtime := info.ModTime().UnixNano()

	r.mu.Lock()
	entry, ok := r.charts[name]
	r.mu.Unlock()
	if ok && entry.path == path && entry.mtime == mtime {
		return entry.chart, nil
	}

	parseStart := time.Now()
	parser := s57.NewParser()
	chart, err := parser.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if r.logger != nil {
		r.logger.Infof("enc parse: cell=%s size=%d bytes feats=%d in %s",
			name, info.Size(), len(chart.Features()), time.Since(parseStart).Round(time.Millisecond))
	}

	r.mu.Lock()
	r.charts[name] = &chartEntry{chart: chart, mtime: mtime, path: path}
	r.mu.Unlock()
	return chart, nil
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
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	wantFeatures := (maxLon-minLon) < 0.05 && (maxLat-minLat) < 0.05

	reports := make([]DebugCellReport, 0, len(cells))
	for _, cell := range cells {
		rep := DebugCellReport{
			Name:           cell.Name,
			CScale:         cell.CScale,
			BBox:           [4]float64{cell.MinLon, cell.MinLat, cell.MaxLon, cell.MaxLat},
			ByGeometryType: map[string]int{},
			ByClass:        map[string]int{},
			ClassesByGeom:  map[string]map[string]int{},
		}
		chart, err := r.chartFor(cell.Name)
		if err != nil {
			rep.ParseError = err.Error()
			reports = append(reports, rep)
			continue
		}
		if chart == nil {
			rep.ParseError = "cell not on disk"
			reports = append(reports, rep)
			continue
		}
		for _, f := range chart.Features() {
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
		reports = append(reports, rep)
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
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}

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

	for _, cell := range cells {
		chart, err := r.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
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
			// Drop structures whose geometry comes back from the s57 lib
			// with phantom-segment jumps — concatenated raw edges from a
			// topology-resolution fallback. Their vertex bags include
			// km-scale jumps to disconnected sub-features, so the centroid
			// used for the dedup key below lands on neither real bridge
			// and can coincide with an unrelated bridge's centroid inside
			// the 10 m dedup grid, collapsing two distinct structures
			// into one. Finer-scale cells almost always carry a clean
			// encoding of the same bridge; if every cell's encoding is
			// phantom the feature drops out of the hover layer, which
			// beats showing a merged-but-wrong popup.
			if hasPhantomEdge(geom.Coordinates, structurePhantomJumpM) {
				continue
			}
			props := map[string]any{}
			for _, k := range StructureAttributeKeys {
				if v, ok := f.Attribute(k); ok {
					props[k] = v
				}
			}
			var sg StructureGeom
			switch geom.Type {
			case s57.GeometryTypePoint:
				sg = StructureGeom{Type: "Point", Coordinates: geom.Coordinates[0]}
			case s57.GeometryTypeLineString:
				sg = StructureGeom{Type: "LineString", Coordinates: geom.Coordinates}
			case s57.GeometryTypePolygon:
				// Wrap the single ring as GeoJSON Polygon coordinates
				// expect: [outer-ring, ...holes]. Holes aren't surfaced
				// by the s57 library at this level so we always emit a
				// one-ring polygon.
				sg = StructureGeom{Type: "Polygon", Coordinates: [][][]float64{geom.Coordinates}}
			default:
				continue
			}
			// Centroid for dedup so the same bridge in two cells (which
			// often differ in segmentation/first-vertex) maps to the
			// same key. Plain unweighted mean of vertices — close enough
			// for grouping; not used for rendering.
			var sumLon, sumLat float64
			var n int
			for _, c := range geom.Coordinates {
				if len(c) >= 2 {
					sumLon += c[0]
					sumLat += c[1]
					n++
				}
			}
			if n == 0 {
				continue
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
			s := Structure{Class: class, Geometry: sg, Properties: props}
			if existing, ok := seen[k]; !ok || len(props) > len(existing.Properties) {
				seen[k] = s
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
	"bridge":             {},
	"footbridge":         {},
	"foot bridge":        {},
	"pedestrian bridge":  {},
	"railway bridge":     {},
	"rail bridge":        {},
	"rr bridge":          {},
	"r.r. bridge":        {},
	"road bridge":        {},
	"highway bridge":     {},
	"hwy bridge":         {},
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
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}

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

	for _, cell := range cells {
		chart, err := r.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
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
const ENCRenderRulesVersion = 4

// OSMRenderRulesVersion is the same idea, scoped to the OSM raster pipeline
// (RenderOSMTile via osmtiler). Bump on any change to the rasteriser that
// should invalidate the on-disk cache. Independent from ENC so an ENC bump
// doesn't rebuild OSM tiles (and vice versa).
//
// Also mirror this value in src/marineMap.svelte's OSM_RENDER_VERSION so
// the frontend URL pattern bumps `?osmv=` and busts browser caches on the
// next page load — otherwise a Go-only bump leaves stale tiles cached
// client-side for up to a day.
const OSMRenderRulesVersion = 4

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
}

func (r *ENCRenderer) RenderTile(z, x, y int, opts RenderOptions) ([]byte, error) {
	safeDepthM := opts.SafeDepthM
	style := opts.Style
	skipNavaids := opts.SkipNavaids
	transparentLand := opts.TransparentLand
	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	// Only pull in cells whose compilation scale is appropriate for this
	// display zoom. Without this, every Berthing-cell wreck and sounding gets
	// painted at z=12 and overview-cell continents get painted at z=16.
	minScale, maxScale := cellScaleRangeFor(z, (minLat+maxLat)/2)
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, minScale, maxScale)
	// Fall back if the scale window left us with nothing. At chart-detail
	// zoom we'll use ANY available cell (better something coarse than an
	// empty tile). At coarse zoom we restrict the fall-back to only
	// continent-scale cells (≥1:200 k) so the tile doesn't end up showing
	// the blocky seams between fine-cell coverage extents.
	if len(cells) == 0 {
		minScaleFallback := 0
		if z < 10 {
			minScaleFallback = 800_000
		}
		cells = r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, minScaleFallback, 0)
	}
	// Paint coarsest first so finer-scale cells overwrite their detail on top.
	// CScale is the compilation-scale denominator, so larger CScale = coarser.
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale > cells[j].CScale })


	// Area-fill base pass: at chart-detail zoom we use ALL overlapping
	// cells (no scale filter) because fine-scale Berthing cells often have
	// only the on-the-water detail (DEPARE, COALNE) — the LNDARE / BUAARE
	// coverage for the surrounding land lives in the coarser Approach
	// cell.
	//
	// At coarse zoom (z<10) the regular scale filter turns up empty and
	// we fall back to all-cells, but mixing fine (1:80 k) cells with the
	// 1:1.2 M overview creates visibly rectangular cell-coverage seams
	// because each cell's bbox is comparable in size to a tile. So at
	// coarse zoom we restrict the base pass to only continent-scale
	// (1:200 k or coarser) cells — they tile cleanly without seams.
	allCellsRaw := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	allCells := allCellsRaw
	if z < 10 {
		// Only continent-scale (≥1:800 k) cells to avoid coverage-edge
		// seams between adjacent finer cells. Applies to land area fills
		// — for DEPARE we use allCellsRaw below so harbour-cell
		// narrow-band polygons can still drive shading at z=7..9.
		filtered := allCells[:0]
		for _, c := range allCellsRaw {
			if c.CScale >= 800_000 {
				filtered = append(filtered, c)
			}
		}
		allCells = filtered
	} else {
		// Respect cell coverage hierarchy for LNDARE: walk cells finest-
		// first, marking their tile-intersection extent on a 32×32
		// coverage mask. Skip any cell whose own tile-intersection is
		// fully claimed by finer cells. This prevents a 1:45 k Approach
		// cell's imprecise "Long Beach east" LANDA from painting yellow
		// over open water that the 1:22 k Harbour cell (covering the
		// same tile area, but with no LNDARE because it's all water)
		// has already claimed authoritative coverage for.
		const mw = 32
		var mask [mw * mw]bool
		cellsFinest := make([]ENCCell, len(allCellsRaw))
		copy(cellsFinest, allCellsRaw)
		sort.SliceStable(cellsFinest, func(i, j int) bool {
			return cellsFinest[i].CScale < cellsFinest[j].CScale
		})
		toPxBox := func(c ENCCell) (xmin, ymin, xmax, ymax int) {
			fx := func(lon float64) int {
				if lon <= minLon {
					return 0
				}
				if lon >= maxLon {
					return mw
				}
				return int((lon - minLon) / (maxLon - minLon) * mw)
			}
			fy := func(lat float64) int {
				if lat <= minLat {
					return mw
				}
				if lat >= maxLat {
					return 0
				}
				return int((maxLat - lat) / (maxLat - minLat) * mw)
			}
			return fx(c.MinLon), fy(c.MaxLat), fx(c.MaxLon), fy(c.MinLat)
		}
		filtered := allCells[:0]
		for _, c := range cellsFinest {
			xmin, ymin, xmax, ymax := toPxBox(c)
			if xmin >= xmax || ymin >= ymax {
				continue
			}
			allClaimed := true
			for y := ymin; y < ymax && allClaimed; y++ {
				for x := xmin; x < xmax; x++ {
					if !mask[y*mw+x] {
						allClaimed = false
						break
					}
				}
			}
			if allClaimed {
				continue
			}
			filtered = append(filtered, c)
			for y := ymin; y < ymax; y++ {
				for x := xmin; x < xmax; x++ {
					mask[y*mw+x] = true
				}
			}
		}
		allCells = filtered
	}
	sort.SliceStable(allCells, func(i, j int) bool { return allCells[i].CScale > allCells[j].CScale })
	// DEPARE base-pass cell list. At z≥10 use the same coverage-filtered
	// set as LNDARE so a coarser cell's "0–1.8 m" polygon (mid=0.9 m →
	// DEPVS) doesn't paint saturated blue over a finer cell's
	// authoritative water shading. At z<10 keep the unfiltered set —
	// harbour cells still need to drive narrow-band shading at z=7..9
	// without coverage gating (the continent-scale LNDARE filter would
	// otherwise drop them).
	var allCellsForDEPARE []ENCCell
	if z < 10 {
		allCellsForDEPARE = make([]ENCCell, len(allCellsRaw))
		copy(allCellsForDEPARE, allCellsRaw)
		sort.SliceStable(allCellsForDEPARE, func(i, j int) bool {
			return allCellsForDEPARE[i].CScale > allCellsForDEPARE[j].CScale
		})
	} else {
		allCellsForDEPARE = allCells
	}

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

	skipAll := opts.SkipClasses != nil && opts.SkipClasses["*"]
	drawCell := func(chart *s57.Chart, pass drawPass) {
		if skipAll {
			return
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			class := f.ObjectClass()
			if skipNavaids && IsNavaidClass(class) {
				continue
			}
			if transparentLand && isLandClass(class) {
				continue
			}
			// Area fills are handled by the all-cells base pass above so
			// the finest cell's polygon wins. Skip here to avoid letting
			// a coarser in-scale-window cell stomp on it.
			if pass == passAreas {
				switch class {
				case "LNDARE", "BUAARE", "BUISGL",
					"DEPARE", "DRGARE", "LOKBSN", "UNSARE":
					continue
				}
			}
			if opts.SkipClasses != nil && opts.SkipClasses[class] {
				continue
			}
			drawFeature(dc, &f, pass, project, scale, safeDepthM, bbox, z, style)
		}
	}

	// Land area-fill base pass from the (possibly seam-filtered) cell set,
	// coarsest first. Uses allCells so at z<10 only continent-scale cells
	// paint LNDARE/BUAARE — adjacent finer cells with bbox-aligned
	// coverage extents would otherwise show as seams across the land mask.
	// Skipped entirely under TransparentLand — the basemap (OSM) is what
	// the user wants to see for land in that mode.
	if !transparentLand && !skipAll {
		for _, cell := range allCells {
			chart, err := r.chartFor(cell.Name)
			if err != nil || chart == nil {
				continue
			}
			for _, f := range chart.FeaturesInBounds(bbox) {
				class := f.ObjectClass()
				switch class {
				case "LNDARE", "BUAARE", "BUISGL":
				default:
					continue
				}
				if opts.SkipClasses != nil && opts.SkipClasses[class] {
					continue
				}
				drawFeature(dc, &f, passAreas, project, scale, safeDepthM, bbox, z, style)
			}
		}
	}

	// Water area-fill base pass from the UNFILTERED cell set, coarsest
	// first. The finest cell's narrow-band DEPARE polygons land on top
	// regardless of zoom, so deep channels render as DEPDW even at
	// z=7..12 where the regular scale filter excludes harbour cells. The
	// main pass below skips these classes so we don't double-paint with
	// a stale coarser polygon. Runs even under TransparentLand — only
	// LNDARE is suppressed in that mode (so the OSM basemap shows
	// through for land), depth shading is the whole point of the chart.
	if !skipAll {
		for _, cell := range allCellsForDEPARE {
			chart, err := r.chartFor(cell.Name)
			if err != nil || chart == nil {
				continue
			}
			for _, f := range chart.FeaturesInBounds(bbox) {
				class := f.ObjectClass()
				switch class {
				case "DEPARE", "DRGARE", "LOKBSN", "UNSARE":
				default:
					continue
				}
				if opts.SkipClasses != nil && opts.SkipClasses[class] {
					continue
				}
				drawFeature(dc, &f, passAreas, project, scale, safeDepthM, bbox, z, style)
			}
		}
	}

	for _, pass := range []drawPass{passAreas, passLines, passPoints} {
		for _, cell := range cells {
			chart, err := r.chartFor(cell.Name)
			if err != nil {
				r.logger.Warnf("enc render: %s: %v", cell.Name, err)
				continue
			}
			if chart == nil {
				continue
			}
			drawCell(chart, pass)
		}
	}

	// Label-only pass over ALL overlapping cells (no scale filter). Place
	// names live on harbour/berthing-scale cells we'd otherwise drop at
	// coastal zoom; without this pass, "Radio Island" disappears from a
	// z=14 tile because its LNDARE feature only exists in the 1:10 000
	// cell. Drawing labels uses centroid/extent guards in drawAreaLabel
	// so off-tile features don't get rendered.
	// We pull "harbour-scale only" features (channels, fairways) from
	// any overlapping cell, even if outside the regular scale window.
	// Reason: Ambrose-Channel-style FAIRWY polygons live in 1:12 000
	// Berthing cells which the scale filter excludes until very high
	// zoom. NOAA shows these channel lines at z=9 onward; without this
	// pass we'd be missing the most navigationally-important feature.
	if z >= 9 && !skipAll {
		raw := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
		// Coarsest first so finest cell's feature wins.
		sort.SliceStable(raw, func(i, j int) bool { return raw[i].CScale > raw[j].CScale })
		for _, cell := range raw {
			chart, err := r.chartFor(cell.Name)
			if err != nil || chart == nil {
				continue
			}
			for _, f := range chart.FeaturesInBounds(bbox) {
				class := f.ObjectClass()
				switch class {
				case "FAIRWY", "RECTRC", "NAVLNE", "DWRTPT", "TWRTPT":
				default:
					continue
				}
				if opts.SkipClasses != nil && opts.SkipClasses[class] {
					continue
				}
				drawFeature(dc, &f, passLines, project, scale, safeDepthM, bbox, z, style)
			}
		}
	}

	// Place-name labels at overview zooms (z >= 10). Bigger features get
	// priority: we collect all viable candidates, dedupe by name keeping
	// the largest polygon, sort largest-first, then place greedily with
	// collision detection so a tile full of named marshes/coves doesn't
	// turn into stacked unreadable text. Without this z=11 was painting
	// 10+ overlapping labels per tile.
	if z >= 10 && !skipAll {
		type labelCand struct {
			name              string
			px, py            float64
			labelScale        float64
			halfW, halfH, pad float64
			area              float64
		}
		bestByName := map[string]labelCand{}
		for _, cell := range allCells {
			chart, err := r.chartFor(cell.Name)
			if err != nil || chart == nil {
				continue
			}
			for _, f := range chart.FeaturesInBounds(bbox) {
				class := f.ObjectClass()
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
		}

		cands := make([]labelCand, 0, len(bestByName))
		for _, c := range bestByName {
			cands = append(cands, c)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].area > cands[j].area })

		type rect struct{ minX, minY, maxX, maxY float64 }
		var placed []rect
		for _, c := range cands {
			r := rect{
				c.px - c.halfW - c.pad,
				c.py - c.halfH - c.pad,
				c.px + c.halfW + c.pad,
				c.py + c.halfH + c.pad,
			}
			overlap := false
			for _, p := range placed {
				if !(r.maxX < p.minX || r.minX > p.maxX || r.maxY < p.minY || r.minY > p.maxY) {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			placed = append(placed, r)
			dc.SetFontFace(basicfont.Face7x13)
			dc.SetColor(s52CHBLK)
			dc.Push()
			dc.ScaleAbout(c.labelScale, c.labelScale, c.px, c.py)
			dc.DrawStringAnchored(c.name, c.px, c.py, 0.5, 0.5)
			dc.Pop()
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderOSMTile draws our self-hosted OSM raster for the given XYZ
// tile. The region manager looks up which extract covers the tile
// center; if the region is known but not yet parsed, the manager
// kicks off a background download/parse and we return a blank tile —
// the next request after parsing finishes will get a populated PNG.
// Water is omitted by design (the chart layer underneath provides it).
//
// The second return value is true when an actual feature-backed
// render happened, false when this is the transparent fallback
// (no region attached, region not yet loaded, or no covering
// region in the catalog). The handler uses this to decide between
// long-cache (real render) and no-cache (fallback that needs to be
// re-attempted soon).
func (r *ENCRenderer) RenderOSMTile(z, x, y int) ([]byte, bool, error) {
	t0 := time.Now()
	if r.osm != nil {
		tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
		// Use the tile's bbox so every region that overlaps gets
		// drawn (low-zoom tiles span multiple states) and offshore
		// tiles still match a region.
		minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
		maxLon, minLat := mercToLonLat(tileXmax, tileYmin)
		fss, covered := r.osm.FeatureSetsForBBox(minLon, minLat, maxLon, maxLat)
		if len(fss) > 0 {
			data, err := osmtiler.RenderTileMulti(fss, z, x, y)
			if err == nil {
				// Mask out pixels where the chart says "water" so
				// the chart's depth-shading shows through the OSM
				// tile's yellow land base.
				if masked, mErr := r.maskChartWater(data, z, x, y); mErr == nil {
					data = masked
				} else if r.logger != nil {
					r.logger.Warnf("osm-tile water mask z=%d x=%d y=%d: %v", z, x, y, mErr)
				}
				if r.logger != nil && time.Since(t0) > 200*time.Millisecond {
					r.logger.Infof("osm-tile rendered z=%d x=%d y=%d in %s (%d bytes, %d region(s))",
						z, x, y, time.Since(t0).Round(time.Millisecond), len(data), len(fss))
				}
				return data, true, nil
			}
			if r.logger != nil {
				r.logger.Warnf("osm-tile render z=%d x=%d y=%d: %v", z, x, y, err)
			}
		} else if r.logger != nil && covered {
			r.logger.Debugf("osm-tile z=%d x=%d y=%d: region(s) still loading", z, x, y)
		}
	}
	dc := gg.NewContext(256, 256)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), false, nil
}

// maskChartWater decodes the rendered OSM PNG, sets every pixel that
// the chart claims is water (DEPARE / DRGARE / LOKBSN minus LNDARE /
// BUAARE / BUISGL) to fully transparent, and re-encodes. The OSM
// renderer paints a yellow land base under everything; without this
// post-pass the chart's depth shading underneath never shows through.
func (r *ENCRenderer) maskChartWater(pngBytes []byte, z, x, y int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)
	project := func(lon, lat float64) (float64, float64) {
		mx, my := lonLatToMerc(lon, lat)
		px := (mx - tileXmin) / (tileXmax - tileXmin) * 256
		py := (tileYmax - my) / (tileYmax - tileYmin) * 256
		return px, py
	}
	bbox := s57.Bounds{MinLon: minLon, MinLat: minLat, MaxLon: maxLon, MaxLat: maxLat}
	mask := r.buildChartWaterMask(minLon, minLat, maxLon, maxLat, bbox, project)

	srcBounds := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for py := 0; py < 256; py++ {
		for px := 0; px < 256; px++ {
			_, _, _, ma := mask.At(px, py).RGBA()
			if ma > 0 {
				// Water — leave transparent.
				continue
			}
			r8, g8, b8, a8 := img.At(srcBounds.Min.X+px, srcBounds.Min.Y+py).RGBA()
			out.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r8 >> 8),
				G: uint8(g8 >> 8),
				B: uint8(b8 >> 8),
				A: uint8(a8 >> 8),
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

// buildChartWaterMask rasterises a binary water mask for the supplied
// tile: opaque pixels where the chart's DEPARE / DRGARE / LOKBSN
// polygons say water, minus any LNDARE / BUAARE / BUISGL that fall
// inside (overview-cell DEPARE polygons have imprecise hole rings and
// would otherwise mask out actual land at the southern tip of
// Manhattan and similar spots).
func (r *ENCRenderer) buildChartWaterMask(
	minLon, minLat, maxLon, maxLat float64,
	bbox s57.Bounds,
	project func(lon, lat float64) (float64, float64),
) image.Image {
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)

	waterCtx := gg.NewContext(256, 256)
	waterCtx.SetColor(color.RGBA{0, 0, 0, 0})
	waterCtx.Clear()
	waterCtx.SetColor(color.RGBA{0, 0, 0, 0xFF})

	landCtx := gg.NewContext(256, 256)
	landCtx.SetColor(color.RGBA{0, 0, 0, 0})
	landCtx.Clear()
	landCtx.SetColor(color.RGBA{0, 0, 0, 0xFF})

	for _, cell := range cells {
		chart, err := r.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			if isOversizedPolygon(geom.Coordinates, bbox, 200) {
				continue
			}
			if isDegeneratePixelPolygon(geom.Coordinates, project) {
				continue
			}
			switch f.ObjectClass() {
			case "DEPARE", "DRGARE", "LOKBSN":
				tracePolygonPath(waterCtx, geom.Coordinates, project)
				waterCtx.SetFillRuleEvenOdd()
				waterCtx.Fill()
			case "LNDARE", "BUAARE", "BUISGL":
				tracePolygonPath(landCtx, geom.Coordinates, project)
				landCtx.SetFillRuleEvenOdd()
				landCtx.Fill()
			}
		}
	}

	water := waterCtx.Image()
	land := landCtx.Image()
	out := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for py := 0; py < 256; py++ {
		for px := 0; px < 256; px++ {
			_, _, _, wa := water.At(px, py).RGBA()
			if wa == 0 {
				continue
			}
			_, _, _, la := land.At(px, py).RGBA()
			if la > 0 {
				continue // land wins
			}
			out.SetNRGBA(px, py, color.NRGBA{R: 0, G: 0, B: 0, A: 0xFF})
		}
	}
	return out
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

// minZoomForFeature returns the lowest map zoom at which an S-57 object
// class should render. Below this, the feature is dropped so coarse zooms
// aren't blanketed in symbols. Matches the spirit of S-52 scale-dependent
// symbology — NOAA's WMS thins out wrecks, obstructions, soundings, and
// minor navaids at zoomed-out scales for exactly this reason. Tuned by
// eyeballing the compare test against z=12/14/16 NOAA tiles.
func minZoomForFeature(class string) int {
	switch class {
	// Major area fills — always show.
	case "LNDARE", "DEPARE", "DRGARE", "BUAARE", "UNSARE", "LOKBSN":
		return 0
	// Single buildings (commercial / conspicuous structures): only at
	// chart-detail zoom.
	case "BUISGL":
		return 14
	// Coastline + depth contours — always.
	case "COALNE", "DEPCNT":
		return 0
	// Shoreline construction (piers, jetties, seawalls): hundreds per
	// harbour cell, way too dense at coastal zoom. Show only at chart
	// detail.
	case "SLCONS":
		return 15
	// Major navaids visible at overview. NOAA renders these at z=9
	// (sometimes z=8) so a sailor scanning a chart at coastal scale still
	// sees major lights, lateral marks, and major hazards.
	case "BOYLAT", "BCNLAT", "LIGHTS":
		return 9
	case "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB":
		return 11
	case "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		return 13
	// Topmarks attach to buoys/beacons and only make sense at chart-detail
	// zoom; smaller and they're indistinguishable from their parent symbol.
	case "TOPMAR":
		return 14
	case "DAYMAR":
		return 14
	// Hazards. Wrecks/obstructions are dense in harbour cells; only show
	// at chart-detail zoom. Underwater rocks even more so.
	case "WRECKS", "OBSTRN":
		return 15
	case "UWTROC":
		return 16
	// Soundings: NOAA renders depth labels at z=9 already (offshore tiles
	// show "65", "83", "95"-style depth labels). They're the densest
	// feature class, so dropping them at z<9 keeps the chart readable.
	case "SOUNDG":
		return 9
	// Mooring/pile/anchorage: harbour-detail zoom.
	case "MORFAC", "PILPNT", "MOORNG", "ACHBRT":
		return 15
	// Linear features.
	case "RIVERS", "BRIDGE", "CAUSWY":
		return 11
	// Overhead structures: cables, pipes, conveyors. The structures
	// vector layer kicks in at z >= 13 and is responsible for the
	// hover-able icon; below that the tile must draw the structure
	// itself, otherwise it would disappear off the chart between
	// coastal and harbour zoom. Same z=11 threshold as BRIDGE so all
	// four classes show up together when the vector layer is off.
	case "CBLOHD", "PIPOHD", "CONVYR":
		return 11
	// Channel limits / fairways / restricted areas — magenta lines show
	// at z=9 in NOAA charts (busy in our renders below that).
	case "FAIRWY", "RECTRC", "NAVLNE", "ACHARE", "DWRTPT", "TWRTPT", "RESARE":
		return 9
	case "PIPSOL", "CBLSUB":
		return 15
	case "DAMCON", "PONTON":
		return 14
	case "DOCARE", "HRBFAC", "HRBARE", "PIPARE":
		return 13
	}
	return 14
}

// drawFeature dispatches to the right pass based on the feature's object class
// and geometry. Anything we don't recognise is silently skipped. `scale` is a
// zoom-derived multiplier applied to stroke widths and symbol sizes.
// `safeDepthM` controls DEPARE depth-band colouring. `tileBbox` is used to
// reject polygons that are way larger than the tile (overview-cell rings).
func drawFeature(dc *gg.Context, f *s57.Feature, pass drawPass, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, tileBbox s57.Bounds, z int, style RenderStyle) {
	geom := f.Geometry()
	if len(geom.Coordinates) == 0 {
		return
	}
	class := f.ObjectClass()
	if z < minZoomForFeature(class) {
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
			// Overview-cell rings get their outer boundary clipped to the
			// ENC cell's bounding box, threading the polygon along a
			// constant lat or lon for kilometres. Painting these at chart
			// zoom leaves triangular fill wedges where the boundary
			// segments intersect — and finer cells iterated later will
			// supply the actual coastline detail anyway. Detect the clip
			// directly via consecutive vertices that share an exact lon
			// or lat over a multi-km span; that's a fingerprint no real
			// coastline produces.
			if hasCellBoundaryClipEdge(geom.Coordinates, overviewClipEdgeM) {
				return
			}
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
func drawNavaidLabel(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func drawLightLabel(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func drawContourLabel(dc *gg.Context, f *s57.Feature, project func(lon, lat float64) (float64, float64), scale float64, z int) {
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
func areaFill(class string, f *s57.Feature, safeDepthM float64, z int) color.Color {
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
func depthRange(f *s57.Feature) (min, max float64) {
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

func lineStroke(class string, f *s57.Feature, safeDepthM float64, style RenderStyle, z int) (color.Color, float64) {
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
func drawPoint(dc *gg.Context, class string, f *s57.Feature, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, style RenderStyle, z int) {
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
func drawTopmark(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func drawBuoy(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func drawBeacon(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func drawLight(dc *gg.Context, f *s57.Feature, px, py, scale float64) {
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
func buoyColours(f *s57.Feature) []color.RGBA {
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
func intAttr(f *s57.Feature, key string) int {
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

