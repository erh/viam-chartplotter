package render

import "math"

// Tile / Web-Mercator math used by the renderer. These mirror the helpers in
// the WMS cache (package vc) but live here so the render package is
// self-contained and never imports vc (which would be an import cycle, since
// vc imports render).

// mercatorMax is the Web-Mercator (EPSG:3857) extent half-width in metres.
const mercatorMax = 20037508.342789244

// tileXYZ identifies a slippy-map tile.
type tileXYZ struct{ x, y, z int }

// lonLatToTile returns the XY tile indices containing a lon/lat at zoom z.
func lonLatToTile(lon, lat float64, z int) (int, int) {
	n := float64(int(1) << z)
	x := int((lon + 180.0) / 360.0 * n)
	latRad := lat * math.Pi / 180.0
	y := int((1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * n)
	return x, y
}

// tileBBoxMercator returns a tile's bounds in Web-Mercator metres.
func tileBBoxMercator(t tileXYZ) (float64, float64, float64, float64) {
	size := 2 * mercatorMax / float64(int(1)<<t.z)
	xmin := -mercatorMax + float64(t.x)*size
	xmax := xmin + size
	ymax := mercatorMax - float64(t.y)*size
	ymin := ymax - size
	return xmin, ymin, xmax, ymax
}

// haversineMeters is the great-circle distance between two lon/lat points.
func haversineMeters(aLat, aLng, bLat, bLng float64) float64 {
	const R = 6371000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	lat1 := toRad(aLat)
	lat2 := toRad(bLat)
	dLat := toRad(bLat - aLat)
	dLng := toRad(bLng - aLng)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}
