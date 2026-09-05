package render

import (
	"math"
	"sort"
)

// ---------------------------------------------------------------------------
// navGrid: the raster the auto-router searches.
//
// This file is deliberately free of ENC/Mongo knowledge — it is a lon/lat
// aligned grid of "how deep is it here" / "can I go here" plus an A* over it,
// so it can be unit-tested with hand-built polygons. autoroute.go owns the
// translation from S-57 features into this grid.
// ---------------------------------------------------------------------------

// Cell flags. A cell can carry several at once (a dredged channel inside a
// restricted area, say).
const (
	cellLand        uint8 = 1 << iota // charted land / shoreline construction
	cellObstruction                   // wreck, rock, pile — hard block
	cellDredged                       // maintained channel: never depth-penalised
	cellUnsurveyed                    // UNSARE: nothing was surveyed here
	cellRestricted                    // RESARE and friends: entry is regulated
	cellBlockedHard                   // computed in finalize(): impassable
)

// cellAvoid is any cell there is a charted reason to stay out of. The two
// reasons are stored separately because they are different facts — one is an
// absence of survey, the other a rule — but the cost model prices them alike.
const cellAvoid = cellUnsurveyed | cellRestricted

// metresPerDegreeLat is the WGS84 mean; good to ~0.5% anywhere, which is well
// inside the grid quantisation we route on.
const metresPerDegreeLat = 111320.0

// navGrid is a lon/lat aligned raster covering a bbox. Cell (ix,iy) has its
// centre at (minLon+(ix+0.5)*dLon, minLat+(iy+0.5)*dLat).
type navGrid struct {
	minLon, minLat float64
	dLon, dLat     float64 // degrees per cell
	nx, ny         int
	mx, my         float64 // metres per cell, x and y (mx uses the mid-latitude)

	// depth is the charted depth in metres for each cell, NaN where nothing
	// charted it. scaleOf records the compilation scale (1:N denominator) of
	// the feature that last wrote depth, so a finer cell overwrites a coarser
	// one and equal-scale overlaps keep the shoalest value.
	depth   []float64
	scaleOf []int32
	flags   []uint8

	// Filled by finalize().
	cost       []float32 // cost multiplier, >= 1; +Inf where impassable
	clearCells []float32 // distance to the nearest hard-blocked cell, in cells
}

// gridCost carries the tunables finalize() turns into per-cell costs. Depths
// are metres; distances metres.
type gridCost struct {
	SafeDepthM  float64 // shoaler than this is impassable
	IdealDepthM float64 // shoaler than this is penalised, proportionally
	// HardClearanceM is a no-go buffer around every hard-blocked cell (land,
	// shoal, obstruction). SoftClearanceM is the wider band that is merely
	// expensive, so the route prefers the middle of a channel.
	HardClearanceM float64
	SoftClearanceM float64

	DepthPenalty   float64 // added at safe depth, tapering to 0 at ideal depth
	ShorePenalty   float64 // added right at the hard-clearance edge, tapering to 0
	UnknownPenalty float64 // added on cells no DEPARE charted
	AvoidPenalty   float64 // added on cellAvoid cells
}

// newNavGrid sizes a grid over the bbox: square-ish cells, at least
// minCellM across, no more than maxCells total and maxDim on a side.
func newNavGrid(minLon, minLat, maxLon, maxLat float64, maxCells int, minCellM float64, maxDim int) *navGrid {
	cosLat := clampCosLat((minLat + maxLat) / 2)
	cell := cellSizeFor(minLon, minLat, maxLon, maxLat, maxCells, minCellM, maxDim)

	dLat := cell / metresPerDegreeLat
	dLon := cell / (metresPerDegreeLat * cosLat)
	nx := int(math.Ceil((maxLon-minLon)/dLon)) + 1
	ny := int(math.Ceil((maxLat-minLat)/dLat)) + 1

	g := &navGrid{
		minLon: minLon, minLat: minLat,
		dLon: dLon, dLat: dLat,
		nx: nx, ny: ny,
		mx: cell, my: cell,
	}
	n := nx * ny
	g.depth = make([]float64, n)
	g.scaleOf = make([]int32, n)
	g.flags = make([]uint8, n)
	for i := range g.depth {
		g.depth[i] = math.NaN()
		g.scaleOf[i] = math.MaxInt32 // "coarser than anything real"
	}
	return g
}

// cellSizeFor is the cell size newNavGrid would pick for a bbox: square-ish
// cells, at least minCellM across, no more than maxCells in total and maxDim on
// a side. Split out so a caller can find out how coarse the grid would be
// *before* paying for the chart query that fills it.
func cellSizeFor(minLon, minLat, maxLon, maxLat float64, maxCells int, minCellM float64, maxDim int) float64 {
	cosLat := clampCosLat((minLat + maxLat) / 2)
	widthM := (maxLon - minLon) * metresPerDegreeLat * cosLat
	heightM := (maxLat - minLat) * metresPerDegreeLat

	cell := math.Sqrt(widthM * heightM / float64(maxCells))
	if cell < minCellM {
		cell = minCellM
	}
	// Honour the per-side cap too — a long thin bbox can blow past maxDim
	// while staying under maxCells.
	if n := widthM / float64(maxDim); n > cell {
		cell = n
	}
	if n := heightM / float64(maxDim); n > cell {
		cell = n
	}
	return cell
}

// clampCosLat keeps the metres-per-degree-longitude conversion finite near the
// poles.
func clampCosLat(latDeg float64) float64 {
	c := math.Cos(latDeg * math.Pi / 180)
	if c < 0.05 {
		return 0.05
	}
	return c
}

func (g *navGrid) cellSizeM() float64 { return g.mx }

func (g *navGrid) idx(ix, iy int) int { return iy*g.nx + ix }

// cellCentre returns the lon/lat at the centre of a cell.
func (g *navGrid) cellCentre(ix, iy int) (lon, lat float64) {
	return g.minLon + (float64(ix)+0.5)*g.dLon, g.minLat + (float64(iy)+0.5)*g.dLat
}

func (g *navGrid) centreOf(i int) (lon, lat float64) {
	return g.cellCentre(i%g.nx, i/g.nx)
}

// cellAt maps a lon/lat to a cell, reporting false when it falls outside.
func (g *navGrid) cellAt(lon, lat float64) (int, bool) {
	ix := int(math.Floor((lon - g.minLon) / g.dLon))
	iy := int(math.Floor((lat - g.minLat) / g.dLat))
	if ix < 0 || iy < 0 || ix >= g.nx || iy >= g.ny {
		return 0, false
	}
	return g.idx(ix, iy), true
}

// setDepth records a charted depth on a cell, honouring the finest-cell-wins
// rule the renderer paints with: a finer compilation scale (smaller
// denominator) always overwrites, an equal scale keeps the shoaler reading,
// and a coarser one is ignored.
func (g *navGrid) setDepth(i int, depthM float64, scale int32) {
	switch {
	case scale < g.scaleOf[i]:
		g.depth[i] = depthM
		g.scaleOf[i] = scale
	case scale == g.scaleOf[i]:
		if math.IsNaN(g.depth[i]) || depthM < g.depth[i] {
			g.depth[i] = depthM
		}
	}
}

func (g *navGrid) mark(i int, f uint8) { g.flags[i] |= f }

// fillRings rasterises a polygon (outer ring plus any holes/parts, in the
// concatenated-ring convention splitRings unpacks) by even-odd scanline over
// cell centres, calling fn once per covered cell. Even-odd matches how the
// renderer fills these polygons, so holes drop out for free.
func (g *navGrid) fillRings(rings [][][]float64, fn func(i int)) {
	minLat, maxLat := math.Inf(1), math.Inf(-1)
	for _, ring := range rings {
		for _, p := range ring {
			if len(p) < 2 {
				continue
			}
			minLat = math.Min(minLat, p[1])
			maxLat = math.Max(maxLat, p[1])
		}
	}
	if math.IsInf(minLat, 1) {
		return
	}
	iy0 := int(math.Floor((minLat - g.minLat) / g.dLat))
	iy1 := int(math.Ceil((maxLat - g.minLat) / g.dLat))
	if iy0 < 0 {
		iy0 = 0
	}
	if iy1 >= g.ny {
		iy1 = g.ny - 1
	}

	var xs []float64
	for iy := iy0; iy <= iy1; iy++ {
		lat := g.minLat + (float64(iy)+0.5)*g.dLat
		xs = xs[:0]
		for _, ring := range rings {
			for k := 0; k+1 < len(ring); k++ {
				a, b := ring[k], ring[k+1]
				if len(a) < 2 || len(b) < 2 {
					continue
				}
				if (a[1] > lat) == (b[1] > lat) {
					continue
				}
				t := (lat - a[1]) / (b[1] - a[1])
				xs = append(xs, a[0]+t*(b[0]-a[0]))
			}
		}
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		for k := 0; k+1 < len(xs); k += 2 {
			ix0 := int(math.Ceil((xs[k]-g.minLon)/g.dLon - 0.5))
			ix1 := int(math.Floor((xs[k+1]-g.minLon)/g.dLon - 0.5))
			if ix0 < 0 {
				ix0 = 0
			}
			if ix1 >= g.nx {
				ix1 = g.nx - 1
			}
			row := iy * g.nx
			for ix := ix0; ix <= ix1; ix++ {
				fn(row + ix)
			}
		}
	}
}

// stampDisc calls fn for every cell whose centre is within radiusM of the
// point. A radius under half a cell still stamps the containing cell, so a
// single charted rock is never lost to quantisation.
func (g *navGrid) stampDisc(lon, lat, radiusM float64, fn func(i int)) {
	rx := int(math.Ceil(radiusM / g.mx))
	ry := int(math.Ceil(radiusM / g.my))
	cx := int(math.Floor((lon - g.minLon) / g.dLon))
	cy := int(math.Floor((lat - g.minLat) / g.dLat))
	for iy := cy - ry; iy <= cy+ry; iy++ {
		if iy < 0 || iy >= g.ny {
			continue
		}
		for ix := cx - rx; ix <= cx+rx; ix++ {
			if ix < 0 || ix >= g.nx {
				continue
			}
			if ix == cx && iy == cy {
				fn(g.idx(ix, iy))
				continue
			}
			dx := float64(ix-cx) * g.mx
			dy := float64(iy-cy) * g.my
			if dx*dx+dy*dy <= radiusM*radiusM {
				fn(g.idx(ix, iy))
			}
		}
	}
}

// stampVertices marks the cell under every vertex of a polygon/line. Scanline
// fill samples cell centres, so a pier or islet thinner than one cell would
// otherwise vanish; walking its outline guarantees it still blocks.
func (g *navGrid) stampVertices(rings [][][]float64, fn func(i int)) {
	for _, ring := range rings {
		for _, p := range ring {
			if len(p) < 2 {
				continue
			}
			if i, ok := g.cellAt(p[0], p[1]); ok {
				fn(i)
			}
		}
	}
}

// finalize turns the rasterised flags/depths into the per-cell cost multiplier
// A* searches, in two steps: decide what is hard-blocked, then grow the
// clearance buffer out from those cells and price proximity to it.
func (g *navGrid) finalize(c gridCost) {
	n := g.nx * g.ny
	g.cost = make([]float32, n)

	blocked := make([]bool, n)
	for i := 0; i < n; i++ {
		f := g.flags[i]
		if f&(cellLand|cellObstruction) != 0 {
			blocked[i] = true
			continue
		}
		d := g.depth[i]
		if !math.IsNaN(d) && d < c.SafeDepthM && f&cellDredged == 0 {
			blocked[i] = true
		}
	}

	g.clearCells = chamferDistance(blocked, g.nx, g.ny)

	span := c.IdealDepthM - c.SafeDepthM
	for i := 0; i < n; i++ {
		if blocked[i] {
			g.cost[i] = float32(math.Inf(1))
			continue
		}
		distM := float64(g.clearCells[i]) * g.mx
		if distM < c.HardClearanceM {
			g.cost[i] = float32(math.Inf(1))
			continue
		}
		mult := 1.0
		f := g.flags[i]
		d := g.depth[i]
		switch {
		case f&cellDredged != 0:
			// Maintained channel — charted or not, it is kept navigable.
		case math.IsNaN(d):
			mult += c.UnknownPenalty
		case span > 0 && d < c.IdealDepthM:
			mult += c.DepthPenalty * (c.IdealDepthM - d) / span
		}
		if c.SoftClearanceM > c.HardClearanceM && distM < c.SoftClearanceM {
			t := (c.SoftClearanceM - distM) / (c.SoftClearanceM - c.HardClearanceM)
			mult += c.ShorePenalty * t
		}
		if f&cellAvoid != 0 {
			mult += c.AvoidPenalty
		}
		g.cost[i] = float32(mult)
	}
}

func (g *navGrid) passable(i int) bool {
	return !math.IsInf(float64(g.cost[i]), 1)
}

// chamferDistance is a two-pass 5-7-11 chamfer transform: the distance, in
// cells, from each cell to the nearest true cell in src. Within ~2% of the
// true Euclidean distance, which is far finer than we price clearance at.
func chamferDistance(src []bool, nx, ny int) []float32 {
	const (
		a = 5.0 / 5.0  // orthogonal step
		b = 7.0 / 5.0  // diagonal step
		c = 11.0 / 5.0 // knight step
	)
	const inf = float32(1e9)
	d := make([]float32, len(src))
	for i := range d {
		if src[i] {
			d[i] = 0
		} else {
			d[i] = inf
		}
	}
	relax := func(ix, iy int, offsets [][3]float64) {
		i := iy*nx + ix
		best := d[i]
		for _, o := range offsets {
			jx, jy := ix+int(o[0]), iy+int(o[1])
			if jx < 0 || jy < 0 || jx >= nx || jy >= ny {
				continue
			}
			if v := d[jy*nx+jx] + float32(o[2]); v < best {
				best = v
			}
		}
		d[i] = best
	}
	fwd := [][3]float64{{-1, 0, a}, {0, -1, a}, {-1, -1, b}, {1, -1, b}, {-2, -1, c}, {-1, -2, c}, {1, -2, c}, {2, -1, c}}
	bwd := [][3]float64{{1, 0, a}, {0, 1, a}, {1, 1, b}, {-1, 1, b}, {2, 1, c}, {1, 2, c}, {-1, 2, c}, {-2, 1, c}}
	for iy := 0; iy < ny; iy++ {
		for ix := 0; ix < nx; ix++ {
			relax(ix, iy, fwd)
		}
	}
	for iy := ny - 1; iy >= 0; iy-- {
		for ix := nx - 1; ix >= 0; ix-- {
			relax(ix, iy, bwd)
		}
	}
	return d
}
