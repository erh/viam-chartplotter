package render

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.viam.com/rdk/logging"
	"go.viam.com/test"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

// Live routing against a seeded Mongo. The synthetic tests elsewhere in this
// package pin the router's rules; these pin the answers it gives on the real
// chart, which is where the rules have repeatedly turned out to mean something
// other than what they said.
//
//	MONGO_URI=mongodb://erh-23big:27017 go test ./render -run TestLiveRoute -count=1
//
// Skips cleanly with no Mongo.
func liveRenderer(t *testing.T) (*ENCRenderer, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("live routing test skipped in short mode")
	}
	mongoURI := envOrDefault("MONGO_URI", "mongodb://erh-23big:27017")
	mongoDB := envOrDefault("MONGO_DB", "osm")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Skipf("mongo connect (%s): %v — skipping live routing test", mongoURI, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("mongo ping (%s): %v — skipping live routing test", mongoURI, err)
	}
	db := client.Database(mongoDB)

	r := NewENCRenderer(logging.NewTestLogger(t))
	r.SetNOAACollection(noaa.OpenCollection(db))
	r.SetOSMCollections(osmtiler.OpenOSMCollections(db))
	navColl := noaa.OpenNavGridCollection(db)
	if err := noaa.EnsureNavGridIndexes(context.Background(), navColl); err != nil {
		t.Logf("navgrid index: %v (continuing)", err)
	}
	r.SetNavGridCollection(navColl)
	return r, func() { _ = client.Disconnect(context.Background()) }
}

// liveRoute plans a route and fails the test if it cannot.
func liveRoute(t *testing.T, r *ENCRenderer, from, to RoutePoint) *AutoRouteResult {
	t.Helper()
	res, err := r.AutoRoute(from, to, DefaultAutoRouteOptions(6/feetPerMetre))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(res.Waypoints), test.ShouldBeGreaterThanOrEqualTo, 2)
	return res
}

func routeNM(res *AutoRouteResult) float64 { return res.DistanceMeters / 1852 }

// waypointsWithin counts the returned waypoints inside a lat/lon box.
func waypointsWithin(res *AutoRouteResult, minLat, minLon, maxLat, maxLon float64) int {
	n := 0
	for _, w := range res.Waypoints {
		if w.Lat >= minLat && w.Lat <= maxLat && w.Lng >= minLon && w.Lng <= maxLon {
			n++
		}
	}
	return n
}

// TestLiveRouteCapeCodCanal pins the narrow-passage case.
//
// The canal is about 146 m wide — narrower than the cell of any grid a 30 nm
// leg can afford — so it exists to the router only because OSM's canal
// centreline is stamped into the grid as a connected channel. Every earlier
// attempt at this route went round the outside of Cape Cod or refused
// outright, and the difference is 33 nm against roughly 130. The depth bound
// is here because the fix that made the canal passable (treating a charted
// zero in a maintained channel as "unspecified") is exactly the kind of change
// that could let the router through water it should not use.
func TestLiveRouteCapeCodCanal(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	res := liveRoute(t, r,
		RoutePoint{Lat: 41.5124, Lng: -70.7971}, // Buzzards Bay
		RoutePoint{Lat: 41.9473, Lng: -70.4121}) // Cape Cod Bay
	t.Logf("Buzzards Bay -> Cape Cod Bay: %.1f nm, %d waypoints", routeNM(res), len(res.Waypoints))

	// Through the canal, not round the Cape: the outside passage is ~130 nm.
	test.That(t, routeNM(res), test.ShouldBeLessThan, 45.0)
	test.That(t, waypointsWithin(res, 41.70, -70.64, 41.80, -70.48), test.ShouldBeGreaterThanOrEqualTo, 3)

	// And in water the boat can use.
	test.That(t, res.MinDepthMeters, test.ShouldNotBeNil)
	test.That(t, *res.MinDepthMeters, test.ShouldBeGreaterThanOrEqualTo, res.SafeDepthMeters)
}

// TestLiveRouteStaysOffLand is the safety test, and the only one here that
// checks the route the way a plotter would draw it rather than the way the
// planner imagined it.
//
// A route is only as accurate as the grid it was planned on. The Cape Cod
// Canal is 146 m wide and a 33 nm leg gets a 136 m grid, so the planner put
// eight of this transit's legs across the banks while believing every one of
// them was in the channel — the smoother could not catch it, because it was
// consulting the same grid. Checking against the finest charted tiles is the
// only way this is visible at all.
func TestLiveRouteStaysOffLand(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	for _, tc := range []struct {
		name     string
		from, to RoutePoint
	}{
		{"Cape Cod Canal", RoutePoint{Lat: 41.5124, Lng: -70.7971}, RoutePoint{Lat: 41.9473, Lng: -70.4121}},
		{"Newport to Block Island Sound", RoutePoint{Lat: 41.47, Lng: -71.33}, RoutePoint{Lat: 41.30, Lng: -71.45}},
		{"Portland to Portsmouth", RoutePoint{Lat: 43.6591, Lng: -70.2568}, RoutePoint{Lat: 43.05, Lng: -70.71}},
		{"New York to Portland", RoutePoint{Lat: 40.74704, Lng: -74.00937}, RoutePoint{Lat: 43.6151, Lng: -70.1613}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res := liveRoute(t, r, tc.from, tc.to)
			bad := r.legsOverLand(res.Waypoints)
			for _, i := range bad {
				t.Errorf("leg %d (%.5f,%.5f -> %.5f,%.5f) crosses charted land",
					i, res.Waypoints[i].Lat, res.Waypoints[i].Lng,
					res.Waypoints[i+1].Lat, res.Waypoints[i+1].Lng)
			}
			t.Logf("%.1f nm, %d waypoints, %d legs over land", routeNM(res), len(res.Waypoints), len(bad))
		})
	}
}

// TestLiveRouteOffshoreLegStaysStraight pins the cost model.
//
// Charted depth runs out offshore, so penalising every uncharted cell made the
// open sea cost twice the surveyed coastal strip and the router hugged the
// coast to stay where the survey was — adding miles to every passage while
// looking perfectly reasonable leg by leg. A distance bound against the great
// circle is the only thing that catches that shape of bug.
func TestLiveRouteOffshoreLegStaysStraight(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	from := RoutePoint{Lat: 42.3400, Lng: -70.9600} // Boston approaches
	to := RoutePoint{Lat: 43.6151, Lng: -70.1613}   // Portland
	res := liveRoute(t, r, from, to)

	direct := haversineMeters(from.Lat, from.Lng, to.Lat, to.Lng) / 1852
	over := routeNM(res) / direct
	t.Logf("Boston -> Portland: %.1f nm against %.1f nm great circle (%.0f%% over), %d waypoints",
		routeNM(res), direct, 100*(over-1), len(res.Waypoints))
	test.That(t, over, test.ShouldBeLessThan, 1.10)
}

// TestLiveRouteConstrainedWaterIsNotZigzagged pins resolution.
//
// This stretch of Vineyard Sound comes back as three clean waypoints when it
// is planned on its own at 20 m cells, and as a four-mark zigzag — turning
// +41, +35, -52, +42 in five miles — when it falls inside a section planned at
// 348 m. Those marks are not smoothing artefacts and cannot be smoothed away:
// at that resolution the straight line between them really is blocked, so the
// router is right to keep them and the grid is wrong to be that coarse.
func TestLiveRouteConstrainedWaterIsNotZigzagged(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	res := liveRoute(t, r,
		RoutePoint{Lat: 41.3687, Lng: -70.8792},
		RoutePoint{Lat: 41.4498, Lng: -70.8491})
	t.Logf("Vineyard Sound: %.2f nm, %d waypoints, cell %.0f m",
		routeNM(res), len(res.Waypoints), res.CellSizeMeters)
	test.That(t, len(res.Waypoints), test.ShouldBeLessThanOrEqualTo, 5)
}

// TestLiveRoutesAllPlan is the regression net.
//
// Twice this session a change that improved one route silently stopped another
// from routing at all — a harbour walled in by a detail mismatch, a canal
// closed by a depth rule. Each of these has been broken and fixed at least
// once; none of them may break again quietly.
func TestLiveRoutesAllPlan(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	for _, tc := range []struct {
		name     string
		from, to RoutePoint
		maxNM    float64
	}{
		{"Newport to Block Island Sound", RoutePoint{Lat: 41.47, Lng: -71.33}, RoutePoint{Lat: 41.30, Lng: -71.45}, 25},
		{"Buzzards Bay to Cape Cod Bay", RoutePoint{Lat: 41.5124, Lng: -70.7971}, RoutePoint{Lat: 41.9473, Lng: -70.4121}, 45},
		{"Portland to Boston", RoutePoint{Lat: 43.6591, Lng: -70.2568}, RoutePoint{Lat: 42.34, Lng: -70.96}, 110},
		{"Portland to Portsmouth", RoutePoint{Lat: 43.6591, Lng: -70.2568}, RoutePoint{Lat: 43.05, Lng: -70.71}, 60},
		{"Chelsea Piers to Provincetown", RoutePoint{Lat: 40.7469, Lng: -74.009}, RoutePoint{Lat: 42.05, Lng: -70.18}, 250},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res := liveRoute(t, r, tc.from, tc.to)
			t.Logf("%.1f nm, %d waypoints", routeNM(res), len(res.Waypoints))
			test.That(t, routeNM(res), test.ShouldBeLessThan, tc.maxNM)
		})
	}
}

// TestLiveRouteNewYorkToPortland is the passage this router was built for, and
// the one that has exposed most of its bugs: out of the Hudson, east along the
// Sound or the outside of Long Island, through the Cape Cod Canal, then up the
// Gulf of Maine.
//
// The distance bound is the point of the test. A router that hugs the coast,
// takes the outside of Long Island when the Sound is open, or wanders around a
// coarse grid's artefacts still returns a perfectly valid route — just a much
// longer one — so correctness here is measured in miles, not in whether an
// answer came back at all.
//
// The bound is deliberately close to the current result rather than generous:
// this passage found the restricted-area pricing bug that was sending every
// New York departure round the outside of Long Island, and a loose bound would
// not have. Expect it to fail on any change that costs more than a mile or two
// here — that is what it is for.
func TestLiveRouteNewYorkToPortland(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	start := RoutePoint{Lat: 40.74704, Lng: -74.00937} // Chelsea Piers
	end := RoutePoint{Lat: 43.6151, Lng: -70.1613}     // Portland, Maine

	res, err := r.AutoRoute(start, end, DefaultAutoRouteOptions(6/feetPerMetre))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(res.Waypoints), test.ShouldBeGreaterThanOrEqualTo, 2)

	nm := res.DistanceMeters / 1852
	t.Logf("New York -> Portland: %.1f nm, %d waypoints, %d sections, coarsest grid %.0f m",
		nm, len(res.Waypoints), res.Sections, res.CellSizeMeters)
	test.That(t, nm, test.ShouldBeLessThan, 295.0)

	// The route has to actually start and finish where it was asked to.
	test.That(t, haversineMeters(res.Waypoints[0].Lat, res.Waypoints[0].Lng, start.Lat, start.Lng),
		test.ShouldBeLessThan, 2000.0)
	last := res.Waypoints[len(res.Waypoints)-1]
	test.That(t, haversineMeters(last.Lat, last.Lng, end.Lat, end.Lng), test.ShouldBeLessThan, 2000.0)

	// And it has to stay in water the boat can use.
	if res.MinDepthMeters != nil {
		test.That(t, *res.MinDepthMeters, test.ShouldBeGreaterThanOrEqualTo, 0.0)
	}
}
