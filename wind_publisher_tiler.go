package vc

import (
	"fmt"
	"math"
)

// Wind-tile publisher: globe is partitioned into a fixed `tileGridCols ×
// tileGridRows` grid of nominal bands, each tile *published* with an
// `tileOverlapDeg` margin so a chartplotter viewport that strays a few
// degrees past a band boundary still finds its data inside one tile.
// Clients snap their viewport to the tile whose published bbox contains
// it; if no single tile covers the viewport (very-zoomed-out views) the
// caller falls back to stitching or to the legacy global JSON.
//
// The grid is intentionally fixed at the package level: changing it
// invalidates every published .json.gz under wind/<model>/<cycle>/, so
// any change must come with a cycle-dir bump (handled by the publisher
// versioning, not silent rebalancing of existing files).
const (
	tileGridCols    = 6    // 60° lon each
	tileGridRows    = 6    // 30° lat each
	tileOverlapDeg  = 10.0 // published margin on every edge
	tileNominalLonW = -180.0
	tileNominalLatS = -90.0
)

// Tile names the published unit. Key is the cycle-stable slug clients
// derive from a viewport bbox; NominalBbox is the no-overlap band used
// for tile lookup; PublishedBbox is what the data file actually covers
// (NominalBbox grown by tileOverlapDeg on each edge, clamped to ±180
// in longitude and ±90 in latitude).
type Tile struct {
	Col, Row      int        // 0..tileGridCols-1, 0..tileGridRows-1
	Key           string     // slug for object-store keys, e.g. "lonW-180_latS-90"
	NominalBbox   [4]float64 // [w, s, e, n]
	PublishedBbox [4]float64 // [w, s, e, n] grown by overlap
}

// AllTiles returns the 36 fixed tiles in row-major order (row 0 = south
// pole band). The slice is freshly allocated each call so callers may
// mutate it freely; the underlying math is deterministic so callers
// that just want "the canonical tile for col=c row=r" can prefer
// TileForCell.
func AllTiles() []Tile {
	out := make([]Tile, 0, tileGridCols*tileGridRows)
	lonStep := 360.0 / tileGridCols
	latStep := 180.0 / tileGridRows
	for row := 0; row < tileGridRows; row++ {
		latS := tileNominalLatS + float64(row)*latStep
		latN := latS + latStep
		for col := 0; col < tileGridCols; col++ {
			lonW := tileNominalLonW + float64(col)*lonStep
			lonE := lonW + lonStep
			out = append(out, Tile{
				Col:           col,
				Row:           row,
				Key:           tileKey(lonW, latS),
				NominalBbox:   [4]float64{lonW, latS, lonE, latN},
				PublishedBbox: clampBbox(lonW-tileOverlapDeg, latS-tileOverlapDeg, lonE+tileOverlapDeg, latN+tileOverlapDeg),
			})
		}
	}
	return out
}

// tileKey builds the cycle-stable slug from a nominal lon/lat origin.
// We use the absolute longitude/latitude value plus a hemisphere suffix
// (W/E, S/N) rather than a raw signed number so the slugs sort
// intuitively and never contain a "-" that an object-store path
// component might mishandle (the previous design here used "lonW-180"
// which is fine but the underscore-separated form is easier for the
// browser-side derivation to parse).
func tileKey(lonW, latS float64) string {
	lonHemi := "E"
	lonAbs := lonW
	if lonW < 0 {
		lonHemi = "W"
		lonAbs = -lonW
	}
	latHemi := "N"
	latAbs := latS
	if latS < 0 {
		latHemi = "S"
		latAbs = -latS
	}
	return fmt.Sprintf("lon%s%g_lat%s%g", lonHemi, lonAbs, latHemi, latAbs)
}

// clampBbox snaps a published-with-overlap bbox to legal lat extents
// (±90). Longitude can legitimately go past ±180 in the published form
// — Crop handles the wrap — so we don't clamp it here, just leave it
// for the caller to interpret modulo 360.
func clampBbox(w, s, e, n float64) [4]float64 {
	if s < -90 {
		s = -90
	}
	if n > 90 {
		n = 90
	}
	return [4]float64{w, s, e, n}
}

// TileForBbox returns the tile whose NominalBbox contains the centre of
// `viewport` ([w,s,e,n]). The second return is false when the viewport
// is wider than one tile's published extent and the caller would need
// to stitch — in that case the caller can either fetch multiple tiles
// or fall back to the legacy global JSON for very-zoomed-out views.
//
// Longitudes are normalised to [-180, 180) before lookup so a viewport
// straddling the antimeridian still resolves to a sensible tile.
func TileForBbox(viewport [4]float64) (Tile, bool) {
	cx := (viewport[0] + viewport[2]) / 2
	cy := (viewport[1] + viewport[3]) / 2
	cx = wrapLon(cx)
	if cy < -90 {
		cy = -90
	} else if cy > 90 {
		cy = 90
	}
	lonStep := 360.0 / tileGridCols
	latStep := 180.0 / tileGridRows
	col := int(math.Floor((cx - tileNominalLonW) / lonStep))
	row := int(math.Floor((cy - tileNominalLatS) / latStep))
	if col < 0 {
		col = 0
	} else if col >= tileGridCols {
		col = tileGridCols - 1
	}
	if row < 0 {
		row = 0
	} else if row >= tileGridRows {
		row = tileGridRows - 1
	}
	tile := tileAt(col, row)
	if !bboxFitsInside(viewport, tile.PublishedBbox) {
		return tile, false
	}
	return tile, true
}

// tileAt rebuilds one Tile from grid indices without allocating the
// full AllTiles slice. Used by lookup paths that don't need iteration.
func tileAt(col, row int) Tile {
	lonStep := 360.0 / tileGridCols
	latStep := 180.0 / tileGridRows
	lonW := tileNominalLonW + float64(col)*lonStep
	latS := tileNominalLatS + float64(row)*latStep
	lonE := lonW + lonStep
	latN := latS + latStep
	return Tile{
		Col:           col,
		Row:           row,
		Key:           tileKey(lonW, latS),
		NominalBbox:   [4]float64{lonW, latS, lonE, latN},
		PublishedBbox: clampBbox(lonW-tileOverlapDeg, latS-tileOverlapDeg, lonE+tileOverlapDeg, latN+tileOverlapDeg),
	}
}

// bboxFitsInside reports whether `inner` is contained within `outer`
// (closed intervals on every side). Used by TileForBbox to decide
// whether the viewport's tile-of-centre is wide enough to satisfy the
// request without stitching.
func bboxFitsInside(inner, outer [4]float64) bool {
	return inner[0] >= outer[0] && inner[1] >= outer[1] &&
		inner[2] <= outer[2] && inner[3] <= outer[3]
}

// wrapLon normalises a longitude to [-180, 180). Used so a viewport
// reported in any of the conventional ranges (0..360, -180..180,
// 180..540) resolves to the same tile.
func wrapLon(lon float64) float64 {
	for lon < -180 {
		lon += 360
	}
	for lon >= 180 {
		lon -= 360
	}
	return lon
}

// CropWindRecord returns a new windRecord whose grid covers `bbox`
// ([lonW, latS, lonE, latN]) at the source's native resolution. The
// source is assumed to be a global regular_ll grid in row-major
// north-to-south scan order (scan mode 0); ECMWF Open Data's
// Lo1=180/Lo2=179.75 convention is handled via longitude wrap.
//
// The returned record's header has Lo1=lonW, La1=latN, Lo2/La2 at the
// last cell, Nx/Ny set by the bbox, Dx/Dy/ScanMode carried over from
// the source. Data is dense (no missing cells); cells outside the
// source grid are not produced (the bbox is clamped to source extent
// in latitude; longitude wraps modulo 360).
func CropWindRecord(src windRecord, bbox [4]float64) windRecord {
	lonW, latS, lonE, latN := bbox[0], bbox[1], bbox[2], bbox[3]
	dx := src.Header.Dx
	dy := src.Header.Dy
	srcNx := src.Header.Nx
	srcNy := src.Header.Ny
	srcLo1 := src.Header.Lo1
	srcLa1 := src.Header.La1

	// Clamp the bbox to the source's latitude extent. Going past the
	// poles makes no sense and would otherwise produce a row index off
	// the source grid.
	if latS < -90 {
		latS = -90
	}
	if latN > 90 {
		latN = 90
	}

	// Number of output cells. We round up by one on each edge to make
	// sure the requested extent is fully covered (so a chartplotter
	// viewport at exactly the published bbox boundary still has data).
	nx := int(math.Ceil((lonE-lonW)/dx)) + 1
	ny := int(math.Ceil((latN-latS)/dy)) + 1
	if nx < 1 {
		nx = 1
	}
	if ny < 1 {
		ny = 1
	}

	out := make([]float64, nx*ny)
	for j := 0; j < ny; j++ {
		// Target row j corresponds to lat = latN - j*dy (north-to-south).
		tlat := latN - float64(j)*dy
		// Source row 0 is at srcLa1 (= +90 for ECMWF). Find nearest
		// source row by rounding the row delta.
		sr := int(math.Round((srcLa1 - tlat) / dy))
		if sr < 0 {
			sr = 0
		} else if sr >= srcNy {
			sr = srcNy - 1
		}
		for i := 0; i < nx; i++ {
			tlon := lonW + float64(i)*dx
			// Source col 0 is at srcLo1; advance modulo 360 to cover
			// data sets whose grid origin isn't the prime meridian
			// (ECMWF Open Data uses srcLo1=180, so target lon=-100
			// lands at source col = ((-100-180) mod 360)/dx = 440).
			delta := tlon - srcLo1
			for delta < 0 {
				delta += 360
			}
			for delta >= 360 {
				delta -= 360
			}
			sc := int(math.Round(delta / dx))
			if sc < 0 {
				sc = 0
			} else if sc >= srcNx {
				sc -= srcNx // wrap exactly one column past the edge
				if sc < 0 || sc >= srcNx {
					sc = srcNx - 1
				}
			}
			out[j*nx+i] = src.Data[sr*srcNx+sc]
		}
	}

	hdr := src.Header
	hdr.Nx = nx
	hdr.Ny = ny
	hdr.Lo1 = lonW
	hdr.La1 = latN
	hdr.Lo2 = lonW + float64(nx-1)*dx
	hdr.La2 = latN - float64(ny-1)*dy
	return windRecord{Header: hdr, Data: out}
}
