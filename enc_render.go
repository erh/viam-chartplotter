package vc

import (
	"bytes"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/font/basicfont"

	"github.com/beetlebugorg/s57/pkg/s57"
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
	logger  logging.Logger

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

	parser := s57.NewParser()
	chart, err := parser.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
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

// RenderTile draws a 256x256 PNG for the given XYZ tile. If no cells overlap, a
// transparent tile is returned so the layer composes cleanly with the basemap.
// safeDepthM is the boat's safety contour in metres; depth-area shading uses a
// gradient from coral at safeDepthM to white at 2×safeDepthM.
func (r *ENCRenderer) RenderTile(z, x, y int, safeDepthM float64) ([]byte, error) {
	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	// Only pull in cells whose compilation scale is appropriate for this
	// display zoom. Without this, every Berthing-cell wreck and sounding gets
	// painted at z=12 and overview-cell continents get painted at z=16.
	minScale, maxScale := cellScaleRangeFor(z, (minLat+maxLat)/2)
	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, minScale, maxScale)
	// Fall back to all cells if the scale window left us with nothing — better
	// to render something coarse than to return an empty tile.
	if len(cells) == 0 {
		cells = r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	}
	// Paint coarsest first so finer-scale cells overwrite their detail on top.
	// CScale is the compilation-scale denominator, so larger CScale = coarser.
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale > cells[j].CScale })

	// Area-fill base pass uses ALL overlapping cells (no scale filter).
	// Reason: a fine-scale Berthing cell often has only the on-the-water
	// detail (DEPARE, COALNE) without the surrounding land-area polygons,
	// because at that scale "land" is implicit context. The corresponding
	// LNDARE / BUAARE coverage lives in the coarser Approach/Coastal cell.
	// Without this pass, a z=16 harbour tile renders as DEPDW white where
	// NOAA paints LANDA / BUAARE yellow.
	allCells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	sort.SliceStable(allCells, func(i, j int) bool { return allCells[i].CScale > allCells[j].CScale })

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

	drawCell := func(chart *s57.Chart, pass drawPass) {
		for _, f := range chart.FeaturesInBounds(bbox) {
			drawFeature(dc, &f, pass, project, scale, safeDepthM, bbox, z)
		}
	}

	// First, the land/built-up area base layer from ALL cells. This gives
	// us shoreline coverage even when the scale-filtered set is missing
	// LNDARE/BUAARE. Land features only — water polygons are handled in
	// the regular passAreas below so they only paint from in-scale cells.
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
			drawFeature(dc, &f, passAreas, project, scale, safeDepthM, bbox, z)
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
	if z >= 12 {
		seen := map[string]bool{}
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
				v, ok := f.Attribute("OBJNAM")
				if !ok {
					continue
				}
				name, _ := v.(string)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				drawAreaLabel(dc, &f, project, scale, bbox)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
	// Lower bound: tighten at coarse zooms because a 1:80 000 Approach
	// cell's coastline has too many vertices to render coherently in a few
	// pixels — produces black squiggle. ≥2× display scale at z≤11.
	div := 2.0
	if z <= 11 {
		div = 0.5
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
	// Below z=12 we don't cap — at z=10/11 those overview cells are the
	// primary source of any land or water coverage at all.
	max := 0
	if z >= 12 {
		max = int(displayScale * 8)
		if max < 0 {
			max = 0
		}
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
	// Major navaids visible at overview.
	case "BOYLAT", "BCNLAT":
		return 11
	case "LIGHTS":
		return 12
	// Other navaids — coastal zoom and up.
	case "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB":
		return 12
	case "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		return 14
	case "DAYMAR":
		return 14
	// Hazards. Wrecks/obstructions are dense in harbour cells; only show
	// at chart-detail zoom. Underwater rocks even more so.
	case "WRECKS", "OBSTRN":
		return 15
	case "UWTROC":
		return 16
	// Soundings: NOAA renders depth labels even at z=12 in coastal areas;
	// they're the primary chart annotation telling a sailor "this part is
	// 14 ft deep". Numeric labels are the densest feature class, so they
	// carry a lot of the visual signal even at low zoom.
	case "SOUNDG":
		return 12
	// Mooring/pile/anchorage: harbour-detail zoom.
	case "MORFAC", "PILPNT", "MOORNG", "ACHBRT":
		return 15
	// Linear features.
	case "RIVERS", "BRIDGE", "CAUSWY":
		return 11
	case "FAIRWY", "RECTRC", "NAVLNE", "ACHARE", "DWRTPT", "TWRTPT", "RESARE":
		return 12
	case "PIPSOL", "CBLSUB", "CBLOHD":
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
func drawFeature(dc *gg.Context, f *s57.Feature, pass drawPass, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, tileBbox s57.Bounds, z int) {
	geom := f.Geometry()
	if len(geom.Coordinates) == 0 {
		return
	}
	class := f.ObjectClass()
	if z < minZoomForFeature(class) {
		return
	}

	// Place-name labels for major area features. Done before the geometry
	// switch because the polygon path returns early on the points pass —
	// labels live conceptually in the points pass even though the feature
	// itself is a polygon.
	if pass == passPoints && z >= 13 {
		switch class {
		case "LNDARE", "LNDRGN", "BUAARE", "SEAARE", "ADMARE", "BUISGL":
			drawAreaLabel(dc, f, project, scale, tileBbox)
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
		stroke, width := lineStroke(class, f)

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
			tracePolygonPath(dc, geom.Coordinates, project)
			dc.SetColor(stroke)
			dc.SetLineWidth(width * scale)
			dc.Stroke()
		default:
			return
		}

	case s57.GeometryTypeLineString:
		if pass != passLines {
			return
		}
		stroke, width := lineStroke(class, f)
		if stroke == nil {
			return
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
		drawPoint(dc, class, f, project, scale)
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

// drawAreaLabel paints OBJNAM at the polygon centroid in chart black, but
// only if the polygon is small enough that a single tile-centred label makes
// sense (a continent-sized polygon's centroid is meaningless for a tile) and
// the centroid lands well inside the tile so the text isn't half-cut.
func drawAreaLabel(dc *gg.Context, f *s57.Feature, project func(lon, lat float64) (float64, float64), scale float64, tileBbox s57.Bounds) {
	v, ok := f.Attribute("OBJNAM")
	if !ok {
		return
	}
	name, _ := v.(string)
	if name == "" {
		return
	}
	geom := f.Geometry()
	if geom.Type != s57.GeometryTypePolygon || len(geom.Coordinates) < 3 {
		return
	}
	// Skip polygons whose own bbox is much bigger than the tile — the
	// centroid is somewhere far away and the label would either render off
	// this tile or end up where it doesn't make sense.
	if isOversizedPolygon(geom.Coordinates, tileBbox, 4) {
		return
	}
	cx, cy := polygonCentroid(geom.Coordinates)
	px, py := project(cx, cy)
	dc.SetFontFace(basicfont.Face7x13)
	// Rough text-width estimate so a label doesn't run past the tile edge:
	// 7 px per char in the basicfont, then scaled.
	labelHalfW := float64(len(name)) * 7 * scale / 2
	const halfH = 6.5
	if px-labelHalfW < 2 || px+labelHalfW > 254 || py-halfH*scale < 2 || py+halfH*scale > 254 {
		return
	}
	dc.SetColor(s52CHBLK)
	dc.Push()
	dc.ScaleAbout(scale, scale, px, py)
	dc.DrawStringAnchored(name, px, py, 0.5, 0.5)
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
	s52LANDF = color.RGBA{0xB7, 0xAE, 0x90, 0xFF} // land features (darker tan)
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
	s52CHMGF = color.RGBA{0xDB, 0xB5, 0xF2, 0xFF} // magenta (faint, sampled)
	s52CHBRN = color.RGBA{0x82, 0x5A, 0x23, 0xFF}

	// Soundings + depth contours.
	s52SNDG1 = color.RGBA{0x72, 0x72, 0x72, 0xFF} // sounding label (deeper than safety)
	s52SNDG2 = color.RGBA{0x00, 0x00, 0x00, 0xFF} // sounding label (shoaler than safety, bolder)
	s52DEPCN = color.RGBA{0x83, 0x99, 0xA8, 0xFF} // depth contour line (subtle steel blue)
	s52DEPSC = color.RGBA{0x57, 0x66, 0x70, 0xFF} // safety contour line (bolder)

	// Light/buoy accents.
	s52LITRD = color.RGBA{0xFF, 0x00, 0x00, 0xFF} // red light flare
	s52LITGN = color.RGBA{0x00, 0xC0, 0x00, 0xFF} // green light flare
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
		// Drying check uses the shallower edge (DRVAL1 < 0). Band selection
		// uses the deeper edge (DRVAL2) so an offshore polygon spanning
		// "5 to 100 ft" reads as deep — matches NOAA's WMS, which paints
		// such polygons DDEAF7 / white rather than the saturated DEPVS our
		// shallow-edge keying produced.
		if !math.IsNaN(min) && min < 0 {
			return s52DEPIT
		}
		key := max
		if math.IsNaN(key) {
			key = min
		}
		if math.IsNaN(key) {
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
		// Unsurveyed area: pale grey, mostly transparent.
		return color.RGBA{0xE0, 0xE0, 0xE0, 0xC0}
	}
	return nil
}

// S-52 contour thresholds in metres. SHALLOW separates drying-ish from
// "covered but shallow"; DEEP separates "safe but mid-depth" from "well past
// safe". DEEP at 7.5 m matches what NOAA's WMS appears to use — z=13
// sampling shows polygons with DRVAL2 ≥ ~9 m painted as DEPDW (white),
// which only happens with DEEP < 9.
const (
	s52ShallowContourM = 2.0
	s52DeepContourM    = 7.5
)

// depthFill returns the water fill for a DEPARE polygon.
//
// At chart-detail zoom (z≥13) we use the full four-band scheme:
//
//   - depth < 0                      → DEPIT (intertidal / drying)
//   - 0 ≤ depth < SHALLOW            → DEPVS (AFCDE1, drying-ish saturated)
//   - SHALLOW ≤ depth < safety       → DEPMS (D1DDEF, below-safety warning)
//   - safety ≤ depth < DEEP          → DEPMD (DDEAF7, safe but mid-depth)
//   - depth ≥ DEEP                   → DEPDW (white)
//
// At coarser zooms (z≤12) NOAA's WMS collapses to roughly two-colour
// shading — saturated blue for water below safety, plain white past it,
// with the intermediate bands either absent or much narrower. Sampling
// NOAA z=12 tiles shows ~50/50 white vs AFCDE1 with the intermediate
// blues nearly invisible. We match by remapping the DEPMD/DEPMS bands to
// DEPDW (white) at z≤12.
func depthFill(depthM, safeDepthM float64, z int) color.Color {
	if safeDepthM <= 0 {
		safeDepthM = s52DeepContourM
	}
	// Don't let safety creep below SHALLOW — we want a non-empty DEPMS band.
	if safeDepthM <= s52ShallowContourM {
		safeDepthM = s52ShallowContourM + 1
	}
	deep := s52DeepContourM
	if deep < safeDepthM {
		deep = safeDepthM * 1.5
	}
	if depthM < 0 {
		return s52DEPIT
	}
	if depthM < s52ShallowContourM {
		return s52DEPVS
	}
	if z <= 12 {
		// Two-colour shading at coarse zoom. NOAA's WMS effective threshold
		// at z=12 lands around 10 m (sampled tiles split ~60/40 white vs
		// AFCDE1 with the intermediate bands collapsed). Override the
		// user's per-boat safety here so the chart-display tone matches
		// NOAA — the per-boat shading is more useful at chart-detail zoom
		// where channel polygons are visible anyway.
		threshold := s52DeepContourM
		if depthM < threshold {
			return s52DEPVS
		}
		return s52DEPDW
	}
	if depthM < safeDepthM {
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

func lineStroke(class string, f *s57.Feature) (color.Color, float64) {
	_ = f
	switch class {
	case "COALNE", "SLCONS":
		// Coastline / shoreline construction: solid black, full weight.
		return s52CSTLN, 1.4
	case "DEPCNT":
		// Depth contour: pale blue. Safety-contour bolding (DEPCNT02 in
		// S-52) would require comparing the contour's VALDCO to the boat's
		// safety depth; we keep it uniform here.
		return s52DEPCN, 0.6
	case "NAVLNE", "RECTRC", "FAIRWY", "ACHARE", "DWRTPT", "TWRTPT", "RESARE":
		// Channel limit / recommended track / fairway / anchorage / deep-
		// water route / restricted area — magenta boundary line.
		return s52CHMGD, 0.8
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
// (and a second colour stripes for safe-water/isolated-danger marks).
func drawPoint(dc *gg.Context, class string, f *s57.Feature, project func(lon, lat float64) (float64, float64), scale float64) {
	coords := f.Geometry().Coordinates
	at := func(c []float64) (float64, float64) { return project(c[0], c[1]) }

	first := func(draw func(px, py float64)) {
		if len(coords) == 0 || len(coords[0]) < 2 {
			return
		}
		px, py := at(coords[0])
		draw(px, py)
	}

	switch class {
	case "BOYLAT", "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB":
		first(func(px, py float64) {
			drawBuoy(dc, f, px, py, scale)
			drawNavaidLabel(dc, f, px, py, scale)
		})
	case "BCNLAT", "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		first(func(px, py float64) {
			drawBeacon(dc, f, px, py, scale)
			drawNavaidLabel(dc, f, px, py, scale)
		})
	case "LIGHTS":
		first(func(px, py float64) {
			drawLight(dc, f, px, py, scale)
			drawLightLabel(dc, f, px, py, scale)
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
		drawSoundings(dc, coords, project, scale)
	}
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

func drawSoundings(dc *gg.Context, coords [][]float64, project func(lon, lat float64) (float64, float64), scale float64) {
	at := func(c []float64) (float64, float64) { return project(c[0], c[1]) }
	dc.SetColor(s52SNDG1)
	dc.SetFontFace(basicfont.Face7x13)
	fontScale := 0.45 * scale
	dotR := 0.7 * scale
	for _, c := range coords {
		if len(c) < 2 {
			continue
		}
		px, py := at(c)
		if len(c) < 3 {
			dc.DrawCircle(px, py, dotR)
			dc.Fill()
			continue
		}
		depthM := c[2]
		if math.IsNaN(depthM) || depthM < 0 {
			dc.DrawCircle(px, py, dotR)
			dc.Fill()
			continue
		}
		depthFt := math.Round(depthM * feetPerMetre)
		label := fmt.Sprintf("%d", int(depthFt))
		dc.Push()
		dc.ScaleAbout(fontScale, fontScale, px, py)
		dc.DrawStringAnchored(label, px, py, 0.5, 0.5)
		dc.Pop()
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
