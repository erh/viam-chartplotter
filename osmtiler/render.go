package osmtiler

import (
	"bytes"
	"image/color"
	"image/png"

	"github.com/fogleman/gg"
)

// TileSize is the side length, in pixels, of every rendered tile.
// Matches tile.openstreetmap.org's 256x256 raster output.
const TileSize = 256

// RenderTile rasterises the (z, x, y) tile from fs. Background is fully
// transparent — the chart renderer underneath supplies water and base
// fills; we draw only land features. Returns PNG bytes.
//
// This is the v0.1 render path: per-class flat colors, no style port,
// no labels. Just enough to validate geometry + projection + the "no
// water" guarantee against real data. See OSM_TILES_PLAN.md for the
// v0.2 work (carto-style port, line/area labels, halos).
func RenderTile(fs *FeatureSet, z, x, y int) ([]byte, error) {
	dc := gg.NewContext(TileSize, TileSize)
	// Leave the buffer transparent — gg's NewContext gives us
	// (0,0,0,0) RGBA already.

	tMinLon, tMinLat, tMaxLon, tMaxLat := TileBoundsLonLat(z, x, y)
	// A small overdraw margin in degrees so lines whose endpoints are
	// just outside the tile still paint the portion that crosses in.
	const pad = 1e-4

	// FeatureSet is pre-sorted into painter's order at load time.
	for i := range fs.Features {
		f := &fs.Features[i]
		if f.MaxLon < tMinLon-pad || f.MinLon > tMaxLon+pad ||
			f.MaxLat < tMinLat-pad || f.MinLat > tMaxLat+pad {
			continue
		}
		drawFeature(dc, f, z, x, y)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawOrder returns the painter's-algorithm priority for a class —
// lower numbers paint first. Pulled from osm-carto's layer order,
// trimmed for the v0.1 class set.
func drawOrder(c Class) int {
	switch c {
	case ClassLanduse:
		return 0
	case ClassNatural:
		return 1
	case ClassLeisure:
		return 2
	case ClassBuilding:
		return 5
	case ClassRoad:
		return 6
	case ClassRailway:
		return 7
	case ClassAeroway:
		return 8
	case ClassAdmin:
		return 9
	case ClassPlace:
		return 10
	case ClassPOI:
		return 11
	}
	return 100
}

func drawFeature(dc *gg.Context, f *Feature, z, x, y int) {
	switch f.Kind {
	case GeomPoint:
		drawPoint(dc, f, z, x, y)
	case GeomLine:
		drawLine(dc, f, z, x, y)
	case GeomPolygon:
		drawPolygon(dc, f, z, x, y)
	}
}

func drawPoint(dc *gg.Context, f *Feature, z, x, y int) {
	px, py := LonLatToTilePx(f.Coords[0].Lon, f.Coords[0].Lat, z, x, y)
	if px < -4 || px > TileSize+4 || py < -4 || py > TileSize+4 {
		return
	}
	style := classStyle(f.Class, z)
	r := style.pointRadius
	if r <= 0 {
		return
	}
	dc.SetColor(style.fill)
	dc.DrawCircle(px, py, r)
	dc.Fill()
}

func drawLine(dc *gg.Context, f *Feature, z, x, y int) {
	style := classStyle(f.Class, z)
	if style.strokeWidth <= 0 {
		return
	}
	dc.SetColor(style.stroke)
	dc.SetLineWidth(style.strokeWidth)
	dc.SetDash(style.dash...)
	moved := false
	for _, c := range f.Coords {
		px, py := LonLatToTilePx(c.Lon, c.Lat, z, x, y)
		if !moved {
			dc.MoveTo(px, py)
			moved = true
		} else {
			dc.LineTo(px, py)
		}
	}
	dc.Stroke()
	dc.SetDash() // reset
}

func drawPolygon(dc *gg.Context, f *Feature, z, x, y int) {
	style := classStyle(f.Class, z)
	if style.fill == nil && style.stroke == nil {
		return
	}
	moved := false
	for _, c := range f.Coords {
		px, py := LonLatToTilePx(c.Lon, c.Lat, z, x, y)
		if !moved {
			dc.MoveTo(px, py)
			moved = true
		} else {
			dc.LineTo(px, py)
		}
	}
	dc.ClosePath()
	if style.fill != nil {
		dc.SetColor(style.fill)
		if style.stroke != nil {
			dc.FillPreserve()
		} else {
			dc.Fill()
		}
	}
	if style.stroke != nil {
		dc.SetColor(style.stroke)
		dc.SetLineWidth(style.strokeWidth)
		dc.Stroke()
	}
}

// styleSpec is the per-class drawing recipe. v0.1 uses flat colors
// inspired by openstreetmap-carto's palette; v0.2 replaces this with
// a tag-aware rule table (highway=motorway vs residential vs service,
// landuse=forest vs residential vs industrial, etc.).
type styleSpec struct {
	fill        color.Color
	stroke      color.Color
	strokeWidth float64
	dash        []float64
	pointRadius float64
}

func classStyle(c Class, z int) styleSpec {
	switch c {
	case ClassLanduse:
		return styleSpec{fill: rgba(0xE8, 0xE0, 0xD0, 0xFF)}
	case ClassNatural:
		return styleSpec{fill: rgba(0xC8, 0xDC, 0xB0, 0xFF)}
	case ClassLeisure:
		return styleSpec{fill: rgba(0xC0, 0xE0, 0xB0, 0xFF)}
	case ClassBuilding:
		return styleSpec{
			fill:        rgba(0xD0, 0xC8, 0xC0, 0xFF),
			stroke:      rgba(0xA0, 0x98, 0x90, 0xFF),
			strokeWidth: 0.5,
		}
	case ClassRoad:
		w := 1.0
		if z >= 14 {
			w = 1.5 + float64(z-14)*0.6
		}
		return styleSpec{stroke: rgba(0x60, 0x60, 0x60, 0xFF), strokeWidth: w}
	case ClassRailway:
		return styleSpec{
			stroke:      rgba(0x40, 0x40, 0x40, 0xFF),
			strokeWidth: 1.2,
			dash:        []float64{4, 3},
		}
	case ClassAeroway:
		return styleSpec{stroke: rgba(0xBB, 0xBB, 0xCC, 0xFF), strokeWidth: 3}
	case ClassAdmin:
		return styleSpec{
			stroke:      rgba(0x88, 0x44, 0x88, 0xFF),
			strokeWidth: 1,
			dash:        []float64{6, 2, 2, 2},
		}
	case ClassPlace:
		// Dot for now — labels land in the next slice.
		r := 0.0
		if z >= 6 {
			r = 1.5
		}
		if z >= 10 {
			r = 3
		}
		return styleSpec{fill: rgba(0x20, 0x20, 0x20, 0xFF), pointRadius: r}
	case ClassPOI:
		r := 0.0
		if z >= 16 {
			r = 2
		}
		return styleSpec{fill: rgba(0x44, 0x44, 0x88, 0xFF), pointRadius: r}
	}
	return styleSpec{}
}

func rgba(r, g, b, a uint8) color.Color {
	return color.NRGBA{R: r, G: g, B: b, A: a}
}
