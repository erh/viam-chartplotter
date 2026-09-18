package render

import (
	"testing"

	"go.viam.com/test"

	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

// TestFollowChannelFollowsAWaterway pins the counterpart to
// TestAutoRouteFollowsTheChannelMarkers: a channel given not by buoy pairs but
// by a charted navigable waterway (a canal, an inlet, a river). Sparse beacons
// gate too few points to chain into a channel — Manasquan Inlet charts three
// gates two miles apart — so following the markers there did nothing. The same
// water is one continuous OSM waterway line, and following THAT is what makes
// "follow the channel" bend the route onto it.
func TestFollowChannelFollowsAWaterway(t *testing.T) {
	// A waterway 222 m north of the rhumb line, in water deep and featureless
	// everywhere, so nothing but the waterway can bend the route.
	scene := []*mongoFeature{deepEverywhere(10)}
	ways := []osmtiler.Feature{{
		Kind: osmtiler.GeomLine,
		Coords: []osmtiler.LonLat{
			{Lon: -71.50, Lat: 41.002},
			{Lon: -71.45, Lat: 41.002},
			{Lon: -71.40, Lat: 41.002},
		},
	}}

	plan := func(opts AutoRouteOptions) *AutoRouteResult {
		opts.normalize(haversineMeters(westPoint.Lat, westPoint.Lng, eastPoint.Lat, eastPoint.Lng))
		bbox := routeBBox(westPoint, eastPoint, opts.CorridorPadM)
		res, err := planRouteViaWithWays(scene, ways, bbox,
			[]RoutePoint{westPoint, eastPoint}, allUserPlaced(2), opts)
		test.That(t, err, test.ShouldBeNil)
		return res
	}

	// Off, the route runs straight along the rhumb line.
	plain := plan(testOptions(2))
	test.That(t, maxWaypointLat(plain), test.ShouldBeLessThan, 41.001)

	// On, it climbs onto the waterway — and only a little longer for it.
	opts := testOptions(2)
	opts.FollowChannelMarkers = true
	steered := plan(opts)
	test.That(t, maxWaypointLat(steered), test.ShouldBeGreaterThan, 41.0013)
	test.That(t, steered.DistanceMeters, test.ShouldBeGreaterThan, plain.DistanceMeters)
	test.That(t, steered.DistanceMeters, test.ShouldBeLessThan, plain.DistanceMeters*1.05)
	// There was a channel to follow, so no caution about there being none.
	for _, w := range steered.Warnings {
		test.That(t, w, test.ShouldNotContainSubstring, "no charted channel markers")
	}
}

// TestFollowChannelTakesADetourOntoTheChannel pins the point of the option: a
// channel that is a real detour is still taken. Following the buoys out of New
// York means running the ship channel southeast and back, not cutting straight
// across the bay, so the off-channel cost has to buy a genuine deviation, not
// just centre a route already passing nearby.
func TestFollowChannelTakesADetourOntoTheChannel(t *testing.T) {
	// A waterway that doglegs well north of the straight line — 3.3 km off it at
	// the peak, so joining it is ~25% further than the rhumb line. Deep and
	// featureless everywhere else.
	scene := []*mongoFeature{deepEverywhere(10)}
	ways := []osmtiler.Feature{{
		Kind: osmtiler.GeomLine,
		Coords: []osmtiler.LonLat{
			{Lon: -71.50, Lat: 41.000},
			{Lon: -71.45, Lat: 41.030},
			{Lon: -71.40, Lat: 41.000},
		},
	}}

	plan := func(opts AutoRouteOptions) *AutoRouteResult {
		opts.normalize(haversineMeters(westPoint.Lat, westPoint.Lng, eastPoint.Lat, eastPoint.Lng))
		bbox := routeBBox(westPoint, eastPoint, opts.CorridorPadM)
		res, err := planRouteViaWithWays(scene, ways, bbox,
			[]RoutePoint{westPoint, eastPoint}, allUserPlaced(2), opts)
		test.That(t, err, test.ShouldBeNil)
		return res
	}

	// Off, the shortest path is the rhumb line — the dogleg is ignored.
	plain := plan(testOptions(2))
	test.That(t, maxWaypointLat(plain), test.ShouldBeLessThan, 41.005)

	// On, the route detours north onto the channel even though it is longer.
	opts := testOptions(2)
	opts.FollowChannelMarkers = true
	steered := plan(opts)
	test.That(t, maxWaypointLat(steered), test.ShouldBeGreaterThan, 41.02)
	test.That(t, steered.DistanceMeters, test.ShouldBeGreaterThan, plain.DistanceMeters*1.1)
}
