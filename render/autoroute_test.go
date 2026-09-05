package render

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beetlebugorg/s57/pkg/s57"
	"go.viam.com/rdk/logging"
	"go.viam.com/test"
)

// ---------------------------------------------------------------------------
// Scene helpers: hand-built ENC features so the router can be exercised with
// no Mongo behind it. Rings are self-closed, matching what featureFromDoc
// hands the draw path (and what splitRings unpacks).
// ---------------------------------------------------------------------------

func ring(minLon, minLat, maxLon, maxLat float64) [][]float64 {
	return [][]float64{
		{minLon, minLat}, {maxLon, minLat}, {maxLon, maxLat}, {minLon, maxLat}, {minLon, minLat},
	}
}

func areaFeature(id, class string, scale int, attrs map[string]any, rings ...[][]float64) *mongoFeature {
	var flat [][]float64
	for _, r := range rings {
		flat = append(flat, r...)
	}
	if attrs == nil {
		attrs = map[string]any{}
	}
	return &mongoFeature{
		id:    id,
		class: class,
		scale: scale,
		attrs: attrs,
		geom:  s57.Geometry{Type: s57.GeometryTypePolygon, Coordinates: flat},
	}
}

func depare(id string, scale int, drval1 float64, r [][]float64) *mongoFeature {
	return areaFeature(id, "DEPARE", scale, map[string]any{"DRVAL1": drval1}, r)
}

// testOptions is a small, fast grid with the clearance buffers off, so a test
// asserts on the charted geometry alone and not on the shore margin.
func testOptions(safeDepthM float64) AutoRouteOptions {
	o := DefaultAutoRouteOptions(safeDepthM)
	o.IdealDepthM = safeDepthM // depth preference off unless a test wants it
	o.HardClearanceM = 0
	o.SoftClearanceM = 0
	o.MaxCells = 40000
	o.MinCellM = 5
	o.MaxCellM = 1000 // the test scenes are tiny; don't trip the resolution guard
	return o
}

// deepEverywhere is a single DEPARE covering far more than any test bbox.
func deepEverywhere(drval1 float64) *mongoFeature {
	return depare("base", 80000, drval1, ring(-71.7, 40.8, -71.2, 41.2))
}

var (
	westPoint = RoutePoint{Lat: 41.00, Lng: -71.50}
	eastPoint = RoutePoint{Lat: 41.00, Lng: -71.40}
)

func planTestRoute(t *testing.T, feats []*mongoFeature, start, end RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	t.Helper()
	opts.normalize(haversineMeters(start.Lat, start.Lng, end.Lat, end.Lng))
	return planRoute(feats, routeBBox(start, end, opts.CorridorPadM), start, end, opts)
}

func maxWaypointLat(res *AutoRouteResult) float64 {
	m := math.Inf(-1)
	for _, w := range res.Waypoints {
		m = math.Max(m, w.Lat)
	}
	return m
}

func minWaypointLat(res *AutoRouteResult) float64 {
	m := math.Inf(1)
	for _, w := range res.Waypoints {
		m = math.Min(m, w.Lat)
	}
	return m
}

// ---------------------------------------------------------------------------

func TestAutoRouteOpenWater(t *testing.T) {
	res, err := planTestRoute(t, []*mongoFeature{deepEverywhere(10)}, westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	// Nothing in the way: one leg, start to finish, at the direct distance.
	test.That(t, len(res.Waypoints), test.ShouldEqual, 2)
	test.That(t, res.Waypoints[0], test.ShouldResemble, westPoint)
	test.That(t, res.Waypoints[1], test.ShouldResemble, eastPoint)
	direct := haversineMeters(westPoint.Lat, westPoint.Lng, eastPoint.Lat, eastPoint.Lng)
	test.That(t, res.DistanceMeters, test.ShouldAlmostEqual, direct, 1.0)
	test.That(t, res.CrossedUnknown, test.ShouldBeFalse)
	test.That(t, res.MinDepthMeters, test.ShouldNotBeNil)
	test.That(t, *res.MinDepthMeters, test.ShouldAlmostEqual, 10.0, 0.001)
}

func TestAutoRouteGoesRoundLand(t *testing.T) {
	// A north-south wall of land across the rhumb line, with a gap between
	// 41.005 and 41.020. The only way through is that gap.
	feats := []*mongoFeature{
		deepEverywhere(10),
		areaFeature("wall-s", "LNDARE", 20000, nil, ring(-71.452, 40.90, -71.448, 41.005)),
		areaFeature("wall-n", "LNDARE", 20000, nil, ring(-71.452, 41.020, -71.448, 41.10)),
	}
	res, err := planTestRoute(t, feats, westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, maxWaypointLat(res), test.ShouldBeGreaterThan, 41.004)
	test.That(t, maxWaypointLat(res), test.ShouldBeLessThan, 41.021)
	// A detour, so necessarily longer than the straight line.
	test.That(t, res.DistanceMeters, test.ShouldBeGreaterThan, res.DirectMeters)
}

func TestAutoRouteNoWayThrough(t *testing.T) {
	feats := []*mongoFeature{
		deepEverywhere(10),
		areaFeature("wall", "LNDARE", 20000, nil, ring(-71.452, 40.80, -71.448, 41.20)),
	}
	_, err := planTestRoute(t, feats, westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "no safe route")
}

func TestAutoRouteSafeDepthIsTheHardConstraint(t *testing.T) {
	// The same barrier, but charted as shoal water rather than land: a finer
	// cell charts a 0.5 m strip straight across the deep base.
	shoal := func() []*mongoFeature {
		return []*mongoFeature{
			deepEverywhere(10),
			depare("shoal-s", 20000, 0.5, ring(-71.452, 40.90, -71.448, 41.005)),
			depare("shoal-n", 20000, 0.5, ring(-71.452, 41.020, -71.448, 41.10)),
		}
	}

	// A boat needing 2 m has to use the gap.
	deepDraft, err := planTestRoute(t, shoal(), westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, maxWaypointLat(deepDraft), test.ShouldBeGreaterThan, 41.004)
	test.That(t, deepDraft.MinDepthMeters, test.ShouldNotBeNil)
	test.That(t, *deepDraft.MinDepthMeters, test.ShouldBeGreaterThanOrEqualTo, 2.0)

	// A dinghy drawing under 0.5 m goes straight over it.
	shallowDraft, err := planTestRoute(t, shoal(), westPoint, eastPoint, testOptions(0.3))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(shallowDraft.Waypoints), test.ShouldEqual, 2)
	test.That(t, shallowDraft.DistanceMeters, test.ShouldAlmostEqual, shallowDraft.DirectMeters, 1.0)
}

// idealDepthScene is a shallow-but-legal channel along the rhumb line and a
// deeper one to the south, joined at both ends.
func idealDepthScene() []*mongoFeature {
	return []*mongoFeature{
		deepEverywhere(0.5), // everything else is unnavigable at a 2 m draft
		depare("direct", 20000, 3.0, ring(-71.51, 40.9955, -71.39, 40.9975)),
		depare("detour", 20000, 10.0, ring(-71.51, 40.9895, -71.39, 40.9915)),
		depare("link-w", 20000, 10.0, ring(-71.505, 40.989, -71.495, 40.998)),
		depare("link-e", 20000, 10.0, ring(-71.405, 40.989, -71.395, 40.998)),
	}
}

func TestAutoRouteIdealDepthPrefersDeeperWater(t *testing.T) {
	start := RoutePoint{Lat: 40.9965, Lng: -71.50}
	end := RoutePoint{Lat: 40.9965, Lng: -71.40}

	// Ideal depth off: the shortest safe route wins, shallow channel and all.
	off := testOptions(2)
	shortest, err := planTestRoute(t, idealDepthScene(), start, end, off)
	test.That(t, err, test.ShouldBeNil)
	// Straight down the shallow channel — it never dips into the deeper one.
	test.That(t, minWaypointLat(shortest), test.ShouldBeGreaterThan, 40.994)

	// Ask for 8 m of water and the router pays the extra distance to get it.
	prefer := testOptions(2)
	prefer.IdealDepthM = 8
	deeper, err := planTestRoute(t, idealDepthScene(), start, end, prefer)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, minWaypointLat(deeper), test.ShouldBeLessThan, 40.992)
	test.That(t, deeper.DistanceMeters, test.ShouldBeGreaterThan, shortest.DistanceMeters)
}

func TestAutoRouteObstructionsBlockUnlessDeepEnough(t *testing.T) {
	// A line of wrecks across the rhumb line, each with 1 m over it.
	wrecks := func(valsou any) []*mongoFeature {
		feats := []*mongoFeature{deepEverywhere(10)}
		for i := 0; i < 40; i++ {
			lat := 40.980 + float64(i)*0.001
			feats = append(feats, &mongoFeature{
				id:    "wreck",
				class: "WRECKS",
				scale: 20000,
				attrs: map[string]any{"VALSOU": valsou},
				geom:  s57.Geometry{Type: s57.GeometryTypePoint, Coordinates: [][]float64{{-71.45, lat}}},
			})
		}
		return feats
	}

	opts := testOptions(2)
	opts.HardClearanceM = 60 // each wreck holds a 60 m berth

	blocked, err := planTestRoute(t, wrecks(1.0), westPoint, eastPoint, opts)
	test.That(t, err, test.ShouldBeNil)
	// It got round the line rather than through it.
	test.That(t, blocked.DistanceMeters, test.ShouldBeGreaterThan, blocked.DirectMeters+100)

	// The same wrecks with 20 m of water over them are no obstacle at all.
	clear, err := planTestRoute(t, wrecks(20.0), westPoint, eastPoint, opts)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, clear.DistanceMeters, test.ShouldAlmostEqual, clear.DirectMeters, 50.0)
}

func TestAutoRouteUnknownWaterIsPassableButCostly(t *testing.T) {
	// Deep charted water everywhere except a band across the middle that no
	// DEPARE covers. With nothing else on offer the route crosses it and says
	// so; given a charted way round, it takes that instead.
	gap := []*mongoFeature{
		depare("south", 20000, 10, ring(-71.7, 40.80, -71.2, 40.995)),
		depare("north", 20000, 10, ring(-71.7, 41.005, -71.2, 41.20)),
	}
	res, err := planTestRoute(t, gap, RoutePoint{Lat: 40.99, Lng: -71.45}, RoutePoint{Lat: 41.01, Lng: -71.45}, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res.CrossedUnknown, test.ShouldBeTrue)
}

func TestAutoRouteSnapsAStartOnLand(t *testing.T) {
	feats := []*mongoFeature{
		deepEverywhere(10),
		areaFeature("dock", "LNDARE", 20000, nil, ring(-71.5005, 40.9995, -71.4995, 41.0005)),
	}
	res, err := planTestRoute(t, feats, westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res.SnappedStart, test.ShouldBeTrue)
	test.That(t, res.Warnings, test.ShouldNotBeEmpty)
	// The snapped start is close to where it was asked for, not somewhere else.
	moved := haversineMeters(res.Waypoints[0].Lat, res.Waypoints[0].Lng, westPoint.Lat, westPoint.Lng)
	test.That(t, moved, test.ShouldBeLessThan, 400)
}

func TestAutoRouteAvoidAreaIsSoft(t *testing.T) {
	// A restricted area straddling the rhumb line with a way round it: with
	// the avoid rule on, the router goes round; it is a cost, not a wall, so
	// it still routes when there is no alternative.
	feats := func() []*mongoFeature {
		return []*mongoFeature{
			deepEverywhere(10),
			areaFeature("res", "RESARE", 20000, nil, ring(-71.47, 40.995, -71.43, 41.02)),
		}
	}
	plain, err := planTestRoute(t, feats(), westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)

	avoiding := testOptions(2)
	avoiding.Avoid = []AvoidArea{RestrictedAreaAvoid(3.0)}
	steered, err := planTestRoute(t, feats(), westPoint, eastPoint, avoiding)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, steered.DistanceMeters, test.ShouldBeGreaterThan, plain.DistanceMeters)
	// It went south, round the bottom edge of the area.
	test.That(t, minWaypointLat(steered), test.ShouldBeLessThan, 40.9955)
}

func TestAutoRouteClassesCoverTheRasteriser(t *testing.T) {
	// The chart query fetches exactly the classes rasterizeForRouting reads;
	// if a class is added to one and not the other the router silently stops
	// seeing that hazard. Assert the query list covers all three tables.
	got := map[string]bool{}
	for _, c := range autoRouteClasses(AutoRouteOptions{Avoid: []AvoidArea{RestrictedAreaAvoid(3)}}) {
		got[c] = true
	}
	for _, table := range []map[string]bool{
		autoRouteLandClasses, autoRouteObstructionClasses, autoRoutePointHazardClasses,
	} {
		for c := range table {
			test.That(t, got[c], test.ShouldBeTrue)
		}
	}
	for _, c := range []string{"DEPARE", "DRGARE", "UNSARE", "RESARE"} {
		test.That(t, got[c], test.ShouldBeTrue)
	}
	// Soundings are the densest class in the store and the router can't read
	// them (no Z coordinate) — fetching them is what blew the query deadline.
	test.That(t, got["SOUNDG"], test.ShouldBeFalse)
}

func TestGridCellSizeGrowsWithTheLeg(t *testing.T) {
	opts := DefaultAutoRouteOptions(2)
	legFor := func(nm float64) []RoutePoint {
		end := RoutePoint{Lat: westPoint.Lat, Lng: westPoint.Lng + nm*1852/(metresPerDegreeLat*clampCosLat(westPoint.Lat))}
		return []RoutePoint{westPoint, end}
	}
	cellFor := func(nm float64) float64 {
		pts := legFor(nm)
		return gridCellSize(pointsBBox(pts, sectionPadM(pts, 0)), opts)
	}

	// Resolution degrades with distance — that is inherent, since the grid
	// covers the whole corridor in a bounded number of cells.
	test.That(t, cellFor(2), test.ShouldBeLessThan, cellFor(20))
	test.That(t, cellFor(20), test.ShouldBeLessThan, cellFor(100))

	// Harbour and coastal legs resolve inside the configured floor.
	test.That(t, cellFor(2), test.ShouldBeLessThan, 30)
	test.That(t, cellFor(20), test.ShouldBeLessThanOrEqualTo, opts.MaxCellM)

	// A long offshore leg is coarser than the floor, and the adaptive limit is
	// what lets it be planned at all rather than refused.
	long := legFor(100)
	test.That(t, cellFor(100), test.ShouldBeGreaterThan, opts.MaxCellM)
	test.That(t, effectiveMaxCellM(long, opts), test.ShouldBeGreaterThan, opts.MaxCellM)
	test.That(t, sectionFits(long, opts, 0), test.ShouldBeTrue)
}

func TestEffectiveMaxCellStaysWithinItsBounds(t *testing.T) {
	opts := DefaultAutoRouteOptions(2)
	short := []RoutePoint{westPoint, {Lat: westPoint.Lat, Lng: westPoint.Lng + 0.01}}
	// A short leg gets no relaxation: the floor is the limit.
	test.That(t, effectiveMaxCellM(short, opts), test.ShouldAlmostEqual, opts.MaxCellM, 0.001)

	// A transatlantic leg is capped at the ceiling, not relaxed without bound —
	// past it the cells are wider than the features that make a route wrong.
	huge := []RoutePoint{{Lat: 41.0, Lng: -71.0}, {Lat: 50.0, Lng: -10.0}}
	test.That(t, effectiveMaxCellM(huge, opts), test.ShouldAlmostEqual, maxCellCeilingM, 0.001)
}

func TestCorridorPadIsCapped(t *testing.T) {
	// The pad lets a route deviate around something; 15 nm is more deviation
	// than any obstacle demands. Uncapped, a long passage asks for a margin
	// that quadruples the area to raster for no routing benefit.
	test.That(t, corridorPadFor(1000), test.ShouldAlmostEqual, 1852.0, 1)   // floor
	test.That(t, corridorPadFor(37000), test.ShouldAlmostEqual, 14800.0, 1) // 40%
	test.That(t, corridorPadFor(500000), test.ShouldAlmostEqual, maxCorridorPadM, 1)
}

func TestUseLowGeomFollowsCellSize(t *testing.T) {
	// The simplified tier throws away detail finer than ~38 m. Below that cell
	// size the router could act on the difference, so it must fetch full
	// geometry; above it the detail is smaller than a cell and fetching it is
	// pure payload.
	test.That(t, lowGeomToleranceMeters, test.ShouldBeGreaterThan, 30.0)
	test.That(t, lowGeomToleranceMeters, test.ShouldBeLessThan, 45.0)
	test.That(t, useLowGeomForCell(15), test.ShouldBeFalse)
	test.That(t, useLowGeomForCell(100), test.ShouldBeTrue)
}

func TestPlanAutoRouteDescribesTheQuery(t *testing.T) {
	// PlanAutoRoute is what chartdiag explains, so it has to agree with what
	// AutoRoute would actually run.
	plan := PlanAutoRoute(westPoint, eastPoint, DefaultAutoRouteOptions(2))
	test.That(t, plan.GridW, test.ShouldBeGreaterThan, 0)
	test.That(t, plan.GridH, test.ShouldBeGreaterThan, 0)
	test.That(t, plan.CellM, test.ShouldBeGreaterThan, 0)
	test.That(t, plan.UseLowGeom, test.ShouldEqual, useLowGeomForCell(plan.CellM))
	test.That(t, plan.Classes, test.ShouldContain, "DEPARE")
	test.That(t, plan.Classes, test.ShouldContain, "LNDARE")
	// The corridor must contain both endpoints, or the plan describes a query
	// that could not answer the request.
	test.That(t, plan.BBox[0], test.ShouldBeLessThan, westPoint.Lng)
	test.That(t, plan.BBox[2], test.ShouldBeGreaterThan, eastPoint.Lng)
}

// ---------------------------------------------------------------------------
// Grid unit tests.
// ---------------------------------------------------------------------------

func TestNavGridFillRingsHonoursHoles(t *testing.T) {
	g := newNavGrid(-71.51, 40.99, -71.49, 41.01, 40000, 5, 1400)
	outer := ring(-71.508, 40.992, -71.492, 41.008)
	hole := ring(-71.504, 40.996, -71.496, 41.004)
	filled := map[int]bool{}
	g.fillRings([][][]float64{outer, hole}, func(i int) { filled[i] = true })

	inHole, ok := g.cellAt(-71.500, 41.000)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, filled[inHole], test.ShouldBeFalse)

	inRing, ok := g.cellAt(-71.506, 41.000)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, filled[inRing], test.ShouldBeTrue)

	outside, ok := g.cellAt(-71.4905, 41.000)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, filled[outside], test.ShouldBeFalse)
}

func TestNavGridSetDepthFinestCellWins(t *testing.T) {
	g := newNavGrid(-71.51, 40.99, -71.49, 41.01, 4000, 5, 1400)
	i, ok := g.cellAt(-71.50, 41.00)
	test.That(t, ok, test.ShouldBeTrue)

	g.setDepth(i, 12, 80000) // coastal cell: deep
	test.That(t, g.depth[i], test.ShouldAlmostEqual, 12.0, 0.001)

	g.setDepth(i, 3, 20000) // harbour cell: shoaler, and finer, so it wins
	test.That(t, g.depth[i], test.ShouldAlmostEqual, 3.0, 0.001)

	g.setDepth(i, 30, 80000) // coarser again: ignored
	test.That(t, g.depth[i], test.ShouldAlmostEqual, 3.0, 0.001)

	g.setDepth(i, 1, 20000) // same scale: keep the shoalest
	test.That(t, g.depth[i], test.ShouldAlmostEqual, 1.0, 0.001)
}

func TestChamferDistance(t *testing.T) {
	const n = 21
	src := make([]bool, n*n)
	src[10*n+10] = true
	d := chamferDistance(src, n, n)
	test.That(t, d[10*n+10], test.ShouldAlmostEqual, 0.0, 0.001)
	test.That(t, float64(d[10*n+14]), test.ShouldAlmostEqual, 4.0, 0.05)
	// Diagonal: true distance is 5*sqrt(2) ~= 7.07.
	test.That(t, float64(d[15*n+15]), test.ShouldAlmostEqual, 7.07, 0.25)
}

func TestPullTautNeverCutsACorner(t *testing.T) {
	// A grid split by a wall with a single gap; the taut path must still go
	// through the gap, not straight across the wall.
	g := newNavGrid(-71.52, 40.99, -71.48, 41.01, 10000, 5, 1400)
	for iy := 0; iy < g.ny; iy++ {
		if iy == g.ny/2 {
			continue // the gap
		}
		for ix := g.nx / 2; ix <= g.nx/2+1 && ix < g.nx; ix++ {
			g.mark(g.idx(ix, iy), cellLand)
		}
	}
	g.finalize(gridCost{SafeDepthM: 2, IdealDepthM: 2})

	start := g.idx(2, 2)
	goal := g.idx(g.nx-3, g.ny-3)
	path := g.findPath(start, goal)
	test.That(t, path, test.ShouldNotBeNil)

	pulled := g.pullTaut(path)
	test.That(t, len(pulled), test.ShouldBeLessThan, len(path))
	test.That(t, pulled[0], test.ShouldEqual, start)
	test.That(t, pulled[len(pulled)-1], test.ShouldEqual, goal)
	// Every leg of the pulled path is still clear.
	for i := 1; i < len(pulled); i++ {
		_, _, ok := g.traverse(pulled[i-1], pulled[i], nil)
		test.That(t, ok, test.ShouldBeTrue)
	}
}

// ---------------------------------------------------------------------------
// HTTP surface.
// ---------------------------------------------------------------------------

func TestAutoRouteHandlerBadRequest(t *testing.T) {
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/noaa-enc/autoroute?startLat=41", nil))
	test.That(t, rec.Code, test.ShouldEqual, http.StatusBadRequest)
}

func TestAutoRouteHandlerNoCharts(t *testing.T) {
	// No Mongo collection attached: the endpoint says so rather than
	// pretending the water is empty.
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/noaa-enc/autoroute?startLat=41&startLon=-71.5&endLat=41&endLon=-71.4", nil))
	test.That(t, rec.Code, test.ShouldEqual, http.StatusServiceUnavailable)
	test.That(t, rec.Body.String(), test.ShouldContainSubstring, "mongo_uri")
}

func TestOptimizeKeepsEveryWaypoint(t *testing.T) {
	// A three-point route whose middle leg runs into a wall of land: the
	// re-planned route must still pass through the operator's own waypoint,
	// because a waypoint is usually there for a reason the chart doesn't hold.
	feats := []*mongoFeature{
		deepEverywhere(10),
		areaFeature("wall", "LNDARE", 20000, nil, ring(-71.452, 40.90, -71.448, 41.005)),
	}
	via := RoutePoint{Lat: 41.03, Lng: -71.45}
	points := []RoutePoint{westPoint, via, eastPoint}

	opts := testOptions(2)
	opts.KeepWaypoints = true
	bbox := pointsBBox(points, opts.CorridorPadM)
	res, err := planRouteVia(feats, bbox, points, opts)
	test.That(t, err, test.ShouldBeNil)

	// The via point survives as a corner of the result.
	nearest := math.Inf(1)
	for _, w := range res.Waypoints {
		nearest = math.Min(nearest, haversineMeters(w.Lat, w.Lng, via.Lat, via.Lng))
	}
	test.That(t, nearest, test.ShouldBeLessThan, 150)
	test.That(t, res.Waypoints[0], test.ShouldResemble, westPoint)
	test.That(t, res.Waypoints[len(res.Waypoints)-1], test.ShouldResemble, eastPoint)
}

func TestOptimizeCanDropRedundantWaypoints(t *testing.T) {
	// Open water, with a pointless dogleg in the middle. With KeepWaypoints
	// off the smoother is allowed to straighten through it.
	feats := []*mongoFeature{deepEverywhere(10)}
	dogleg := RoutePoint{Lat: 41.02, Lng: -71.45}
	points := []RoutePoint{westPoint, dogleg, eastPoint}

	opts := testOptions(2)
	opts.KeepWaypoints = false
	bbox := pointsBBox(points, opts.CorridorPadM)
	res, err := planRouteVia(feats, bbox, points, opts)
	test.That(t, err, test.ShouldBeNil)

	// Straight through: two waypoints, and shorter than the original route.
	test.That(t, len(res.Waypoints), test.ShouldEqual, 2)
	test.That(t, res.DistanceMeters, test.ShouldBeLessThan, res.DirectMeters)

	// Kept, the dogleg stays and the route is no shorter than the original.
	keep := testOptions(2)
	keep.KeepWaypoints = true
	kept, err := planRouteVia(feats, bbox, points, keep)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(kept.Waypoints), test.ShouldBeGreaterThan, 2)
	test.That(t, kept.DistanceMeters, test.ShouldBeGreaterThan, res.DistanceMeters)
}

func TestOptimizeReportsTheOriginalLength(t *testing.T) {
	// direct_meters is the route as it was, so a caller can show what the
	// optimisation cost or saved.
	points := []RoutePoint{westPoint, {Lat: 41.02, Lng: -71.45}, eastPoint}
	opts := testOptions(2)
	res, err := planRouteVia([]*mongoFeature{deepEverywhere(10)}, pointsBBox(points, opts.CorridorPadM), points, opts)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res.DirectMeters, test.ShouldAlmostEqual, pathDistanceM(points), 1.0)
}

func TestOptimizeFailsOnTheLegThatCannotBeRouted(t *testing.T) {
	// A middle waypoint stranded behind an unbroken wall: the error has to
	// name which leg, or the operator has no idea which point to move.
	feats := []*mongoFeature{
		deepEverywhere(10),
		areaFeature("wall", "LNDARE", 20000, nil, ring(-71.452, 40.80, -71.448, 41.20)),
	}
	points := []RoutePoint{westPoint, {Lat: 41.00, Lng: -71.44}, eastPoint}
	opts := testOptions(2)
	_, err := planRouteVia(feats, pointsBBox(points, opts.CorridorPadM), points, opts)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "leg 1 of 2")
}

func TestPointsBBoxCoversEveryWaypoint(t *testing.T) {
	points := []RoutePoint{{Lat: 41.0, Lng: -71.5}, {Lat: 41.6, Lng: -71.1}, {Lat: 40.8, Lng: -71.9}}
	b := pointsBBox(points, 1852)
	for _, p := range points {
		test.That(t, p.Lng, test.ShouldBeGreaterThan, b[0])
		test.That(t, p.Lng, test.ShouldBeLessThan, b[2])
		test.That(t, p.Lat, test.ShouldBeGreaterThan, b[1])
		test.That(t, p.Lat, test.ShouldBeLessThan, b[3])
	}
}

func TestLongestLegSizesTheCorridor(t *testing.T) {
	// The pad is a fraction of a leg; on a multi-waypoint route it's the
	// longest leg that needs the most room to get around something.
	points := []RoutePoint{{Lat: 41.0, Lng: -71.5}, {Lat: 41.01, Lng: -71.49}, {Lat: 41.4, Lng: -71.1}}
	test.That(t, longestLegMeters(points), test.ShouldBeGreaterThan, 40000)
	test.That(t, legTotalMeters(points), test.ShouldBeGreaterThan, longestLegMeters(points))
}

func TestSectionsGroupWhatFitsAndSplitWhatDoesnt(t *testing.T) {
	opts := DefaultAutoRouteOptions(2)
	// Five waypoints marching east, each leg ~20 nm. Two legs share a grid at
	// this spacing; three do not — so the route both groups and splits, which
	// is the behaviour worth pinning. (The corridor pad scales with the
	// LONGEST leg, not the total, so a chain of short legs stays narrow and
	// groups freely — that is why the legs here have to be long.)
	pt := func(lonOffset float64) RoutePoint {
		return RoutePoint{Lat: 41.0, Lng: -71.5 + lonOffset}
	}
	points := []RoutePoint{pt(0), pt(0.45), pt(0.9), pt(1.35), pt(1.8)}

	secs, err := sectionsForResolution(points, opts, 0)
	test.That(t, err, test.ShouldBeNil)
	// Split: the whole route does not fit one grid.
	test.That(t, len(secs), test.ShouldBeGreaterThan, 1)
	test.That(t, sectionFits(points, opts, 0), test.ShouldBeFalse)
	// Grouped: at least one section carries more than a single leg, so nearby
	// legs share a chart query instead of getting one each.
	grouped := false
	for _, sec := range secs {
		if len(sec) > 2 {
			grouped = true
		}
	}
	test.That(t, grouped, test.ShouldBeTrue)

	// Every section resolves at or below the limit — that is the whole point.
	for _, sec := range secs {
		test.That(t, len(sec), test.ShouldBeGreaterThanOrEqualTo, 2)
		test.That(t, sectionFits(sec, opts, 0), test.ShouldBeTrue)
	}
	// Sections are contiguous and share their join waypoint, so the route
	// stays continuous with no gap and no duplicated leg.
	test.That(t, secs[0][0], test.ShouldResemble, points[0])
	test.That(t, secs[len(secs)-1][len(secs[len(secs)-1])-1], test.ShouldResemble, points[len(points)-1])
	for i := 1; i < len(secs); i++ {
		prev := secs[i-1]
		test.That(t, secs[i][0], test.ShouldResemble, prev[len(prev)-1])
	}
}

func TestShortRouteStaysOneSection(t *testing.T) {
	opts := DefaultAutoRouteOptions(2)
	secs, err := sectionsForResolution([]RoutePoint{westPoint, eastPoint}, opts, 0)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(secs), test.ShouldEqual, 1)
	test.That(t, len(secs[0]), test.ShouldEqual, 2)
}

func TestSingleOversizedLegNamesItself(t *testing.T) {
	// One leg too long to resolve can't be split any further, so the error has
	// to say which leg — telling the operator "the route is too long" leaves
	// them with nothing to act on.
	opts := DefaultAutoRouteOptions(2)
	points := []RoutePoint{
		{Lat: 41.0, Lng: -71.5},
		{Lat: 41.1, Lng: -71.4},
		{Lat: 44.0, Lng: -67.0}, // a ~250 nm jump
	}
	_, err := sectionsForResolution(points, opts, 0)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "leg 2 of 2")
	test.That(t, err.Error(), test.ShouldContainSubstring, "intermediate waypoint")
}

func TestMergeRouteResultsStitchesSections(t *testing.T) {
	a := 3.0
	b := 1.5
	original := []RoutePoint{{Lat: 41.0, Lng: -71.5}, {Lat: 41.0, Lng: -71.4}, {Lat: 41.0, Lng: -71.3}}
	parts := []*AutoRouteResult{
		{
			Waypoints:      []RoutePoint{original[0], {Lat: 41.01, Lng: -71.45}, original[1]},
			MinDepthMeters: &a,
			CellSizeMeters: 40, GridWidth: 10, GridHeight: 10,
			BBox:         [4]float64{-71.6, 40.9, -71.3, 41.1},
			FeatureCount: 100,
			SnappedStart: true,
			Warnings:     []string{"start moved to the nearest navigable water"},
		},
		{
			Waypoints:      []RoutePoint{original[1], original[2]},
			MinDepthMeters: &b,
			CellSizeMeters: 90, GridWidth: 20, GridHeight: 20,
			BBox:           [4]float64{-71.5, 40.8, -71.2, 41.2},
			FeatureCount:   50,
			CrossedUnknown: true,
			SnappedEnd:     true,
			Warnings:       []string{"start moved to the nearest navigable water", "thinned"},
		},
	}
	res := mergeRouteResults(parts, original)

	// The shared join waypoint appears once, not twice.
	test.That(t, len(res.Waypoints), test.ShouldEqual, 4)
	test.That(t, res.Sections, test.ShouldEqual, 2)
	// Worst case wins for anything safety-relevant.
	test.That(t, *res.MinDepthMeters, test.ShouldAlmostEqual, 1.5, 0.001)
	test.That(t, res.CrossedUnknown, test.ShouldBeTrue)
	test.That(t, res.CellSizeMeters, test.ShouldAlmostEqual, 90.0, 0.001)
	// Snapping is reported for the route's real ends only.
	test.That(t, res.SnappedStart, test.ShouldBeTrue)
	test.That(t, res.SnappedEnd, test.ShouldBeTrue)
	// Warnings are unioned, not repeated per section.
	test.That(t, len(res.Warnings), test.ShouldEqual, 2)
	test.That(t, res.FeatureCount, test.ShouldEqual, 150)
	// The bbox covers every section.
	test.That(t, res.BBox, test.ShouldResemble, [4]float64{-71.6, 40.8, -71.2, 41.2})
	test.That(t, res.DirectMeters, test.ShouldAlmostEqual, pathDistanceM(original), 1.0)
}

func TestWideDepthRangeIsUnchartedNotShoal(t *testing.T) {
	// A coarse cell charting one huge 0-18.2 m area says nothing about the
	// depth at a point. Reading its DRVAL1 as "0 m" marks open water
	// unnavigable — the bug that made the band ceiling look impossible.
	wide := areaFeature("coarse", "DEPARE", 80000,
		map[string]any{"DRVAL1": 0.0, "DRVAL2": 18.2}, ring(-71.6, 40.8, -71.2, 41.2))
	_, ok := depareKeyDepth(wide)
	test.That(t, ok, test.ShouldBeFalse)

	// A real charted shoal has a narrow range and still blocks.
	shoal := areaFeature("shoal", "DEPARE", 20000,
		map[string]any{"DRVAL1": 0.0, "DRVAL2": 2.0}, ring(-71.5, 40.9, -71.4, 41.0))
	d, ok := depareKeyDepth(shoal)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, d, test.ShouldAlmostEqual, 0.0, 0.001)

	// And a route over water charted only by that coarse area is passable,
	// flagged as crossing uncharted depth rather than refused.
	res, err := planTestRoute(t, []*mongoFeature{wide}, westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res.CrossedUnknown, test.ShouldBeTrue)
}

func TestRoutingUsageBandMatchesTheGrid(t *testing.T) {
	// Fine grid, short leg: take every band, including berth-level detail.
	test.That(t, routingUsageBand(20), test.ShouldEqual, 0)
	// Normal coastal leg: approach scale and coarser.
	test.That(t, routingUsageBand(80), test.ShouldEqual, 4)
	// Long offshore leg: coastal scale and coarser, where the measured payload
	// difference is the whole reason the leg is plannable.
	test.That(t, routingUsageBand(200), test.ShouldEqual, 3)
}
