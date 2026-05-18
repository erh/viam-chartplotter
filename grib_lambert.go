package vc

import (
	"encoding/binary"
	"fmt"
	"math"
)

// lambertGrid describes one GRIB2 Section 3 grid encoded under template
// 3.30 (Lambert Conformal). Coordinates of grid points sit on the
// projection's (x, y) plane in metres from a synthetic origin; we
// reconstruct that origin from La1/Lo1 by forward-projecting the first
// point.
type lambertGrid struct {
	Nx, Ny       int
	Dx, Dy       float64 // metres
	La1, Lo1     float64 // degrees, first grid point
	LaD          float64 // degrees, "true scale" reference latitude
	LoV          float64 // degrees, central meridian (parallel to y-axis)
	Latin1       float64 // degrees, first standard parallel
	Latin2       float64 // degrees, second standard parallel
	ScanMode     int
	EarthRadius  float64 // metres
}

// lambertParams are the precomputed projection constants — derived
// once per grid so the per-pixel inverse projection is fast.
type lambertParams struct {
	n    float64
	f    float64
	rho0 float64
	r    float64 // earth radius in metres
	loV  float64 // central meridian in radians
}

// parseLambertGrid pulls Lambert Conformal fields out of a Section 3
// body. Returns an error if the template isn't 3.30. The byte offsets
// below are 0-indexed into `s3`, matching the existing parseGRIBMessage
// convention; WMO Manual octet number = offset + 1.
func parseLambertGrid(s3 []byte) (*lambertGrid, error) {
	if len(s3) < 81 {
		return nil, fmt.Errorf("lambert section too short (%d bytes)", len(s3))
	}
	tpl := int(binary.BigEndian.Uint16(s3[12:14]))
	if tpl != 30 {
		return nil, fmt.Errorf("expected grid template 30, got %d", tpl)
	}
	shape := int(s3[14])
	g := &lambertGrid{
		Nx:       int(binary.BigEndian.Uint32(s3[30:34])),
		Ny:       int(binary.BigEndian.Uint32(s3[34:38])),
		La1:      signedUint32(binary.BigEndian.Uint32(s3[38:42])) / 1e6,
		Lo1:      signedUint32(binary.BigEndian.Uint32(s3[42:46])) / 1e6,
		LaD:      signedUint32(binary.BigEndian.Uint32(s3[47:51])) / 1e6,
		LoV:      signedUint32(binary.BigEndian.Uint32(s3[51:55])) / 1e6,
		Dx:       signedUint32(binary.BigEndian.Uint32(s3[55:59])) / 1e3, // 10^-3 m units in template 30
		Dy:       signedUint32(binary.BigEndian.Uint32(s3[59:63])) / 1e3,
		ScanMode: int(s3[64]),
		Latin1:   signedUint32(binary.BigEndian.Uint32(s3[65:69])) / 1e6,
		Latin2:   signedUint32(binary.BigEndian.Uint32(s3[69:73])) / 1e6,
	}
	// Shape-of-earth → radius (m). HRRR/NAM use shape=6 (spherical
	// R=6,371,229). Treat the oblate variants as spherical at the
	// equatorial radius — sub-pixel error at 3 km grid spacing.
	switch shape {
	case 6:
		g.EarthRadius = 6371229.0
	case 0:
		g.EarthRadius = 6367470.0
	case 1:
		scale := int(int8(s3[15]))
		scaled := int(binary.BigEndian.Uint32(s3[16:20]))
		g.EarthRadius = float64(scaled) * math.Pow10(-scale)
	default:
		g.EarthRadius = 6371229.0
	}
	if g.Nx <= 0 || g.Ny <= 0 {
		return nil, fmt.Errorf("lambert: bogus Nx=%d Ny=%d", g.Nx, g.Ny)
	}
	// LoV is stored in 0..360 range in some files; normalise to -180..180
	// so the projection math (which subtracts LoV from lon) doesn't
	// produce huge angle deltas.
	if g.LoV > 180 {
		g.LoV -= 360
	}
	if g.Lo1 > 180 {
		g.Lo1 -= 360
	}
	return g, nil
}

// precompute returns the (n, F, rho0) constants for the LCC forward
// formulas using a tangent or secant LCC depending on Latin1 vs Latin2.
func (g *lambertGrid) precompute() lambertParams {
	deg := math.Pi / 180
	lat1 := g.Latin1 * deg
	lat2 := g.Latin2 * deg
	laD := g.LaD * deg
	r := g.EarthRadius
	var n float64
	if math.Abs(lat1-lat2) < 1e-9 {
		n = math.Sin(lat1)
	} else {
		n = math.Log(math.Cos(lat1)/math.Cos(lat2)) /
			math.Log(math.Tan(math.Pi/4+lat2/2)/math.Tan(math.Pi/4+lat1/2))
	}
	f := math.Cos(lat1) * math.Pow(math.Tan(math.Pi/4+lat1/2), n) / n
	rho0 := r * f / math.Pow(math.Tan(math.Pi/4+laD/2), n)
	return lambertParams{n: n, f: f, rho0: rho0, r: r, loV: g.LoV * deg}
}

// forward maps (lat, lon) in degrees to (x, y) in metres in the LCC plane
// centred on (LoV, LaD).
func (p lambertParams) forward(lat, lon float64) (float64, float64) {
	deg := math.Pi / 180
	latR := lat * deg
	rho := p.r * p.f / math.Pow(math.Tan(math.Pi/4+latR/2), p.n)
	dlon := lon*deg - p.loV
	// Wrap to [-π, π] so a grid that straddles the antimeridian doesn't
	// fire theta past ±π and confuse the secant approximation.
	for dlon > math.Pi {
		dlon -= 2 * math.Pi
	}
	for dlon < -math.Pi {
		dlon += 2 * math.Pi
	}
	theta := p.n * dlon
	x := rho * math.Sin(theta)
	y := p.rho0 - rho*math.Cos(theta)
	return x, y
}

// reprojectLambertToLatLon resamples values that live on a Lambert
// Conformal grid `g` onto a regular lat/lon subgrid covering the bbox
// (lonW..lonE, latS..latN) at dlon × dlat resolution. The target grid
// is what ol-wind consumes; we use bilinear interpolation in (x, y)
// space.
//
// scanMode handling: GFS-style scanMode 64 means "left→right, south→
// north", which is the layout we emit for the target latlon grid. HRRR
// uses scanMode 0 (left→right, north→south) for its native LC, so when
// we index source[ix + iy*Nx] we treat iy=0 as the *northern* row. The
// branch below picks the right indexing per scan mode.
func reprojectLambertToLatLon(
	src []float64,
	g *lambertGrid,
	lonW, lonE, latS, latN, dlon, dlat float64,
) ([]float64, int, int, error) {
	if len(src) != g.Nx*g.Ny {
		return nil, 0, 0, fmt.Errorf("reproject: src len %d != Nx*Ny %d", len(src), g.Nx*g.Ny)
	}
	p := g.precompute()
	// Anchor: project the (La1, Lo1) point into LC space so we know what
	// (x0, y0) corresponds to grid index (0, 0). All subsequent samples
	// reference offsets from that anchor.
	x0, y0 := p.forward(g.La1, g.Lo1)
	// Scanning mode bits per GRIB2 table 3.4:
	//   bit 1 (0x80): 0 = +i (W→E), 1 = -i (E→W)
	//   bit 2 (0x40): 0 = -j (N→S), 1 = +j (S→N)
	// We translate to per-axis direction so the inner loop is branch-free.
	iSign := 1.0
	if g.ScanMode&0x80 != 0 {
		iSign = -1.0
	}
	// +j (south→north) means as iy increases, y in LC plane increases.
	// HRRR uses -j: iy=0 is the *northern* row, so source-y *decreases*
	// as iy grows. Encode via jSign.
	jSign := -1.0
	if g.ScanMode&0x40 != 0 {
		jSign = 1.0
	}

	Nlon := int(math.Round((lonE-lonW)/dlon)) + 1
	Nlat := int(math.Round((latN-latS)/dlat)) + 1
	if Nlon <= 0 || Nlat <= 0 {
		return nil, 0, 0, fmt.Errorf("reproject: empty target grid")
	}
	out := make([]float64, Nlon*Nlat)
	// ol-wind expects scan mode 0 (row-major, N→S). Emit rows from
	// north to south so the resulting header (scanMode=0) matches.
	for iy := 0; iy < Nlat; iy++ {
		lat := latN - float64(iy)*dlat
		for ix := 0; ix < Nlon; ix++ {
			lon := lonW + float64(ix)*dlon
			x, y := p.forward(lat, lon)
			// Fractional source-grid indices.
			fx := (x - x0) / (g.Dx * iSign)
			fy := (y - y0) / (g.Dy * jSign)
			out[iy*Nlon+ix] = bilinearSample(src, g.Nx, g.Ny, fx, fy)
		}
	}
	return out, Nlon, Nlat, nil
}

// bilinearSample interpolates src[ix + iy*Nx] at fractional grid index
// (fx, fy). Returns 0 outside the source grid — for HRRR/NAM that lands
// over ocean off CONUS, which the consumers (wind particle layer)
// happily render as "no motion".
func bilinearSample(src []float64, Nx, Ny int, fx, fy float64) float64 {
	if fx < 0 || fy < 0 || fx > float64(Nx-1) || fy > float64(Ny-1) {
		return 0
	}
	ix := int(math.Floor(fx))
	iy := int(math.Floor(fy))
	dx := fx - float64(ix)
	dy := fy - float64(iy)
	ix1 := ix + 1
	iy1 := iy + 1
	if ix1 > Nx-1 {
		ix1 = Nx - 1
	}
	if iy1 > Ny-1 {
		iy1 = Ny - 1
	}
	v00 := src[ix+iy*Nx]
	v10 := src[ix1+iy*Nx]
	v01 := src[ix+iy1*Nx]
	v11 := src[ix1+iy1*Nx]
	a := v00*(1-dx) + v10*dx
	b := v01*(1-dx) + v11*dx
	return a*(1-dy) + b*dy
}
