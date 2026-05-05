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

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/draw"
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

	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
	// Paint coarsest first so finer-scale cells overwrite their detail on top.
	// CScale is the compilation-scale denominator, so larger CScale = coarser.
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].CScale > cells[j].CScale })

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

	// Collect soundings up-front so we can build the bathymetry layer once.
	// The polygon pass runs DEPARE included — DEPARE polygons paint their
	// own DRVAL1-based depth colour so dredged channels render as safe water
	// (e.g. a 14 m DRVAL1 → light blue) instead of being washed out by the
	// IDW blending in adjacent shallow soundings. Bathymetry then composites
	// on top with reduced opacity to soften polygon edges into a gradient.
	soundings := r.gatherSoundings(cells, bbox, project)
	hasBath := len(soundings) >= 12

	drawCell := func(chart *s57.Chart, pass drawPass) {
		for _, f := range chart.FeaturesInBounds(bbox) {
			drawFeature(dc, &f, pass, project, scale, safeDepthM, bbox, z)
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
		if pass == passAreas && hasBath {
			r.compositeBathymetry(dc, cells, bbox, project, soundings, safeDepthM, z,
				tileXmin, tileYmin, tileXmax, tileYmax)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// soundingPt is a depth seed for the IDW field, stored in EPSG:3857 mercator
// metres so the IDW computation is invariant under map zoom (the same physical
// location reads the same depth no matter how zoomed-in the tile is). Pixel
// space would not be invariant — at higher zoom, one metre is more pixels, so
// distance-based weights shift the field.
type soundingPt struct{ mx, my, depthM float64 }

// gatherSoundings collects every SOUNDG vertex AND every DEPARE polygon's
// centroid (plus stride-sampled vertices for normally-sized polygons) from
// the supplied cells, projected to mercator metres. SOUNDG gives the spot
// soundings; DEPARE centroids encode the polygon's DRVAL1 directly into the
// IDW field so dredged channels carry their canonical "this is N metres deep"
// reading even without any soundings.
//
// Oversized DEPARE polygons (continent-sized overview rings) are still
// allowed to seed the IDW via their centroid only — skipping their boundary
// vertices, which sit on the coast and would otherwise dominate coastal
// tiles' IDW with their offshore depth.
//
// Padding ensures features just outside the tile still influence pixels near
// the tile edge so the IDW field has no hard tile-boundary seams.
func (r *ENCRenderer) gatherSoundings(cells []ENCCell, bbox s57.Bounds, project func(lon, lat float64) (float64, float64)) []soundingPt {
	// Pad by max(50% of the tile, absMinPad). The relative term keeps things
	// proportional at low zoom; the absolute floor stabilises the seed set at
	// high zoom, where 50% of a tiny tile would otherwise gather only a
	// handful of nearby points and the IDW field would flip every time the
	// user zoomed in another step. ~0.05° ≈ 5.5 km is a reasonable IDW
	// influence radius for coastal navigation.
	const absMinPad = 0.05
	padLon := math.Max((bbox.MaxLon-bbox.MinLon)*0.5, absMinPad)
	padLat := math.Max((bbox.MaxLat-bbox.MinLat)*0.5, absMinPad)
	queryBox := s57.Bounds{
		MinLon: bbox.MinLon - padLon,
		MinLat: bbox.MinLat - padLat,
		MaxLon: bbox.MaxLon + padLon,
		MaxLat: bbox.MaxLat + padLat,
	}
	var points []soundingPt
	addSeed := func(lon, lat, depthM float64) {
		mx, my := lonLatToMerc(lon, lat)
		points = append(points, soundingPt{mx: mx, my: my, depthM: depthM})
	}
	for _, cell := range cells {
		chart, err := r.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(queryBox) {
			switch f.ObjectClass() {
			case "SOUNDG":
				for _, c := range f.Geometry().Coordinates {
					if len(c) < 3 {
						continue
					}
					if math.IsNaN(c[2]) || c[2] < 0 {
						continue
					}
					addSeed(c[0], c[1], c[2])
				}
			case "DEPARE":
				geom := f.Geometry()
				if geom.Type != s57.GeometryTypePolygon || len(geom.Coordinates) < 3 {
					continue
				}
				if isDegeneratePixelPolygon(geom.Coordinates, project) {
					continue
				}
				min, max := depthRange(&f)
				switch {
				case math.IsNaN(min) && math.IsNaN(max):
					continue
				case math.IsNaN(min):
					min = max
				case min == 0 && !math.IsNaN(max) && max > 5:
					// DRVAL1=0 with meaningful DRVAL2 is a range indicator;
					// use the deeper edge so the polygon doesn't seed the
					// IDW field as a drying flat.
					min = max
				}
				// Only the centroid seeds — never boundary vertices. Vertex
				// sampling caused a polygon's depth to bleed across its
				// edge into adjacent polygons (a deep offshore DEPARE's
				// coastline vertex would inject 200ft into a 12ft channel
				// next to it via IDW), producing the dark/light halos that
				// made deep channels read as shallow and vice versa.
				cx, cy := polygonCentroid(geom.Coordinates)
				addSeed(cx, cy, min)
			}
		}
	}
	return points
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

// compositeBathymetry composites a smooth depth-shaded layer on top of `dc`,
// masked by DEPARE polygon coverage. Bathymetry IDW gives the colour;
// DEPARE polygons define where it shows. Outside DEPARE (= land or
// uncharted) the underlying polygon fill (LNDARE tan etc.) stays visible.
//
// Render at quarter resolution (64x64) and bilinear-upsample for speed and
// to blend adjacent IDW samples into a smoother gradient. The mask is built
// at full 256x256 resolution so polygon edges stay crisp.
//
// IDW distance is computed in EPSG:3857 mercator metres so the depth field
// is invariant under map zoom. Doing it in pixel space caused the same
// physical location to read different depths at different zooms (since one
// metre is more pixels at higher zoom), which interacted with the hard step
// at the safety contour to flip a region between "shallow" and "deep" colour
// across small zoom changes.
func (r *ENCRenderer) compositeBathymetry(
	dc *gg.Context,
	cells []ENCCell,
	bbox s57.Bounds,
	project func(lon, lat float64) (float64, float64),
	points []soundingPt,
	safeDepthM float64,
	z int,
	tileXmin, tileYmin, tileXmax, tileYmax float64,
) {
	const downscale = 4
	const lowW, lowH = 256 / downscale, 256 / downscale

	// 1. Build the water mask by rasterising DEPARE polygons (with the same
	// oversize/degenerate guards as drawFeature) onto a separate gg.Context.
	// gg's anti-aliased fill produces a soft polygon edge — that gives the
	// bathymetry/land transition a clean fade rather than aliased stairsteps.
	maskCtx := gg.NewContext(256, 256)
	maskCtx.SetColor(color.RGBA{0xFF, 0xFF, 0xFF, 0xFF})
	for _, cell := range cells {
		chart, err := r.chartFor(cell.Name)
		if err != nil || chart == nil {
			continue
		}
		for _, f := range chart.FeaturesInBounds(bbox) {
			if f.ObjectClass() != "DEPARE" {
				continue
			}
			geom := f.Geometry()
			if geom.Type != s57.GeometryTypePolygon || len(geom.Coordinates) < 3 {
				continue
			}
			if isOversizedPolygon(geom.Coordinates, bbox, 50) {
				continue
			}
			if isDegeneratePixelPolygon(geom.Coordinates, project) {
				continue
			}
			tracePolygonPath(maskCtx, geom.Coordinates, project)
			maskCtx.Push()
			maskCtx.SetFillRuleEvenOdd()
			maskCtx.Fill()
			maskCtx.Pop()
		}
	}
	maskImg, ok := maskCtx.Image().(*image.RGBA)
	if !ok || maskImg == nil {
		return
	}

	// 2. IDW bathymetry at low resolution. Distances are in mercator metres so
	// the depth field is the same physical surface at any zoom.
	tileWMerc := tileXmax - tileXmin
	tileHMerc := tileYmax - tileYmin
	mxPerPx := tileWMerc / 256
	myPerPx := tileHMerc / 256
	// Floor IDW distance at half a (mercator) pixel² so a seed sitting exactly
	// on a sample point doesn't blow up the weight. This preserves the same
	// "near-zero distance" behaviour the old pixel-space IDW had, just scaled
	// to mercator metres.
	dsqFloor := 0.5 * mxPerPx * myPerPx
	low := image.NewRGBA(image.Rect(0, 0, lowW, lowH))
	for outY := range lowH {
		py := (float64(outY) + 0.5) * downscale
		// Mercator y decreases as image y increases (image y=0 is at top, where
		// mercator y is tileYmax).
		sampleMy := tileYmax - py*myPerPx
		for outX := range lowW {
			px := (float64(outX) + 0.5) * downscale
			sampleMx := tileXmin + px*mxPerPx
			sumW, sumD := 0.0, 0.0
			for _, p := range points {
				dx := p.mx - sampleMx
				dy := p.my - sampleMy
				dsq := dx*dx + dy*dy
				if dsq < dsqFloor {
					dsq = dsqFloor
				}
				// Power-3 IDW (1 / d^3): smoother than power-4 across
				// medium distances, sharper than power-2 at close range.
				w := 1.0 / (dsq * math.Sqrt(dsq))
				sumW += w
				sumD += w * p.depthM
			}
			depthM := sumD / sumW
			c := depthFill(depthM, safeDepthM, z)
			rgba, ok := c.(color.RGBA)
			if !ok {
				r0, g0, b0, a0 := c.RGBA()
				rgba = color.RGBA{R: uint8(r0 / 257), G: uint8(g0 / 257), B: uint8(b0 / 257), A: uint8(a0 / 257)}
			}
			low.SetRGBA(outX, outY, rgba)
		}
	}

	// 3. Bilinear-upsample the bathymetry to full tile resolution.
	high := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.BiLinear.Scale(high, high.Bounds(), low, low.Bounds(), draw.Over, nil)

	// 4. Apply DEPARE mask + reduced opacity. The mask multiplier limits
	// bathymetry to actual water; the opacity multiplier lets the underlying
	// DEPARE polygon colour (which carries the canonical "channel = safe"
	// information from DRVAL1) bleed through. The result reads as the
	// polygon depth-band coloring softened by the smooth IDW gradient.
	//
	// Opacity is intentionally low: DEPARE's DRVAL1-based fill is the
	// authoritative depth signal — sharp, accurate, derived directly from
	// chart data. The IDW field is only here to soften polygon edges into
	// a gradient, not to override the polygon's own depth band. Higher
	// opacity caused IDW artifacts (seed-dominated halos, cross-polygon
	// bleed) to overpower the chart's actual depth bands.
	const bathOpacity = 0.3
	for y := range 256 {
		for x := range 256 {
			ma := maskImg.RGBAAt(x, y).A
			if ma == 0 {
				high.SetRGBA(x, y, color.RGBA{})
				continue
			}
			c := high.RGBAAt(x, y)
			a := float64(c.A) * float64(ma) / 255 * bathOpacity
			c.A = uint8(a)
			high.SetRGBA(x, y, c)
		}
	}

	// 5. Composite onto the main canvas.
	dc.DrawImage(high, 0, 0)
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
func drawFeature(dc *gg.Context, f *s57.Feature, pass drawPass, project func(lon, lat float64) (float64, float64), scale, safeDepthM float64, tileBbox s57.Bounds, z int) {
	geom := f.Geometry()
	if len(geom.Coordinates) == 0 {
		return
	}
	class := f.ObjectClass()

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
		if isOversizedPolygon(geom.Coordinates, tileBbox, 50) {
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

// areaFill returns the fill colour for a polygon feature, or nil to skip.
// safeDepthM drives the DEPARE gradient.
//
// Philosophy: only fill classes that are ACTUALLY area-coloured on a paper
// chart. Fairways, anchorages, harbours, docks, ponton/pontoon, and pipeline
// areas all read as line/symbol/pattern features in S-52 — filling them with
// translucent colours composites badly over depth banding and produces the
// muddy olive/grey overlays we kept hitting. Leave their boundaries to
// `lineStroke`.
//
// Land-side classes (LNDARE, BUAARE) are intentionally not filled here: the
// noaa-local layer is composited over OSM, which already provides high-quality
// land detail (roads, buildings, marinas). Painting NOAA's tan over OSM would
// hide that detail. NOAA contributes only the marine layer — depth shading,
// dredged areas, and the lines/symbols handled elsewhere (COALNE, DEPCNT,
// buoys, soundings, wrecks).
func areaFill(class string, f *s57.Feature, safeDepthM float64, z int) color.Color {
	switch class {
	case "DEPARE": // depth area — gradient driven by the boat's safety contour
		min, max := depthRange(f)
		if math.IsNaN(min) {
			// Some NOAA cells include DEPARE polygons with no DRVAL1 (and
			// occasionally no DRVAL2 either). With nothing to key the gradient
			// off of, skip the fill — better to leave a hole than to flag open
			// ocean as a drying area.
			if math.IsNaN(max) {
				return nil
			}
			min = max
		} else if min == 0 && !math.IsNaN(max) && max > 5 {
			// DRVAL1=0 with a meaningful DRVAL2 is a *range* indicator
			// ("anywhere from drying to N metres") — typical of overview-cell
			// coastal-zone polygons. Painting these as drying flats turns
			// large stretches of open ocean dark blue wherever the ring's
			// rough geometry intersects a tile. Use the deeper edge so the
			// gradient lands somewhere reasonable instead.
			min = max
		}
		return depthFill(min, safeDepthM, z)
	case "DRGARE": // dredged area — slightly bluer than ambient water
		return color.RGBA{0xCC, 0xE0, 0xF2, 0xFF}
	case "LOKBSN": // lock basin
		return color.RGBA{0xC8, 0xD0, 0xE0, 0xFF}
	case "UNSARE": // unsurveyed area
		return color.RGBA{0xE0, 0xE0, 0xE0, 0x80}
	}
	return nil
}

// Depth-shading band scheme (feet, anchor positions on the right):
//
//   - drying / 0 ft ........................... black
//   - 0 ......... < safe                       smooth gradient black → dark navy
//   - safe ...... < safe × midFraction × deep  smooth gradient depthLight → depthMid
//   - safe × m × deep .. < safe × deep         smooth gradient depthMid → white
//   - >= safe × deep ........................... white
//
// Two smooth gradients in the safe-water zone, joined by a saturated mid
// anchor. The mid anchor gives the gradient a non-linear colour path so
// adjacent depths show visibly more variation than a flat pale→white lerp
// would produce.
//
// `deep` is the multiplier on safeDepth at which the colour fully reaches
// white. It scales with zoom: at chart-detail zoom (≥14) we want a wide
// gradient so coastal depths (30–100 ft) show visible variation; at overview
// zoom we compress so deeper water snaps to white quickly.
const (
	depthMidFraction = 0.30 // depthMid lands at safe + (deepEnd-safe) × this
)

var (
	depthBlack    = color.RGBA{0x00, 0x00, 0x00, 0xFF}
	depthDarkNavy = color.RGBA{0x0E, 0x29, 0x52, 0xFF} // approached from black just below safe
	depthLight    = color.RGBA{0xB5, 0xDA, 0xEE, 0xFF} // at safe — stark step up from dark navy
	depthMid      = color.RGBA{0x7A, 0xBE, 0xDC, 0xFF} // saturated mid anchor in the gradient
	depthWhite    = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF} // at and beyond safe × deepMultiplier
)

// depthDeepMultiplierFor returns the safe-multiple at which the depth
// gradient saturates to white for a given map zoom. Higher zooms get a
// wider gradient so coastal depths show variation; lower zooms compress so
// open ocean is mostly white.
//
// The mapping is linear in zoom (4 at z=10 → 30 at z=18) rather than
// bucketed: discrete jumps caused the same DEPARE polygon to render a
// visibly different colour across adjacent integer zooms (e.g. a 100 ft
// polygon went from "mostly white" at z=15 to "midblue" at z=16 because
// the multiplier jumped 20→30). Linear keeps each adjacent-zoom step ~3.3,
// so the same area looks consistent as the user zooms in or out.
func depthDeepMultiplierFor(z int) float64 {
	const minZ, maxZ = 10, 18
	const minMul, maxMul = 4.0, 30.0
	if z <= minZ {
		return minMul
	}
	if z >= maxZ {
		return maxMul
	}
	t := float64(z-minZ) / float64(maxZ-minZ)
	return minMul + (maxMul-minMul)*t
}

// depthFill returns the water fill for a DEPARE keyed off the boat's safety
// depth and the current map zoom. The zoom controls how far the gradient
// extends before hitting white (overview zooms snap to white sooner than
// chart-detail zooms).
func depthFill(minDepthM, safeDepthM float64, z int) color.Color {
	if safeDepthM <= 0 {
		safeDepthM = 1
	}
	minFt := minDepthM * feetPerMetre
	safeFt := safeDepthM * feetPerMetre
	deepEnd := safeFt * depthDeepMultiplierFor(z)

	// Drying area / above MLLW: solid black.
	if minFt <= 0 {
		return depthBlack
	}

	// Below safe: smooth gradient black → dark navy.
	if minFt < safeFt {
		return lerpRGBA(depthBlack, depthDarkNavy, minFt/safeFt)
	}

	// At or above safe: hard step to light blue, then a two-part gradient
	// through a saturated mid blue (depthMid) on the way to white. The mid
	// anchor gives adjacent depths visibly more chromatic variation than a
	// flat pale→white lerp.
	if minFt < deepEnd {
		midFt := safeFt + (deepEnd-safeFt)*depthMidFraction
		if minFt < midFt {
			return lerpRGBA(depthLight, depthMid, (minFt-safeFt)/(midFt-safeFt))
		}
		return lerpRGBA(depthMid, depthWhite, (minFt-midFt)/(deepEnd-midFt))
	}

	// Beyond safe × deep multiplier: solid white.
	return depthWhite
}

// lerpRGBA blends two solid RGBA colours, clamping t to [0, 1].
func lerpRGBA(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return color.RGBA{
		R: lerpByte(a.R, b.R, t),
		G: lerpByte(a.G, b.G, t),
		B: lerpByte(a.B, b.B, t),
		A: lerpByte(a.A, b.A, t),
	}
}

func lerpByte(a, b uint8, t float64) uint8 {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return uint8(float64(a) + (float64(b)-float64(a))*t)
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
		return color.RGBA{0x40, 0x40, 0x40, 0xFF}, 1.5
	case "DEPCNT":
		return color.RGBA{0x66, 0x99, 0xBB, 0xFF}, 0.7
	case "NAVLNE", "RECTRC", "FAIRWY", "ACHARE", "DWRTPT", "TWRTPT":
		// Channel limit / recommended track / fairway / anchorage / deep-water
		// route: conventional magenta boundary line. (Some of these come back
		// as polygons; we only stroke the ring, not fill the interior.)
		return color.RGBA{0x99, 0x33, 0x99, 0xCC}, 1.0
	case "RIVERS":
		return color.RGBA{0x7F, 0xB0, 0xCB, 0xFF}, 0.8
	case "BRIDGE", "CAUSWY":
		return color.RGBA{0x33, 0x33, 0x33, 0xFF}, 1.2
	case "PIPSOL", "CBLSUB", "CBLOHD":
		return color.RGBA{0x99, 0x44, 0x44, 0x99}, 0.7
	case "DAMCON":
		return color.RGBA{0x55, 0x55, 0x55, 0xFF}, 1.0
	case "PONTON":
		// Pontoon outline only — interior fill made marinas look black.
		return color.RGBA{0x55, 0x55, 0x55, 0xFF}, 0.8
	case "DOCARE", "HRBFAC", "HRBARE", "PIPARE":
		// Outline-only area features.
		return color.RGBA{0x66, 0x66, 0x66, 0x99}, 0.7
	}
	return nil, 0
}

// drawPoint renders point/multi-point features (buoys, beacons, lights,
// hazards, soundings). The shapes are intentionally simple — no S-52 symbology —
// but the colours track the COLOUR attribute when present. `scale` grows symbol
// sizes with zoom so a 3-px circle isn't a 1-px speck at z=16.
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
		r := 3 * scale
		first(func(px, py float64) {
			dc.SetColor(buoyColour(f))
			dc.DrawCircle(px, py, r)
			dc.Fill()
			dc.SetColor(color.Black)
			dc.SetLineWidth(0.5 * scale)
			dc.DrawCircle(px, py, r)
			dc.Stroke()
		})
	case "BCNLAT", "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		half := 2.5 * scale
		first(func(px, py float64) {
			dc.SetColor(buoyColour(f))
			dc.DrawRectangle(px-half, py-half, 2*half, 2*half)
			dc.Fill()
			dc.SetColor(color.Black)
			dc.SetLineWidth(0.5 * scale)
			dc.DrawRectangle(px-half, py-half, 2*half, 2*half)
			dc.Stroke()
		})
	case "LIGHTS":
		r := 3.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0xFF, 0xCC, 0x00, 0xFF})
			dc.DrawCircle(px, py, r)
			dc.Fill()
			dc.SetColor(color.RGBA{0xCC, 0x00, 0xCC, 0xFF})
			dc.SetLineWidth(0.8 * scale)
			dc.DrawCircle(px, py, r)
			dc.Stroke()
		})
	case "WRECKS", "OBSTRN":
		// Real wrecks/obstructions: bold red cross.
		arm := 2.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0xCC, 0x00, 0x00, 0xFF})
			dc.SetLineWidth(1.2 * scale)
			dc.DrawLine(px-arm, py-arm, px+arm, py+arm)
			dc.DrawLine(px-arm, py+arm, px+arm, py-arm)
			dc.Stroke()
		})
	case "UWTROC":
		// Underwater rock — thousands per harbor cell. Subtle small + symbol so
		// they don't blanket the chart. Use a thin, semi-transparent stroke.
		arm := 1.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0x99, 0x33, 0x33, 0xAA})
			dc.SetLineWidth(0.6 * scale)
			dc.DrawLine(px-arm, py, px+arm, py)
			dc.DrawLine(px, py-arm, px, py+arm)
			dc.Stroke()
		})
	case "MORFAC", "PILPNT", "MOORNG":
		// Mooring/dolphin/pile point — small dark square, useful at harbor zoom.
		half := 1.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0x33, 0x33, 0x33, 0xFF})
			dc.DrawRectangle(px-half, py-half, 2*half, 2*half)
			dc.Fill()
		})
	case "ACHBRT":
		// Anchorage berth — small open circle.
		r := 2.5 * scale
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0x99, 0x66, 0x00, 0xFF})
			dc.SetLineWidth(0.8 * scale)
			dc.DrawCircle(px, py, r)
			dc.Stroke()
		})
	case "SOUNDG":
		// SOUNDG is a multi-point: each coord is (lon, lat, depth_in_metres).
		// We render the depth in feet as a numeric label centred on the projected
		// pixel position. Soft slate-blue, smaller than the chart's other symbols
		// so dense fields don't visually dominate. If the Z coord is missing or
		// invalid we fall back to a dot so the location is still visible.
		soundColour := color.RGBA{0x55, 0x77, 0x99, 0xCC}
		dc.SetColor(soundColour)
		dc.SetFontFace(basicfont.Face7x13)
		// Face7x13 is 13px; baseline ~6px, then grow gently with zoom.
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
			depthFt := math.Round(depthM * 3.28084)
			label := fmt.Sprintf("%d", int(depthFt))
			dc.Push()
			dc.ScaleAbout(fontScale, fontScale, px, py)
			dc.DrawStringAnchored(label, px, py, 0.5, 0.5)
			dc.Pop()
		}
	}
}

// buoyColour reads the COLOUR attribute (S-57 codes 1..13). NOAA cells store it
// as a comma-separated string; we use the first colour present.
func buoyColour(f *s57.Feature) color.Color {
	v, ok := f.Attribute("COLOUR")
	if !ok {
		return color.RGBA{0x80, 0x80, 0x80, 0xFF}
	}
	s, _ := v.(string)
	if s == "" {
		return color.RGBA{0x80, 0x80, 0x80, 0xFF}
	}
	first := strings.SplitN(s, ",", 2)[0]
	switch strings.TrimSpace(first) {
	case "1":
		return color.White
	case "2":
		return color.Black
	case "3":
		return color.RGBA{0xCC, 0x00, 0x00, 0xFF}
	case "4":
		return color.RGBA{0x00, 0x99, 0x00, 0xFF}
	case "5":
		return color.RGBA{0x00, 0x44, 0xCC, 0xFF}
	case "6":
		return color.RGBA{0xFF, 0xCC, 0x00, 0xFF}
	case "7":
		return color.RGBA{0xCC, 0x66, 0x00, 0xFF}
	case "8":
		return color.RGBA{0xCC, 0x33, 0xCC, 0xFF}
	case "9":
		return color.RGBA{0xFF, 0x99, 0xCC, 0xFF}
	}
	return color.RGBA{0x80, 0x80, 0x80, 0xFF}
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
