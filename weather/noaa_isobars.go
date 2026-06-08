package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// GFS PRMSL isobar overlay. Fetches the PRMSL (Pressure Reduced to Mean
// Sea Level) field from NOMADS at the same 0.25° grid the existing wind
// fetch uses, runs marching squares at 4 hPa intervals, and returns a
// GeoJSON FeatureCollection of LineString features keyed by pressure
// level (in hPa). Frontend renders as a thin OL Vector layer next to
// the wind / waves overlays.
//
// We piggyback on the wind cache pipeline (same disk cache, same gzip
// sibling, same forecast-hour slider) via the FetchBytes branch on
// WeatherModel — no separate handler / cache files / stats path.

const (
	// NOMADS GFS PRMSL filter — same template as wind but with the
	// PRMSL variable + MSL level instead of UGRD/VGRD@10m. ~150 KB
	// GRIB2 per hour, decoded to a 1.5-3 MB GeoJSON.
	nomadsGFSPRMSLURLTemplate = "https://nomads.ncep.noaa.gov/cgi-bin/filter_gfs_0p25.pl" +
		"?file=gfs.t%02dz.pgrb2.0p25.f%03d" +
		"&lev_mean_sea_level=on&var_PRMSL=on" +
		"&dir=%%2Fgfs.%s%%2F%02d%%2Fatmos"

	// GRIB2 product identification for PRMSL: discipline 0
	// (Meteorological), category 3 (Mass), number 1 (PRMSL), surface
	// 101 (Mean Sea Level). See WMO Manual on Codes, Table 4.2-0-3.
	gribParamCatMass        = 3
	gribParamPRMSL          = 1
	gribSurfaceMeanSeaLevel = 101

	// Isobar contour spacing in hectopascals. 2 hPa picks up gradients
	// the 4 hPa NWS standard hides — frontend gives every 4th level a
	// heavier stroke so it still reads as the canonical analysis at a
	// glance. Range covers anything from a very deep low (~920 hPa,
	// hurricane core) to a strong high (~1060 hPa).
	isobarStepHPa = 2
	isobarMinHPa  = 920
	isobarMaxHPa  = 1064

	// Local-extremum search window for "H" / "L" labels. PRMSL field is
	// on a ~28 km grid (GFS 0.25°), so a 9-cell radius ≈ ~250 km — the
	// synoptic scale at which H/L pairs are typically labelled on
	// surface analysis charts. Anything tighter picks up noise; wider
	// would miss neighboring lobes in a complex pattern.
	extremumRadius = 9
	// Minimum required pressure offset (Pa) from the window's opposite
	// extreme before a point qualifies as an H or L. Stops the layer
	// from labelling every flat plateau pixel — only meaningful highs
	// and lows. 100 Pa = 1 hPa — coarser than the 2 hPa contour step
	// so we'd never label something that's also between contour lines.
	extremumMinOffsetPa = 100
)

func gfsIsobarsModel() *WeatherModel {
	m := &WeatherModel{
		Name:        "gfs-isobars",
		DisplayName: "Isobars (GFS PRMSL, 0.25° global)",
		Kind:        "isobars",
		Domain:      "global",
		CycleHours:  []int{0, 6, 12, 18},
		MinFh:       0,
		MaxFh:       240,
		StepFh:      3,
		PublishLagH: 4,
	}
	m.FetchBytes = func(ctx context.Context, client *http.Client, _ time.Time, fh int) ([]byte, error) {
		body, runT, err := WalkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
			date := t.Format("20060102")
			cc := t.Hour()
			url := fmt.Sprintf(nomadsGFSPRMSLURLTemplate, cc, fh, date, cc)
			return fetchURL(ctx, client, url)
		})
		if err != nil {
			return nil, err
		}
		return decodeGFSIsobars(body, runT, fh)
	}
	return m
}

// decodeGFSIsobars parses the PRMSL GRIB2 message, runs marching squares
// at 4 hPa intervals, and emits a GeoJSON FeatureCollection. The header
// section (refTime, forecastTime) rides along on the FeatureCollection
// as a top-level `meta` key so the frontend can show "valid: <date>".
func decodeGFSIsobars(grib []byte, runTime time.Time, fh int) ([]byte, error) {
	wantPRMSL := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
		return paramCat == gribParamCatMass &&
			paramNum == gribParamPRMSL &&
			surfType == gribSurfaceMeanSeaLevel
	}
	rec, err := decodeRegularLLMessage(grib, runTime, fh, wantPRMSL)
	if err != nil {
		return nil, fmt.Errorf("PRMSL decode: %w", err)
	}
	if rec == nil {
		return nil, fmt.Errorf("PRMSL not found in GRIB body")
	}
	features := contourLatLonGrid(rec)
	features = append(features, extremaLatLonGrid(rec)...)
	fc := geoJSONFeatureCollection{
		Type:     "FeatureCollection",
		Features: features,
		Meta: &isobarMeta{
			RefTime:      rec.Header.RefTime,
			ForecastTime: rec.Header.ForecastTime,
			StepHPa:      isobarStepHPa,
		},
	}
	return json.Marshal(fc)
}

// extremaLatLonGrid scans the PRMSL grid for local highs and lows over a
// `extremumRadius`-cell window — the radius defines the smallest scale
// at which we'll call something an H or L. A point qualifies as the
// extreme if it strictly beats every neighbour in the window AND
// differs from the window's opposite extreme by at least
// extremumMinOffsetPa, so we don't label every micro-perturbation in a
// flat pressure plateau. Emits one Point feature per extremum.
func extremaLatLonGrid(rec *WindRecord) []geoJSONFeature {
	h := rec.Header
	nx, ny := h.Nx, h.Ny
	if nx < 2*extremumRadius+1 || ny < 2*extremumRadius+1 {
		return nil
	}
	shiftLon := func(l float64) float64 {
		if l >= 180 {
			return l - 360
		}
		return l
	}
	out := make([]geoJSONFeature, 0, 64)
	for iy := extremumRadius; iy < ny-extremumRadius; iy++ {
		for ix := extremumRadius; ix < nx-extremumRadius; ix++ {
			v := rec.Data[iy*nx+ix]
			if !valid(v) {
				continue
			}
			isHigh := true
			isLow := true
			winMin := v
			winMax := v
			for dy := -extremumRadius; dy <= extremumRadius && (isHigh || isLow); dy++ {
				for dx := -extremumRadius; dx <= extremumRadius; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					// Wrap longitude for the global grid; latitude doesn't
					// wrap so the loop bounds already guarantee in-range.
					nix := ix + dx
					if nix < 0 {
						nix += nx
					} else if nix >= nx {
						nix -= nx
					}
					nv := rec.Data[(iy+dy)*nx+nix]
					if !valid(nv) {
						isHigh = false
						isLow = false
						break
					}
					if nv >= v {
						isHigh = false
					}
					if nv <= v {
						isLow = false
					}
					if nv < winMin {
						winMin = nv
					}
					if nv > winMax {
						winMax = nv
					}
				}
			}
			if !isHigh && !isLow {
				continue
			}
			if isHigh && v-winMin < extremumMinOffsetPa {
				continue
			}
			if isLow && winMax-v < extremumMinOffsetPa {
				continue
			}
			lon := shiftLon(h.Lo1 + float64(ix)*h.Dx)
			lat := h.La1 - float64(iy)*h.Dy
			kind := "H"
			if isLow {
				kind = "L"
			}
			out = append(out, geoJSONFeature{
				Type: "Feature",
				Geometry: geoJSONPoint{
					Type:        "Point",
					Coordinates: [2]float64{lon, lat},
				},
				Properties: isobarProperties{
					HPa:  int(math.Round(v / 100)),
					Kind: kind,
				},
			})
		}
	}
	return out
}

// --- GeoJSON shape --------------------------------------------------------

type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
	Meta     *isobarMeta      `json:"meta,omitempty"`
}

type geoJSONFeature struct {
	Type       string           `json:"type"`
	Geometry   any              `json:"geometry"`
	Properties isobarProperties `json:"properties"`
}

// geoJSONLineString is the LineString shape used for isobar contour
// segments. Coordinates are an array of [lon, lat] pairs.
type geoJSONLineString struct {
	Type        string       `json:"type"`
	Coordinates [][2]float64 `json:"coordinates"`
}

// geoJSONPoint is the Point shape used for H/L extremum markers. A
// single [lon, lat] pair sits at the field's local high or low.
type geoJSONPoint struct {
	Type        string     `json:"type"`
	Coordinates [2]float64 `json:"coordinates"`
}

type isobarProperties struct {
	// HPa is the contour level for LineString features, or the field
	// value at the extremum for Point features.
	HPa int `json:"hPa"`
	// Kind is "H" or "L" on Point features (omitted on LineStrings).
	Kind string `json:"kind,omitempty"`
}

type isobarMeta struct {
	RefTime      string `json:"refTime"`
	ForecastTime int    `json:"forecastTime"`
	StepHPa      int    `json:"stepHPa"`
}

// --- Marching squares -----------------------------------------------------

// contourLatLonGrid walks every grid cell of `rec` and emits one
// LineString feature per (cell, level) crossing. We don't stitch
// adjacent segments into longer polylines — disjoint segments render
// identically in OpenLayers, and stitching adds ~200 lines of code for
// marginal wire-savings. The frontend can group by hPa for labelling.
//
// Grid layout: data is stored row-major north-to-south (GFS scan mode
// 0). Index `iy*Nx + ix` gives lat=La1 - iy*Dy, lon=Lo1 + ix*Dx (with
// Lo1 wrapping at 360°). PRMSL values are in Pa.
func contourLatLonGrid(rec *WindRecord) []geoJSONFeature {
	h := rec.Header
	nx, ny := h.Nx, h.Ny
	if nx < 2 || ny < 2 || len(rec.Data) < nx*ny {
		return nil
	}
	// NOMADS publishes GFS with Lo1=0, scan W→E across [0, 360). We
	// emit features in [-180, 180]. The shift is per-cell rather than
	// per-segment-endpoint: any cell whose left edge sits at or beyond
	// 180° gets its entire cell shifted west by 360°, so interpolated
	// contour points all land in the same hemisphere. Per-endpoint
	// wrap is wrong here — the cell at ix where lonL=180 produces
	// segments with one endpoint sitting exactly on 180 and the other
	// just past it, which a per-point wrap would turn into a 360°-wide
	// horizontal stripe across the chart.
	shiftLon := func(l float64) float64 {
		if l >= 180 {
			return l - 360
		}
		return l
	}
	// Build a level list once — Pa values to match the data units.
	levels := make([]float64, 0, (isobarMaxHPa-isobarMinHPa)/isobarStepHPa+1)
	for hpa := isobarMinHPa; hpa <= isobarMaxHPa; hpa += isobarStepHPa {
		levels = append(levels, float64(hpa)*100)
	}

	// Pre-size: typical GFS frame produces ~15-25k segments globally.
	out := make([]geoJSONFeature, 0, 20000)
	for iy := 0; iy < ny-1; iy++ {
		// Latitudes of the top and bottom edges of this cell row. La1
		// is the northern edge for GFS scan mode 0; rows advance south.
		latT := h.La1 - float64(iy)*h.Dy
		latB := h.La1 - float64(iy+1)*h.Dy
		for ix := 0; ix < nx-1; ix++ {
			// Cell corners — labelled like the MS literature:
			//   tl ── tr
			//   │      │
			//   bl ── br
			tl := rec.Data[iy*nx+ix]
			tr := rec.Data[iy*nx+ix+1]
			bl := rec.Data[(iy+1)*nx+ix]
			br := rec.Data[(iy+1)*nx+ix+1]
			// Skip cells with any sentinel / NaN value.
			if !valid(tl) || !valid(tr) || !valid(bl) || !valid(br) {
				continue
			}
			lonL := shiftLon(h.Lo1 + float64(ix)*h.Dx)
			lonR := lonL + h.Dx
			// Cell-level min/max — cheap reject of cells with no
			// contour crossings before the level loop.
			cellMin := tl
			cellMax := tl
			for _, v := range [3]float64{tr, bl, br} {
				if v < cellMin {
					cellMin = v
				}
				if v > cellMax {
					cellMax = v
				}
			}
			for li, level := range levels {
				if level < cellMin || level > cellMax {
					continue
				}
				segs := marchCell(level, tl, tr, bl, br, lonL, lonR, latT, latB)
				for _, s := range segs {
					// Defense in depth against wrap regressions: any
					// segment wider than one cell (~Dx + small slack) or
					// taller than one cell is structurally impossible
					// here, so it's the marching-squares output of a
					// shift/interp bug. Dropping it keeps a future
					// regression from repainting horizontal stripes
					// across the chart.
					if math.Abs(s[0]-s[2]) > h.Dx*2 || math.Abs(s[1]-s[3]) > h.Dy*2 {
						continue
					}
					out = append(out, geoJSONFeature{
						Type: "Feature",
						Geometry: geoJSONLineString{
							Type: "LineString",
							Coordinates: [][2]float64{
								{s[0], s[1]},
								{s[2], s[3]},
							},
						},
						Properties: isobarProperties{
							HPa: isobarMinHPa + li*isobarStepHPa,
						},
					})
				}
			}
		}
	}
	return out
}

// valid screens out the GRIB2 missing-value sentinel and NaN. PRMSL
// itself doesn't carry a bitmap, but we treat anything wildly outside
// the realisable range (~870-1085 hPa) as missing too.
func valid(v float64) bool {
	if math.IsNaN(v) {
		return false
	}
	if v < 70000 || v > 110000 {
		return false
	}
	return true
}

// marchCell returns 0, 1, or 2 segments through one cell where the
// scalar field crosses `level`. Each segment is encoded as
// [lon0, lat0, lon1, lat1].
//
// Standard marching-squares case index, with tl=8, tr=4, br=2, bl=1
// summed into a 4-bit number. Cases 0 and 15 emit nothing; 5 and 10
// are the "saddle" cases — we resolve by the cell's mean (the standard
// disambiguation, no asymptotic decider needed for pressure fields
// which are smooth at our spacing).
func marchCell(level, tl, tr, bl, br, lonL, lonR, latT, latB float64) [][4]float64 {
	idx := 0
	if tl > level {
		idx |= 8
	}
	if tr > level {
		idx |= 4
	}
	if br > level {
		idx |= 2
	}
	if bl > level {
		idx |= 1
	}
	if idx == 0 || idx == 15 {
		return nil
	}
	// Linear interpolation along each cell edge for the contour
	// crossing point. Edge labels: T = top (between tl, tr), R = right
	// (tr, br), B = bottom (bl, br), L = left (tl, bl).
	t := func(a, b float64) float64 { return (level - a) / (b - a) }
	edgeT := [2]float64{lonL + t(tl, tr)*(lonR-lonL), latT}
	edgeR := [2]float64{lonR, latT - t(tr, br)*(latT-latB)}
	edgeB := [2]float64{lonL + t(bl, br)*(lonR-lonL), latB}
	edgeL := [2]float64{lonL, latT - t(tl, bl)*(latT-latB)}
	seg := func(a, b [2]float64) [4]float64 {
		return [4]float64{a[0], a[1], b[0], b[1]}
	}
	switch idx {
	case 1, 14:
		return [][4]float64{seg(edgeL, edgeB)}
	case 2, 13:
		return [][4]float64{seg(edgeB, edgeR)}
	case 3, 12:
		return [][4]float64{seg(edgeL, edgeR)}
	case 4, 11:
		return [][4]float64{seg(edgeT, edgeR)}
	case 6, 9:
		return [][4]float64{seg(edgeT, edgeB)}
	case 7, 8:
		return [][4]float64{seg(edgeT, edgeL)}
	case 5:
		// Saddle: tl + br above, tr + bl below. Resolve by cell mean
		// to decide which pair of arcs to connect.
		mean := (tl + tr + bl + br) * 0.25
		if mean > level {
			return [][4]float64{seg(edgeT, edgeL), seg(edgeB, edgeR)}
		}
		return [][4]float64{seg(edgeT, edgeR), seg(edgeB, edgeL)}
	case 10:
		// Saddle: tr + bl above, tl + br below.
		mean := (tl + tr + bl + br) * 0.25
		if mean > level {
			return [][4]float64{seg(edgeT, edgeR), seg(edgeB, edgeL)}
		}
		return [][4]float64{seg(edgeT, edgeL), seg(edgeB, edgeR)}
	}
	return nil
}
