package noaa

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CollNOAA is the single collection holding parsed NOAA ENC features. It
// lives in the same database as the four osm_* collections — one database,
// five collections.
const CollNOAA = "noaa"

// OpenCollection returns the noaa feature collection from a database handle.
func OpenCollection(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(CollNOAA)
}

// EnsureIndexes creates the indexes the tile-query path needs: a 2dsphere on
// geometry for $geoIntersects, plus scalar indexes on the fields we filter
// and group by. Safe to call repeatedly — Mongo is idempotent on identical
// index specs.
func EnsureIndexes(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("noaa: nil collection")
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "geometry", Value: "2dsphere"},
				{Key: "minZoom", Value: 1},
				{Key: "objectClass", Value: 1},
			},
			Options: options.Index().SetName("geo_minZoom_class"),
		},
		{
			Keys:    bson.D{{Key: "cell", Value: 1}},
			Options: options.Index().SetName("cell_1"),
		},
	})
	if err != nil {
		return fmt.Errorf("noaa: create indexes: %w", err)
	}
	return nil
}

// UpsertDocs bulk-upserts feature documents keyed on _id. Writes are
// unordered so a single document rejected by the 2dsphere validator (a
// self-intersecting ENC polygon, say) doesn't abort the rest of the batch;
// the count of write errors is returned alongside the number applied.
func UpsertDocs(ctx context.Context, coll *mongo.Collection, docs []FeatureDoc, batchSize int) (applied, writeErrs int, err error) {
	if coll == nil {
		return 0, 0, fmt.Errorf("noaa: nil collection")
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}
		models := make([]mongo.WriteModel, 0, end-start)
		for _, d := range docs[start:end] {
			models = append(models, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"_id": d.ID}).
				SetUpdate(bson.M{"$set": d}).
				SetUpsert(true))
		}
		res, bwErr := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
		if res != nil {
			applied += int(res.UpsertedCount + res.ModifiedCount + res.MatchedCount)
		}
		if bwErr != nil {
			// A BulkWriteException with only per-document 2dsphere
			// validation failures is expected and survivable; surface the
			// count but keep going. Any other error is fatal.
			var bwe mongo.BulkWriteException
			if ok := asBulkWriteException(bwErr, &bwe); ok {
				writeErrs += len(bwe.WriteErrors)
				continue
			}
			return applied, writeErrs, fmt.Errorf("noaa: bulk write: %w", bwErr)
		}
	}
	return applied, writeErrs, nil
}

// asBulkWriteException unwraps a BulkWriteException so callers can tolerate
// per-document write errors while still failing on transport/auth errors.
func asBulkWriteException(err error, out *mongo.BulkWriteException) bool {
	if bwe, ok := err.(mongo.BulkWriteException); ok {
		*out = bwe
		return true
	}
	return false
}

// LookupMeta returns the stored CellMeta for a cell, or ok=false if the cell
// hasn't been ingested.
func LookupMeta(ctx context.Context, coll *mongo.Collection, cell string) (CellMeta, bool, error) {
	if coll == nil {
		return CellMeta{}, false, fmt.Errorf("noaa: nil collection")
	}
	var m CellMeta
	err := coll.FindOne(ctx, bson.M{"_id": "_meta:" + cell}).Decode(&m)
	if err == mongo.ErrNoDocuments {
		return CellMeta{}, false, nil
	}
	if err != nil {
		return CellMeta{}, false, err
	}
	return m, true, nil
}

// WriteMeta upserts the per-cell ingest metadata used for dedup.
func WriteMeta(ctx context.Context, coll *mongo.Collection, m CellMeta) error {
	if coll == nil {
		return fmt.Errorf("noaa: nil collection")
	}
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": m.ID},
		bson.M{"$set": m},
		options.Update().SetUpsert(true))
	return err
}

// QueryBBox returns the feature documents whose geometry intersects the given
// lon/lat box, optionally filtered to those renderable at or below maxZoom
// (pass a negative maxZoom to disable zoom filtering) and to a single object
// class (pass "" for all). It is the read counterpart to UpsertDocs, intended
// for a future Mongo-backed ENC renderer and for inspection tooling.
func QueryBBox(ctx context.Context, coll *mongo.Collection, minLon, minLat, maxLon, maxLat float64, maxZoom int, objectClass string) ([]FeatureDoc, error) {
	return QueryBBoxBanded(ctx, coll, minLon, minLat, maxLon, maxLat, maxZoom, 0, nil, objectClass)
}

// QueryBBoxBanded is QueryBBox with an additional S-57 usage-band ceiling:
// features from cells finer than maxUsageBand (1=overview … 6=berthing) are
// excluded. At overview zooms a tile spans many fine harbour cells whose detail
// is invisible and never painted; restricting to the coarse bands (1=overview,
// 2=general, 3=coastal — the coastal band is where overview DEPARE depth lives)
// keeps the chart readable and cheap. maxUsageBand<=0 disables the ceiling
// (identical to QueryBBox). usageBand is the right lever here, not compilation
// scale: scale varies cell-to-cell, but band is the discrete navigational-
// purpose tier the renderer actually wants to gate on.
//
// alwaysClasses lists object classes that bypass the maxZoom filter — fetched
// whenever their geometry intersects, regardless of their stored minZoom. This
// surfaces the coarse depth contours (DEPCNT) at overview zoom: they're stored
// at minZoom≈11 but NOAA shows the major fathom contours from z7, and the band
// ceiling already keeps only the coarse-cell ones. The band ceiling still
// applies to them.
func QueryBBoxBanded(ctx context.Context, coll *mongo.Collection, minLon, minLat, maxLon, maxLat float64, maxZoom, maxUsageBand int, alwaysClasses []string, objectClass string) ([]FeatureDoc, error) {
	if coll == nil {
		return nil, fmt.Errorf("noaa: nil collection")
	}
	polygon := bson.M{
		"type": "Polygon",
		"coordinates": [][][]float64{{
			{minLon, minLat},
			{maxLon, minLat},
			{maxLon, maxLat},
			{minLon, maxLat},
			{minLon, minLat},
		}},
	}
	filter := bson.M{
		"geometry": bson.M{"$geoIntersects": bson.M{"$geometry": polygon}},
	}
	if maxZoom >= 0 {
		if len(alwaysClasses) > 0 {
			// minZoom<=z OR class in alwaysClasses (surface coarse contours etc.)
			filter["$or"] = []bson.M{
				{"minZoom": bson.M{"$lte": maxZoom}},
				{"objectClass": bson.M{"$in": alwaysClasses}},
			}
		} else {
			filter["minZoom"] = bson.M{"$lte": maxZoom}
		}
	}
	if maxUsageBand > 0 {
		filter["usageBand"] = bson.M{"$lte": maxUsageBand}
	}
	if objectClass != "" {
		filter["objectClass"] = objectClass
	}
	cur, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("noaa: find: %w", err)
	}
	defer cur.Close(ctx)
	out := make([]FeatureDoc, 0, 64)
	for cur.Next(ctx) {
		var d FeatureDoc
		if err := cur.Decode(&d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, cur.Err()
}
