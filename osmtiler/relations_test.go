package osmtiler

import (
	"reflect"
	"testing"

	"github.com/paulmach/osm"
)

func pts(coords ...float64) []LonLat {
	out := make([]LonLat, 0, len(coords)/2)
	for i := 0; i+1 < len(coords); i += 2 {
		out = append(out, LonLat{Lon: coords[i], Lat: coords[i+1]})
	}
	return out
}

func TestAssembleRings_SingleClosedWay(t *testing.T) {
	// One way that's already a closed ring → emit as-is.
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1, 0, 1, 0, 0),
	}
	got := AssembleOuterRings([]osm.WayID{1}, coords)
	if len(got) != 1 {
		t.Fatalf("rings=%d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0], coords[1]) {
		t.Fatalf("ring mismatch:\n got=%v\nwant=%v", got[0], coords[1])
	}
}

func TestAssembleRings_TwoWaysHeadToTail(t *testing.T) {
	// Two open ways whose endpoints connect into a single ring.
	//   way 1: (0,0) → (1,0) → (1,1)
	//   way 2: (1,1) → (0,1) → (0,0)
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1),
		2: pts(1, 1, 0, 1, 0, 0),
	}
	got := AssembleOuterRings([]osm.WayID{1, 2}, coords)
	if len(got) != 1 {
		t.Fatalf("rings=%d, want 1", len(got))
	}
	want := pts(0, 0, 1, 0, 1, 1, 0, 1, 0, 0)
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("ring mismatch:\n got=%v\nwant=%v", got[0], want)
	}
}

func TestAssembleRings_ReversedSecondWay(t *testing.T) {
	// way 2 is supplied in the opposite direction; the stitcher must
	// detect that its tail (not head) matches the running ring's tail
	// and append it reversed.
	//   way 1: (0,0) → (1,0) → (1,1)
	//   way 2: (0,0) → (0,1) → (1,1)   (reverse of what we need)
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1),
		2: pts(0, 0, 0, 1, 1, 1),
	}
	got := AssembleOuterRings([]osm.WayID{1, 2}, coords)
	if len(got) != 1 {
		t.Fatalf("rings=%d, want 1", len(got))
	}
	if got[0][0] != got[0][len(got[0])-1] {
		t.Fatalf("ring not closed: first=%v last=%v", got[0][0], got[0][len(got[0])-1])
	}
}

func TestAssembleRings_TwoIndependentRings(t *testing.T) {
	// Two disjoint closed ways → two separate rings.
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1, 0, 1, 0, 0),
		2: pts(10, 10, 11, 10, 11, 11, 10, 11, 10, 10),
	}
	got := AssembleOuterRings([]osm.WayID{1, 2}, coords)
	if len(got) != 2 {
		t.Fatalf("rings=%d, want 2", len(got))
	}
}

func TestAssembleRings_UnclosedDropped(t *testing.T) {
	// A dangling way that can't be stitched into a closed ring is
	// dropped silently (rather than emitting bogus geometry).
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1), // open chain, no partner
	}
	got := AssembleOuterRings([]osm.WayID{1}, coords)
	if len(got) != 0 {
		t.Fatalf("rings=%d, want 0 (unclosed must be dropped)", len(got))
	}
}

func TestAssembleRings_MissingMemberWay(t *testing.T) {
	// A relation referencing a way ID we never loaded (e.g. the way
	// was clipped at the extract boundary) shouldn't blow up.
	coords := map[osm.WayID][]LonLat{
		1: pts(0, 0, 1, 0, 1, 1, 0, 1, 0, 0),
	}
	got := AssembleOuterRings([]osm.WayID{1, 999}, coords)
	if len(got) != 1 {
		t.Fatalf("rings=%d, want 1 (missing member tolerated)", len(got))
	}
}
