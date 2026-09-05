// Package poi holds charted points of interest that are NOT in the ENC:
// wrecks from NOAA's AWOIS/OCS database, artificial reefs from the state reef
// programs, offshore platforms. The chart names what matters to navigation;
// these datasets name what matters to a fisherman or a diver, and the two
// barely overlap — AWOIS carries ~19k wrecks and obstructions, most of them
// never charted because they are no danger to surface navigation.
//
// A POI is stored as a noaa.FeatureDoc in its own collection. Sharing the
// document shape is the whole trick: every query, index and render path that
// already works on ENC features works on these unchanged, so adding a dataset
// is an ingest and a symbol, not a second pipeline. Keeping them in a separate
// COLLECTION is equally deliberate — provenance stays visible (a search result
// can say "AWOIS" rather than pretending to be the chart), a bad feed is one
// drop away, and the ENC collection remains exactly what NOAA published, which
// is what the compare tests measure against.
package poi

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// Coll is the collection holding every POI dataset. It lives in the same
// database as the noaa and osm_* collections.
const Coll = "poi"

// Object classes. These are deliberately NOT S-57 acronyms: a POI is not a
// charted feature, and a class that reads like one would let unverified
// positions leak into code that assumes chart truth (the auto-router's hazard
// classes, the ENC compare tests). The POI_ prefix makes provenance obvious
// wherever a class string surfaces — debug output, search results, logs.
const (
	ClassWreck       = "POI_WRECK"
	ClassObstruction = "POI_OBSTRUCTION"
	ClassReef        = "POI_REEF"
	ClassPlatform    = "POI_PLATFORM"
)

// AttrDataset, AttrIngest and AttrDetail are the attribute keys every POI
// carries: which dataset it came from, when that dataset was last ingested
// (see Prune), and a one-line human description for the popup/label.
const (
	AttrDataset = "poi_dataset"
	AttrIngest  = "poi_ingest"
	AttrDetail  = "poi_detail"
)

// Open returns the POI collection from a database handle.
func Open(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(Coll)
}

// OpenIfBuilt returns the POI collection only when it holds documents, and nil
// otherwise. The renderer wires through this so a deployment that has never
// run `mapsync ingest-poi` skips the per-tile POI query entirely rather than
// paying for a lookup that can only come back empty.
func OpenIfBuilt(ctx context.Context, db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	coll := db.Collection(Coll)
	if n, err := coll.EstimatedDocumentCount(ctx); err != nil || n == 0 {
		return nil
	}
	return coll
}

// EnsureIndexes creates the three indexes the collection needs: the 2dsphere
// the tile query intersects, the partial name index the gazetteer builder and
// the search fallback scan, and the dataset index Prune and re-ingest use.
//
// This is a much smaller set than noaa.EnsureIndexes builds. The collection is
// tens of thousands of documents rather than hundreds of millions, so the
// partial-index tricks that make the ENC collection queryable buy nothing here
// — and the index names match noaa's (`name_search`) where a shared caller
// hints them.
func EnsureIndexes(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("poi: nil collection")
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "geometry", Value: "2dsphere"}},
			Options: options.Index().SetName("geo"),
		},
		{
			// No collation, for the same reason as the ENC name index: an
			// unanchored $regex can only use an index under the simple binary
			// collation, and the search fallback is exactly that query.
			Keys: bson.D{{Key: "name", Value: 1}},
			Options: options.Index().SetName("name_search").
				SetPartialFilterExpression(bson.M{"name": bson.M{"$exists": true}}),
		},
		{
			Keys:    bson.D{{Key: "cell", Value: 1}},
			Options: options.Index().SetName("cell_1"),
		},
	})
	if err != nil {
		return fmt.Errorf("poi: create indexes: %w", err)
	}
	return nil
}

// Point is one point of interest, before it becomes a FeatureDoc. Adapters
// (see datasets.go) produce these from whatever shape the upstream feed has.
type Point struct {
	// ExternalID is the upstream record id when the feed has one. It makes the
	// document id stable across a position correction, which a hash of the
	// position would not be — AWOIS records get re-surveyed and move.
	ExternalID string
	Name       string
	Class      string
	Lat        float64
	Lng        float64
	// Detail is the one-line human description shown in search results and on
	// the chart popup: "Sank 1956, 240 ft" or "Concrete culverts, 62 ft".
	Detail string
	// Attributes are the upstream fields worth keeping, already normalised to
	// lower_snake keys.
	Attributes map[string]any
}

// Doc converts a Point into the FeatureDoc the collection stores.
//
// Geometry is always a Point, even where the upstream feed has a polygon: a
// POI draws as a symbol, and the position accuracy of these datasets (AWOIS
// records positions "as reported", sometimes to the nearest minute) does not
// support drawing an outline that looks surveyed. The bbox is the same point,
// so extent-based callers degrade to centring on it.
//
// It also means every stored geometry is trivially valid for the 2dsphere
// index. External polygons are the one thing that reliably breaks a geo index
// (ring winding, self-intersection), and a feed that fails to insert is a feed
// that silently half-lands.
func Doc(dataset string, p Point, ingestedAt time.Time) (noaa.FeatureDoc, bool) {
	if p.Class == "" || !validLatLng(p.Lat, p.Lng) {
		return noaa.FeatureDoc{}, false
	}
	attrs := map[string]any{}
	for k, v := range p.Attributes {
		attrs[k] = v
	}
	attrs[AttrDataset] = dataset
	attrs[AttrIngest] = ingestedAt.UTC()
	if p.Detail != "" {
		attrs[AttrDetail] = p.Detail
	}
	// OBJNAM so the label/search code paths that read the S-57 name attribute
	// off a feature (drawWreckLabel, the notable-landmark test) work on a POI
	// without a special case.
	if p.Name != "" {
		attrs["OBJNAM"] = p.Name
	}
	return noaa.FeatureDoc{
		ID:          ID(dataset, p),
		Cell:        dataset,
		ObjectClass: p.Class,
		Kind:        "point",
		Name:        p.Name,
		MinZoom:     noaa.MinZoomForObjectClass(p.Class),
		BBox:        [4]float64{p.Lng, p.Lat, p.Lng, p.Lat},
		Geometry: map[string]any{
			"type":        "Point",
			"coordinates": []float64{p.Lng, p.Lat},
		},
		Attributes: attrs,
	}, true
}

// ID builds a stable document id so re-ingesting a feed is an idempotent
// upsert. Keyed on the upstream record id where there is one; otherwise on
// name and position rounded to ~10 m, which is the best available identity for
// a feed that hands out no keys.
func ID(dataset string, p Point) string {
	key := dataset + "|" + p.ExternalID
	if p.ExternalID == "" {
		key = fmt.Sprintf("%s|%s|%s|%.4f|%.4f", dataset, p.Class, strings.ToLower(p.Name), p.Lat, p.Lng)
	}
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// validLatLng rejects the null island / out-of-range rows these feeds carry.
// A wreck at 0,0 is a missing position, not a wreck in the Gulf of Guinea, and
// one that lands in the middle of the chart is worse than one that is absent.
func validLatLng(lat, lng float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return false
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return false
	}
	return lat != 0 || lng != 0
}
