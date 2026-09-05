package poi

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// Stats summarises one ingest run.
type Stats struct {
	Read    int // features decoded from the feed
	Dropped int // features with no usable position or class
	Written int // documents upserted
	Pruned  int // documents deleted as stale (see Prune)
}

// Ingest converts a decoded feed into documents and upserts them, returning
// the run's timestamp so a caller can Prune what this run didn't touch.
//
// Writes are idempotent upserts keyed on the document id, so a re-run over the
// same feed rewrites the same rows: interrupting an ingest costs progress, not
// consistency, and re-running is always safe.
func Ingest(ctx context.Context, coll *mongo.Collection, dataset string, features []Feature, adapter Adapter, bbox *[4]float64) (Stats, time.Time, error) {
	began := time.Now().UTC()
	var stats Stats
	if coll == nil {
		return stats, began, fmt.Errorf("poi: nil collection")
	}
	docs := make([]noaa.FeatureDoc, 0, len(features))
	for _, f := range features {
		stats.Read++
		p, ok := adapter(f)
		if !ok {
			stats.Dropped++
			continue
		}
		if bbox != nil && !inBBox(p.Lat, p.Lng, *bbox) {
			continue
		}
		doc, ok := Doc(dataset, p, began)
		if !ok {
			stats.Dropped++
			continue
		}
		docs = append(docs, doc)
	}
	applied, writeErrs, err := noaa.UpsertDocs(ctx, coll, docs, 0)
	stats.Written = applied
	stats.Dropped += writeErrs
	if err != nil {
		return stats, began, err
	}
	return stats, began, nil
}

// Prune deletes documents of a dataset that the run starting at `since` did
// not write — the rows that have disappeared upstream.
//
// This runs after a SUCCESSFUL full-feed ingest and never during one. A partial
// ingest (an interrupted fetch, a bbox-scoped run) has not seen the rows it
// didn't write, and pruning on that would delete the rest of the country.
func Prune(ctx context.Context, coll *mongo.Collection, dataset string, since time.Time) (int, error) {
	if coll == nil {
		return 0, fmt.Errorf("poi: nil collection")
	}
	res, err := coll.DeleteMany(ctx, bson.M{
		"cell":                     dataset,
		"attributes." + AttrIngest: bson.M{"$lt": since.UTC()},
	})
	if err != nil {
		return 0, fmt.Errorf("poi: prune %s: %w", dataset, err)
	}
	return int(res.DeletedCount), nil
}

// Drop removes an entire dataset. A feed that turns out to be wrong — bad
// positions, a licence that doesn't allow redistribution — should be one
// command away from gone.
func Drop(ctx context.Context, coll *mongo.Collection, dataset string) (int, error) {
	if coll == nil {
		return 0, fmt.Errorf("poi: nil collection")
	}
	res, err := coll.DeleteMany(ctx, bson.M{"cell": dataset})
	if err != nil {
		return 0, fmt.Errorf("poi: drop %s: %w", dataset, err)
	}
	return int(res.DeletedCount), nil
}

func inBBox(lat, lng float64, b [4]float64) bool {
	return lng >= b[0] && lng <= b[2] && lat >= b[1] && lat <= b[3]
}
