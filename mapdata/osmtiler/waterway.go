package osmtiler

import (
	"context"
	"fmt"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Navigable waterways, for routing.
//
// A chart draws a canal as a polygon and leaves it to you to notice it is
// continuous. Rasterise that polygon onto a grid whose cells are wider than
// the canal and it becomes a row of disconnected dots — the Cape Cod Canal
// vanishes at any resolution a 30 nm leg can afford, and even at 36 m cells
// the ENC's own coverage has seams. OSM draws the same canal as a LINE, which
// is connected by construction. Used for topology (this waterway joins these
// two bodies of water) rather than for position, it is what makes a narrow
// passage routable at all.

// navigableWaterwayFilter selects ways that are navigable by definition.
//
// Deliberately NOT a bare waterway=river: OSM tags every creek and stream that
// way, and treating them all as navigable would route a boat up a brook. What
// is admitted is water that is navigable by construction (canal, fairway,
// tidal channel), by charting convention (the seamark tags), or by explicit
// statement (boat/motorboat/ship=yes). Depth still governs — the caller checks
// each cell against the boat's draft — so this grants connectivity, not
// permission.
func navigableWaterwayFilter(minLon, minLat, maxLon, maxLat float64) bson.M {
	polygon := bson.M{
		"type": "Polygon",
		"coordinates": [][][]float64{{
			{minLon, minLat}, {maxLon, minLat}, {maxLon, maxLat},
			{minLon, maxLat}, {minLon, minLat},
		}},
	}
	return bson.M{
		"geometry": bson.M{"$geoIntersects": bson.M{"$geometry": polygon}},
		"kind":     "line",
		"$or": []bson.M{
			// Navigable by construction or by charting convention. A tidal
			// channel is a channel — the East River and Hell Gate are tagged
			// that way, and without them a New York departure has to go the
			// outside of Long Island, which is some 45 nm further.
			{"tags.waterway": bson.M{"$in": []string{"canal", "fairway", "tidal_channel"}}},
			{"tags.seamark:type": bson.M{"$in": []string{
				"fairway", "navigation_line", "recommended_track", "separation_lane",
			}}},
			// Or navigable because somebody said so. This is what lets a river
			// in — the Harlem River carries boat=yes motorboat=yes — without
			// admitting every creek tagged waterway=river.
			{"$and": []bson.M{
				{"tags.waterway": bson.M{"$exists": true}},
				{"$or": []bson.M{
					{"tags.boat": "yes"},
					{"tags.motorboat": "yes"},
					{"tags.ship": "yes"},
				}},
			}},
		},
	}
}

// NavigableWaterways returns the navigable waterway centrelines intersecting
// the bbox, across every collection.
func NavigableWaterways(ctx context.Context, colls *OSMCollections, minLon, minLat, maxLon, maxLat float64) ([]Feature, error) {
	if colls == nil {
		return nil, nil
	}
	filter := navigableWaterwayFilter(minLon, minLat, maxLon, maxLat)
	all := waterwayCollections(colls)
	results := make([][]Feature, len(all))
	errs := make([]error, len(all))

	var wg sync.WaitGroup
	for i, coll := range all {
		if coll == nil {
			continue
		}
		wg.Add(1)
		go func(i int, coll *mongo.Collection) {
			defer wg.Done()
			cur, err := coll.Find(ctx, filter, options.Find().
				SetProjection(bson.M{"geomLow": 0}).
				SetLimit(navigableWaterwayLimit))
			if err != nil {
				errs[i] = fmt.Errorf("osm: waterways %s: %w", coll.Name(), err)
				return
			}
			defer cur.Close(ctx)
			for cur.Next(ctx) {
				f, err := DecodeFeature(cur.Current)
				if err != nil || len(f.Coords) < 2 {
					continue
				}
				results[i] = append(results[i], f)
			}
			errs[i] = cur.Err()
		}(i, coll)
	}
	wg.Wait()

	var out []Feature
	for i := range all {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

// waterwayCollections are the buckets a navigable waterway can live in.
//
// Only the coarse ones. A canal or a marked fairway is a low-minZoom feature,
// so that is where they are — every Cape Cod Canal and Hell Gate hit is in
// osm_overview — while osm_detail holds 179M mostly-urban documents that a
// geo query has to walk before the tag filter rejects them. Including it made
// a per-tile lookup take longer than the whole tile build's budget for no hits
// at all.
func waterwayCollections(c *OSMCollections) []*mongo.Collection {
	if c == nil {
		return nil
	}
	return []*mongo.Collection{c.Overview, c.Coastal}
}

// navigableWaterwayLimit bounds one collection's contribution. Canals and
// fairways are sparse, so a corridor holding thousands of them means the query
// has gone wrong, not that the water is unusually complicated.
const navigableWaterwayLimit = 4000
