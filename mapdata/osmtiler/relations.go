package osmtiler

import "github.com/paulmach/osm"

// AssembleOuterRings stitches the supplied member-way coords into
// closed rings via greedy endpoint matching. Each ring is emitted
// with its starting point repeated at the end (Coords[0] ==
// Coords[len-1]). Partial rings — when the member ways don't form a
// closed loop — are dropped silently; OSM data has occasional broken
// multipolygons and we'd rather omit than render garbage.
//
// Used by the offline ingest tool (cmd/osmtools) to convert OSM
// multipolygon relations into renderable polygon features at upsert
// time. The runtime renderer queries those pre-built polygons from
// MongoDB and never sees the constituent member ways.
func AssembleOuterRings(wayIDs []osm.WayID, coords map[osm.WayID][]LonLat) [][]LonLat {
	// Both loops below walk `ordered` — the caller's own member order — rather
	// than ranging over `available`. Go randomises map iteration, so seeding a
	// ring from the map would emit the same loop starting at a different vertex
	// from run to run: geometrically identical, but a different document every
	// time the ingest re-reads the relation. `available` is now only the
	// membership test.
	ordered := make([]osm.WayID, 0, len(wayIDs))
	available := make(map[osm.WayID]bool, len(wayIDs))
	for _, id := range wayIDs {
		if available[id] {
			continue // a way listed twice is still one way
		}
		if c, ok := coords[id]; ok && len(c) >= 2 {
			available[id] = true
			ordered = append(ordered, id)
		}
	}

	var rings [][]LonLat
	for _, startID := range ordered {
		if !available[startID] {
			continue // already consumed by an earlier ring
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
			for _, id := range ordered {
				if !available[id] {
					continue
				}
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
