package vc

import (
	"bytes"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"

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

// RenderTile draws a 256x256 PNG for the given XYZ tile. If no cells overlap, a
// transparent tile is returned so the layer composes cleanly with the basemap.
func (r *ENCRenderer) RenderTile(z, x, y int) ([]byte, error) {
	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)

	cells := r.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)

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

	drawCell := func(chart *s57.Chart, pass drawPass) {
		for _, f := range chart.FeaturesInBounds(bbox) {
			drawFeature(dc, &f, pass, project)
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

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawFeature dispatches to the right pass based on the feature's object class
// and geometry. Anything we don't recognise is silently skipped.
func drawFeature(dc *gg.Context, f *s57.Feature, pass drawPass, project func(lon, lat float64) (float64, float64)) {
	geom := f.Geometry()
	if len(geom.Coordinates) == 0 {
		return
	}
	class := f.ObjectClass()

	switch geom.Type {
	case s57.GeometryTypePolygon:
		if pass != passAreas {
			return
		}
		fill := areaFill(class, f)
		if fill == nil {
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
		dc.ClosePath()
		dc.SetColor(fill)
		dc.Fill()

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
		dc.SetLineWidth(width)
		dc.Stroke()

	case s57.GeometryTypePoint:
		if pass != passPoints {
			return
		}
		drawPoint(dc, class, f, project)
	}
}

// areaFill returns the fill colour for a polygon feature, or nil to skip.
func areaFill(class string, f *s57.Feature) color.Color {
	switch class {
	case "LNDARE": // land area
		return color.RGBA{0xF4, 0xE5, 0xBC, 0xFF}
	case "DEPARE": // depth area — shade by the shallower edge depth
		min, _ := depthRange(f)
		return depthFill(min)
	case "DRGARE":
		return color.RGBA{0xCC, 0xE0, 0xF2, 0xFF}
	case "UNSARE":
		return color.RGBA{0xE0, 0xE0, 0xE0, 0x80}
	case "BUAARE": // built-up area
		return color.RGBA{0xE5, 0xC8, 0xA8, 0xFF}
	}
	return nil
}

// depthFill returns the water fill for a DEPARE based on the minimum depth
// (DRVAL1). Shallower water is rendered darker so it stands out at a glance.
func depthFill(minDepth float64) color.Color {
	switch {
	case minDepth < 2:
		return color.RGBA{0x9F, 0xC9, 0xE5, 0xFF}
	case minDepth < 5:
		return color.RGBA{0xBC, 0xDA, 0xEF, 0xFF}
	case minDepth < 10:
		return color.RGBA{0xD2, 0xE6, 0xF4, 0xFF}
	case minDepth < 20:
		return color.RGBA{0xE3, 0xF0, 0xF8, 0xFF}
	default:
		return color.RGBA{0xEE, 0xF6, 0xFB, 0xFF}
	}
}

func depthRange(f *s57.Feature) (min, max float64) {
	min = math.NaN()
	max = math.NaN()
	if v, ok := f.Attribute("DRVAL1"); ok {
		min = numAttr(v)
	}
	if v, ok := f.Attribute("DRVAL2"); ok {
		max = numAttr(v)
	}
	if math.IsNaN(min) {
		min = 0
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
	case "NAVLNE", "RECTRC":
		return color.RGBA{0x99, 0x33, 0x99, 0xCC}, 0.8
	case "RIVERS":
		return color.RGBA{0x7F, 0xB0, 0xCB, 0xFF}, 0.8
	}
	return nil, 0
}

// drawPoint renders point/multi-point features (buoys, beacons, lights,
// hazards, soundings). The shapes are intentionally simple — no S-52 symbology —
// but the colours track the COLOUR attribute when present.
func drawPoint(dc *gg.Context, class string, f *s57.Feature, project func(lon, lat float64) (float64, float64)) {
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
			dc.SetColor(buoyColour(f))
			dc.DrawCircle(px, py, 3)
			dc.Fill()
			dc.SetColor(color.Black)
			dc.SetLineWidth(0.5)
			dc.DrawCircle(px, py, 3)
			dc.Stroke()
		})
	case "BCNLAT", "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		first(func(px, py float64) {
			dc.SetColor(buoyColour(f))
			dc.DrawRectangle(px-2.5, py-2.5, 5, 5)
			dc.Fill()
			dc.SetColor(color.Black)
			dc.SetLineWidth(0.5)
			dc.DrawRectangle(px-2.5, py-2.5, 5, 5)
			dc.Stroke()
		})
	case "LIGHTS":
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0xFF, 0xCC, 0x00, 0xFF})
			dc.DrawCircle(px, py, 3.5)
			dc.Fill()
			dc.SetColor(color.RGBA{0xCC, 0x00, 0xCC, 0xFF})
			dc.SetLineWidth(0.8)
			dc.DrawCircle(px, py, 3.5)
			dc.Stroke()
		})
	case "WRECKS", "OBSTRN", "UWTROC":
		first(func(px, py float64) {
			dc.SetColor(color.RGBA{0xCC, 0x00, 0x00, 0xFF})
			dc.SetLineWidth(1.5)
			dc.DrawLine(px-3, py-3, px+3, py+3)
			dc.DrawLine(px-3, py+3, px+3, py-3)
			dc.Stroke()
		})
	case "SOUNDG":
		// SOUNDG is a multi-point: draw each sounding as a small blue dot. Depth
		// labels would require a font face; that's a follow-up.
		dc.SetColor(color.RGBA{0x44, 0x77, 0xAA, 0xFF})
		for _, c := range coords {
			if len(c) < 2 {
				continue
			}
			px, py := at(c)
			dc.DrawCircle(px, py, 0.9)
			dc.Fill()
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

func numAttr(v interface{}) float64 {
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
