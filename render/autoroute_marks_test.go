package render

import (
	"strings"
	"testing"

	"github.com/beetlebugorg/s57/pkg/s57"
	"go.viam.com/test"
)

// markFeature is a charted aid to navigation: a point feature, plus the CATLAM
// that says which hand of the channel a lateral mark stands on (0 leaves it
// off, which is how a real chart records a mark that gates nothing).
func markFeature(id, class string, catlam int, lon, lat float64) *mongoFeature {
	attrs := map[string]any{}
	if catlam > 0 {
		attrs["CATLAM"] = catlam
	}
	return &mongoFeature{
		id:    id,
		class: class,
		scale: 20000,
		attrs: attrs,
		geom:  s57.Geometry{Type: s57.GeometryTypePoint, Coordinates: [][]float64{{lon, lat}}},
	}
}

// buoyedChannel lays a channel of lateral marks along a line of latitude: a
// port-hand mark north of the centreline and a starboard-hand one south of it,
// repeated every step of longitude.
func buoyedChannel(centreLat, halfWidthDeg, fromLon, toLon, stepLon float64) []*mongoFeature {
	var out []*mongoFeature
	n := 0
	for lon := fromLon; lon <= toLon; lon += stepLon {
		out = append(out,
			markFeature(idFor("port", n), "BOYLAT", 1, lon, centreLat+halfWidthDeg),
			markFeature(idFor("stbd", n), "BOYLAT", 2, lon, centreLat-halfWidthDeg),
		)
		n++
	}
	return out
}

func idFor(prefix string, n int) string {
	return prefix + string(rune('a'+n%26)) + string(rune('a'+n/26))
}

func TestChannelMarksReadTheirSide(t *testing.T) {
	cases := []struct {
		class  string
		catlam int
		want   []markSide
	}{
		{"BOYLAT", 1, []markSide{markPort}},      // port-hand
		{"BOYLAT", 2, []markSide{markStarboard}}, // starboard-hand
		{"BOYLAT", 3, []markSide{markPort}},      // preferred channel to starboard
		{"BOYLAT", 4, []markSide{markStarboard}}, // preferred channel to port
		{"BCNLAT", 1, []markSide{markPort}},
		{"BOYSAW", 0, []markSide{markSafeWater}}, // mid-channel: no CATLAM to read
		{"BCNSAW", 0, []markSide{markSafeWater}},
		{"BCNLAT", 2, []markSide{markStarboard}},
		// A lateral mark with no CATLAM says nothing about which side of the
		// channel it is, and an unknown one is not a guess worth making.
		{"BOYLAT", 0, nil},
		{"BOYLAT", 9, nil},
		// Everything else is about a hazard, an area or a light — not about
		// where a channel runs.
		{"BOYCAR", 0, nil},
		{"BOYISD", 0, nil},
		{"BOYSPP", 0, nil},
		{"BOYINB", 0, nil},
		{"BCNCAR", 0, nil},
		{"BCNSPP", 0, nil},
		{"LIGHTS", 0, nil},
		{"DAYMAR", 0, nil},
	}
	for _, c := range cases {
		got := channelMarksIn(markFeature("m", c.class, c.catlam, -71.45, 41.0))
		test.That(t, len(got), test.ShouldEqual, len(c.want))
		for i := range c.want {
			test.That(t, got[i].side, test.ShouldEqual, c.want[i])
		}
	}
}

func TestChannelMarksIgnoreNonPointGeometry(t *testing.T) {
	// A lateral mark charted as an area is not a position to gate a channel
	// with. (Nothing charts one that way, but the rasteriser must not take a
	// polygon's first vertex for a buoy either.)
	f := areaFeature("wrong", "BOYLAT", 20000, map[string]any{"CATLAM": 1}, ring(-71.46, 40.99, -71.44, 41.01))
	test.That(t, channelMarksIn(f), test.ShouldBeNil)
}

func TestChannelMarksDedupeAcrossCells(t *testing.T) {
	// The same buoy is charted in every ENC cell that covers it. Two readings
	// a few metres apart are one mark, not two competing for the pairing.
	feats := []*mongoFeature{
		markFeature("a", "BOYLAT", 1, -71.45, 41.0),
		markFeature("b", "BOYLAT", 1, -71.450001, 41.000001), // ~0.1 m away
		markFeature("c", "BOYLAT", 1, -71.44, 41.0),          // a different buoy
	}
	test.That(t, len(channelMarksFrom(feats)), test.ShouldEqual, 2)
}

func TestChannelGatesNeedAMutualOppositePair(t *testing.T) {
	g := newNavGrid(-71.55, 40.95, -71.35, 41.05, 40000, 5, 1400)

	// A red and a green 111 m apart: a gate, and its midpoint is between them.
	pair := []channelMark{
		{lon: -71.45, lat: 41.0005, side: markPort},
		{lon: -71.45, lat: 40.9995, side: markStarboard},
	}
	gates := channelGates(g, pair)
	test.That(t, len(gates), test.ShouldEqual, 1)
	test.That(t, gates[0].midLat, test.ShouldAlmostEqual, 41.0, 1e-9)
	test.That(t, gates[0].halfWidthM, test.ShouldBeBetween, 50.0, 62.0)

	// Two marks on the same hand gate nothing — a channel needs both sides.
	sameSide := []channelMark{
		{lon: -71.45, lat: 41.0005, side: markPort},
		{lon: -71.45, lat: 40.9995, side: markPort},
	}
	test.That(t, len(channelGates(g, sameSide)), test.ShouldEqual, 0)

	// Nor does a lone mark. On its own it says which side to pass it, which
	// without a heading is not a channel.
	test.That(t, len(channelGates(g, pair[:1])), test.ShouldEqual, 0)

	// Too far apart to be two sides of one channel (1.1 km at this latitude).
	far := []channelMark{
		{lon: -71.45, lat: 41.005, side: markPort},
		{lon: -71.45, lat: 40.995, side: markStarboard},
	}
	test.That(t, len(channelGates(g, far)), test.ShouldEqual, 0)

	// A safe-water mark is mid-channel by definition: a gate of no width, on
	// its own.
	saw := []channelMark{{lon: -71.45, lat: 41.0, side: markSafeWater}}
	gates = channelGates(g, saw)
	test.That(t, len(gates), test.ShouldEqual, 1)
	test.That(t, gates[0].halfWidthM, test.ShouldEqual, 0.0)
}

func TestChannelGatesDoNotPairAcrossLand(t *testing.T) {
	// Two marks either side of a spit are near each other without being the
	// two sides of anything. Charted land between them is how we tell.
	g := newNavGrid(-71.55, 40.95, -71.35, 41.05, 40000, 5, 1400)
	rasterizeForRouting(g, []*mongoFeature{
		deepEverywhere(10),
		areaFeature("spit", "LNDARE", 20000, nil, ring(-71.4502, 40.9998, -71.4498, 41.0002)),
	}, testOptions(2))

	marks := []channelMark{
		{lon: -71.45, lat: 41.0005, side: markPort},
		{lon: -71.45, lat: 40.9995, side: markStarboard},
	}
	test.That(t, len(channelGates(g, marks)), test.ShouldEqual, 0)
}

func TestStampChannelMarkersMarksTheChannelAndItsEdges(t *testing.T) {
	g := newNavGrid(-71.55, 40.95, -71.35, 41.05, 40000, 5, 1400)
	feats := buoyedChannel(41.0, 0.0005, -71.48, -71.42, 0.008)
	test.That(t, stampChannelMarkers(g, channelMarksFrom(feats), 250), test.ShouldBeGreaterThan, 5)

	flagsAt := func(lon, lat float64) uint16 {
		i, ok := g.cellAt(lon, lat)
		test.That(t, ok, test.ShouldBeTrue)
		return g.flags[i]
	}

	// Mid-channel, between two gates: inside the channel, so no cost.
	mid := flagsAt(-71.45, 41.0)
	test.That(t, mid&cellMarkedChannel, test.ShouldNotEqual, uint16(0))
	test.That(t, mid&cellMarkerZone, test.ShouldNotEqual, uint16(0))

	// 222 m off to the side: in reach of the channel but outside it, which is
	// exactly the water this option makes expensive.
	beside := flagsAt(-71.45, 41.002)
	test.That(t, beside&cellMarkedChannel, test.ShouldEqual, uint16(0))
	test.That(t, beside&cellMarkerZone, test.ShouldNotEqual, uint16(0))

	// Well clear of it: untouched. Open water is not priced by a channel it is
	// nowhere near.
	away := flagsAt(-71.45, 41.02)
	test.That(t, away&(cellMarkedChannel|cellMarkerZone), test.ShouldEqual, uint16(0))

	// Nothing charted here got blocked or unblocked: a marker corridor is an
	// opinion about where the channel is, not a statement about the water.
	test.That(t, mid&(cellLand|cellObstruction|cellChannel), test.ShouldEqual, uint16(0))
}

func TestStampChannelMarkersDoesNothingWithoutGates(t *testing.T) {
	g := newNavGrid(-71.55, 40.95, -71.35, 41.05, 40000, 5, 1400)
	// A line of unpaired port-hand marks: no gates, so no corridor at all.
	var feats []*mongoFeature
	for i := 0; i < 8; i++ {
		feats = append(feats, markFeature(idFor("p", i), "BOYLAT", 1, -71.48+float64(i)*0.008, 41.0))
	}
	test.That(t, stampChannelMarkers(g, channelMarksFrom(feats), 250), test.ShouldEqual, 0)
	for _, f := range g.flags {
		test.That(t, f&(cellMarkedChannel|cellMarkerZone), test.ShouldEqual, uint16(0))
	}
}

func TestAutoRouteFollowsTheChannelMarkers(t *testing.T) {
	// A buoyed channel 222 m north of the rhumb line, in water that is deep
	// and featureless everywhere — so nothing but the marks can bend the
	// route. Off, it runs straight; on, it climbs into the channel.
	scene := func() []*mongoFeature {
		return append([]*mongoFeature{deepEverywhere(10)},
			buoyedChannel(41.002, 0.0005, -71.50, -71.40, 0.008)...)
	}

	plain, err := planTestRoute(t, scene(), westPoint, eastPoint, testOptions(2))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, maxWaypointLat(plain), test.ShouldBeLessThan, 41.001)

	opts := testOptions(2)
	opts.FollowChannelMarkers = true
	steered, err := planTestRoute(t, scene(), westPoint, eastPoint, opts)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, maxWaypointLat(steered), test.ShouldBeGreaterThan, 41.0013)
	// Following the buoys is a detour, not a shortcut — and a short one.
	test.That(t, steered.DistanceMeters, test.ShouldBeGreaterThan, plain.DistanceMeters)
	test.That(t, steered.DistanceMeters, test.ShouldBeLessThan, plain.DistanceMeters*1.05)
	// There were marks to follow, so no caution about their absence.
	for _, w := range steered.Warnings {
		test.That(t, w, test.ShouldNotContainSubstring, "no charted channel markers")
	}
}

func TestAutoRouteSaysWhenThereAreNoMarkersToFollow(t *testing.T) {
	// Asked to follow the buoys in water that has none. The route is still a
	// route — but the operator chose this expecting the marks to shape it, so
	// the result has to say they did not.
	opts := testOptions(2)
	opts.FollowChannelMarkers = true
	res, err := planTestRoute(t, []*mongoFeature{deepEverywhere(10)}, westPoint, eastPoint, opts)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.Join(res.Warnings, "|"), test.ShouldContainSubstring, "no charted channel markers")
}

func TestChannelMarkerClassesAreFetchedOnlyWhenAsked(t *testing.T) {
	// The marks are a per-request preference, so they must not widen the chart
	// query for every other route: the classes are fetched only with the
	// option on.
	off := map[string]bool{}
	for _, c := range autoRouteClasses(AutoRouteOptions{}) {
		off[c] = true
	}
	on := map[string]bool{}
	for _, c := range autoRouteClasses(AutoRouteOptions{FollowChannelMarkers: true}) {
		on[c] = true
	}
	for _, c := range channelMarkClasses {
		test.That(t, off[c], test.ShouldBeFalse)
		test.That(t, on[c], test.ShouldBeTrue)
	}
	// And CATLAM, which is the only attribute that makes a mark readable, is
	// in the projection the query fetches.
	_, ok := RoutingProjection()["attributes.CATLAM"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestChannelMarkerPenaltyIsOptIn(t *testing.T) {
	opts := DefaultAutoRouteOptions(2)
	test.That(t, channelMarkerPenalty(opts), test.ShouldEqual, 0.0)
	opts.FollowChannelMarkers = true
	test.That(t, channelMarkerPenalty(opts), test.ShouldBeGreaterThan, 0.0)
}
