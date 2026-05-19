package osmtiler

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

// relationDesc is the slimmed-down view of a multipolygon relation we
// keep across the two-pass PBF load. We only care about the relation's
// classification + name and the IDs of its outer-role member ways;
// member-way geometry is resolved during the second pass.
type relationDesc struct {
	Class     Class
	Name      string
	OuterWays []osm.WayID
}

// scanMultipolygonRelations performs the first PBF pass: it walks
// relations only (SkipNodes / SkipWays at the encoded level) and
// collects every multipolygon relation whose classification is an
// area class we want to draw. Returns the descriptors plus the set of
// member way IDs whose geometry the second pass must remember.
func scanMultipolygonRelations(ctx context.Context, path string) ([]relationDesc, map[osm.WayID]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := osmpbf.New(ctx, f, runtime.NumCPU())
	sc.SkipNodes = true
	sc.SkipWays = true
	defer sc.Close()

	var out []relationDesc
	memberWays := make(map[osm.WayID]struct{})

	for sc.Scan() {
		r, ok := sc.Object().(*osm.Relation)
		if !ok {
			continue
		}
		if r.Tags.Find("type") != "multipolygon" {
			continue
		}
		class := Classify(r.Tags)
		if class == ClassSkip || !isAreaClass(class) {
			continue
		}
		rd := relationDesc{
			Class: class,
			Name:  r.Tags.Find("name"),
		}
		for _, m := range r.Members {
			if m.Type != osm.TypeWay {
				continue
			}
			// Empty role is sometimes used by older data instead of
			// "outer"; treat it as outer. Inner rings (holes) are
			// dropped in v0.2a — we drop water anyway, and visible
			// holes in parks (e.g. lakes) get the same transparent
			// behavior we want.
			if m.Role != "outer" && m.Role != "" {
				continue
			}
			wid := osm.WayID(m.Ref)
			rd.OuterWays = append(rd.OuterWays, wid)
			memberWays[wid] = struct{}{}
		}
		if len(rd.OuterWays) > 0 {
			out = append(out, rd)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("relations pass: %w", err)
	}
	return out, memberWays, nil
}

// assembleRings stitches the supplied member-way coords into closed
// rings via greedy endpoint matching. Each ring is emitted with its
// starting point repeated at the end (Coords[0] == Coords[len-1]).
// Partial rings — when the member ways don't actually form a closed
// loop — are dropped silently; OSM data has occasional broken
// multipolygons and we'd rather omit than render garbage.
func assembleRings(wayIDs []osm.WayID, coords map[osm.WayID][]LonLat) [][]LonLat {
	available := make(map[osm.WayID]bool, len(wayIDs))
	for _, id := range wayIDs {
		if c, ok := coords[id]; ok && len(c) >= 2 {
			available[id] = true
		}
	}

	var rings [][]LonLat
	for len(available) > 0 {
		// Seed a new ring with any remaining way.
		var startID osm.WayID
		for id := range available {
			startID = id
			break
		}
		delete(available, startID)
		ring := append([]LonLat(nil), coords[startID]...)

		// A single closed way is already a ring on its own.
		if len(ring) > 2 && ring[0] == ring[len(ring)-1] {
			rings = append(rings, ring)
			continue
		}

		// Walk endpoint-to-endpoint, picking any way that connects.
		for {
			tail := ring[len(ring)-1]
			var (
				pickID   osm.WayID
				reversed bool
				found    bool
			)
			for id := range available {
				c := coords[id]
				if c[0] == tail {
					pickID, reversed, found = id, false, true
					break
				}
				if c[len(c)-1] == tail {
					pickID, reversed, found = id, true, true
					break
				}
			}
			if !found {
				break
			}
			delete(available, pickID)
			c := coords[pickID]
			if reversed {
				for i := len(c) - 2; i >= 0; i-- {
					ring = append(ring, c[i])
				}
			} else {
				ring = append(ring, c[1:]...)
			}
			if ring[0] == ring[len(ring)-1] {
				rings = append(rings, ring)
				break
			}
		}
		// Unclosed ring: dropped.
	}
	return rings
}
