package render

import (
	"testing"

	"go.viam.com/test"
)

// TestLiveRouteManasquanInlet pins a reported bug: a route from Chelsea Piers to
// a point up the Manasquan River, NJ ran over land at the inlet and wandered in
// a tight, backtracking cluster of waypoints near it.
//
// Manasquan Inlet is a narrow dredged passage the coarse grid a 41 nm leg gets
// cannot resolve, and the river channel behind it charts 0.9 m — shoaler than a
// 6 ft safe depth — so the OSM navigable-waterway centreline is the only thing
// that can carry the route through. Two defects broke that: the centreline is
// baked into the nav tiles but lost when they are re-sampled into the routing
// grid (three cells reverted to the 0.9 m shoal and cut the destination off from
// the sea), and even where it survived, stampWaterways left cellChannelDepth set
// so the shoal charted depth still walled it off. With the ways stamped straight
// onto the routing grid and the coincident charted depth treated as unspecified,
// the route reaches the river without crossing land.
func TestLiveRouteManasquanInlet(t *testing.T) {
	r, done := liveRenderer(t)
	defer done()

	res := liveRoute(t, r,
		RoutePoint{Lat: 40.7470, Lng: -74.0087},     // Chelsea Piers
		RoutePoint{Lat: 40.105676, Lng: -74.051427}) // up the Manasquan River
	bad := r.legsOverLand(res.Waypoints)
	for _, i := range bad {
		t.Errorf("leg %d (%.5f,%.5f -> %.5f,%.5f) crosses charted land",
			i, res.Waypoints[i].Lat, res.Waypoints[i].Lng,
			res.Waypoints[i+1].Lat, res.Waypoints[i+1].Lng)
	}
	t.Logf("Chelsea Piers -> Manasquan River: %.1f nm, %d waypoints, %d legs over land",
		routeNM(res), len(res.Waypoints), len(bad))

	// It must actually get up the river to the destination, not stop short at
	// the coast: the last waypoint is on the west (river) side of the inlet.
	last := res.Waypoints[len(res.Waypoints)-1]
	test.That(t, last.Lng, test.ShouldBeLessThan, -74.045)
	test.That(t, last.Lat, test.ShouldBeGreaterThan, 40.10)
}
