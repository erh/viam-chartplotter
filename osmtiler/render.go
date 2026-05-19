package osmtiler

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sort"

	"github.com/fogleman/gg"
)

// TileSize is the side length, in pixels, of every rendered tile.
// Matches tile.openstreetmap.org's 256x256 raster output.
const TileSize = 256

// LabelBuffer is the per-side overdraw, in pixels, used by RenderTile
// for cross-tile label consistency. We rasterise into a canvas that's
// (TileSize + 2*LabelBuffer) on a side and crop to the inner TileSize
// at output time. A label whose anchor sits inside this tile's buffer
// area but in a neighbour's interior is still drawn here at the same
// position, so adjacent tiles stitch without text vanishing or shifting
// across the seam.
//
// 64 px ≈ enough for any single-word label at our largest font size.
// Bump if multi-word road labels start being clipped at tile edges.
const LabelBuffer = 64

const bufferedSize = TileSize + 2*LabelBuffer

// RenderTile rasterises the (z, x, y) tile from fs. Background is fully
// transparent — the chart renderer underneath supplies water and base
// fills; we draw only land features. Returns PNG bytes.
//
// Geometry uses the painter's algorithm with per-class fills/strokes
// from a v0.1 flat palette. After geometry, a label pass paints names
// for ClassPlace / ClassPOI features whose MinLabelZoom is reached.
// Both passes draw into a buffered canvas (TileSize + 2*LabelBuffer)
// and we crop on the way out, so a label that straddles the tile edge
// is fully painted on this side and on its neighbour at consistent
// coordinates.
func RenderTile(fs *FeatureSet, z, x, y int) ([]byte, error) {
	dc := gg.NewContext(bufferedSize, bufferedSize)
	// Shift the origin so feature projection still treats (0, 0) as
	// the top-left of the inner tile; everything in the buffer ring
	// rasterises into the surrounding LabelBuffer pixels.
	dc.Translate(LabelBuffer, LabelBuffer)

	tMinLon, tMinLat, tMaxLon, tMaxLat := TileBoundsLonLat(z, x, y)
	// Expand the lon/lat reject filter so features whose geometry
	// lives entirely in the buffer ring still get drawn.
	nTiles := math.Exp2(float64(z))
	bufDeg := float64(LabelBuffer) / TileSize * 360.0 / nTiles
	// Latitude pad is approximate (Mercator stretches near the poles);
	// at chart-use latitudes the lon pad is a close-enough overestimate.
	eMinLon, eMaxLon := tMinLon-bufDeg, tMaxLon+bufDeg
	eMinLat, eMaxLat := tMinLat-bufDeg, tMaxLat+bufDeg

	// Geometry pass — split into three sub-passes so road casings
	// and fills paint as two global sweeps. Otherwise residential
	// streets' casings would cut across motorway fills wherever they
	// intersect, instead of appearing to flow under them.
	//
	//   1. Non-road geometry (landuse, buildings, leisure, ...)
	//   2. All road casings, lowest-class first
	//   3. All road fills, lowest-class first
	zu8 := uint8(z)
	type roadCandidate struct {
		idx   int
		order int
	}
	var roads []roadCandidate

	for i := range fs.Features {
		f := &fs.Features[i]
		if zu8 < f.MinZoom {
			continue
		}
		if f.MaxLon < eMinLon || f.MinLon > eMaxLon ||
			f.MaxLat < eMinLat || f.MinLat > eMaxLat {
			continue
		}
		if f.Class == ClassRoad {
			roads = append(roads, roadCandidate{i, roadClassPaintOrder(f.RoadKind)})
			continue
		}
		drawFeature(dc, f, z, x, y)
	}

	sort.SliceStable(roads, func(i, j int) bool {
		return roads[i].order < roads[j].order
	})
	scale := roadWidthScale(z)
	for _, rc := range roads {
		f := &fs.Features[rc.idx]
		s := roadStyles[f.RoadKind]
		strokeRoadAlong(dc, f, z, x, y, s.casingColor, s.casingWidth*scale)
	}
	for _, rc := range roads {
		f := &fs.Features[rc.idx]
		s := roadStyles[f.RoadKind]
		strokeRoadAlong(dc, f, z, x, y, s.fillColor, s.fillWidth*scale)
	}

	// Label pass.
	placed, err := drawLabels(dc, fs, z, x, y, eMinLon, eMinLat, eMaxLon, eMaxLat)
	if err != nil {
		return nil, err
	}

	// Shields pass — paints route refs (I-95, NY-9A, ...) on top of
	// road labels but sharing the same collision tracker so they
	// don't pile up.
	if err := drawShields(dc, fs, z, x, y, eMinLon, eMinLat, eMaxLon, eMaxLat, &placed); err != nil {
		return nil, err
	}

	// Crop the buffered render down to the inner TileSize × TileSize.
	inner := image.NewRGBA(image.Rect(0, 0, TileSize, TileSize))
	draw.Draw(inner, inner.Bounds(), dc.Image(),
		image.Pt(LabelBuffer, LabelBuffer), draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, inner); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// labelRect is an axis-aligned bounding box used by the within-tile
// collision tracker. Line labels supply the AABB of their rotated
// glyph run so the same first-wins greedy logic applies to both
// point and curved labels.
type labelRect struct{ x0, y0, x1, y1 float64 }

func (r labelRect) overlaps(o labelRect) bool {
	return r.x0 < o.x1 && r.x1 > o.x0 && r.y0 < o.y1 && r.y1 > o.y0
}

// namedAnchor records where each line label landed so we can skip
// neighbouring same-name labels — OSM ways are often per-block, so
// a long avenue like Broadway is a chain of 20+ named ways that
// would otherwise label every segment.
type namedAnchor struct {
	name string
	x, y float64
}

// minSameNameLineDist is the centre-to-centre distance (tile pixels)
// below which two same-name line labels are considered duplicates.
const minSameNameLineDist = 150.0

// drawLabels paints names for every labellable feature. Point labels
// (ClassPlace, ClassPOI) sit above their anchor; line labels (named
// roads) follow the longest left-to-right segment of the way.
// Collisions are resolved greedy first-wins — features are pre-sorted
// into painter's order at load time, which puts higher-importance
// classes (Place < POI < Road) first so they claim space.
func drawLabels(dc *gg.Context, fs *FeatureSet, z, x, y int, tMinLon, tMinLat, tMaxLon, tMaxLat float64) ([]labelRect, error) {
	zu8 := uint8(z)
	var placed []labelRect
	var placedLineNames []namedAnchor

	// gg caches the active font face; we set per size as we encounter
	// new classes. Most tiles only have one or two class types in their
	// label pass so the switching cost is negligible.
	var curSize float64
	for i := range fs.Features {
		f := &fs.Features[i]
		if f.MinLabelZoom == 0 || zu8 < f.MinLabelZoom || f.Name == "" {
			continue
		}
		if f.MaxLon < tMinLon || f.MinLon > tMaxLon ||
			f.MaxLat < tMinLat || f.MinLat > tMaxLat {
			continue
		}
		size := labelSizeForClass(f.Class)
		if size != curSize {
			face, err := labelFontFace(size)
			if err != nil {
				return nil, err
			}
			dc.SetFontFace(face)
			curSize = size
		}

		switch f.Kind {
		case GeomPoint:
			drawPointLabel(dc, f, z, x, y, size, &placed)
		case GeomLine:
			drawLineLabel(dc, f, z, x, y, size, &placed, &placedLineNames)
		case GeomPolygon:
			drawAreaLabel(dc, f, z, x, y, size, &placed)
		}
	}
	return placed, nil
}

// drawAreaLabel places a name at the vertex-average centroid of a
// polygon (named parks, named landuse, etc.). Polygons whose bbox is
// narrower than the label's own width are skipped — at low zooms the
// 100s-of-meters-wide tile cells contain dozens of named small parks
// whose text would be wider than the park itself.
//
// Vertex average is a cheap stand-in for the true area-weighted
// centroid. For long thin polygons (Riverside Park) the result is
// roughly in the middle of the spread, which is acceptable; the
// proper "label point" (pole of inaccessibility) is a v0.4 polish item.
func drawAreaLabel(dc *gg.Context, f *Feature, z, x, y int, size float64, placed *[]labelRect) {
	var sumLon, sumLat float64
	for _, c := range f.Coords {
		sumLon += c.Lon
		sumLat += c.Lat
	}
	n := float64(len(f.Coords))
	cLon, cLat := sumLon/n, sumLat/n

	px, py := LonLatToTilePx(cLon, cLat, z, x, y)
	if px < -LabelBuffer || px > TileSize+LabelBuffer ||
		py < -LabelBuffer || py > TileSize+LabelBuffer {
		return
	}

	w, _ := dc.MeasureString(f.Name)

	// Cull small polygons whose visible width can't host the label.
	nTiles := math.Exp2(float64(z))
	lonPxPerDeg := 256 * nTiles / 360
	if (f.MaxLon-f.MinLon)*lonPxPerDeg < w {
		return
	}

	const pad = 2.0
	box := labelRect{
		x0: px - w/2 - pad, y0: py - size/2 - pad,
		x1: px + w/2 + pad, y1: py + size/2 + pad,
	}
	for _, p := range *placed {
		if box.overlaps(p) {
			return
		}
	}
	*placed = append(*placed, box)
	drawLabelWithHalo(dc, px, py, f.Name)
}

func drawPointLabel(dc *gg.Context, f *Feature, z, x, y int, size float64, placed *[]labelRect) {
	px, py := LonLatToTilePx(f.Coords[0].Lon, f.Coords[0].Lat, z, x, y)
	if px < -LabelBuffer || px > TileSize+LabelBuffer ||
		py < -LabelBuffer || py > TileSize+LabelBuffer {
		return
	}
	w, _ := dc.MeasureString(f.Name)
	const pad = 2.0
	cx, cy := px, py-7 // anchor 7px above the geometry dot
	box := labelRect{
		x0: cx - w/2 - pad, y0: cy - size/2 - pad,
		x1: cx + w/2 + pad, y1: cy + size/2 + pad,
	}
	for _, p := range *placed {
		if box.overlaps(p) {
			return
		}
	}
	*placed = append(*placed, box)
	drawLabelWithHalo(dc, cx, cy, f.Name)
}

// drawLineLabel lays a road's name along its polyline. It picks the
// longest segment whose direction is "readable" (closer to horizontal
// than vertical), reverses if running right-to-left, and walks the
// chosen segment placing glyphs one at a time with per-glyph rotation.
// Halo and fill are each one pass over the glyph run (8 + 1 = 9 total),
// which is cheap relative to gg's per-glyph state-push overhead.
func drawLineLabel(dc *gg.Context, f *Feature, z, x, y int, size float64, placed *[]labelRect, placedNames *[]namedAnchor) {
	pts := make([][2]float64, len(f.Coords))
	for i, c := range f.Coords {
		px, py := LonLatToTilePx(c.Lon, c.Lat, z, x, y)
		pts[i] = [2]float64{px, py}
	}

	w, _ := dc.MeasureString(f.Name)
	// Require a small amount of segment slack beyond the text width
	// so labels aren't crammed end-to-end on segment boundaries.
	const slack = 1.05

	// Pick the longest horizontal-ish segment that fits the label.
	bestIdx := -1
	bestLen := 0.0
	for i := 0; i+1 < len(pts); i++ {
		dx := pts[i+1][0] - pts[i][0]
		dy := pts[i+1][1] - pts[i][1]
		// Filter out near-vertical segments — text on them would
		// require a 90° rotation and be hard to read at low zooms.
		if math.Abs(dx) < math.Abs(dy) {
			continue
		}
		segLen := math.Sqrt(dx*dx + dy*dy)
		if segLen < w*slack {
			continue
		}
		if segLen > bestLen {
			bestLen = segLen
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return
	}
	p0, p1 := pts[bestIdx], pts[bestIdx+1]
	if p1[0] < p0[0] {
		p0, p1 = p1, p0 // ensure left-to-right
	}
	dx := p1[0] - p0[0]
	dy := p1[1] - p0[1]
	segLen := math.Sqrt(dx*dx + dy*dy)
	dirX, dirY := dx/segLen, dy/segLen
	angle := math.Atan2(dy, dx)

	// Center the label run along the segment.
	cursor := (segLen - w) / 2
	cx := p0[0] + dirX*(cursor+w/2)
	cy := p0[1] + dirY*(cursor+w/2)

	// Same-name dedup: if another instance of this name landed too
	// close already, skip this one. Avenues encoded as many small
	// ways (NYC's per-block tagging) would otherwise label every block.
	for _, prev := range *placedNames {
		if prev.name != f.Name {
			continue
		}
		dx, dy := prev.x-cx, prev.y-cy
		if dx*dx+dy*dy < minSameNameLineDist*minSameNameLineDist {
			return
		}
	}

	// Rotated AABB: |hw·cos| + |hh·sin| in x, |hw·sin| + |hh·cos| in y.
	hw, hh := w/2, size/2
	c, s := math.Abs(math.Cos(angle)), math.Abs(math.Sin(angle))
	ex := hw*c + hh*s
	ey := hw*s + hh*c
	const pad = 2.0
	box := labelRect{
		x0: cx - ex - pad, y0: cy - ey - pad,
		x1: cx + ex + pad, y1: cy + ey + pad,
	}
	// Reject if entirely outside the buffered canvas.
	if box.x1 < -LabelBuffer || box.x0 > TileSize+LabelBuffer ||
		box.y1 < -LabelBuffer || box.y0 > TileSize+LabelBuffer {
		return
	}
	for _, p := range *placed {
		if box.overlaps(p) {
			return
		}
	}
	*placed = append(*placed, box)
	*placedNames = append(*placedNames, namedAnchor{name: f.Name, x: cx, y: cy})

	drawTextAlongLine(dc, f.Name, p0, dirX, dirY, angle, cursor)
}

// drawTextAlongLine lays out runes from `text` along the line starting
// at p0 with direction (dirX, dirY), the first glyph's left edge at
// distance `cursor` from p0. White halo (8 offsets) + black fill =
// nine passes over the glyph run.
func drawTextAlongLine(dc *gg.Context, text string, p0 [2]float64, dirX, dirY, angle, cursor float64) {
	runes := []rune(text)

	paintPass := func(ox, oy float64) {
		c := cursor
		for _, r := range runes {
			gw, _ := dc.MeasureString(string(r))
			gx := p0[0] + dirX*(c+gw/2) + ox
			gy := p0[1] + dirY*(c+gw/2) + oy
			dc.Push()
			dc.RotateAbout(angle, gx, gy)
			dc.DrawStringAnchored(string(r), gx, gy, 0.5, 0.5)
			dc.Pop()
			c += gw
		}
	}

	dc.SetRGB(1, 1, 1)
	for _, dxo := range [...]float64{-1, 0, 1} {
		for _, dyo := range [...]float64{-1, 0, 1} {
			if dxo == 0 && dyo == 0 {
				continue
			}
			paintPass(dxo, dyo)
		}
	}
	dc.SetRGB(0, 0, 0)
	paintPass(0, 0)
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
