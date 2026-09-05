// Package places is the gazetteer: one small collection of everything named,
// drawn from both the ENC and OSM, with a text index over the names.
//
// It exists because name search over the source collections doesn't scale. An
// unanchored regex there is an index scan whose cost tracks the index size
// rather than the number of matches — measured at 0.4 s for a common word and
// 15 s for a rare one over 274M documents, with the same query varying by 30x
// on cache warmth alone. A text index is an inverted index: cost tracks the
// matches. That is the difference between a search box that works and one that
// times out on the name you actually wanted.
package places

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Coll is the gazetteer collection.
const Coll = "places"

// Source says which dataset a place came from. The two answer different
// questions — the chart names navigation features, OSM names places and
// businesses — and a searcher should be able to tell them apart.
const (
	SourceChart = "chart"
	SourceOSM   = "osm"
)

// Place is one named thing.
type Place struct {
	ID     string `bson:"_id"`
	Name   string `bson:"name"`
	Source string `bson:"source"`
	// Class is the S-57 acronym for a chart place, or "key=value" for an OSM
	// one (leisure=marina, place=town).
	Class string     `bson:"class"`
	Lat   float64    `bson:"lat"`
	Lng   float64    `bson:"lng"`
	BBox  [4]float64 `bson:"bbox"`
	Cell  string     `bson:"cell,omitempty"`
}

// ID builds a stable document id from a place's identity, so rebuilding is an
// idempotent upsert and the same feature charted in several cells collapses to
// one row. Position is rounded to ~100 m: the same light in a coastal and a
// harbour cell differs slightly, and they are not two places.
func ID(source, class, name string, lat, lng float64) string {
	key := fmt.Sprintf("%s|%s|%s|%.3f|%.3f", source, class, strings.ToLower(name), lat, lng)
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// Open returns the gazetteer collection.
func Open(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(Coll)
}

// EnsureIndexes creates the text index the search needs, plus the lookup index
// the regional builder uses to report coverage.
func EnsureIndexes(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("places: nil collection")
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "name", Value: "text"}},
			Options: options.Index().SetName("name_text").SetDefaultLanguage("english"),
		},
		{
			Keys:    bson.D{{Key: "source", Value: 1}},
			Options: options.Index().SetName("source_1"),
		},
	})
	if err != nil {
		return fmt.Errorf("places: create indexes: %w", err)
	}
	return nil
}

// Hit is a search result, carrying the text-index relevance score so the
// caller can weigh it against distance.
type Hit struct {
	Place
	Score float64
}

// Search finds places whose names match the query. Matching is by word, via
// the text index, so it is case-insensitive, punctuation-insensitive and
// stemmed: "chandler's wharf" and "Chandlers Wharf" find each other.
//
// Ranking is left to the caller. Text relevance alone would put a distant
// exact name above the one in the next bay, and distance alone would put a
// vaguely-matching neighbour above the place actually asked for; only the
// caller knows where the boat is.
func Search(ctx context.Context, coll *mongo.Collection, query string, limit int) ([]Hit, error) {
	if coll == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" || limit <= 0 {
		return nil, nil
	}
	// Exact phrase first, loose terms only if that finds nothing. A phrase in
	// $text is REQUIRED, not merely preferred, so the two cannot be combined
	// into one query: "chandlers wharf" as a phrase plus the loose words is
	// still phrase-mandatory, and returns nothing when the name is spelled
	// even slightly differently.
	if len(strings.Fields(query)) > 1 {
		hits, err := textSearch(ctx, coll, fmt.Sprintf("%q", query), limit)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			return hits, nil
		}
	}
	return textSearch(ctx, coll, query, limit)
}

func textSearch(ctx context.Context, coll *mongo.Collection, search string, limit int) ([]Hit, error) {
	cur, err := coll.Find(ctx,
		bson.M{"$text": bson.M{"$search": search}},
		options.Find().
			SetProjection(bson.M{"score": bson.M{"$meta": "textScore"}, "name": 1, "source": 1, "class": 1, "lat": 1, "lng": 1, "bbox": 1, "cell": 1}).
			SetSort(bson.M{"score": bson.M{"$meta": "textScore"}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("places: search: %w", err)
	}
	defer cur.Close(ctx)

	var out []Hit
	for cur.Next(ctx) {
		var d struct {
			Place `bson:",inline"`
			Score float64 `bson:"score"`
		}
		if err := cur.Decode(&d); err != nil {
			continue
		}
		out = append(out, Hit{Place: d.Place, Score: d.Score})
	}
	return out, cur.Err()
}

// Upsert writes places idempotently, so a build can be re-run, resumed, or
// scoped to a region without duplicating what is already there.
func Upsert(ctx context.Context, coll *mongo.Collection, batch []Place) (int, error) {
	if coll == nil || len(batch) == 0 {
		return 0, nil
	}
	models := make([]mongo.WriteModel, 0, len(batch))
	for _, p := range batch {
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"_id": p.ID}).SetReplacement(p).SetUpsert(true))
	}
	res, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return 0, fmt.Errorf("places: upsert: %w", err)
	}
	n := 0
	if res != nil {
		// Matched and Modified overlap — a replaced document reports both — so
		// counting all three inflates the total past the number of documents
		// actually written.
		n = int(res.UpsertedCount + res.MatchedCount)
	}
	return n, nil
}
