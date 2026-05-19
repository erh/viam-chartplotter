package osmtiler

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

// LoadPBF streams an .osm.pbf, applies Classify to every feature, and
// returns the kept features with geometry resolved to lon/lat coords.
// Multipolygon relations are not handled in this v0.1 slice — closed
// ways become polygons, open ways become lines, tagged nodes become
// points. See OSM_TILES_PLAN.md for the v0.2 plan.
//
// Memory: a `map[NodeID]{Lat,Lon}` is held for every node in the file
// (~24 bytes each). For a city-scale extract that's tens of MB; for the
// planet it's tens of GB and the SQLite-backed pipeline kicks in instead.
func LoadPBF(ctx context.Context, path string) (*FeatureSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return loadPBFReader(ctx, f)
}

func loadPBFReader(ctx context.Context, r io.Reader) (*FeatureSet, error) {
	nodes := make(map[osm.NodeID]LonLat)
	var fs FeatureSet

	sc := osmpbf.New(ctx, r, runtime.NumCPU())
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
			f := Feature{
				Class:  class,
				Kind:   GeomPoint,
				Coords: []LonLat{{Lon: e.Lon, Lat: e.Lat}},
				Name:   e.Tags.Find("name"),
			}
			f.computeBounds()
			fs.Features = append(fs.Features, f)

		case *osm.Way:
			if len(e.Tags) == 0 {
				continue
			}
			class := Classify(e.Tags)
			if class == ClassSkip {
				continue
			}

			coords := make([]LonLat, 0, len(e.Nodes))
			for _, n := range e.Nodes {
				p, ok := nodes[n.ID]
				if !ok {
					// Node missing from the extract — skip this vertex
					// rather than fail. Geofabrik clips ways at the
					// extract boundary so the rest of the polyline is
					// still useful.
					continue
				}
				coords = append(coords, p)
			}
			if len(coords) < 2 {
				continue
			}

			kind := GeomLine
			if isAreaClass(class) && coords[0] == coords[len(coords)-1] {
				kind = GeomPolygon
			}

			f := Feature{
				Class:  class,
				Kind:   kind,
				Coords: coords,
				Name:   e.Tags.Find("name"),
			}
			f.computeBounds()
			fs.Features = append(fs.Features, f)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("pbf scan: %w", err)
	}

	// Sort once into painter's order so the renderer can iterate in
	// place. Without this, RenderTile was paying ~O(n log n) per tile.
	sort.SliceStable(fs.Features, func(i, j int) bool {
		return drawOrder(fs.Features[i].Class) < drawOrder(fs.Features[j].Class)
	})
	return &fs, nil
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
