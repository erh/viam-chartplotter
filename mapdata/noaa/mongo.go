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

// CollLowZoom is the curated overview-band collection: a copy of the coarse
// NOAA features (usage band <= LowZoomMaxBand, rendered by LowZoomMaxZoom) whose
// stored geometry is the *valid-simplified* one. Querying it makes the huge
// overview-tile $geoIntersects fast: full-resolution coastlines/contours blow
// up the 2dsphere multikey index (a z7/z10 box walks 30-150k index keys to
// return ~2k features), but their simplified geometry occupies far fewer S2
// cells. Built by `mapsync backfill-noaa-lowzoom`; the renderer reads it at
// z <= LowZoomMaxZoom and falls back to CollNOAA when it's absent.
const CollLowZoom = "noaa_lowzoom"

// LowZoomMaxBand is the usage-band ceiling for membership in CollLowZoom. Band 4
// (approach) is the finest the overview band (z7..z10) ever paints; bands 5-6
// (harbour/berthing) only appear at detail zoom, which reads CollNOAA.
const LowZoomMaxBand = 4

// LowZoomMaxZoom is the highest XYZ zoom served from CollLowZoom. Above it the
// tile box is small and the per-cell feature count low, so CollNOAA's full
// geometry is cheap and crisper.
const LowZoomMaxZoom = 10

// LowGeomMaxZoom is the highest XYZ zoom served from the pre-simplified
// FeatureDoc.GeomLow geometry (built at ~1px resolution for this zoom; see
// lowGeomTolerance). At or below it the tile-query path coalesces to geomLow so
// the giant full-resolution coastlines/contours never cross the wire when their
// sub-pixel detail is invisible. Above it the full Geometry is used so high-zoom
// tiles stay crisp — those tiles cover little ground and carry few features, so
// transferring full geometry there is cheap.
const LowGeomMaxZoom = 12

// OpenCollection returns the noaa feature collection from a database handle.
func OpenCollection(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(CollNOAA)
}

// OpenLowZoomCollection returns the curated overview-band collection (CollLowZoom)
// from a database handle. The renderer attaches it alongside the main collection
// and queries it at z <= LowZoomMaxZoom; nil means "not built — use CollNOAA".
func OpenLowZoomCollection(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(CollLowZoom)
}

// OpenLowZoomCollectionIfBuilt returns CollLowZoom only when it actually holds
// documents, and nil otherwise. The renderer must NOT attach an empty/absent
// low-zoom collection: it would route overview tiles to it and draw blank.
// Wiring through this means a deployment that hasn't run `backfill-noaa-lowzoom`
// yet transparently keeps rendering from the full collection.
func OpenLowZoomCollectionIfBuilt(ctx context.Context, db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	coll := db.Collection(CollLowZoom)
	if n, err := coll.EstimatedDocumentCount(ctx); err != nil || n == 0 {
		return nil
	}
	return coll
}

// EnsureLowZoomIndex builds the two 2dsphere indexes the CollLowZoom tile query
// needs — the renderer's overview zooms split into two query shapes:
//
//   - z7..z9 add a usageBand ceiling (<= lowZoomMaxUsageBand). band_geo
//     (usageBand FIRST, then 2dsphere) lets that predicate bound the geo scan,
//     so the huge z7 box doesn't walk the band-4 geometry it discards. Without
//     it z7 scans every in-bbox geometry then filters band — slower than the
//     main collection, defeating the point.
//   - z10 has no band ceiling. geo_minZoom_class (geometry FIRST) drives that
//     query straight off the 2dsphere; here the simplified geometry's smaller
//     S2 footprint is the whole win (z10 ~2.6s -> ~0.2s).
//
// The planner picks per query: band_geo when a usageBand predicate is present,
// geo_minZoom_class otherwise. Build these on the EMPTY collection before
// inserting so the backfill's per-document 2dsphere validation runs on insert
// (invalid simplified geometries are rejected one-by-one, not en masse).
func EnsureLowZoomIndex(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("noaa: nil low-zoom collection")
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
			Keys: bson.D{
				{Key: "usageBand", Value: 1},
				{Key: "geometry", Value: "2dsphere"},
			},
			Options: options.Index().SetName("band_geo"),
		},
	})
	if err != nil {
		return fmt.Errorf("noaa: low-zoom index: %w", err)
	}
	return nil
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
			// Overview-zoom tiles add a usage-band ceiling (usageBand <= N) to the
			// query. With usageBand as the index PREFIX (before the 2dsphere key)
			// the band predicate bounds the geo scan, so a huge z7 box examines
			// only the coarse-band features it paints (~2.5k docs / ~2s) instead
			// of every feature it intersects (~39k / ~7s). Field order is what
			// matters: a discriminator AFTER a 2dsphere key can't bound the scan
			// (an earlier {geometry, scale} attempt did not prune) — it must lead.
			Keys: bson.D{
				{Key: "usageBand", Value: 1},
				{Key: "geometry", Value: "2dsphere"},
			},
			Options: options.Index().SetName("band_geo"),
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
	return QueryBBoxBanded(ctx, coll, minLon, minLat, maxLon, maxLat, maxZoom, 0, nil, objectClass, false)
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
// useLowGeom selects the pre-simplified geometry tier (see LowGeomMaxZoom):
// when true, the query coalesces geomLow→geometry server-side so only the
// thinned geometry is transferred, and the result's Geometry field carries it;
// when false the full Geometry is returned (and the geomLow duplicate excluded).
func QueryBBoxBanded(ctx context.Context, coll *mongo.Collection, minLon, minLat, maxLon, maxLat float64, maxZoom, maxUsageBand int, alwaysClasses []string, objectClass string, useLowGeom bool) ([]FeatureDoc, error) {
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

	if useLowGeom {
		// Aggregate so the geomLow→geometry coalesce happens server-side: only
		// the thinned geometry crosses the wire, and docs ingested before
		// geomLow existed transparently fall back to their full geometry. The
		// $match-first $geoIntersects still uses the 2dsphere / band_geo index.
		pipeline := mongo.Pipeline{
			bson.D{{Key: "$match", Value: filter}},
			bson.D{{Key: "$set", Value: bson.M{"geometry": bson.M{"$ifNull": bson.A{"$geomLow", "$geometry"}}}}},
			bson.D{{Key: "$unset", Value: "geomLow"}},
		}
		cur, err := coll.Aggregate(ctx, pipeline)
		if err != nil {
			return nil, fmt.Errorf("noaa: aggregate: %w", err)
		}
		defer cur.Close(ctx)
		return decodeFeatureDocs(ctx, cur)
	}

	// Full-geometry path: exclude geomLow so its duplicate never transfers.
	cur, err := coll.Find(ctx, filter, options.Find().SetProjection(bson.M{"geomLow": 0}))
	if err != nil {
		return nil, fmt.Errorf("noaa: find: %w", err)
	}
	defer cur.Close(ctx)
	return decodeFeatureDocs(ctx, cur)
}

// decodeFeatureDocs drains a cursor into FeatureDocs, skipping any single
// document that fails to decode rather than aborting the whole tile.
func decodeFeatureDocs(ctx context.Context, cur *mongo.Cursor) ([]FeatureDoc, error) {
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
