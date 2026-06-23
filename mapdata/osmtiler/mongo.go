package osmtiler

import (
	"context"
	"fmt"
	"math"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MinZoom buckets — features are partitioned across collections by
// their pre-computed minZoom so a low-zoom tile query touches a small
// index. Without this split, the global-bbox $geoIntersects at z=7
// walked the full 200M-doc index and took 20+ seconds. Sizes are tuned
// empirically: most data clusters in the coastal bucket (residential
// landuse, town labels, secondary roads), so detail can keep its own
// big index without slowing down low-zoom queries.
//
// The "everything-skip" sentinel (minZoom=255 from GeomMinZoom's
// fall-through) lives in BucketSkip; it's not in any tile-query path
// so its bucket exists only to give those docs somewhere to land at
// ingest time. None of the runtime fan-out code queries it.
type MinZoomBucket int

const (
	BucketOverview MinZoomBucket = iota // minZoom 0..7
	BucketCoastal                       // minZoom 8..11
	BucketDetail                        // minZoom 12..22
	BucketSkip                          // minZoom == 255 (never rendered)
)

// Collection names. Suffixes match the bucket-name convention so it's
// obvious in mongosh which collection holds what.
const (
	CollOverview = "osm_overview"
	CollCoastal  = "osm_coastal"
	CollDetail   = "osm_detail"
	CollSkip     = "osm_skip"
	// CollLowZoom is a curated, cross-cutting collection holding ONLY the
	// features visible at the z7..z8 overview band: land-cover/road/admin/place
	// classes (no water/buildings/POIs), kept only when a point label or large
	// enough to be visible (see QualifiesForLowZoom), with pre-simplified
	// geometry. It exists because the overview bucket is huge and dense (41M
	// docs, water + millions of sub-pixel features), so a z7 query over its
	// 280km bbox examined ~368k index entries and fetched ~137k docs (~14s).
	// This collection is small, so the same query is ~1-2s. z9+ still use the
	// overview/coastal buckets (their bbox is small enough to be fast).
	CollLowZoom = "osm_lowzoom"
)

// LowZoomBandMaxZoom is the highest zoom served from CollLowZoom. The overview
// band spans osmtiler-consumer-defined z7..z11; at z<=LowZoomBandMaxZoom the
// curated low-zoom collection is queried (huge bbox, needs the small set), and
// above it the normal overview+coastal buckets are fast enough.
const LowZoomBandMaxZoom = 8

// lowZoomBandMinZoom is the COARSEST zoom served from CollLowZoom. The size
// threshold below is keyed to it (not the max), because at the coarsest zoom a
// huge bbox returns the most features — that's the case that has to stay fast,
// and anything sub-pixel there is invisible anyway.
const lowZoomBandMinZoom = 7

// LowZoomMinSpanDeg is the minimum bbox span (longer side, degrees) for a
// non-point feature to be included in CollLowZoom: ~2 web-mercator pixels at
// lowZoomBandMinZoom (z7). Keyed to the coarsest band zoom so a dense-metro z7
// tile returns a few thousand visible features (~1-2s) instead of tens of
// thousands of sub-pixel ones (~8s+). Smaller features return with the full
// overview/coastal buckets at z9+. Points (place labels) are kept regardless.
const LowZoomMinSpanDeg = 2 * 360.0 / float64(256*(1<<lowZoomBandMinZoom))

// QualifiesForLowZoom reports whether a feature belongs in CollLowZoom: a
// band class (land cover / network / admin / place), and either a point (label)
// or big enough to be visible at the coarsest band zoom (spanDeg = longer bbox
// side in degrees).
func QualifiesForLowZoom(class Class, isPoint bool, spanDeg float64) bool {
	switch class {
	case ClassNatural, ClassLanduse, ClassLeisure, ClassRoad,
		ClassRailway, ClassAeroway, ClassAdmin, ClassPlace:
	default:
		return false
	}
	if isPoint {
		return true
	}
	return spanDeg >= LowZoomMinSpanDeg
}

// BucketForMinZoom routes a feature to its bucket. The boundaries are:
// overview (0..7) for low-zoom-only features (country/state labels,
// motorways, coastline, big forests); coastal (8..11) for the bulk of
// readable detail (towns, secondary roads, parks, residential landuse,
// city POIs); detail (12+) for chart-zoom-only (residential streets,
// buildings, single POIs). Sentinel minZoom=255 lands in skip.
func BucketForMinZoom(minZoom uint8) MinZoomBucket {
	switch {
	case minZoom == 255:
		return BucketSkip
	case minZoom <= 7:
		return BucketOverview
	case minZoom <= 11:
		return BucketCoastal
	default:
		return BucketDetail
	}
}

// CollectionName returns the configured collection name for a bucket.
func (b MinZoomBucket) CollectionName() string {
	switch b {
	case BucketOverview:
		return CollOverview
	case BucketCoastal:
		return CollCoastal
	case BucketDetail:
		return CollDetail
	case BucketSkip:
		return CollSkip
	}
	return ""
}

// bucketsForQueryZoom returns the bucket set a tile-render query at
// effective zoom z should consult. Always includes overview; adds
// coastal when there's a chance of an in-range minZoom; adds detail
// when chart-detail zoom is being asked for. Skip is never queried.
func bucketsForQueryZoom(effectiveZoom int) []MinZoomBucket {
	switch {
	case effectiveZoom <= 7:
		return []MinZoomBucket{BucketOverview}
	case effectiveZoom <= 11:
		return []MinZoomBucket{BucketOverview, BucketCoastal}
	default:
		return []MinZoomBucket{BucketOverview, BucketCoastal, BucketDetail}
	}
}

// OSMCollections bundles the per-bucket Mongo collections the
// renderer fans out across. Constructed once from a *mongo.Database
// via OpenOSMCollections.
type OSMCollections struct {
	Overview *mongo.Collection
	Coastal  *mongo.Collection
	Detail   *mongo.Collection
	Skip     *mongo.Collection
	LowZoom  *mongo.Collection
}

// OpenOSMCollections grabs the four bucket collections from a database
// handle. Naming is fixed (CollOverview / CollCoastal / CollDetail /
// CollSkip) — the database's name is the only knob.
func OpenOSMCollections(db *mongo.Database) *OSMCollections {
	if db == nil {
		return nil
	}
	return &OSMCollections{
		Overview: db.Collection(CollOverview),
		Coastal:  db.Collection(CollCoastal),
		Detail:   db.Collection(CollDetail),
		Skip:     db.Collection(CollSkip),
		LowZoom:  db.Collection(CollLowZoom),
	}
}

// For returns the bucket's collection by enum.
func (c *OSMCollections) For(b MinZoomBucket) *mongo.Collection {
	if c == nil {
		return nil
	}
	switch b {
	case BucketOverview:
		return c.Overview
	case BucketCoastal:
		return c.Coastal
	case BucketDetail:
		return c.Detail
	case BucketSkip:
		return c.Skip
	}
	return nil
}

// tileQueryProjection lists fields the cursor SHOULDN'T return — the
// upsert-only identity columns the runtime renderer never touches.
// Implicit-include semantics keep every other field in the doc
// (class, kind, geometry, bbox, name, ref, roadKind, minZoom,
// minLabelZoom) without us having to re-list them on every ingest
// schema tweak.
var tileQueryProjection = bson.M{
	"_id":       0,
	"region":    0,
	"osmType":   0,
	"osmID":     0,
	"ringIndex": 0,
	// geomLow is the pre-simplified low-zoom geometry; the full-geometry path
	// never needs it and it can be large, so exclude it from transfer. The
	// low-geom path (UseLowGeom) coalesces it into "geometry" via aggregation
	// and uses a different projection.
	"geomLow": 0,
}

// tileQueryProjectionWithID is tileQueryProjection minus the _id exclusion —
// used by the quad fan-out (IncludeID), which dedups on _id.
var tileQueryProjectionWithID = bson.M{
	"region":    0,
	"osmType":   0,
	"osmID":     0,
	"ringIndex": 0,
	"geomLow":   0,
}

// RenderZoomOffset is how many zoom levels we "back off" the minZoom
// filter at chart-detail zooms — at z > 10 we render the feature set
// that would normally appear at z-RenderZoomOffset, which thins out
// residential streets / POIs / minor landuse on tiles where the user
// already has the chart for context. At z ≤ 10 the offset is dropped
// (we filter at the tile's actual zoom) so coastal overviews show the
// natural-zoom feature set rather than artificially-coarsened one.
// See effectiveQueryZoom.
const RenderZoomOffset = 3

// effectiveQueryZoom returns the minZoom threshold we send to Mongo for
// a tile at zoom z. It's max(11, z - RenderZoomOffset):
//
//   - Floor of 11 means every tile at z ≤ 14 renders the z=11 feature
//     set — town labels, secondary roads, residential landuse, parks,
//     etc. all show through. At coastal-overview zooms (z=5..10) that
//     was the missing density: with the previous "effective=z" rule a
//     z=9 tile in rural area looked empty because the GeomMinZoom
//     thresholds for those mid-importance classes are 10..11.
//   - Offset takes over at z ≥ 15 (z-3 ≥ 12), thinning out clutter for
//     chart-detail zooms where the user already has the chart underneath.
//
// Monotonic in z, so zooming in never reduces the visible feature set.
func effectiveQueryZoom(z int) int {
	zoom := z - RenderZoomOffset
	if zoom < 11 {
		zoom = 11
	}
	return zoom
}

// QueryOptions bundles the optional filter knobs for a tile query.
// The zero value (with IncludeMinZoom=true) gives you "features at
// z - RenderZoomOffset or lower" — the same view the live renderer
// uses on every request.
type QueryOptions struct {
	// IncludeMinZoom, when true, adds {minZoom: {$lte: effectiveZoom}}.
	// effectiveZoom is ZoomOverride if it's ≥ 0; otherwise it's the
	// tile's own z minus RenderZoomOffset (clamped to 0). Without
	// this filter a low-zoom tile would pull back every residential
	// street in the bbox.
	IncludeMinZoom bool

	// ZoomOverride forces a specific zoom into the minZoom filter,
	// bypassing the RenderZoomOffset default. Negative means "use
	// the offset-applied default."
	ZoomOverride int

	// Region, if non-empty, restricts to docs whose region field
	// matches. Useful for inspection / dedup queries; in normal
	// rendering we want every region that overlaps so border
	// coverage works.
	Region string

	// Class, if non-empty, restricts to docs whose class field matches.
	// Use for class-specific render paths or debugging.
	Class string

	// Classes, if non-empty, restricts to docs whose class is in the set
	// ($in). Takes precedence over Class. Used by the overview-marker path
	// to pull only admin boundaries + place labels cheaply, skipping the
	// ~200k water/landuse features that make a full low-zoom query huge.
	Classes []string

	// PadBuffer, when true, expands the bbox by LabelBuffer pixels'
	// worth of degrees on each side so the renderer's cross-tile
	// label overdraw has the features it needs. Leave false for
	// plain counting / inspection queries.
	PadBuffer bool

	// UseLowGeom, when true, coalesces the pre-simplified geomLow into
	// "geometry" server-side (via aggregation), falling back to the full
	// geometry for features that have no geomLow. Set at the overview band
	// (z<=LowGeomMaxZoom), where full-resolution polygons make the huge-bbox
	// query too slow. Has no effect on features that were never simplified.
	UseLowGeom bool

	// IncludeID, when true, keeps the document _id in the result (decoded
	// into Feature.ID) so the quad fan-out can dedup features straddling
	// sub-box boundaries. Off by default — _id is dead weight for a plain
	// single-bbox query.
	IncludeID bool
}

// tilePaddedBounds returns the tile's lon/lat bbox, expanded by the label
// buffer when opts.PadBuffer is set. This is THE query bbox — the quad
// fan-out splits exactly this box so its union matches the single query.
func tilePaddedBounds(z, x, y int, opts QueryOptions) (minLon, minLat, maxLon, maxLat float64) {
	minLon, minLat, maxLon, maxLat = TileBoundsLonLat(z, x, y)
	if opts.PadBuffer {
		bufDeg := float64(LabelBuffer) / float64(TileSize) * 360.0 / math.Exp2(float64(z))
		minLon -= bufDeg
		maxLon += bufDeg
		minLat -= bufDeg
		maxLat += bufDeg
	}
	return minLon, minLat, maxLon, maxLat
}

// buildBBoxFilter constructs the bson.M filter for a $geoIntersects query over
// an explicit lon/lat box, with the optional scalar predicates from
// QueryOptions applied on top. z feeds the effectiveQueryZoom/ZoomOverride
// resolution for the minZoom predicate.
func buildBBoxFilter(minLon, minLat, maxLon, maxLat float64, z int, opts QueryOptions) bson.M {
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
	zoom := effectiveQueryZoom(z)
	if opts.ZoomOverride >= 0 {
		zoom = opts.ZoomOverride
	}
	if opts.IncludeMinZoom {
		filter["minZoom"] = bson.M{"$lte": zoom}
	}
	if opts.Region != "" {
		filter["region"] = opts.Region
	}
	if len(opts.Classes) > 0 {
		filter["class"] = bson.M{"$in": opts.Classes}
	} else if opts.Class != "" {
		filter["class"] = opts.Class
	}
	return filter
}

// BuildTileFilter constructs the bson.M filter for the standard
// $geoIntersects-by-tile-bbox query, with the optional scalar
// predicates from QueryOptions applied on top.
func BuildTileFilter(z, x, y int, opts QueryOptions) bson.M {
	minLon, minLat, maxLon, maxLat := tilePaddedBounds(z, x, y, opts)
	return buildBBoxFilter(minLon, minLat, maxLon, maxLat, z, opts)
}

// FetchStats is per-query bookkeeping returned alongside the decoded
// features. Useful for log lines / dev tooling that want to surface
// "this tile pulled N bytes off the wire."
type FetchStats struct {
	Docs       int   // documents returned by the cursor
	BytesRead  int64 // total BSON bytes read for those documents
	DecodeFail int   // docs we couldn't decode and skipped
}

// FetchTileFeatures runs the standard tile-bbox query against a single
// collection and decodes every matching document. Building block for
// the multi-bucket fan-out (FetchTileFeaturesMulti) and the inspection
// subcommands in cmd/osmtools.
func FetchTileFeatures(ctx context.Context, coll *mongo.Collection, z, x, y int, opts QueryOptions) ([]Feature, FetchStats, error) {
	return fetchFiltered(ctx, coll, BuildTileFilter(z, x, y, opts), opts)
}

// fetchFiltered runs one prepared filter against one collection and decodes
// every matching document — the shared core of the tile, quad and inspection
// fetch paths.
func fetchFiltered(ctx context.Context, coll *mongo.Collection, filter bson.M, opts QueryOptions) ([]Feature, FetchStats, error) {
	if coll == nil {
		return nil, FetchStats{}, fmt.Errorf("osmtiler: nil mongo collection")
	}
	var cur *mongo.Cursor
	var err error
	if opts.UseLowGeom {
		// Aggregate so the geomLow→geometry coalesce happens server-side: only
		// the (smaller) chosen geometry transfers, and features without geomLow
		// fall back to their full geometry transparently. Mirrors mapdata/noaa's
		// low-geom query path.
		unset := bson.A{"geomLow", "region", "osmType", "osmID", "ringIndex"}
		if !opts.IncludeID {
			unset = append(unset, "_id")
		}
		pipeline := mongo.Pipeline{
			bson.D{{Key: "$match", Value: filter}},
			bson.D{{Key: "$set", Value: bson.M{"geometry": bson.M{"$ifNull": bson.A{"$geomLow", "$geometry"}}}}},
			bson.D{{Key: "$unset", Value: unset}},
		}
		cur, err = coll.Aggregate(ctx, pipeline)
	} else {
		proj := tileQueryProjection
		if opts.IncludeID {
			proj = tileQueryProjectionWithID
		}
		cur, err = coll.Find(ctx, filter, options.Find().SetProjection(proj))
	}
	if err != nil {
		return nil, FetchStats{}, fmt.Errorf("find: %w", err)
	}
	defer cur.Close(ctx)

	var stats FetchStats
	features := make([]Feature, 0, 64)
	for cur.Next(ctx) {
		stats.Docs++
		stats.BytesRead += int64(len(cur.Current))
		feat, err := DecodeFeature(cur.Current)
		if err != nil {
			// Skip individual decode failures so one bad doc can't
			// black out an entire tile; the rendered tile is the
			// useful artefact and missing a single building rarely
			// matters.
			stats.DecodeFail++
			continue
		}
		features = append(features, feat)
	}
	if err := cur.Err(); err != nil {
		return nil, stats, fmt.Errorf("cursor: %w", err)
	}
	return features, stats, nil
}

// FetchTileFeaturesMulti is the runtime renderer's fetch — fans out
// across the bucket collections that overlap the query zoom, runs the
// fetches in parallel, and returns the merged Feature list with
// per-bucket stats summed. Order of features in the returned slice is
// not stable across calls; the renderer's painter algorithm sorts by
// class anyway, and intra-class draw order was never deterministic
// from a single-collection cursor either.
func FetchTileFeaturesMulti(ctx context.Context, colls *OSMCollections, z, x, y int, opts QueryOptions) ([]Feature, FetchStats, error) {
	if colls == nil {
		return nil, FetchStats{}, fmt.Errorf("osmtiler: nil OSMCollections")
	}
	// Decide which buckets to consult from the effective zoom — we use
	// the same effectiveQueryZoom as the per-collection filter, so a
	// ZoomOverride trickles through consistently.
	zoom := effectiveQueryZoom(z)
	if opts.ZoomOverride >= 0 {
		zoom = opts.ZoomOverride
	}
	buckets := bucketsForQueryZoom(zoom)

	// Fan out: one goroutine per bucket. Each goroutine runs an
	// independent Mongo Find; the driver pool handles concurrency.
	type result struct {
		feats []Feature
		stats FetchStats
		err   error
	}
	results := make([]result, len(buckets))
	var wg sync.WaitGroup
	for i, b := range buckets {
		coll := colls.For(b)
		if coll == nil {
			continue
		}
		wg.Add(1)
		go func(i int, coll *mongo.Collection) {
			defer wg.Done()
			feats, stats, err := FetchTileFeatures(ctx, coll, z, x, y, opts)
			results[i] = result{feats: feats, stats: stats, err: err}
		}(i, coll)
	}
	wg.Wait()

	// Aggregate. A single bucket erroring shouldn't black out the whole
	// tile (the other buckets' features are still useful), so we record
	// the first error but keep the partial result.
	var firstErr error
	var total FetchStats
	merged := make([]Feature, 0, 64)
	for i, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", buckets[i].CollectionName(), r.err)
		}
		total.Docs += r.stats.Docs
		total.BytesRead += r.stats.BytesRead
		total.DecodeFail += r.stats.DecodeFail
		merged = append(merged, r.feats...)
	}
	return merged, total, firstErr
}

// quadBoxes splits a lon/lat box into its 2×2 equal sub-boxes. The sub-boxes
// tile the parent exactly, so the union of their $geoIntersects result sets
// equals the parent's (features straddling the midlines appear in more than
// one — hence the _id dedup in the quad fetchers).
func quadBoxes(minLon, minLat, maxLon, maxLat float64) [4][4]float64 {
	midLon := (minLon + maxLon) / 2
	midLat := (minLat + maxLat) / 2
	return [4][4]float64{
		{minLon, minLat, midLon, midLat},
		{midLon, minLat, maxLon, midLat},
		{minLon, midLat, midLon, maxLat},
		{midLon, midLat, maxLon, maxLat},
	}
}

// dedupFeaturesByID drops duplicate features (same non-empty ID), keeping the
// first occurrence. Quad sub-box results are concatenated in fixed quadrant
// order before this runs, so the surviving copy is deterministic.
func dedupFeaturesByID(feats []Feature) []Feature {
	seen := make(map[string]struct{}, len(feats))
	out := feats[:0]
	for _, f := range feats {
		if f.ID != "" {
			if _, dup := seen[f.ID]; dup {
				continue
			}
			seen[f.ID] = struct{}{}
		}
		out = append(out, f)
	}
	return out
}

// FetchTileFeaturesQuad is FetchTileFeatures with the tile's padded bbox split
// into 2×2 sub-boxes queried concurrently. Mongo executes each $geoIntersects
// scan on one thread, so a huge overview bbox is wall-clock-bound by a single
// core; four quarter-scans run in parallel for the same total work (z7
// overview tile: ~2.6s → ~1.4s). Results are merged in fixed quadrant order
// and deduped by _id, so the feature SET matches the single query exactly.
func FetchTileFeaturesQuad(ctx context.Context, coll *mongo.Collection, z, x, y int, opts QueryOptions) ([]Feature, FetchStats, error) {
	minLon, minLat, maxLon, maxLat := tilePaddedBounds(z, x, y, opts)
	boxes := quadBoxes(minLon, minLat, maxLon, maxLat)
	opts.IncludeID = true

	type result struct {
		feats []Feature
		stats FetchStats
		err   error
	}
	results := make([]result, len(boxes))
	var wg sync.WaitGroup
	for i, b := range boxes {
		wg.Add(1)
		go func(i int, b [4]float64) {
			defer wg.Done()
			feats, stats, err := fetchFiltered(ctx, coll, buildBBoxFilter(b[0], b[1], b[2], b[3], z, opts), opts)
			results[i] = result{feats: feats, stats: stats, err: err}
		}(i, b)
	}
	wg.Wait()

	var firstErr error
	var total FetchStats
	merged := make([]Feature, 0, 64)
	for _, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		total.Docs += r.stats.Docs
		total.BytesRead += r.stats.BytesRead
		total.DecodeFail += r.stats.DecodeFail
		merged = append(merged, r.feats...)
	}
	return dedupFeaturesByID(merged), total, firstErr
}

// FetchTileFeaturesMultiQuad is FetchTileFeaturesMulti with the quad split of
// FetchTileFeaturesQuad layered on: every (bucket × quadrant) pair queries
// concurrently, results merge in fixed bucket-major order and dedup by _id.
// Use for the big overview-band scans; at chart-detail zoom the plain Multi
// is already fast and the split is pointless overhead.
func FetchTileFeaturesMultiQuad(ctx context.Context, colls *OSMCollections, z, x, y int, opts QueryOptions) ([]Feature, FetchStats, error) {
	if colls == nil {
		return nil, FetchStats{}, fmt.Errorf("osmtiler: nil OSMCollections")
	}
	zoom := effectiveQueryZoom(z)
	if opts.ZoomOverride >= 0 {
		zoom = opts.ZoomOverride
	}
	buckets := bucketsForQueryZoom(zoom)
	minLon, minLat, maxLon, maxLat := tilePaddedBounds(z, x, y, opts)
	boxes := quadBoxes(minLon, minLat, maxLon, maxLat)
	opts.IncludeID = true

	type result struct {
		feats []Feature
		stats FetchStats
		err   error
	}
	results := make([]result, len(buckets)*len(boxes))
	var wg sync.WaitGroup
	for bi, b := range buckets {
		coll := colls.For(b)
		if coll == nil {
			continue
		}
		for qi, box := range boxes {
			wg.Add(1)
			go func(slot int, coll *mongo.Collection, box [4]float64) {
				defer wg.Done()
				feats, stats, err := fetchFiltered(ctx, coll, buildBBoxFilter(box[0], box[1], box[2], box[3], z, opts), opts)
				results[slot] = result{feats: feats, stats: stats, err: err}
			}(bi*len(boxes)+qi, coll, box)
		}
	}
	wg.Wait()

	// Same partial-result convention as FetchTileFeaturesMulti: record the
	// first error but keep what the other scans returned.
	var firstErr error
	var total FetchStats
	merged := make([]Feature, 0, 64)
	for slot, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", buckets[slot/len(boxes)].CollectionName(), r.err)
		}
		total.Docs += r.stats.Docs
		total.BytesRead += r.stats.BytesRead
		total.DecodeFail += r.stats.DecodeFail
		merged = append(merged, r.feats...)
	}
	return dedupFeaturesByID(merged), total, firstErr
}

// DecodeFeature turns a raw BSON feature document (the shape written
// by cmd/osmtools ingest) into the in-memory Feature the renderer
// expects. Exported so cmd/osmtools gentile and the runtime renderer
// can share the same conversion path.
func DecodeFeature(raw bson.Raw) (Feature, error) {
	var d struct {
		ID           string            `bson:"_id"`
		Class        string            `bson:"class"`
		Kind         string            `bson:"kind"`
		Name         string            `bson:"name"`
		Ref          string            `bson:"ref"`
		RoadKind     string            `bson:"roadKind"`
		MinZoom      int               `bson:"minZoom"`
		MinLabelZoom int               `bson:"minLabelZoom"`
		BBox         []float64         `bson:"bbox"`
		Tags         map[string]string `bson:"tags"`
		Geometry     struct {
			Type        string `bson:"type"`
			Coordinates bson.A `bson:"coordinates"`
		} `bson:"geometry"`
	}
	if err := bson.Unmarshal(raw, &d); err != nil {
		return Feature{}, fmt.Errorf("unmarshal: %w", err)
	}
	coords, err := coordsFromGeoJSON(d.Geometry.Type, d.Geometry.Coordinates)
	if err != nil {
		return Feature{}, err
	}
	feat := Feature{
		ID:           d.ID,
		Class:        ClassFromString(d.Class),
		Kind:         GeomKindFromString(d.Kind),
		Coords:       coords,
		Name:         d.Name,
		Ref:          d.Ref,
		MinZoom:      uint8(d.MinZoom),
		MinLabelZoom: uint8(d.MinLabelZoom),
		RoadKind:     RoadKindFromString(d.RoadKind),
		Tags:         d.Tags,
	}
	if len(d.BBox) == 4 {
		feat.MinLon = d.BBox[0]
		feat.MinLat = d.BBox[1]
		feat.MaxLon = d.BBox[2]
		feat.MaxLat = d.BBox[3]
	}
	return feat, nil
}

func coordsFromGeoJSON(typ string, raw bson.A) ([]LonLat, error) {
	switch typ {
	case "Point":
		ll, err := xyFromArray(raw)
		if err != nil {
			return nil, fmt.Errorf("Point: %w", err)
		}
		return []LonLat{ll}, nil
	case "LineString":
		return ringFromArray(raw, "LineString")
	case "Polygon":
		if len(raw) == 0 {
			return nil, nil
		}
		outer, ok := raw[0].(bson.A)
		if !ok {
			return nil, fmt.Errorf("Polygon: outer ring not an array")
		}
		return ringFromArray(outer, "Polygon outer")
	}
	return nil, fmt.Errorf("unsupported geometry type %q", typ)
}

func ringFromArray(arr bson.A, what string) ([]LonLat, error) {
	out := make([]LonLat, 0, len(arr))
	for i, v := range arr {
		pa, ok := v.(bson.A)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: not a coord array", what, i)
		}
		ll, err := xyFromArray(pa)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", what, i, err)
		}
		out = append(out, ll)
	}
	return out, nil
}

func xyFromArray(arr bson.A) (LonLat, error) {
	if len(arr) < 2 {
		return LonLat{}, fmt.Errorf("expected [lon, lat], got %d elements", len(arr))
	}
	lon, lonOK := arr[0].(float64)
	lat, latOK := arr[1].(float64)
	if !lonOK || !latOK {
		return LonLat{}, fmt.Errorf("expected [float64, float64], got %T, %T", arr[0], arr[1])
	}
	return LonLat{Lon: lon, Lat: lat}, nil
}

// ClassFromString is the inverse of Class.String(). Lives next to
// DecodeFeature so callers parsing the on-disk format have a single
// place to look.
func ClassFromString(s string) Class {
	switch s {
	case "road":
		return ClassRoad
	case "building":
		return ClassBuilding
	case "landuse":
		return ClassLanduse
	case "leisure":
		return ClassLeisure
	case "natural":
		return ClassNatural
	case "place":
		return ClassPlace
	case "poi":
		return ClassPOI
	case "admin":
		return ClassAdmin
	case "railway":
		return ClassRailway
	case "aeroway":
		return ClassAeroway
	case "water":
		return ClassWater
	}
	return ClassSkip
}

// GeomKindFromString is the inverse of the lowercase strings the
// ingest tool writes ("point", "line", "polygon").
func GeomKindFromString(s string) GeomKind {
	switch s {
	case "point":
		return GeomPoint
	case "line":
		return GeomLine
	case "polygon":
		return GeomPolygon
	}
	return GeomPoint
}

// RoadKindFromString is the inverse of the lowercase strings the
// ingest tool writes for road sub-classes.
func RoadKindFromString(s string) RoadKind {
	switch s {
	case "motorway":
		return RoadMotorway
	case "trunk":
		return RoadTrunk
	case "primary":
		return RoadPrimary
	case "secondary":
		return RoadSecondary
	case "tertiary":
		return RoadTertiary
	case "residential":
		return RoadResidential
	case "service":
		return RoadService
	case "path":
		return RoadPath
	}
	return RoadUnknown
}
