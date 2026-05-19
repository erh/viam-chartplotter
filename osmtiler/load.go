package osmtiler

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

// LoadPBF streams an .osm.pbf, applies Classify to every feature, and
// returns the kept features with geometry resolved to lon/lat coords.
//
// Two-pass: first pass walks relations (SkipNodes/SkipWays at the
// encoded level) to discover multipolygon relations and the IDs of
// the outer-role member ways we'll need geometry for. Second pass
// walks nodes + ways, builds the node map, emits features for tagged
// ways, and stashes coords for every way that any relation referenced.
// After the second pass, member-way coords are stitched into rings
// and emitted as polygon features carrying the relation's class.
//
// Memory: nodes map (~24 bytes/node) + member-way coords (~16 bytes
// per vertex × ~hundreds of K member ways at city scale). For a NYC
// extract that's a few hundred MB total — fine. For the planet we'll
// switch to the SQLite-backed pipeline from the plan.
func LoadPBF(ctx context.Context, path string) (*FeatureSet, error) {
	relations, memberWays, err := scanMultipolygonRelations(ctx, path)
	if err != nil {
		return nil, err
	}

	fs, memberCoords, err := loadNodesAndWays(ctx, path, memberWays)
	if err != nil {
		return nil, err
	}

	for _, rd := range relations {
		minLabelZoom := LabelMinZoom(rd.Class, nil)
		for _, ring := range assembleRings(rd.OuterWays, memberCoords) {
			feat := Feature{
				Class:        rd.Class,
				Kind:         GeomPolygon,
				Coords:       ring,
				Name:         rd.Name,
				MinLabelZoom: minLabelZoom,
			}
			feat.computeBounds()
			fs.Features = append(fs.Features, feat)
		}
	}

	// Sort once into painter's order so the renderer can iterate in
	// place. Without this, RenderTile was paying ~O(n log n) per tile.
	sort.SliceStable(fs.Features, func(i, j int) bool {
		return drawOrder(fs.Features[i].Class) < drawOrder(fs.Features[j].Class)
	})
	return fs, nil
}

func loadNodesAndWays(ctx context.Context, path string, memberWays map[osm.WayID]struct{}) (*FeatureSet, map[osm.WayID][]LonLat, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	nodes := make(map[osm.NodeID]LonLat)
	memberCoords := make(map[osm.WayID][]LonLat, len(memberWays))
	var fs FeatureSet

	sc := osmpbf.New(ctx, f, runtime.NumCPU())
	sc.SkipRelations = true
	defer sc.Close()

	for sc.Scan() {
		switch e := sc.Object().(type) {
		case *osm.Node:
			// Every node is stored — referenced ways need lookup, and
			// the same memory slot also serves as the geometry for any
			// classified point feature this node represents.
			nodes[e.ID] = LonLat{Lon: e.Lon, Lat: e.Lat}
			if len(e.Tags) == 0 {
				continue
			}
			class := Classify(e.Tags)
			if class == ClassSkip {
				continue
			}
			feat := Feature{
				Class:        class,
				Kind:         GeomPoint,
				Coords:       []LonLat{{Lon: e.Lon, Lat: e.Lat}},
				Name:         e.Tags.Find("name"),
				MinLabelZoom: LabelMinZoom(class, e.Tags),
			}
			feat.computeBounds()
			fs.Features = append(fs.Features, feat)

		case *osm.Way:
			// Resolve coords up front because both feature emission
			// and relation stitching want them.
			coords := make([]LonLat, 0, len(e.Nodes))
			for _, n := range e.Nodes {
				p, ok := nodes[n.ID]
				if !ok {
					// Node missing from the extract — skip this
					// vertex. Geofabrik clips ways at the extract
					// boundary so the rest of the polyline is still
					// useful.
					continue
				}
				coords = append(coords, p)
			}

			if _, want := memberWays[e.ID]; want && len(coords) >= 2 {
				memberCoords[e.ID] = coords
			}

			if len(e.Tags) == 0 || len(coords) < 2 {
				continue
			}
			class := Classify(e.Tags)
			if class == ClassSkip {
				continue
			}
			kind := GeomLine
			if isAreaClass(class) && coords[0] == coords[len(coords)-1] {
				kind = GeomPolygon
			}
			feat := Feature{
				Class:        class,
				Kind:         kind,
				Coords:       coords,
				Name:         e.Tags.Find("name"),
				MinLabelZoom: LabelMinZoom(class, e.Tags),
			}
			feat.computeBounds()
			fs.Features = append(fs.Features, feat)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("ways/nodes pass: %w", err)
	}
	return &fs, memberCoords, nil
}

// isAreaClass returns true when a closed way of this class should be
// drawn as a filled polygon rather than a line. Roads/railways/admin
// lines stay linear even if they happen to close (a loop road is still
// a line, not a fill).
func isAreaClass(c Class) bool {
	switch c {
	case ClassBuilding, ClassLanduse, ClassLeisure, ClassNatural:
		return true
	}
	return false
}
