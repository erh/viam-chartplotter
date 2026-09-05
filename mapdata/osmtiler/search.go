package osmtiler

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Name search over the ingested OSM data.
//
// The ENC is a chart, not a gazetteer: it names lights, wrecks, channels and
// sea areas, but a marina, a fuel dock, a chandlery or a town appears in it
// rarely and under whatever the surveyor wrote. OSM names all of those, and it
// is already on disk here — so "where is the marina" is answerable locally,
// with no API key, no rate limit, and no internet, which is the condition a
// boat is actually in.

// searchCollections are the buckets a name search covers. LowZoom is a
// curated copy of the coarse features and would only duplicate hits, so it is
// left out.
func searchCollections(c *OSMCollections) []*mongo.Collection {
	if c == nil {
		return nil
	}
	return []*mongo.Collection{c.Overview, c.Coastal, c.Detail, c.Skip}
}

// SearchIndexName is the partial index on `name` these queries need.
const SearchIndexName = "name_search"

// EnsureSearchIndexes creates the name index on every feature collection.
// Partial over just the named features (most OSM geometry is unnamed road and
// landuse), and with no collation, so an unanchored $regex can still use it —
// a collated index is unusable for regex and would silently mean a full scan.
func EnsureSearchIndexes(ctx context.Context, colls *OSMCollections) error {
	model := mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetName(SearchIndexName).
			SetPartialFilterExpression(bson.M{"name": bson.M{"$exists": true, "$gt": ""}}),
	}
	for _, coll := range searchCollections(colls) {
		if coll == nil {
			continue
		}
		if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
			return fmt.Errorf("osm: create %s on %s: %w", SearchIndexName, coll.Name(), err)
		}
	}
	return nil
}

// SearchHit is one named OSM feature.
type SearchHit struct {
	Name  string
	Class string
	// Kind is the OSM tag that says what this is, as "key=value"
	// (leisure=marina, amenity=fuel, place=town). The chart has no equivalent,
	// and it is what tells a marina from a car park with the same name.
	Kind string
	Lat  float64
	Lng  float64
	BBox [4]float64
}

// nameFilter matches every word of the query somewhere in the name, in any
// order — the same rule the chart search uses, and for the same reason:
// people don't type names the way a database spells them.
func nameFilter(query string, kinds map[string][]string) bson.M {
	words := strings.Fields(query)
	if len(words) > maxSearchWords {
		words = words[:maxSearchWords]
	}
	clauses := []bson.M{{"name": bson.M{"$exists": true, "$gt": ""}}}
	for _, w := range words {
		clauses = append(clauses, bson.M{"name": bson.M{
			"$regex":   regexp.QuoteMeta(w),
			"$options": "i",
		}})
	}
	f := bson.M{"$and": clauses}
	if len(kinds) > 0 {
		var any []bson.M
		for key, values := range kinds {
			any = append(any, bson.M{"tags." + key: bson.M{"$in": values}})
		}
		f["$or"] = any
	}
	return f
}

const maxSearchWords = 6

// SearchByName finds named OSM features matching the query across every
// collection, concurrently. bbox, when non-nil, restricts to that box; kinds,
// when non-empty, restricts to features carrying one of those tag values
// (see MarineKinds).
func SearchByName(ctx context.Context, colls *OSMCollections, query string, limit int, bbox *[4]float64, kinds map[string][]string) ([]SearchHit, error) {
	if colls == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" || limit <= 0 {
		return nil, nil
	}
	filter := nameFilter(query, kinds)
	if bbox != nil {
		filter["bbox.0"] = bson.M{"$lte": bbox[2]}
		filter["bbox.2"] = bson.M{"$gte": bbox[0]}
		filter["bbox.1"] = bson.M{"$lte": bbox[3]}
		filter["bbox.3"] = bson.M{"$gte": bbox[1]}
	}

	all := searchCollections(colls)
	results := make([][]SearchHit, len(all))
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, coll := range all {
		if coll == nil {
			continue
		}
		wg.Add(1)
		go func(i int, coll *mongo.Collection) {
			defer wg.Done()
			results[i], errs[i] = searchOne(ctx, coll, filter, limit)
		}(i, coll)
	}
	wg.Wait()

	var out []SearchHit
	for i := range all {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

func searchOne(ctx context.Context, coll *mongo.Collection, filter bson.M, limit int) ([]SearchHit, error) {
	// Geometry is not projected: a search hit needs a position, and bbox
	// already carries one. Fetching the full geometry of a named coastline or
	// landuse polygon is the whole payload for none of the value.
	opts := options.Find().
		SetProjection(bson.M{"name": 1, "class": 1, "bbox": 1, "tags": 1}).
		SetLimit(int64(limit)).
		SetHint(SearchIndexName)
	cur, err := coll.Find(ctx, filter, opts)
	if err != nil {
		// The index may not exist yet on a database ingested before search
		// was added; fall back rather than failing the whole query.
		if !strings.Contains(err.Error(), "hint provided does not correspond") {
			return nil, fmt.Errorf("osm: search %s: %w", coll.Name(), err)
		}
		cur, err = coll.Find(ctx, filter, options.Find().
			SetProjection(bson.M{"name": 1, "class": 1, "bbox": 1, "tags": 1}).
			SetLimit(int64(limit)))
		if err != nil {
			return nil, fmt.Errorf("osm: search %s: %w", coll.Name(), err)
		}
	}
	defer cur.Close(ctx)

	var out []SearchHit
	for cur.Next(ctx) {
		var d struct {
			Name  string            `bson:"name"`
			Class string            `bson:"class"`
			BBox  []float64         `bson:"bbox"`
			Tags  map[string]string `bson:"tags"`
		}
		if err := cur.Decode(&d); err != nil {
			continue // one bad document shouldn't lose the search
		}
		if d.Name == "" || len(d.BBox) != 4 {
			continue
		}
		out = append(out, SearchHit{
			Name:  d.Name,
			Class: d.Class,
			Kind:  KindFromTags(d.Tags),
			Lat:   (d.BBox[1] + d.BBox[3]) / 2,
			Lng:   (d.BBox[0] + d.BBox[2]) / 2,
			BBox:  [4]float64{d.BBox[0], d.BBox[1], d.BBox[2], d.BBox[3]},
		})
	}
	return out, cur.Err()
}

// kindTagKeys are the OSM tag keys worth reporting as "what this is", in
// priority order — the first one present wins.
var kindTagKeys = []string{
	"leisure", "amenity", "shop", "seamark:type", "man_made",
	"harbour", "waterway", "place", "natural", "tourism", "landuse",
}

// KindFromTags picks the tag that best says what a feature is, as "key=value".
func KindFromTags(tags map[string]string) string {
	for _, k := range kindTagKeys {
		if v, ok := tags[k]; ok && v != "" {
			return k + "=" + v
		}
	}
	return ""
}

// MarineKinds restricts a search to the things a boat cares about: marinas,
// harbours, slipways, piers, fuel, chandleries, moorings, and the settlements
// they sit in. Passed as kinds to SearchByName.
func MarineKinds() map[string][]string {
	return map[string][]string{
		"leisure":      {"marina", "slipway"},
		"amenity":      {"fuel", "boat_rental", "ferry_terminal"},
		"shop":         {"boat", "chandlery", "fishing"},
		"man_made":     {"pier", "breakwater", "lighthouse"},
		"seamark:type": {"harbour", "mooring", "berth", "anchorage"},
		"harbour":      {"yes"},
		"waterway":     {"boatyard", "fuel", "dock"},
		"place":        {"city", "town", "village", "hamlet", "island", "islet", "suburb"},
	}
}
