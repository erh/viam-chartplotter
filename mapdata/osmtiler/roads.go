package osmtiler

import (
	"image/color"

	"github.com/fogleman/gg"
)

// RoadKind sub-classifies ClassRoad features so the renderer can give
// motorways the heavy red casing, residential streets a thin white
// casing, etc. Mapped from the OSM `highway=` tag at load time.
type RoadKind uint8

const (
	RoadUnknown RoadKind = iota
	RoadMotorway
	RoadTrunk
	RoadPrimary
	RoadSecondary
	RoadTertiary
	RoadResidential
	RoadService
	RoadPath // pedestrian/footway/cycleway/path/etc.
)

func RoadKindFor(highway string) RoadKind {
	switch highway {
	case "motorway", "motorway_link":
		return RoadMotorway
	case "trunk", "trunk_link":
		return RoadTrunk
	case "primary", "primary_link":
		return RoadPrimary
	case "secondary", "secondary_link":
		return RoadSecondary
	case "tertiary", "tertiary_link":
		return RoadTertiary
	case "unclassified", "residential", "living_street":
		return RoadResidential
	case "service":
		return RoadService
	case "pedestrian", "footway", "cycleway", "path", "bridleway", "steps", "track":
		return RoadPath
	}
	return RoadResidential
}

// roadStyle holds the casing and fill recipe for a road kind. Widths
// are at "base zoom" (z=14); the renderer scales them up for higher
// zooms via roadWidthScale.
type roadStyle struct {
	casingColor color.Color
	fillColor   color.Color
	casingWidth float64
	fillWidth   float64
}

var roadStyles = map[RoadKind]roadStyle{
	RoadMotorway: {
		casingColor: rgba(0xc0, 0x4a, 0x4f, 0xff),
		fillColor:   rgba(0xe8, 0x92, 0xa2, 0xff),
		casingWidth: 5.0, fillWidth: 3.5,
	},
	RoadTrunk: {
		casingColor: rgba(0xc8, 0x4e, 0x2f, 0xff),
		fillColor:   rgba(0xf9, 0xb2, 0x9c, 0xff),
		casingWidth: 4.5, fillWidth: 3.0,
	},
	RoadPrimary: {
		casingColor: rgba(0xa0, 0x6b, 0x00, 0xff),
		fillColor:   rgba(0xfc, 0xd6, 0xa4, 0xff),
		casingWidth: 4.0, fillWidth: 2.8,
	},
	RoadSecondary: {
		casingColor: rgba(0x70, 0x7d, 0x05, 0xff),
		fillColor:   rgba(0xf7, 0xfa, 0xbf, 0xff),
		casingWidth: 3.5, fillWidth: 2.4,
	},
	RoadTertiary: {
		casingColor: rgba(0xb8, 0xb8, 0xb0, 0xff),
		fillColor:   rgba(0xff, 0xff, 0xff, 0xff),
		casingWidth: 3.0, fillWidth: 2.0,
	},
	RoadResidential: {
		casingColor: rgba(0xb8, 0xb8, 0xb8, 0xff),
		fillColor:   rgba(0xff, 0xff, 0xff, 0xff),
		casingWidth: 2.4, fillWidth: 1.6,
	},
	RoadService: {
		casingColor: rgba(0xc8, 0xc8, 0xc8, 0xff),
		fillColor:   rgba(0xff, 0xff, 0xff, 0xff),
		casingWidth: 1.8, fillWidth: 1.2,
	},
	RoadPath: {
		// Paths get no casing — they're rendered as a thin dashed
		// line in OSM-carto. We just give them a single thin stroke.
		fillColor: rgba(0x99, 0x66, 0x66, 0xff),
		fillWidth: 1.0,
	},
}

// roadWidthScale maps zoom → multiplier on the base width. Base is
// z=14; lower zooms shrink, higher zooms expand. Loose match to
// osm-carto's exponential ramp.
func roadWidthScale(z int) float64 {
	switch {
	case z <= 10:
		return 0.25
	case z == 11:
		return 0.35
	case z == 12:
		return 0.5
	case z == 13:
		return 0.7
	case z == 14:
		return 1.0
	case z == 15:
		return 1.4
	case z == 16:
		return 1.9
	case z == 17:
		return 2.6
	default: // z >= 18
		return 3.5
	}
}

// strokeRoadAlong applies a stroke of the given color/width to the
// road's polyline. The caller passes the stroke parameters; this
// helper just handles the moveTo/lineTo path-walking so both the
// casing pass and the fill pass can share it.
func strokeRoadAlong(dc *gg.Context, f *Feature, z, x, y int, c color.Color, width float64) {
	if c == nil || width <= 0 {
		return
	}
	dc.SetColor(c)
	dc.SetLineWidth(width)
	moved := false
	for _, p := range f.Coords {
		px, py := LonLatToTilePx(p.Lon, p.Lat, z, x, y)
		if !moved {
			dc.MoveTo(px, py)
			moved = true
		} else {
			dc.LineTo(px, py)
		}
	}
	dc.Stroke()
}

// shieldMinZoom is the smallest zoom at which a route shield should
// be drawn for this road class. Mirrors osm-carto's "carry the ref
// shield down to the major-road-network zoom" rules.
func shieldMinZoom(k RoadKind) int {
	switch k {
	case RoadMotorway:
		return 8
	case RoadTrunk:
		return 9
	case RoadPrimary:
		return 10
	case RoadSecondary:
		return 12
	}
	return 13
}

// drawShields paints route-number shields ("I-95", "NY-9A", ...) at
// the midpoint of each named road's polyline. Shields share the label
// collision tracker so they don't pile on top of road or place labels.
// Country-specific shield shapes (US interstate trefoil, UK motorway
// blue) are out of scope; we use a single rounded rectangle whose
// border matches the road's casing color so the class is still legible.
func drawShields(dc *gg.Context, features []Feature, z, x, y int,
	tMinLon, tMinLat, tMaxLon, tMaxLat float64,
	placed *[]labelRect) error {
	if z < 8 {
		return nil
	}
	face, err := labelFontFace(10)
	if err != nil {
		return err
	}
	dc.SetFontFace(face)

	// Same-ref dedup so a long route encoded as many ways (NY 9A
	// per-block, FDR Drive per-block) doesn't get a shield on each
	// segment. AABB collision alone is too lax here — shields are
	// tiny enough that two of them comfortably fit 40 px apart and
	// the eye reads it as repetition.
	var placedRefs []namedAnchor
	const minSameRefDist = 220.0

	for i := range features {
		f := &features[i]
		if f.Class != ClassRoad || f.Ref == "" {
			continue
		}
		if z < shieldMinZoom(f.RoadKind) {
			continue
		}
		if f.MaxLon < tMinLon || f.MinLon > tMaxLon ||
			f.MaxLat < tMinLat || f.MinLat > tMaxLat {
			continue
		}
		if len(f.Coords) == 0 {
			continue
		}
		mid := f.Coords[len(f.Coords)/2]
		px, py := LonLatToTilePx(mid.Lon, mid.Lat, z, x, y)
		if px < -LabelBuffer || px > TileSize+LabelBuffer ||
			py < -LabelBuffer || py > TileSize+LabelBuffer {
			continue
		}

		tooClose := false
		for _, prev := range placedRefs {
			if prev.name != f.Ref {
				continue
			}
			dx, dy := prev.x-px, prev.y-py
			if dx*dx+dy*dy < minSameRefDist*minSameRefDist {
				tooClose = true
				break
			}
		}
		if tooClose {
			continue
		}

		w, _ := dc.MeasureString(f.Ref)
		const padX, padY = 5.0, 3.0
		boxW := w + 2*padX
		boxH := 12.0 + 2*padY

		box := labelRect{
			x0: px - boxW/2, y0: py - boxH/2,
			x1: px + boxW/2, y1: py + boxH/2,
		}
		collides := false
		for _, p := range *placed {
			if box.overlaps(p) {
				collides = true
				break
			}
		}
		if collides {
			continue
		}
		*placed = append(*placed, box)
		placedRefs = append(placedRefs, namedAnchor{name: f.Ref, x: px, y: py})

		dc.SetColor(rgba(0xff, 0xff, 0xff, 0xff))
		dc.DrawRoundedRectangle(box.x0, box.y0, boxW, boxH, 3)
		dc.Fill()
		s := roadStyles[f.RoadKind]
		if s.casingColor != nil {
			dc.SetColor(s.casingColor)
		} else {
			dc.SetColor(rgba(0x80, 0x80, 0x80, 0xff))
		}
		dc.SetLineWidth(1)
		dc.DrawRoundedRectangle(box.x0, box.y0, boxW, boxH, 3)
		dc.Stroke()
		dc.SetRGB(0, 0, 0)
		dc.DrawStringAnchored(f.Ref, px, py, 0.5, 0.5)
	}
	return nil
}

// roadClassPaintOrder defines the painter's-algorithm order for road
// classes: lower-importance roads paint first so motorways and trunks
// sit on top at intersections. Mirrors osm-carto's z-index ordering.
func roadClassPaintOrder(k RoadKind) int {
	switch k {
	case RoadPath:
		return 0
	case RoadService:
		return 1
	case RoadResidential:
		return 2
	case RoadTertiary:
		return 3
	case RoadSecondary:
		return 4
	case RoadPrimary:
		return 5
	case RoadTrunk:
		return 6
	case RoadMotorway:
		return 7
	}
	return -1
}
