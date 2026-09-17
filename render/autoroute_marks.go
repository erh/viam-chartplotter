package render

import (
	"math"
	"sort"

	"github.com/beetlebugorg/s57/pkg/s57"
)

// ---------------------------------------------------------------------------
// Following the channel markers.
//
// The depth model in autoroute.go routes on the water: it knows where it is
// deep and where it is not, and among safe routes it prefers the deep one.
// That is not the same as following the buoys. A skipper coming into a harbour
// stays between the red and the green not because the water outside them is
// necessarily too thin, but because the marks are the local authority on where
// the channel is — they carry the dredging, the tide, the shifting bar and the
// traffic separation that a DEPARE polygon compiled years ago does not.
//
// So this is a preference the operator turns on, and it is expressed as a cost
// OUTSIDE the channel rather than a discount inside it. Two reasons. The A*
// heuristic is straight-line metres at multiplier 1, so a multiplier below 1
// would make it inadmissible and the "optimal" route would stop being optimal.
// And a discount is global — it would pull a route towards any buoy anywhere,
// including ones marking something that is not a channel at all.
//
// The channel is reconstructed from the marks in three steps:
//
//  1. GATES. A port-hand mark and a starboard-hand mark that are each other's
//     nearest opposite-handed neighbour, close enough together to be the two
//     sides of one channel, are a gate: the water you pass between. A
//     safe-water mark (BOYSAW/BCNSAW) is a gate on its own — it means
//     mid-channel — with no width of its own.
//  2. CENTRELINE. Gate midpoints are linked to their nearest neighbours, which
//     for buoys laid along a channel is the gate ahead and the gate astern.
//     The chain of those links is the channel's centreline.
//  3. CORRIDOR. The gates and the links between them are stamped as a band
//     half a gate wide, and a wider band around that is the zone where being
//     outside the channel costs (see gridCost.ChannelMarkerPenalty).
//
// A mark with no partner is ignored entirely. On its own a lateral mark says
// which side to pass it, and without knowing which way you are going that is
// not a channel — penalising the water around a lone buoy would push a route
// away from it in every direction, including down the channel it marks.
// ---------------------------------------------------------------------------

// channelMarkClasses are the S-57 aids to navigation that gate a channel:
// lateral marks, which come in pairs, and safe-water marks, which are
// mid-channel by definition. Cardinal, isolated-danger and special-purpose
// marks are deliberately absent — they say something about a hazard or an
// area, not about where a channel runs.
var channelMarkClasses = []string{"BOYLAT", "BCNLAT", "BOYSAW", "BCNSAW"}

// markSide is which hand of the channel a mark stands on.
type markSide uint8

const (
	markPort      markSide = iota // leave to port inbound: CATLAM 1 or 4
	markStarboard                 // leave to starboard inbound: CATLAM 2 or 3
	markSafeWater                 // mid-channel: a gate all by itself
)

// channelMark is one charted mark, reduced to what the router needs.
type channelMark struct {
	lon, lat float64
	side     markSide
}

// channelGate is the water between a pair of opposite-handed marks — or the
// point a safe-water mark stands on, which is a gate of no width.
type channelGate struct {
	// aLon/aLat and bLon/bLat are the two marks; they coincide for a
	// safe-water mark.
	aLon, aLat float64
	bLon, bLat float64
	// midLon/midLat is the middle of the gate: a point on the channel's
	// centreline. halfWidthM is half the distance between the marks.
	midLon, midLat float64
	halfWidthM     float64
}

// maxGateWidthM is how far apart two lateral marks may be and still be read as
// the two sides of one channel. A ship channel into a commercial port is a few
// hundred metres across; much beyond that and the "pair" is far more likely to
// be two marks on unrelated channels, or the two sides of a harbour that do
// not connect.
const maxGateWidthM = 800

// maxGateSpacingM is how far apart two gates may be and still be read as
// consecutive gates of the same channel. Buoys along a channel are typically a
// quarter to half a mile apart, wider in an open approach; past about a mile
// the link says more about there being nothing else nearby than about the two
// gates belonging together.
const maxGateSpacingM = 2000

// gateChainDegree is how many neighbours each gate links to. Two, because a
// gate in the middle of a channel has exactly two: the one ahead and the one
// astern. Linking only the single nearest leaves a channel of staggered marks
// as a scatter of isolated pairs rather than a line.
const gateChainDegree = 2

// markDedupM is how close two marks must be to be treated as the same buoy.
// The same aid appears in every ENC cell that covers it, at slightly different
// positions per compilation scale, and a duplicate would otherwise compete for
// the pairing its own twin should win.
const markDedupM = 10

// channelMarksFrom reduces a feature list to the charted channel marks in it,
// deduped across overlapping ENC cells and in a deterministic order so the
// same chart always produces the same gates.
func channelMarksFrom(features []*mongoFeature) []channelMark {
	var out []channelMark
	for _, f := range features {
		out = append(out, channelMarksIn(f)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].lon != out[j].lon {
			return out[i].lon < out[j].lon
		}
		if out[i].lat != out[j].lat {
			return out[i].lat < out[j].lat
		}
		return out[i].side < out[j].side
	})
	return dedupeMarks(out)
}

// dedupeMarks drops marks that repeat one already kept, within markDedupM and
// on the same side. The input is sorted by longitude, so a linear scan back
// over the recent neighbours is enough.
func dedupeMarks(marks []channelMark) []channelMark {
	out := marks[:0]
	for _, m := range marks {
		dup := false
		for k := len(out) - 1; k >= 0; k-- {
			// Sorted by longitude: once the longitude gap alone exceeds the
			// threshold, nothing earlier can be closer.
			if (m.lon-out[k].lon)*metresPerDegreeLat*clampCosLat(m.lat) > markDedupM {
				break
			}
			if out[k].side == m.side &&
				haversineMeters(m.lat, m.lon, out[k].lat, out[k].lon) <= markDedupM {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, m)
		}
	}
	return out
}

// channelMarksIn reads one feature as channel marks, and returns nothing for
// anything that is not one — a wrong class, a non-point geometry, or a lateral
// mark whose CATLAM does not say which hand of the channel it stands on.
//
// A point feature can carry several coordinates (featureFromDoc maps GeoJSON
// MultiPoint onto one), so every one of them is a mark.
func channelMarksIn(f encFeature) []channelMark {
	geom := f.Geometry()
	if geom.Type != s57.GeometryTypePoint {
		return nil
	}

	var side markSide
	switch f.ObjectClass() {
	case "BOYSAW", "BCNSAW":
		side = markSafeWater
	case "BOYLAT", "BCNLAT":
		v, ok := f.Attribute("CATLAM")
		if !ok {
			return nil
		}
		cat := numAttr(v)
		if math.IsNaN(cat) {
			return nil
		}
		// S-57 CATLAM: 1 port-hand, 2 starboard-hand, 3 preferred channel to
		// starboard — which you leave to port, so the mark itself is port-hand
		// — and 4 preferred channel to port, which is starboard-hand.
		switch int(cat) {
		case 1, 3:
			side = markPort
		case 2, 4:
			side = markStarboard
		default:
			return nil
		}
	default:
		return nil
	}

	out := make([]channelMark, 0, len(geom.Coordinates))
	for _, c := range geom.Coordinates {
		if len(c) < 2 {
			continue
		}
		out = append(out, channelMark{lon: c[0], lat: c[1], side: side})
	}
	return out
}

// channelGates pairs the marks into gates. A lateral pair has to be mutual —
// each the other's nearest opposite-handed mark — so one buoy at a junction
// cannot claim every mark around it, and the answer does not depend on the
// order the chart happened to arrive in. The grid is consulted only to reject
// a pair whose gate crosses charted land, which is the cheap way to tell a
// channel from two marks that merely look close on a chart.
func channelGates(g *navGrid, marks []channelMark) []channelGate {
	nearest := make([]int, len(marks))
	for i, m := range marks {
		nearest[i] = -1
		if m.side == markSafeWater {
			continue
		}
		best := math.Inf(1)
		consider := func(j int) {
			o := marks[j]
			if o.side == markSafeWater || o.side == m.side {
				return
			}
			d := haversineMeters(m.lat, m.lon, o.lat, o.lon)
			if d > maxGateWidthM || d >= best {
				return
			}
			if g.segmentCrossesLand(m.lon, m.lat, o.lon, o.lat) {
				return
			}
			best, nearest[i] = d, j
		}
		forEachMarkWithin(marks, i, maxGateWidthM, consider)
	}

	var gates []channelGate
	for i, m := range marks {
		if m.side == markSafeWater {
			gates = append(gates, channelGate{
				aLon: m.lon, aLat: m.lat,
				bLon: m.lon, bLat: m.lat,
				midLon: m.lon, midLat: m.lat,
			})
			continue
		}
		j := nearest[i]
		// Mutual, and counted once: only the lower index emits the gate.
		if j < 0 || nearest[j] != i || j < i {
			continue
		}
		o := marks[j]
		gates = append(gates, channelGate{
			aLon: m.lon, aLat: m.lat,
			bLon: o.lon, bLat: o.lat,
			midLon: (m.lon + o.lon) / 2, midLat: (m.lat + o.lat) / 2,
			halfWidthM: haversineMeters(m.lat, m.lon, o.lat, o.lon) / 2,
		})
	}
	return gates
}

// forEachMarkWithin calls fn for every mark that could be within radiusM of
// marks[i], walking out from i in both directions and stopping as soon as the
// longitude gap alone exceeds the radius.
//
// The marks are sorted by longitude, so this turns what would be a quadratic
// sweep over every buoy in the corridor — a coastal one holds thousands — into
// a scan of the handful that are actually nearby.
func forEachMarkWithin(marks []channelMark, i int, radiusM float64, fn func(j int)) {
	lonSpan := func(j int) float64 {
		return math.Abs(marks[j].lon-marks[i].lon) * metresPerDegreeLat * clampCosLat(marks[i].lat)
	}
	for j := i - 1; j >= 0 && lonSpan(j) <= radiusM; j-- {
		fn(j)
	}
	for j := i + 1; j < len(marks) && lonSpan(j) <= radiusM; j++ {
		fn(j)
	}
}

// gateLink joins two gates that are consecutive along one channel.
type gateLink struct{ a, b int }

// chainGates links each gate to its nearest few neighbours, which along a
// buoyed channel are the gate ahead and the gate astern. Links are deduped, so
// the usual mutual nearest pair appears once, and a link that crosses charted
// land is dropped — two channels either side of a point are near each other
// without being the same channel.
//
// gates must be sorted by midLon (sortGates), which is what lets the scan stop
// at the longitude window instead of comparing every gate with every other.
// The returned links index into that slice.
func chainGates(g *navGrid, gates []channelGate) []gateLink {
	type cand struct {
		j int
		d float64
	}
	seen := make(map[gateLink]bool)
	var out []gateLink
	var near []cand
	for i := range gates {
		a := gates[i]
		near = near[:0]
		lonSpan := func(j int) float64 {
			return math.Abs(gates[j].midLon-a.midLon) * metresPerDegreeLat * clampCosLat(a.midLat)
		}
		add := func(j int) {
			d := haversineMeters(a.midLat, a.midLon, gates[j].midLat, gates[j].midLon)
			if d <= maxGateSpacingM {
				near = append(near, cand{j, d})
			}
		}
		for j := i - 1; j >= 0 && lonSpan(j) <= maxGateSpacingM; j-- {
			add(j)
		}
		for j := i + 1; j < len(gates) && lonSpan(j) <= maxGateSpacingM; j++ {
			add(j)
		}
		sort.Slice(near, func(x, y int) bool {
			if near[x].d != near[y].d {
				return near[x].d < near[y].d
			}
			return near[x].j < near[y].j
		})
		for k := 0; k < len(near) && k < gateChainDegree; k++ {
			j := near[k].j
			link := gateLink{a: min(i, j), b: max(i, j)}
			if seen[link] {
				continue
			}
			b := gates[j]
			if g.segmentCrossesLand(a.midLon, a.midLat, b.midLon, b.midLat) {
				continue
			}
			seen[link] = true
			out = append(out, link)
		}
	}
	return out
}

// sortGates orders gates west to east, which is the order chainGates' windowed
// scan relies on.
func sortGates(gates []channelGate) {
	sort.Slice(gates, func(i, j int) bool {
		if gates[i].midLon != gates[j].midLon {
			return gates[i].midLon < gates[j].midLon
		}
		return gates[i].midLat < gates[j].midLat
	})
}

// stampChannelMarkers paints the buoyed channel onto the grid and returns how
// many gates it found. Cells inside the channel get cellMarkedChannel; cells
// within influenceM of it, inside or out, get cellMarkerZone. finalize() reads
// the difference: in the zone but not in the channel is what costs.
//
// Nothing here blocks or unblocks a cell. A marker corridor is an opinion
// about where the channel is, and an opinion must not be able to open water
// the depth model closed, nor close water it left open.
func stampChannelMarkers(g *navGrid, marks []channelMark, influenceM float64) int {
	if len(marks) == 0 {
		return 0
	}
	gates := channelGates(g, marks)
	if len(gates) == 0 {
		return 0
	}
	sortGates(gates)
	links := chainGates(g, gates)
	g.markerGates += len(gates)

	inside := func(i int) { g.mark(i, cellMarkedChannel) }
	zone := func(i int) { g.mark(i, cellMarkerZone) }

	// half is the corridor's reach either side of a gate or a link. The order
	// the two bands are stamped in does not matter: a cell can carry both
	// flags, and a cell any gate puts inside the channel is inside it.
	//
	// It is deliberately generous. The corridor is a capsule, so it reaches
	// half a gate's width past the marks themselves, and never narrower than a
	// cell — a corridor thinner than the raster would read as its own edge and
	// price the channel it is meant to prefer. Erring wide costs a little of
	// the pull toward mid-channel; erring narrow would turn the channel into
	// somewhere to avoid.
	half := func(gt channelGate) float64 {
		return math.Max(gt.halfWidthM, 0.75*g.cellSizeM())
	}
	for _, gt := range gates {
		h := half(gt)
		g.stampCorridor(gt.aLon, gt.aLat, gt.bLon, gt.bLat, h+influenceM, zone)
		g.stampCorridor(gt.aLon, gt.aLat, gt.bLon, gt.bLat, h, inside)
	}
	for _, l := range links {
		a, b := gates[l.a], gates[l.b]
		h := (half(a) + half(b)) / 2
		g.stampCorridor(a.midLon, a.midLat, b.midLon, b.midLat, h+influenceM, zone)
		g.stampCorridor(a.midLon, a.midLat, b.midLon, b.midLat, h, inside)
	}
	return len(gates)
}
