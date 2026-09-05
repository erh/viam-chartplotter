package render

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// Building and reading the precomputed navigability tiles.
//
// The expensive half of routing was never the search — it was fetching the
// polygons and rasterising them, every time, for water that hadn't changed
// since the last ingest. These tiles move that work to the database once.
//
// Tiles are built on demand rather than in one enormous upfront pass: the
// first route through a piece of water pays for it, everything after reads it.
// A prewarm pass over a region is the same code driven by a CLI.

// navTileZoomFor picks the tile zoom whose cells are at or finer than the
// requested resolution. A tile is NavTileSize cells across one slippy tile, so
// cell size halves with each zoom level.
func navTileZoomFor(cellM, lat float64) int {
	for z := navMinZoom; z <= navMaxZoom; z++ {
		if navCellSizeM(z, lat) <= cellM {
			return z
		}
	}
	return navMaxZoom
}

// navMinZoom / navMaxZoom bound the tile ladder. z9 is ~234 m cells at 40N —
// open-water resolution — and z13 is ~15 m, finer than any routing grid we
// build, so there is nothing to gain below it.
const (
	navMinZoom = 9
	navMaxZoom = 13
)

// NavMinZoom / NavMaxZoom expose the tile ladder for the prewarm command.
const (
	NavMinZoom = navMinZoom
	NavMaxZoom = navMaxZoom
)

// NavCellSizeM and NavTilesForBBox are exported for the prewarm command, so it
// reports and covers exactly what the router will read.
func NavCellSizeM(z int, lat float64) float64 { return navCellSizeM(z, lat) }

func NavTilesForBBox(z int, minLon, minLat, maxLon, maxLat float64) [][3]int {
	return navTilesForBBox(z, minLon, minLat, maxLon, maxLat)
}

// navCellSizeM is the ground size of one tile cell at a zoom and latitude.
func navCellSizeM(z int, lat float64) float64 {
	degPerCell := 360.0 / float64(int(1)<<z) / float64(noaa.NavTileSize)
	return degPerCell * metresPerDegreeLat * clampCosLat(lat)
}

// navTileBounds returns a tile's lon/lat bounds. Tiles are Web-Mercator, so
// their latitude span varies; the grid assembled from them is therefore
// Mercator-aligned rather than equirectangular.
func navTileBounds(z, x, y int) (minLon, minLat, maxLon, maxLat float64) {
	n := float64(int(1) << z)
	minLon = float64(x)/n*360.0 - 180.0
	maxLon = float64(x+1)/n*360.0 - 180.0
	maxLat = mercTileLat(float64(y), n)
	minLat = mercTileLat(float64(y+1), n)
	return
}

func mercTileLat(y, n float64) float64 {
	t := math.Pi * (1 - 2*y/n)
	return math.Atan(math.Sinh(t)) * 180.0 / math.Pi
}

// navTilesForBBox lists the tiles covering a bbox at a zoom.
func navTilesForBBox(z int, minLon, minLat, maxLon, maxLat float64) [][3]int {
	x0, y0 := lonLatToTile(minLon, maxLat, z) // north-west
	x1, y1 := lonLatToTile(maxLon, minLat, z) // south-east
	max := int(1) << z
	clamp := func(v int) int {
		if v < 0 {
			return 0
		}
		if v >= max {
			return max - 1
		}
		return v
	}
	x0, x1, y0, y1 = clamp(x0), clamp(x1), clamp(y0), clamp(y1)
	var out [][3]int
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			out = append(out, [3]int{z, x, y})
		}
	}
	return out
}

// navTileBuildConcurrency bounds how many missing tiles are built at once.
// Each build is its own chart query plus a rasterise, so this is a database
// and memory ceiling.
const navTileBuildConcurrency = 4

// navTileQueryTimeout bounds one tile's chart query. A tile is a small area,
// so this is generous; it exists so a stuck query can't hold a route.
const navTileQueryTimeout = 30 * time.Second

// NavTiles returns the tiles covering a bbox at the given zoom, building and
// storing any that are missing. Tiles already built are read straight back.
func (r *ENCRenderer) NavTiles(ctx context.Context, z int, minLon, minLat, maxLon, maxLat float64) (map[[3]int]*noaa.NavTile, error) {
	keys := navTilesForBBox(z, minLon, minLat, maxLon, maxLat)
	if len(keys) == 0 {
		return nil, nil
	}

	have, err := noaa.GetNavTiles(ctx, r.navColl, keys)
	if err != nil {
		// A cache read failure is not a routing failure: fall through and
		// build what we need from the polygons.
		r.logger.Warnf("navgrid: read failed, building from source: %v", err)
		have = nil
	}
	if have == nil {
		have = map[[3]int]*noaa.NavTile{}
	}

	var missing [][3]int
	for _, k := range keys {
		if _, ok := have[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return have, nil
	}

	built := make([]*noaa.NavTile, len(missing))
	errs := make([]error, len(missing))
	var wg sync.WaitGroup
	sem := make(chan struct{}, navTileBuildConcurrency)
	for i, k := range missing {
		wg.Add(1)
		go func(i int, k [3]int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			built[i], errs[i] = r.buildNavTile(ctx, k[0], k[1], k[2])
		}(i, k)
	}
	wg.Wait()

	fresh := make([]*noaa.NavTile, 0, len(missing))
	for i, t := range built {
		if errs[i] != nil {
			return nil, errs[i]
		}
		have[missing[i]] = t
		fresh = append(fresh, t)
	}
	if r.navColl != nil {
		if err := noaa.PutNavTiles(ctx, r.navColl, fresh); err != nil {
			// Storing is an optimisation for next time; having built the tiles
			// we can still answer this request.
			r.logger.Warnf("navgrid: store failed, tiles will be rebuilt next time: %v", err)
		}
	}
	return have, nil
}

// buildNavTile rasterises one tile from the chart polygons.
//
// The tile records facts, not judgements: charted depth, land, an obstruction
// with no charted depth over it. A wreck WITH a sounding folds into the depth
// instead of becoming a blanket obstruction, so a deep-draft boat and a dinghy
// can read the same tile and reach different, correct conclusions.
func (r *ENCRenderer) buildNavTile(ctx context.Context, z, x, y int) (*noaa.NavTile, error) {
	minLon, minLat, maxLon, maxLat := navTileBounds(z, x, y)
	// A feature just outside the tile can still cover cells inside it, so the
	// query is padded by a couple of cells' worth.
	pad := 2 * navCellSizeM(z, (minLat+maxLat)/2) / metresPerDegreeLat
	qctx, cancel := context.WithTimeout(ctx, navTileQueryTimeout)
	defer cancel()

	cellM := navCellSizeM(z, (minLat+maxLat)/2)
	feats, err := r.queryFeaturesClasses(qctx,
		minLon-pad, minLat-pad, maxLon+pad, maxLat+pad,
		noaa.ClassQuery{
			Classes: navTileClasses(),
			// The tile IS the resolution limit, so thin the source to match it
			// in both directions: coarser geometry, and no cells charted finer
			// than the tile can express. A z9 tile spans ~60 km, and fetching
			// every berthing-scale feature in that to average it down to 234 m
			// cells is the same query that times out on a live corridor.
			MaxUsageBand: routingUsageBand(cellM),
			UseLowGeom:   useLowGeomForCell(cellM),
			Projection:   RoutingProjection(),
		})
	if err != nil {
		return nil, wrapChartQueryError(err, "navgrid tile build",
			"run `chartdiag route` to check the chart indexes")
	}

	g := newTileGrid(z, x, y)
	rasterizeForNavTile(g, feats)
	return g.tile(), nil
}

// navTileClasses is every class the tile records. Deliberately independent of
// any AutoRouteOptions: a tile serves every boat and every avoid setting, so
// it must carry restricted areas (as a flag the router may ignore) rather than
// leave them out when the request that built it didn't ask for them.
func navTileClasses() []string {
	opts := AutoRouteOptions{Avoid: []AvoidArea{RestrictedAreaAvoid(1)}}
	return autoRouteClasses(opts)
}

// tileGrid is a navGrid covering exactly one slippy tile, used while building.
type tileGrid struct {
	*navGrid
	z, x, y int
}

// newTileGrid builds a NavTileSize x NavTileSize grid over one tile. Rows run
// north to south to match the stored tile's row-major order.
func newTileGrid(z, x, y int) *tileGrid {
	minLon, minLat, maxLon, maxLat := navTileBounds(z, x, y)
	n := noaa.NavTileSize
	g := &navGrid{
		minLon: minLon, minLat: minLat,
		dLon: (maxLon - minLon) / float64(n),
		dLat: (maxLat - minLat) / float64(n),
		nx:   n, ny: n,
	}
	g.mx = g.dLon * metresPerDegreeLat * clampCosLat((minLat+maxLat)/2)
	g.my = g.dLat * metresPerDegreeLat
	size := n * n
	g.depth = make([]float64, size)
	g.scaleOf = make([]int32, size)
	g.flags = make([]uint8, size)
	for i := range g.depth {
		g.depth[i] = math.NaN()
		g.scaleOf[i] = math.MaxInt32
	}
	return &tileGrid{navGrid: g, z: z, x: x, y: y}
}

// tile converts the built grid into its stored form.
func (t *tileGrid) tile() *noaa.NavTile {
	n := noaa.NavTileSize
	depth := make([]int16, n*n)
	flags := make([]uint8, n*n)
	for iy := 0; iy < n; iy++ {
		// Stored rows run north-to-south; the grid's rows run south-to-north.
		src := (n - 1 - iy) * n
		dst := iy * n
		for ix := 0; ix < n; ix++ {
			d := t.depth[src+ix]
			if math.IsNaN(d) {
				depth[dst+ix] = noaa.NavDepthUncharted
			} else {
				depth[dst+ix] = clampDecimetres(d)
			}
			flags[dst+ix] = storedFlags(t.flags[src+ix])
		}
	}
	return &noaa.NavTile{Z: t.z, X: t.x, Y: t.y, Depth: depth, Flags: flags}
}

// clampDecimetres converts metres to the stored int16 decimetres, saturating
// rather than wrapping — a bad depth should read as very deep or very shoal,
// never as its opposite.
func clampDecimetres(m float64) int16 {
	dm := math.Round(m * 10)
	if dm > math.MaxInt16 {
		return math.MaxInt16
	}
	if dm <= noaa.NavDepthUncharted {
		return noaa.NavDepthUncharted + 1
	}
	return int16(dm)
}

// storedFlags maps the in-memory cell flags to the stored bits. cellAvoid is
// split: unsurveyed and restricted are different facts, and a router may want
// to treat them differently.
func storedFlags(f uint8) uint8 {
	var out uint8
	if f&cellLand != 0 {
		out |= noaa.NavFlagLand
	}
	if f&cellObstruction != 0 {
		out |= noaa.NavFlagObstruction
	}
	if f&cellDredged != 0 {
		out |= noaa.NavFlagDredged
	}
	if f&cellUnsurveyed != 0 {
		out |= noaa.NavFlagUnsurveyed
	}
	if f&cellRestricted != 0 {
		out |= noaa.NavFlagRestricted
	}
	return out
}

// sampleTilesIntoGrid fills a routing grid from precomputed tiles, reporting
// how many cells a tile covered.
//
// It samples rather than assembles, and that is the whole point. Tiles are
// Web-Mercator, so their rows are not evenly spaced in latitude; stitching
// them into a grid and then reading row index as linear latitude puts the
// chart in the wrong place — measured at 2.6 km, some eleven cells, over a
// three-degree grid. Sampling each cell at its own lon/lat leaves the routing
// grid exactly the equirectangular one the rest of the router already agrees
// on, and pushes the projection into one well-tested conversion per cell.
func sampleTilesIntoGrid(g *navGrid, tiles map[[3]int]*noaa.NavTile, z int) int {
	if len(tiles) == 0 {
		return 0
	}
	n := noaa.NavTileSize
	scale := float64(int(1) << z)
	covered := 0

	for iy := 0; iy < g.ny; iy++ {
		for ix := 0; ix < g.nx; ix++ {
			lon, lat := g.cellCentre(ix, iy)
			fx, fy := lonLatToTileFrac(lon, lat, scale)
			tx, ty := int(math.Floor(fx)), int(math.Floor(fy))
			tile := tiles[[3]int{z, tx, ty}]
			if tile == nil {
				continue // no tile here: stays uncharted, which is passable-but-costly
			}
			px := clampIndex(int((fx-math.Floor(fx))*float64(n)), n)
			// Tile rows already run north-to-south, matching slippy y.
			py := clampIndex(int((fy-math.Floor(fy))*float64(n)), n)
			src := py*n + px
			dst := iy*g.nx + ix
			if d := tile.Depth[src]; d != noaa.NavDepthUncharted {
				g.depth[dst] = float64(d) / 10.0
			}
			g.flags[dst] = liveFlags(tile.Flags[src])
			covered++
		}
	}
	return covered
}

// lonLatToTileFrac is lonLatToTile without the truncation, so a caller can
// find the pixel within the tile as well as the tile.
func lonLatToTileFrac(lon, lat, scale float64) (x, y float64) {
	x = (lon + 180.0) / 360.0 * scale
	latRad := lat * math.Pi / 180.0
	y = (1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * scale
	return
}

func clampIndex(v, n int) int {
	if v < 0 {
		return 0
	}
	if v >= n {
		return n - 1
	}
	return v
}

// liveFlags maps stored tile bits back to the in-memory cell flags.
func liveFlags(f uint8) uint8 {
	var out uint8
	if f&noaa.NavFlagLand != 0 {
		out |= cellLand
	}
	if f&noaa.NavFlagObstruction != 0 {
		out |= cellObstruction
	}
	if f&noaa.NavFlagDredged != 0 {
		out |= cellDredged
	}
	if f&noaa.NavFlagUnsurveyed != 0 {
		out |= cellUnsurveyed
	}
	if f&noaa.NavFlagRestricted != 0 {
		out |= cellRestricted
	}
	return out
}
