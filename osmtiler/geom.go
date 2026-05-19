package osmtiler

import "math"

// GeomKind tells the renderer how to interpret a feature's coordinate
// sequence: a single point, an open polyline, or a closed polygon.
type GeomKind uint8

const (
	GeomPoint GeomKind = iota
	GeomLine
	GeomPolygon
)

// LonLat is a (longitude, latitude) coordinate in WGS84 degrees.
type LonLat struct{ Lon, Lat float64 }

// Feature is a single drawable item — a road, a building, a place
// label anchor, etc. Coordinates are kept in lon/lat so the same
// feature set can be rendered into any tile (the per-tile projection
// happens in render.go).
//
// MinLon/Min/Max are the lon/lat bounding box, populated once at load
// time so the renderer can reject off-tile features without touching
// every coordinate. Without this, drawing a single high-zoom tile from
// a city-sized feature set would iterate ~all of them.
type Feature struct {
	Class  Class
	Kind   GeomKind
	Coords []LonLat
	Name   string // empty when no label is wanted

	MinLon, MaxLon float64
	MinLat, MaxLat float64

	// MinZoom is the smallest zoom at which this feature's geometry
	// should be drawn. Below it the renderer skips the feature
	// entirely. Used to mirror osm-carto's road-class / building /
	// landuse thresholds so low-zoom tiles don't smear every
	// residential street and building footprint into a gray mess.
	MinZoom uint8

	// MinLabelZoom is the smallest zoom at which the label for this
	// feature should be drawn. 0 means "never label". Populated at
	// load time so the render path doesn't need the original tags.
	MinLabelZoom uint8

	// RoadKind sub-classifies ClassRoad features (motorway, primary,
	// residential, ...) so the renderer can pick per-kind colors,
	// widths, and paint order. Zero (RoadUnknown) for non-road
	// features.
	RoadKind RoadKind

	// Ref is the route number tag (`ref=`) for roads — "I-95",
	// "NY-9A", "M25" etc. Used by the shield pass to draw small
	// labelled markers along the route. Empty for almost every
	// feature; the per-feature string-header cost is real but
	// kept here so the renderer doesn't need a side table.
	Ref string
}

// computeBounds fills in MinLon/MaxLon/MinLat/MaxLat from Coords.
func (f *Feature) computeBounds() {
	if len(f.Coords) == 0 {
		return
	}
	f.MinLon, f.MaxLon = f.Coords[0].Lon, f.Coords[0].Lon
	f.MinLat, f.MaxLat = f.Coords[0].Lat, f.Coords[0].Lat
	for _, c := range f.Coords[1:] {
		if c.Lon < f.MinLon {
			f.MinLon = c.Lon
		} else if c.Lon > f.MaxLon {
			f.MaxLon = c.Lon
		}
		if c.Lat < f.MinLat {
			f.MinLat = c.Lat
		} else if c.Lat > f.MaxLat {
			f.MaxLat = c.Lat
		}
	}
}

// FeatureSet is the in-memory result of LoadPBF. For planet-scale runs
// we'll replace this with the SQLite per-tile index from the plan; at
// regional/city scale (this v0.1 slice) holding everything in memory
// is simpler and keeps the render loop free of I/O.
type FeatureSet struct {
	Features []Feature
}

// --- Web Mercator (EPSG:3857) tile math --------------------------------
//
// Standard XYZ tile scheme matching tile.openstreetmap.org:
//   - Tile (z, x, y) at zoom z has 2^z tiles per side.
//   - x grows east from longitude -180, y grows south from latitude +85.05113.
//   - Each tile is 256 px on a side at native resolution.

// LonLatToWorldPx converts a lon/lat to global pixel coordinates at
// zoom z (i.e. world-pixel-units where one tile = 256 units). Sub-pixel
// precision is preserved; callers floor as needed.
func LonLatToWorldPx(lon, lat float64, z int) (px, py float64) {
	n := math.Exp2(float64(z))
	px = (lon + 180.0) / 360.0 * n * 256.0
	latRad := lat * math.Pi / 180.0
	py = (1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * n * 256.0
	return
}

// LonLatToTile returns the integer (x, y) tile index containing
// (lon, lat) at zoom z. Off-world inputs are clamped to [0, 2^z).
func LonLatToTile(lon, lat float64, z int) (x, y int) {
	px, py := LonLatToWorldPx(lon, lat, z)
	n := int(math.Exp2(float64(z)))
	x = clamp(int(math.Floor(px/256.0)), 0, n-1)
	y = clamp(int(math.Floor(py/256.0)), 0, n-1)
	return
}

// LonLatToTilePx returns the (px, py) pixel position of (lon, lat)
// inside tile (z, x, y). May return values outside [0, 256) when the
// point is outside the tile (useful for clipping with a buffer).
func LonLatToTilePx(lon, lat float64, z, tileX, tileY int) (px, py float64) {
	wx, wy := LonLatToWorldPx(lon, lat, z)
	return wx - float64(tileX)*256.0, wy - float64(tileY)*256.0
}

// TileBoundsLonLat returns the lon/lat bounding box of tile (z, x, y):
// (minLon, minLat, maxLon, maxLat). Used by the renderer to reject
// features that lie entirely outside the tile.
func TileBoundsLonLat(z, x, y int) (minLon, minLat, maxLon, maxLat float64) {
	n := math.Exp2(float64(z))
	minLon = float64(x)/n*360.0 - 180.0
	maxLon = float64(x+1)/n*360.0 - 180.0
	// y grows south; tile-top is the larger latitude.
	maxLat = mercY2Lat(1.0 - 2.0*float64(y)/n)
	minLat = mercY2Lat(1.0 - 2.0*float64(y+1)/n)
	return
}

func mercY2Lat(t float64) float64 {
	// Inverse of (1 - log(tan(lat)+1/cos(lat))/pi)/2 → lat
	return math.Atan(math.Sinh(t*math.Pi)) * 180.0 / math.Pi
}

// TilesCoveringBBox returns the inclusive range of tile (x, y) indices
// at zoom z that overlap the bbox [(minLon, minLat), (maxLon, maxLat)].
func TilesCoveringBBox(minLon, minLat, maxLon, maxLat float64, z int) (xMin, yMin, xMax, yMax int) {
	xMin, yMax = LonLatToTile(minLon, minLat, z) // SW corner → smallest x, largest y
	xMax, yMin = LonLatToTile(maxLon, maxLat, z) // NE corner → largest x, smallest y
	if xMin > xMax {
		xMin, xMax = xMax, xMin
	}
	if yMin > yMax {
		yMin, yMax = yMax, yMin
	}
	return
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
