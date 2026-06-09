// mapsync is a multi-purpose CLI for the chartplotter's OSM data
// pipeline. Today it has one subcommand:
//
//	mapsync ingest --pbf <path> --mongo <uri> --region <key>
//
// More subcommands (e.g. region inspection, tile prerender, mongo
// query helpers) can be added to the dispatch table in main().
//
// Usage examples:
//
//	mapsync ingest --pbf /tmp/NewYork.osm.pbf \
//	                --mongo mongodb://localhost:27017 \
//	                --region us-new-york
//
//	mapsync ingest --pbf /tmp/NewYork.osm.pbf \
//	                --mongo "mongodb+srv://user:pass@cluster.example.net/?retryWrites=true" \
//	                --db osm --coll features \
//	                --region us-new-york
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

func main() {
	if len(os.Args) < 2 {
		topUsage()
		os.Exit(2)
	}
	sub, args := os.Args[1], os.Args[2:]
	switch sub {
	case "ingest":
		if err := runIngest(args); err != nil {
			fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
			os.Exit(1)
		}
	case "query":
		if err := runQuery(args); err != nil {
			fmt.Fprintf(os.Stderr, "query: %v\n", err)
			os.Exit(1)
		}
	case "gentile":
		if err := runGenTile(args); err != nil {
			fmt.Fprintf(os.Stderr, "gentile: %v\n", err)
			os.Exit(1)
		}
	case "backfill-geomlow":
		if err := runBackfillGeomLow(args); err != nil {
			fmt.Fprintf(os.Stderr, "backfill-geomlow: %v\n", err)
			os.Exit(1)
		}
	case "backfill-lowzoom":
		if err := runBackfillLowZoom(args); err != nil {
			fmt.Fprintf(os.Stderr, "backfill-lowzoom: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		topUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n", sub)
		topUsage()
		os.Exit(2)
	}
}

func topUsage() {
	fmt.Fprintln(os.Stderr, "Usage: mapsync <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  OSM (osm_* collections):")
	fmt.Fprintln(os.Stderr, "    ingest        Read a .osm.pbf and upsert kept features into MongoDB")
	fmt.Fprintln(os.Stderr, "    query         Show + count the features a given tile would query for")
	fmt.Fprintln(os.Stderr, "    gentile       Render a tile PNG by querying the MongoDB collection")
	fmt.Fprintln(os.Stderr, "    backfill-geomlow  Add simplified geomLow to existing osm_overview/osm_coastal docs (no re-ingest)")
	fmt.Fprintln(os.Stderr, "    backfill-lowzoom  Build the curated osm_lowzoom collection (z7/z8 band) from existing docs")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "NOAA ENC ingest lives in the datasync binary (see `make ingest-noaa`).")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, `Run "mapsync <subcommand> --help" for subcommand flags.`)
}

// ----- ingest --------------------------------------------------------------

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	pbfFlag := fs.String("pbf", "",
		"path to input .osm.pbf (kept for back-compat; you can also pass PBF paths as positional args)")
	mongoURI := fs.String("mongo", "", "MongoDB connection URI (required)")
	dbName := fs.String("db", "osm", "MongoDB database name")
	// --coll is gone — we now write to fixed per-bucket collection names
	// (osm_overview/coastal/detail/skip). The bucket split is the
	// whole point of this rev; one collection-name knob would mean either
	// per-bucket prefixes (operational noise) or breaking back-compat
	// silently (worse). Easier to make it explicit.
	region := fs.String("region", "",
		"region key recorded on every document (only valid with a single PBF; multi-file mode derives one region per file)")
	batchSize := fs.Int("batch", 1000, "bulk upsert batch size")
	procs := fs.Int("procs", 0,
		"PBF decoder workers per file (default: runtime.NumCPU() / workers, min 1)")
	workers := fs.Int("workers", 0,
		"concurrent PBF ingest workers when multiple PBFs are given (default min(2, len(paths)); tune up for larger boxes)")
	force := fs.Bool("force", false,
		"re-ingest even when an ingest-meta doc says this region's PBF hash already matches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mongoURI == "" {
		fs.Usage()
		return fmt.Errorf("--mongo is required")
	}

	// Collect PBF paths from --pbf (back-compat) plus positional args.
	// Positional usage: `mapsync ingest --mongo ... a.pbf b.pbf c.pbf`.
	var pbfPaths []string
	if *pbfFlag != "" {
		pbfPaths = append(pbfPaths, *pbfFlag)
	}
	pbfPaths = append(pbfPaths, fs.Args()...)
	if len(pbfPaths) == 0 {
		fs.Usage()
		return fmt.Errorf("no PBF paths given; pass with --pbf or as positional args")
	}
	if *region != "" && len(pbfPaths) > 1 {
		return fmt.Errorf("--region is only valid with a single PBF; with %d files each region is derived from the filename",
			len(pbfPaths))
	}

	if *workers <= 0 {
		*workers = 2
		if *workers > len(pbfPaths) {
			*workers = len(pbfPaths)
		}
	}
	if *procs <= 0 {
		// Default per-file decoder thread count. With N workers running
		// in parallel and the same NumCPU as before, we'd oversubscribe
		// massively; divide so total active threads ≈ NumCPU.
		*procs = runtime.NumCPU() / *workers
		if *procs < 1 {
			*procs = 1
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One Mongo connection, shared across all worker goroutines. The
	// driver pool is goroutine-safe and lets us amortise the setup cost
	// (connect + ensureFeatureIndexes is non-trivial against a remote
	// Mongo). Workers each grab the same *mongo.Collection.
	connectCtx, connectCancel := context.WithTimeout(ctx, 15*time.Second)
	defer connectCancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()
	if err := client.Ping(connectCtx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	db := client.Database(*dbName)
	colls := osmtiler.OpenOSMCollections(db)
	fmt.Fprintf(os.Stderr, "connected to %s (%s/%s/%s/%s)\n",
		*dbName, osmtiler.CollOverview, osmtiler.CollCoastal, osmtiler.CollDetail, osmtiler.CollSkip)
	for _, b := range []osmtiler.MinZoomBucket{
		osmtiler.BucketOverview, osmtiler.BucketCoastal, osmtiler.BucketDetail, osmtiler.BucketSkip,
	} {
		if err := ensureFeatureIndexes(ctx, colls.For(b), b); err != nil {
			return fmt.Errorf("ensure indexes %s: %w", b.CollectionName(), err)
		}
	}
	// The curated low-zoom collection (z7/z8 band) gets the same render index.
	if _, err := colls.LowZoom.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "geometry", Value: "2dsphere"},
			{Key: "minZoom", Value: 1},
			{Key: "class", Value: 1},
		},
		Options: options.Index().SetName("geo_minZoom_class"),
	}); err != nil {
		return fmt.Errorf("ensure indexes %s: %w", osmtiler.CollLowZoom, err)
	}

	// Per-file ingest opts. Derive a region key from each filename
	// unless the caller explicitly supplied one (single-file only).
	jobs := make([]ingestOpts, len(pbfPaths))
	multi := len(pbfPaths) > 1
	for i, p := range pbfPaths {
		jobOpts := ingestOpts{
			pbfPath:   p,
			mongoURI:  *mongoURI,
			dbName:    *dbName,
			batchSize: *batchSize,
			procs:     *procs,
			force:     *force,
		}
		if *region != "" {
			jobOpts.region = *region
		} else {
			jobOpts.region = deriveRegionFromPBF(p)
		}
		if multi {
			jobOpts.out = &prefixWriter{prefix: jobOpts.region}
		}
		jobs[i] = jobOpts
	}

	fmt.Fprintf(os.Stderr, "ingesting %d PBF(s) with up to %d workers (%d decoder threads each)\n",
		len(jobs), *workers, *procs)

	// Worker pool: cap concurrency at *workers, surface the first
	// error so we don't keep churning on the rest, but let in-flight
	// workers drain rather than hard-cancel mid-batch (a hard cancel
	// would leave a partial ingest-meta record that could confuse a
	// later re-run).
	sem := make(chan struct{}, *workers)
	var wg sync.WaitGroup
	errs := make([]error, len(jobs))
	for i := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[idx] = ingest(ctx, colls, jobs[idx])
		}(i)
	}
	wg.Wait()

	// Aggregate. Return the first failure; print the rest so the
	// operator can see them all in one go.
	var firstErr error
	for i, e := range errs {
		if e == nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "[%s] FAILED: %v\n", jobs[i].region, e)
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", jobs[i].region, e)
		}
	}
	return firstErr
}

// deriveRegionFromPBF turns "europe/germany-latest.osm.pbf" into
// "germany". Strips the directory, the ".osm.pbf"/".pbf" extension, and
// the Geofabrik convention "-latest" suffix.
func deriveRegionFromPBF(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".osm.pbf")
	base = strings.TrimSuffix(base, ".pbf")
	base = strings.TrimSuffix(base, "-latest")
	return base
}

type ingestOpts struct {
	pbfPath   string
	mongoURI  string
	dbName    string
	region    string
	batchSize int
	procs     int
	force     bool
	// out receives all human-readable progress log lines. nil falls
	// back to os.Stderr so the single-file path keeps its old shape.
	// Parallel multi-file mode wires a region-prefixed writer here so
	// the operator can tell whose progress is whose at a glance.
	out io.Writer
}

func (o ingestOpts) writer() io.Writer {
	if o.out != nil {
		return o.out
	}
	return os.Stderr
}

// stderrLineMu serialises whole-line writes to os.Stderr across
// prefixWriters from different workers, so a long progress line from
// region A never interleaves with one from region B at the byte level.
// Each prefixWriter still has its own per-goroutine partial-line buffer.
var stderrLineMu sync.Mutex

// prefixWriter prepends "[<prefix>] " to every newline-terminated line
// written into it before forwarding to os.Stderr under stderrLineMu.
// Partial trailing lines are buffered until a newline arrives.
type prefixWriter struct {
	prefix string
	buf    []byte
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	pw.buf = append(pw.buf, p...)
	for {
		i := -1
		for j, b := range pw.buf {
			if b == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		line := pw.buf[:i+1]
		stderrLineMu.Lock()
		_, _ = os.Stderr.WriteString("[" + pw.prefix + "] ")
		_, _ = os.Stderr.Write(line)
		stderrLineMu.Unlock()
		pw.buf = pw.buf[i+1:]
	}
	return len(p), nil
}

// featureDoc is the BSON shape we write to MongoDB. _id is a stable
// composite of (region, OSM type, OSM id [, ring index]) so a second
// ingest of a refreshed PBF upserts in place rather than duplicating.
//
// The Geometry field is GeoJSON-shaped so callers can drop a 2dsphere
// index on it and use $geoIntersects to query features for a tile bbox
// without rebuilding any of this in application code.
type featureDoc struct {
	ID           string            `bson:"_id"`
	Region       string            `bson:"region"`
	OSMType      string            `bson:"osmType"`
	OSMID        int64             `bson:"osmID"`
	RingIndex    int               `bson:"ringIndex,omitempty"`
	Class        string            `bson:"class"`
	Kind         string            `bson:"kind"`
	Name         string            `bson:"name,omitempty"`
	Ref          string            `bson:"ref,omitempty"`
	RoadKind     string            `bson:"roadKind,omitempty"`
	MinZoom      int               `bson:"minZoom"`
	MinLabelZoom int               `bson:"minLabelZoom"`
	BBox         [4]float64        `bson:"bbox"` // [minLon, minLat, maxLon, maxLat]
	Geometry     interface{}       `bson:"geometry"`
	// GeomLow is the geometry pre-simplified (Douglas–Peucker) to ~1px at
	// osmtiler.LowGeomMaxZoom, set only for line/polygon features whose
	// simplification actually drops vertices. The overview-band query path
	// coalesces it into "geometry" so z7..z11 land-cover renders don't choke on
	// full-resolution forest polygons. nil (omitted) → query falls back to
	// the full Geometry.
	GeomLow interface{}       `bson:"geomLow,omitempty"`
	Tags    map[string]string `bson:"tags,omitempty"`
}

// lowGeomFor returns the simplified low-zoom geometry for a line/polygon's
// coords, or nil when simplification wouldn't help (point geometry, or no
// vertices dropped, or the result would degenerate below a valid line/ring).
func lowGeomFor(kind string, coords []osmtiler.LonLat) interface{} {
	if kind != "line" && kind != "polygon" {
		return nil
	}
	low, reduced := osmtiler.SimplifyLonLat(coords, osmtiler.LowGeomTolerance)
	if !reduced {
		return nil
	}
	minPts := 2
	if kind == "polygon" {
		minPts = 4
	}
	if len(low) < minPts {
		return nil
	}
	return geometryForRing(kind, low)
}

// ingest processes a single PBF into the four bucket collections.
// Caller owns the Mongo connection and is responsible for having
// already run ensureFeatureIndexes on each bucket — both are one-shot
// setup that's shared across all PBFs in a parallel multi-file ingest.
func ingest(ctx context.Context, colls *osmtiler.OSMCollections, opts ingestOpts) error {
	out := opts.writer()

	// Hash the PBF and check ingest-meta — if a previous ingest
	// of this region recorded the same hash and the collections
	// still have roughly the expected number of feature docs,
	// there's nothing new to write. Saves ~minutes per re-run.
	hashStart := time.Now()
	pbfHash, pbfSize, err := hashFile(opts.pbfPath)
	if err != nil {
		return fmt.Errorf("hash pbf: %w", err)
	}
	fmt.Fprintf(out, "pbf hash: sha256:%s… (%s, %s)\n",
		pbfHash[:12], humanBytes(pbfSize), time.Since(hashStart).Round(time.Millisecond))

	if !opts.force {
		if skip, why := shouldSkipIngest(ctx, colls, opts.region, pbfHash); skip {
			fmt.Fprintf(out, "skip: %s\n", why)
			return nil
		} else if why != "" {
			fmt.Fprintf(out, "re-ingest: %s\n", why)
		}
	}

	// Pass 1 — relations only. Identify the multipolygons we'll
	// later emit, plus the way IDs they reference so we know to
	// keep their geometry in pass 2.
	relPass, memberWays, err := scanRelations(ctx, opts.pbfPath, opts.procs)
	if err != nil {
		return fmt.Errorf("relations pass: %w", err)
	}
	fmt.Fprintf(out, "pass 1: %d multipolygon relations, %d member way ids\n",
		len(relPass), len(memberWays))

	// Pass 2 — nodes + ways. Emit node POIs / way features directly;
	// stash coords for relation members. After pass 2 we stitch and
	// emit the multipolygon features.
	w := newBucketRouter(colls, opts.batchSize, out)
	memberCoords, err := scanNodesAndWays(ctx, opts, memberWays, w)
	if err != nil {
		return fmt.Errorf("nodes/ways pass: %w", err)
	}
	fmt.Fprintf(out, "pass 2: emitted %d node/way features, kept %d member way geometries\n",
		w.emitted(), len(memberCoords))

	if err := emitRelations(ctx, opts, relPass, memberCoords, w); err != nil {
		return fmt.Errorf("emit relations: %w", err)
	}
	fmt.Fprintf(out, "emitted %d total features\n", w.emitted())

	if err := w.flush(ctx); err != nil {
		return fmt.Errorf("final flush: %w", err)
	}
	if be := w.bulkErrors(); be > 0 {
		fmt.Fprintf(out, "done: %d upserts (%d batches, %d docs rejected by server)\n",
			w.upserted(), w.batches(), be)
	} else {
		fmt.Fprintf(out, "done: %d upserts (%d batches)\n", w.upserted(), w.batches())
	}

	// Persist the ingest-meta so a future run on the same PBF can
	// short-circuit. Count the actual region docs across all four
	// bucket collections (BulkWrite Ordered=false can have left a few
	// behind in any bucket) so the skip-tolerance check has a true
	// baseline. The meta doc itself lives in the overview collection
	// (any single one would do — overview is just the convention).
	actualCount, err := countRegionDocs(ctx, colls, opts.region)
	if err != nil {
		return fmt.Errorf("post-count: %w", err)
	}
	if err := writeIngestMeta(ctx, colls.Overview, opts.region, pbfHash, pbfSize, actualCount); err != nil {
		return fmt.Errorf("write ingest meta: %w", err)
	}
	fmt.Fprintf(out, "ingest-meta: region=%s hash=%s… docs=%d\n",
		opts.region, pbfHash[:12], actualCount)
	return nil
}

// countRegionDocs sums the per-bucket doc counts for the given region.
func countRegionDocs(ctx context.Context, colls *osmtiler.OSMCollections, region string) (int64, error) {
	var total int64
	for _, b := range []osmtiler.MinZoomBucket{
		osmtiler.BucketOverview, osmtiler.BucketCoastal, osmtiler.BucketDetail, osmtiler.BucketSkip,
	} {
		n, err := colls.For(b).CountDocuments(ctx, bson.M{"region": region})
		if err != nil {
			return 0, fmt.Errorf("%s: %w", b.CollectionName(), err)
		}
		total += n
	}
	return total, nil
}

// ----- ingest-meta helpers -------------------------------------------------

// ingestMetaID returns the _id we use for the per-region meta doc.
// Lives in the same collection as the features so we don't have to
// configure / index a second collection; the `_ingest_meta:` prefix
// guarantees no collision with feature documents (whose ids look
// like "<region>:<osmType>:<osmID>[:<ring>]").
func ingestMetaID(region string) string { return "_ingest_meta:" + region }

// shouldSkipIngest returns (true, why) when the recorded meta says
// this PBF has already been fully ingested and the bucket collections
// still hold ~the expected number of region docs. Returns (false, why)
// when we recognize the region but think it needs re-ingest, with
// `why` describing the reason. (false, "") means no prior meta.
//
// The meta lives in the overview collection by convention (any single
// one would do); the doc count is summed across all four buckets.
func shouldSkipIngest(ctx context.Context, colls *osmtiler.OSMCollections, region, pbfHash string) (bool, string) {
	var meta struct {
		PBFHash  string `bson:"pbfHash"`
		DocCount int64  `bson:"docCount"`
	}
	err := colls.Overview.FindOne(ctx, bson.M{"_id": ingestMetaID(region)}).Decode(&meta)
	if err != nil {
		return false, ""
	}
	if meta.PBFHash != pbfHash {
		return false, fmt.Sprintf("PBF hash changed (was %s… now %s…)",
			truncHash(meta.PBFHash), truncHash(pbfHash))
	}
	actual, err := countRegionDocs(ctx, colls, region)
	if err != nil {
		return false, fmt.Sprintf("count docs: %v", err)
	}
	// Tolerate up to 5% missing (manual cleanup, partial bulk-write
	// rejects, etc.). Below that we re-ingest to refill.
	minOK := meta.DocCount * 95 / 100
	if actual < minOK {
		return false, fmt.Sprintf("doc count too low (have %d, expected ~%d, recorded %d)",
			actual, minOK, meta.DocCount)
	}
	return true, fmt.Sprintf("already ingested: hash sha256:%s… matches, %d docs across buckets (recorded %d)",
		truncHash(pbfHash), actual, meta.DocCount)
}

// writeIngestMeta upserts the meta document for one region.
func writeIngestMeta(ctx context.Context, coll *mongo.Collection, region, pbfHash string, pbfSize, docCount int64) error {
	doc := bson.M{
		"_id":        ingestMetaID(region),
		"region":     region,
		"pbfHash":    pbfHash,
		"pbfSize":    pbfSize,
		"docCount":   docCount,
		"ingestedAt": time.Now(),
	}
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": doc["_id"]},
		bson.M{"$set": doc},
		options.Update().SetUpsert(true))
	return err
}

// hashFile streams a SHA-256 over the file at path and returns the
// hex digest plus the number of bytes read. Used to detect "is this
// PBF byte-identical to the one we ingested before?"
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func truncHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ----- PBF walk ------------------------------------------------------------

type relDesc struct {
	ID        osm.RelationID
	Class     osmtiler.Class
	Name      string
	Tags      osm.Tags
	OuterWays []osm.WayID
}

func scanRelations(ctx context.Context, pbfPath string, procs int) ([]relDesc, map[osm.WayID]struct{}, error) {
	f, err := os.Open(pbfPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	sc := osmpbf.New(ctx, f, procs)
	sc.SkipNodes = true
	sc.SkipWays = true
	sc.FilterRelation = func(r *osm.Relation) bool {
		// Keep every multipolygon AND every boundary relation. We used to
		// gate on the class classifier here, which is what made adding a new
		// render rule a re-ingest event — every refinement we've ever done
		// needed the relation pass re-run. Storing all of these is the
		// "ingest cleanly, no filtering" rule from OSM_TILES_PLAN.md;
		// classification happens at render time from the stored tag map.
		//
		// boundary relations matter for admin lines: a state/county border's
		// geometry lives in the relation (type=boundary), and its member ways
		// are usually untagged or carry only a lower (county) admin_level — so
		// borders defined relation-only, like the NY state line inland, never
		// got ingested. They're stored as ClassAdmin polygons (boundary=
		// administrative → ClassAdmin); the renderer strokes the ring as the
		// border line (no fill).
		t := r.Tags.Find("type")
		return t == "multipolygon" || t == "boundary"
	}
	defer sc.Close()

	var out []relDesc
	members := map[osm.WayID]struct{}{}
	for sc.Scan() {
		r, ok := sc.Object().(*osm.Relation)
		if !ok {
			continue
		}
		rd := relDesc{
			ID:    r.ID,
			Class: osmtiler.Classify(r.Tags),
			Name:  r.Tags.Find("name"),
			Tags:  r.Tags,
		}
		for _, m := range r.Members {
			if m.Type != osm.TypeWay {
				continue
			}
			if m.Role != "outer" && m.Role != "" {
				continue
			}
			wid := osm.WayID(m.Ref)
			rd.OuterWays = append(rd.OuterWays, wid)
			members[wid] = struct{}{}
		}
		if len(rd.OuterWays) > 0 {
			out = append(out, rd)
		}
	}
	return out, members, sc.Err()
}

// scanNodesAndWays drives pass 2 of the ingest. To bound memory on
// state-sized PBFs we split it into three sub-passes through the file,
// each filtering down to a single element type:
//
//  1. ways → collect the set of node IDs the kept-ways actually
//     reference (`neededNodes`). Without this filter we'd be forced to
//     buffer every node coord in the PBF (most are unused); for
//     California that's a ~3 GB map per worker. Pre-filtering trims
//     it to roughly the half that's actually referenced.
//  2. nodes → store only `neededNodes` coords in a packed `nodeStore`,
//     and emit POI docs for tagged nodes as we go. Untagged nodes that
//     aren't in `neededNodes` are simply dropped on the floor.
//  3. ways again → with the now-populated nodeStore, build per-way
//     coord slices and emit way docs + relation `memberCoords`.
//
// The extra two PBF reads cost wall-clock (~30s per pass on a 1 GB
// extract from local disk), but pay for themselves many times over in
// reduced RAM peak — which matters more for parallel ingest, where
// peak × workers is the figure that fits in physical memory.
//
// `memberWays` is the set of way IDs the relations pass flagged as
// outer-ring members; we keep those + every tagged way.
func scanNodesAndWays(ctx context.Context, opts ingestOpts, memberWays map[osm.WayID]struct{}, w *bucketRouter) (map[osm.WayID][]osmtiler.LonLat, error) {
	out := opts.writer()

	// Predicate shared by passes 1 and 3: do we want this way's
	// geometry? Both passes need exactly the same set so the per-way
	// node IDs we collected match the per-way coords we'll resolve.
	keepWay := func(wy *osm.Way) bool {
		if _, ok := memberWays[wy.ID]; ok {
			return true
		}
		return len(wy.Tags) > 0
	}

	info, statErr := os.Stat(opts.pbfPath)
	if statErr == nil {
		fmt.Fprintf(out, "pass 2: scanning %.1f MB of PBF (3 sub-passes for memory budget)\n",
			float64(info.Size())/(1<<20))
	}

	// --- pass 2a: walk ways, collect needed node IDs --------------
	neededNodes := map[osm.NodeID]struct{}{}
	if err := withWaysOnlyScanner(ctx, opts, keepWay, func(wy *osm.Way) error {
		for _, n := range wy.Nodes {
			neededNodes[n.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("pass 2a (way node ids): %w", err)
	}
	fmt.Fprintf(out, "pass 2a: %d distinct nodes referenced by kept ways\n", len(neededNodes))

	// --- pass 2b: walk nodes, store the ones we need + emit POIs --
	nodes := newNodeStore()
	nodes.hint(len(neededNodes))
	if err := withNodesOnlyScanner(ctx, opts, func(n *osm.Node) bool {
		// Keep the node if it's referenced by a kept way OR carries
		// tags (every tagged node becomes a POI doc). Either way the
		// decoder hands it back to us; we decide what to do with it
		// in the body below.
		if _, need := neededNodes[n.ID]; need {
			return true
		}
		return len(n.Tags) > 0
	}, func(n *osm.Node) error {
		if _, need := neededNodes[n.ID]; need {
			nodes.Set(n.ID, n.Lon, n.Lat)
		}
		if len(n.Tags) == 0 {
			return nil
		}
		doc := nodeDoc(opts.region, n, osmtiler.Classify(n.Tags))
		return w.upsert(ctx, doc)
	}); err != nil {
		return nil, fmt.Errorf("pass 2b (node store): %w", err)
	}
	fmt.Fprintf(out, "pass 2b: stored %d node coords\n", nodes.Len())

	// Drop the needed-set now that nodeStore is populated — saves
	// ~16 bytes per node ID before we go into pass 2c.
	neededNodes = nil

	// --- pass 2c: walk ways again, emit docs and memberCoords ------
	memberCoords := map[osm.WayID][]osmtiler.LonLat{}
	if err := withWaysOnlyScanner(ctx, opts, keepWay, func(e *osm.Way) error {
		coords := make([]osmtiler.LonLat, 0, len(e.Nodes))
		for _, n := range e.Nodes {
			p, ok := nodes.Get(n.ID)
			if !ok {
				continue
			}
			coords = append(coords, p)
		}
		if _, want := memberWays[e.ID]; want && len(coords) >= 2 {
			memberCoords[e.ID] = coords
		}
		if len(e.Tags) == 0 || len(coords) < 2 {
			return nil
		}
		doc := wayDoc(opts.region, e, osmtiler.Classify(e.Tags), coords)
		return w.upsert(ctx, doc)
	}); err != nil {
		return nil, fmt.Errorf("pass 2c (way docs): %w", err)
	}

	return memberCoords, nil
}

// withWaysOnlyScanner opens the PBF, runs `cb` for each way the filter
// admits, and tears the scanner down on the way out. Nodes and
// relations blobs are skipped at the protobuf level so the decoder
// doesn't pay the deserialization cost (the bytes still get read off
// disk, but that's bounded by I/O).
func withWaysOnlyScanner(ctx context.Context, opts ingestOpts, filter func(*osm.Way) bool, cb func(*osm.Way) error) error {
	f, err := os.Open(opts.pbfPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := osmpbf.New(ctx, f, opts.procs)
	sc.SkipNodes = true
	sc.SkipRelations = true
	sc.FilterWay = filter
	defer sc.Close()
	for sc.Scan() {
		wy, ok := sc.Object().(*osm.Way)
		if !ok {
			continue
		}
		if err := cb(wy); err != nil {
			return err
		}
	}
	return sc.Err()
}

// withNodesOnlyScanner does the equivalent for nodes — only blobs
// containing nodes are decoded; ways and relations are skipped.
func withNodesOnlyScanner(ctx context.Context, opts ingestOpts, filter func(*osm.Node) bool, cb func(*osm.Node) error) error {
	f, err := os.Open(opts.pbfPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := osmpbf.New(ctx, f, opts.procs)
	sc.SkipWays = true
	sc.SkipRelations = true
	sc.FilterNode = filter
	defer sc.Close()
	for sc.Scan() {
		n, ok := sc.Object().(*osm.Node)
		if !ok {
			continue
		}
		if err := cb(n); err != nil {
			return err
		}
	}
	return sc.Err()
}

func emitRelations(ctx context.Context, opts ingestOpts, rels []relDesc, memberCoords map[osm.WayID][]osmtiler.LonLat, w *bucketRouter) error {
	skippedHuge := 0
	for _, rd := range rels {
		// Admin boundaries are stored as per-member-way LINES, not assembled
		// rings. A state/country outline assembled into one closed ring blows
		// past maxRingVertices and gets dropped (that's why the NY/US state
		// lines were missing); and it needn't close (it spans extracts). Each
		// member way is small and renders the same — the renderer strokes each.
		if rd.Class == osmtiler.ClassAdmin {
			idx := 0
			for _, wid := range rd.OuterWays {
				coords := memberCoords[wid]
				if len(coords) < 2 {
					continue
				}
				if err := w.upsert(ctx, relRingDoc(opts.region, rd, idx, coords)); err != nil {
					return err
				}
				idx++
			}
			continue
		}
		rings := osmtiler.AssembleOuterRings(rd.OuterWays, memberCoords)
		for i, ring := range rings {
			if len(ring) > maxRingVertices {
				// Greenland / Nunavut / Antarctica coastline rings can
				// run to millions of vertices, exceeding MongoDB's 16 MB
				// BSON doc limit and getting the whole batch rejected
				// client-side. Drop the ring with a log line — the rest
				// of the relation's rings (and every other relation in
				// this PBF) should still ingest cleanly.
				//
				// Long-term fix would be Douglas-Peucker simplification
				// at the per-zoom level we render at, but for a chart
				// underlay even a missing Greenland is fine — the chart
				// itself draws the coastline.
				skippedHuge++
				fmt.Fprintf(opts.writer(),
					"  warn: skipping relation %d ring %d (%q): %d vertices > %d limit\n",
					rd.ID, i, rd.Name, len(ring), maxRingVertices)
				continue
			}
			doc := relRingDoc(opts.region, rd, i, ring)
			if err := w.upsert(ctx, doc); err != nil {
				return err
			}
		}
	}
	if skippedHuge > 0 {
		fmt.Fprintf(opts.writer(), "skipped %d rings over %d vertices\n", skippedHuge, maxRingVertices)
	}
	return nil
}

// maxRingVertices is the per-ring vertex cap we use to keep an
// assembled multipolygon below MongoDB's 16 MB BSON doc limit. At
// ~24 bytes per [lon,lat] pair (2 doubles + BSON array overhead),
// 500_000 vertices serialise to roughly 12 MB — comfortably under
// 16 MB once the doc's metadata (tags, region, class, …) is added.
const maxRingVertices = 500_000

// ----- doc builders --------------------------------------------------------

func nodeDoc(region string, n *osm.Node, class osmtiler.Class) featureDoc {
	return featureDoc{
		ID:           fmt.Sprintf("%s:node:%d", region, n.ID),
		Region:       region,
		OSMType:      "node",
		OSMID:        int64(n.ID),
		Class:        class.String(),
		Kind:         "point",
		Name:         n.Tags.Find("name"),
		Ref:          n.Tags.Find("ref"),
		MinZoom:      int(osmtiler.GeomMinZoom(class, n.Tags)),
		MinLabelZoom: int(osmtiler.LabelMinZoom(class, n.Tags)),
		BBox:         [4]float64{n.Lon, n.Lat, n.Lon, n.Lat},
		Geometry: bson.M{
			"type":        "Point",
			"coordinates": []float64{n.Lon, n.Lat},
		},
		Tags: tagsAsMap(n.Tags),
	}
}

func wayDoc(region string, w *osm.Way, class osmtiler.Class, coords []osmtiler.LonLat) featureDoc {
	closed := len(coords) >= 4 && coords[0] == coords[len(coords)-1]
	kind := "line"
	if closed && isAreaClass(class) {
		kind = "polygon"
	}
	doc := featureDoc{
		ID:           fmt.Sprintf("%s:way:%d", region, w.ID),
		Region:       region,
		OSMType:      "way",
		OSMID:        int64(w.ID),
		Class:        class.String(),
		Kind:         kind,
		Name:         w.Tags.Find("name"),
		Ref:          w.Tags.Find("ref"),
		MinZoom:      int(osmtiler.GeomMinZoom(class, w.Tags)),
		MinLabelZoom: int(osmtiler.LabelMinZoom(class, w.Tags)),
		BBox:         bboxOf(coords),
	}
	if class == osmtiler.ClassRoad {
		doc.RoadKind = roadKindName(osmtiler.RoadKindFor(w.Tags.Find("highway")))
	}
	doc.Geometry = geometryForRing(kind, coords)
	doc.GeomLow = lowGeomFor(kind, coords)
	doc.Tags = tagsAsMap(w.Tags)
	return doc
}

// tagsAsMap copies the osm.Tags slice into a plain map[string]string for
// BSON storage. Returns nil for an empty input so the BSON ",omitempty"
// tag elides the field on disk for tagless features (only happens for
// relation rings that pick up tags from their parent relation).
func tagsAsMap(tags osm.Tags) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[t.Key] = t.Value
	}
	return m
}

// geometryForRing builds a 2dsphere-acceptable GeoJSON object from
// coords. For polygons we clean the ring (drop consecutive duplicates,
// ensure closure) and verify simplicity. OSM has many self-touching
// "polygons" — buildings with pinch points, parks tagged as a single
// closed way that figure-eights — which MongoDB rejects with
// "Loop is not valid: ... Duplicate vertices: a and b". When that
// happens we downgrade to a LineString: still spatially queryable
// via $geoIntersects, still draws as an outline, just no fill.
func geometryForRing(kind string, coords []osmtiler.LonLat) bson.M {
	if kind == "polygon" {
		if cleaned, ok := cleanSimpleRing(coords); ok {
			return bson.M{
				"type":        "Polygon",
				"coordinates": []any{lonLatRing(cleaned)},
			}
		}
		// Fall through: downgrade to LineString for the index. The
		// doc's `kind` stays "polygon" so a downstream renderer can
		// still treat it as an area if it has its own simpler check.
	}
	return bson.M{
		"type":        "LineString",
		"coordinates": lonLatRing(coords),
	}
}

// cleanSimpleRing removes consecutive duplicate vertices, ensures the
// ring closes (first vertex == last), and returns ok=false if the
// resulting ring still has any non-adjacent duplicate vertex (the
// MongoDB "Duplicate vertices" failure mode).
func cleanSimpleRing(coords []osmtiler.LonLat) ([]osmtiler.LonLat, bool) {
	if len(coords) < 3 {
		return nil, false
	}
	out := make([]osmtiler.LonLat, 0, len(coords))
	out = append(out, coords[0])
	for i := 1; i < len(coords); i++ {
		if coords[i] != coords[i-1] {
			out = append(out, coords[i])
		}
	}
	if len(out) < 3 {
		return nil, false
	}
	if out[0] != out[len(out)-1] {
		out = append(out, out[0])
	}
	if len(out) < 4 {
		return nil, false
	}
	seen := make(map[osmtiler.LonLat]struct{}, len(out)-1)
	for i := 0; i < len(out)-1; i++ {
		if _, dup := seen[out[i]]; dup {
			return out, false
		}
		seen[out[i]] = struct{}{}
	}
	return out, true
}

func relRingDoc(region string, rd relDesc, ringIdx int, coords []osmtiler.LonLat) featureDoc {
	// Admin boundaries are stored as LINES, not area polygons. As a polygon a
	// state/county outline $geoIntersects every interior tile (the tile lies
	// inside the area), so every tile in the state would fetch and stroke the
	// whole outline — huge waste. As a line it only matches tiles the border
	// actually crosses, and the renderer strokes ClassAdmin lines as the border.
	kind := "polygon"
	if rd.Class == osmtiler.ClassAdmin {
		kind = "line"
	}
	return featureDoc{
		ID:           fmt.Sprintf("%s:rel:%d:%d", region, rd.ID, ringIdx),
		Region:       region,
		OSMType:      "rel",
		OSMID:        int64(rd.ID),
		RingIndex:    ringIdx,
		Class:        rd.Class.String(),
		Kind:         kind,
		Name:         rd.Name,
		MinZoom:      int(osmtiler.GeomMinZoom(rd.Class, rd.Tags)),
		MinLabelZoom: int(osmtiler.LabelMinZoom(rd.Class, rd.Tags)),
		BBox:         bboxOf(coords),
		Geometry:     geometryForRing(kind, coords),
		GeomLow:      lowGeomFor(kind, coords),
		Tags:         tagsAsMap(rd.Tags),
	}
}

// ensureFeatureIndexes creates the indexes the tile-render query path
// expects for a single bucket collection. Called once per bucket at the
// start of every ingest run so fresh collections come up correctly
// without a separate mongo-shell step. Idempotent when the spec matches
// an existing same-named index (no rebuild); spec mismatch returns
// IndexOptionsConflict so the operator can decide. If you change the
// spec on a populated collection, drop the old index by hand once
// (`db.features_<bucket>.dropIndex("geo_minZoom_class")`) and re-run.
//
// The skip bucket gets only the region index — it holds minZoom=255
// docs the renderer never $geoIntersects-queries, so the 2dsphere
// index would be pure overhead.
func ensureFeatureIndexes(ctx context.Context, coll *mongo.Collection, bucket osmtiler.MinZoomBucket) error {
	models := []mongo.IndexModel{
		{
			// Admin/inspection queries that don't pin geography
			// (e.g. "all docs in this region", border-dedup work,
			// post-ingest region count).
			Keys:    bson.D{{Key: "region", Value: 1}},
			Options: options.Index().SetName("region_1"),
		},
	}
	if bucket != osmtiler.BucketSkip {
		// Primary render query: $geoIntersects on a tile bbox, optionally
		// filtered by minZoom (range) and class. The compound key serves
		// bbox-only, bbox+minZoom, and the full bbox+minZoom+class via
		// index-prefix matching. No partial filter needed — the bucket
		// split already restricts this collection to a known minZoom
		// range, so every doc here is queryable.
		models = append(models, mongo.IndexModel{
			Keys: bson.D{
				{Key: "geometry", Value: "2dsphere"},
				{Key: "minZoom", Value: 1},
				{Key: "class", Value: 1},
			},
			Options: options.Index().SetName("geo_minZoom_class"),
		})
	}
	created, err := coll.Indexes().CreateMany(ctx, models)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "indexes ready for %s: %v\n", bucket.CollectionName(), created)
	return nil
}

// ----- bulk upsert ---------------------------------------------------------

type upserter struct {
	coll      *mongo.Collection
	batchSize int
	out       io.Writer
	pending   []mongo.WriteModel
	emitted   int
	upserted  int
	batches   int

	// Per-doc-failure tracking. With Ordered=false BulkWrite, the
	// 2dsphere validator can reject individual docs without us
	// losing the rest of the batch — we count them and log a sample
	// instead of aborting.
	bulkErrors   int
	loggedSample bool
}

func newUpserter(coll *mongo.Collection, batchSize int, out io.Writer) *upserter {
	if out == nil {
		out = os.Stderr
	}
	return &upserter{coll: coll, batchSize: batchSize, out: out}
}

func (u *upserter) upsert(ctx context.Context, doc featureDoc) error {
	u.pending = append(u.pending,
		mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": doc.ID}).
			SetUpdate(bson.M{"$set": doc}).
			SetUpsert(true),
	)
	u.emitted++
	if len(u.pending) >= u.batchSize {
		return u.flush(ctx)
	}
	// Periodic progress is the bucketRouter's job in the bucket-split
	// world — one aggregate "N emitted" line beats four uncoordinated
	// per-collection lines.
	return nil
}

func (u *upserter) flush(ctx context.Context) error {
	if len(u.pending) == 0 {
		return nil
	}
	res, err := u.coll.BulkWrite(ctx, u.pending,
		options.BulkWrite().SetOrdered(false))
	// With Ordered=false, BulkWrite returns a BulkWriteException on
	// per-op failures but still applies the rest of the batch. We
	// log a sample of the per-doc errors and treat the batch as
	// "partial success" instead of aborting — OSM has plenty of
	// quirky geometry that the 2dsphere validator rejects (self-
	// touching polygons, etc.), and losing those particular docs
	// shouldn't take down the whole ingest.
	if err != nil {
		var bwe mongo.BulkWriteException
		if errors.As(err, &bwe) {
			u.bulkErrors += len(bwe.WriteErrors)
			if !u.loggedSample && len(bwe.WriteErrors) > 0 {
				we := bwe.WriteErrors[0]
				fmt.Fprintf(u.out, "  warn: %d docs rejected by server in batch; sample [code=%d]: %s\n",
					len(bwe.WriteErrors), we.Code, we.Message)
				u.loggedSample = true
			}
		} else {
			return err
		}
	}
	if res != nil {
		u.upserted += int(res.UpsertedCount) + int(res.ModifiedCount) + int(res.MatchedCount)
	}
	u.batches++
	u.pending = u.pending[:0]
	return nil
}

// ----- bucket router -------------------------------------------------------

// bucketRouter holds one upserter per minZoom-bucket collection and
// routes each upserted doc to the right one based on its minZoom. The
// public interface (upsert / flush) mirrors *upserter so the scan
// functions don't care which one they have. Aggregate counters
// (emitted / upserted / batches / bulkErrors) sum across all four
// buckets; the periodic progress line lives here, not on the inner
// upserters, so the operator sees one cohesive log stream instead of
// four interleaved ones.
type bucketRouter struct {
	overview, coastal, detail, skip *upserter
	// lowzoom is the curated z7/z8 collection — a CROSS-CUTTING copy, not a
	// minZoom bucket: qualifying band features are written here (with simplified
	// geometry) IN ADDITION to their normal bucket. Kept out of all() so the
	// emitted/upserted counters don't double-count the copies.
	lowzoom *upserter
	out     io.Writer

	lastLog time.Time
}

func newBucketRouter(colls *osmtiler.OSMCollections, batchSize int, out io.Writer) *bucketRouter {
	if out == nil {
		out = os.Stderr
	}
	return &bucketRouter{
		overview: newUpserter(colls.Overview, batchSize, out),
		coastal:  newUpserter(colls.Coastal, batchSize, out),
		detail:   newUpserter(colls.Detail, batchSize, out),
		skip:     newUpserter(colls.Skip, batchSize, out),
		lowzoom:  newUpserter(colls.LowZoom, batchSize, out),
		out:      out,
	}
}

// upsertersInBucketOrder enumerates the four upserters in the same
// order as the bucket enum so iteration is deterministic.
func (r *bucketRouter) all() []*upserter {
	return []*upserter{r.overview, r.coastal, r.detail, r.skip}
}

// pickUpserter routes a doc by its pre-computed minZoom field.
func (r *bucketRouter) pickUpserter(doc featureDoc) *upserter {
	switch osmtiler.BucketForMinZoom(uint8(doc.MinZoom)) {
	case osmtiler.BucketOverview:
		return r.overview
	case osmtiler.BucketCoastal:
		return r.coastal
	case osmtiler.BucketDetail:
		return r.detail
	default:
		return r.skip
	}
}

func (r *bucketRouter) upsert(ctx context.Context, doc featureDoc) error {
	u := r.pickUpserter(doc)
	if err := u.upsert(ctx, doc); err != nil {
		return err
	}
	// Also copy qualifying band features into the curated low-zoom collection,
	// with the pre-simplified geometry, so the z7/z8 query stays tiny+fast.
	if lz, ok := lowZoomVariant(doc); ok {
		if err := r.lowzoom.upsert(ctx, lz); err != nil {
			return err
		}
	}
	if time.Since(r.lastLog) > 5*time.Second {
		msg := fmt.Sprintf("  ... %d emitted (%d upserted) [overview=%d coastal=%d detail=%d skip=%d]",
			r.emitted(), r.upserted(),
			r.overview.emitted, r.coastal.emitted, r.detail.emitted, r.skip.emitted)
		fmt.Fprintln(r.out, msg)
		r.lastLog = time.Now()
	}
	return nil
}

func (r *bucketRouter) flush(ctx context.Context) error {
	for _, u := range r.all() {
		if err := u.flush(ctx); err != nil {
			return err
		}
	}
	return r.lowzoom.flush(ctx)
}

// lowZoomVariant returns the curated-collection copy of a doc when it qualifies
// for the z7/z8 band (band class; point label or big enough to be visible),
// substituting the pre-simplified geomLow geometry. ok=false → don't copy it.
func lowZoomVariant(doc featureDoc) (featureDoc, bool) {
	span := doc.BBox[2] - doc.BBox[0]
	if sy := doc.BBox[3] - doc.BBox[1]; sy > span {
		span = sy
	}
	if !osmtiler.QualifiesForLowZoom(osmtiler.ClassFromString(doc.Class), doc.Kind == "point", span) {
		return featureDoc{}, false
	}
	// Copy verbatim: the indexed geometry stays the valid original; the render
	// path uses geomLow via UseLowGeom (a DP-simplified ring can self-intersect,
	// which a 2dsphere index would reject, so geomLow must not be the indexed
	// geometry).
	return doc, true
}

func (r *bucketRouter) emitted() int {
	n := 0
	for _, u := range r.all() {
		n += u.emitted
	}
	return n
}

func (r *bucketRouter) upserted() int {
	n := 0
	for _, u := range r.all() {
		n += u.upserted
	}
	return n
}

func (r *bucketRouter) batches() int {
	n := 0
	for _, u := range r.all() {
		n += u.batches
	}
	return n
}

func (r *bucketRouter) bulkErrors() int {
	n := 0
	for _, u := range r.all() {
		n += u.bulkErrors
	}
	return n
}

// ----- nodeStore -----------------------------------------------------------

// nodeStore is the per-PBF in-memory cache of node coords used during
// pass 2 to resolve way geometry. The naive `map[osm.NodeID]LonLat`
// implementation costs ~32 bytes per entry (8-byte key + 16-byte LonLat
// + ~8 bytes of bucket overhead at typical Go-map load); for a
// California-sized PBF (~100M nodes) that's ~3.2 GB per worker.
//
// nodeStore packs each coord pair into a single uint64 by quantising
// lon/lat to int32 micro-degrees (0.1 m precision at the equator), so
// the value half drops 16→8 bytes. Same key, smaller value → ~22 bytes
// per entry (~30% saving). Combined with the 3-pass scan that drops
// unreferenced nodes entirely, peak memory falls to roughly a third of
// the original map.
type nodeStore struct {
	m map[osm.NodeID]uint64
}

func newNodeStore() *nodeStore {
	return &nodeStore{m: map[osm.NodeID]uint64{}}
}

// hint pre-sizes the map. Use the count returned by the needed-node-IDs
// scan so the map doesn't grow-and-realloc its way up to final size,
// which is by far the costliest part of building a 100M-entry map.
func (s *nodeStore) hint(n int) {
	if n > len(s.m) {
		s.m = make(map[osm.NodeID]uint64, n)
	}
}

func (s *nodeStore) Set(id osm.NodeID, lon, lat float64) {
	s.m[id] = packLonLat(lon, lat)
}

func (s *nodeStore) Get(id osm.NodeID) (osmtiler.LonLat, bool) {
	p, ok := s.m[id]
	if !ok {
		return osmtiler.LonLat{}, false
	}
	return unpackLonLat(p), true
}

func (s *nodeStore) Len() int { return len(s.m) }

// packLonLat folds two float64 lon/lat values into one uint64 by
// rounding each to int32 micro-degrees. Lon ∈ [-180, 180] × 1e7 fits
// comfortably in int32 (range ±2.1e9), and 1e-7° ≈ 11 mm at the equator
// — far below the precision OSM contributors edit at.
func packLonLat(lon, lat float64) uint64 {
	li := int32(math.Round(lon * 1e7))
	la := int32(math.Round(lat * 1e7))
	return uint64(uint32(li))<<32 | uint64(uint32(la))
}

func unpackLonLat(p uint64) osmtiler.LonLat {
	li := int32(p >> 32)
	la := int32(p)
	return osmtiler.LonLat{
		Lon: float64(li) / 1e7,
		Lat: float64(la) / 1e7,
	}
}

// ----- helpers -------------------------------------------------------------

func bboxOf(coords []osmtiler.LonLat) [4]float64 {
	if len(coords) == 0 {
		return [4]float64{}
	}
	minLon, maxLon := coords[0].Lon, coords[0].Lon
	minLat, maxLat := coords[0].Lat, coords[0].Lat
	for _, c := range coords[1:] {
		switch {
		case c.Lon < minLon:
			minLon = c.Lon
		case c.Lon > maxLon:
			maxLon = c.Lon
		}
		switch {
		case c.Lat < minLat:
			minLat = c.Lat
		case c.Lat > maxLat:
			maxLat = c.Lat
		}
	}
	return [4]float64{minLon, minLat, maxLon, maxLat}
}

func lonLatRing(coords []osmtiler.LonLat) [][]float64 {
	out := make([][]float64, len(coords))
	for i, c := range coords {
		out[i] = []float64{c.Lon, c.Lat}
	}
	return out
}

func isAreaClass(c osmtiler.Class) bool {
	switch c {
	case osmtiler.ClassBuilding, osmtiler.ClassLanduse, osmtiler.ClassLeisure, osmtiler.ClassNatural, osmtiler.ClassWater:
		return true
	}
	return false
}

// ----- shared flag set for query / gentile --------------------------------

// tileQueryOpts wraps osmtiler.QueryOptions with the Mongo connection
// flags both `query` and `gentile` need. Keeping the wrapper here
// (rather than in the library) lets the library stay flag-agnostic
// while these two subcommands share one place to register all the
// common knobs.
type tileQueryOpts struct {
	mongoURI string
	dbName   string
	osmtiler.QueryOptions
}

func addTileQueryFlags(fs *flag.FlagSet) *tileQueryOpts {
	o := &tileQueryOpts{}
	fs.StringVar(&o.mongoURI, "mongo", "", "MongoDB connection URI (required)")
	fs.StringVar(&o.dbName, "db", "osm", "MongoDB database name")
	fs.BoolVar(&o.IncludeMinZoom, "min-zoom", true,
		"include {minZoom: {$lte: z}} so only features visible at this zoom are returned")
	fs.IntVar(&o.ZoomOverride, "zoom", -1,
		"override the zoom used in the minZoom filter; default is the tile's own z")
	fs.StringVar(&o.Region, "region", "", "if set, restrict to docs whose region == this key")
	fs.StringVar(&o.Class, "class", "", "if set, restrict to docs whose class == this string")
	return o
}

// ----- query ---------------------------------------------------------------

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	opts := addTileQueryFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if opts.mongoURI == "" || len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("usage: query --mongo <uri> [flags] <z>/<x>/<y>")
	}
	z, x, y, err := parseTileCoord(rest[0])
	if err != nil {
		return err
	}

	q := opts.QueryOptions
	q.PadBuffer = false // plain counts use the tile's exact bbox
	filter := osmtiler.BuildTileFilter(z, x, y, q)
	minLon, minLat, maxLon, maxLat := osmtiler.TileBoundsLonLat(z, x, y)

	fmt.Printf("tile        z=%d x=%d y=%d\n", z, x, y)
	fmt.Printf("bbox        lon=[%.6f .. %.6f]  lat=[%.6f .. %.6f]\n",
		minLon, maxLon, minLat, maxLat)
	fmt.Println()

	pretty, err := json.MarshalIndent(filter, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println("filter (paste into mongosh, run against each bucket):")
	fmt.Printf("  db.%s.find(%s).count()\n", osmtiler.CollOverview, indentJSON(string(pretty), "  "))
	fmt.Printf("  db.%s.find(%s).count()\n", osmtiler.CollCoastal, indentJSON(string(pretty), "  "))
	fmt.Printf("  db.%s.find(%s).count()\n", osmtiler.CollDetail, indentJSON(string(pretty), "  "))
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(opts.mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	colls := osmtiler.OpenOSMCollections(client.Database(opts.dbName))

	// Count per bucket so the operator can see where the cost lands —
	// at low zoom most of the time should be in overview; coastal
	// dominates at z=8..11; detail at z=12+.
	var total int64
	var totalElapsed time.Duration
	for _, b := range []osmtiler.MinZoomBucket{
		osmtiler.BucketOverview, osmtiler.BucketCoastal, osmtiler.BucketDetail,
	} {
		start := time.Now()
		n, err := colls.For(b).CountDocuments(ctx, filter)
		elapsed := time.Since(start)
		if err != nil {
			return fmt.Errorf("count %s: %w", b.CollectionName(), err)
		}
		fmt.Printf("  %-20s %8d features in %s\n", b.CollectionName(), n, elapsed.Round(time.Millisecond))
		total += n
		totalElapsed += elapsed
	}
	fmt.Printf("count       %d features in %s (sum across buckets)\n", total, totalElapsed.Round(time.Millisecond))
	return nil
}

// ----- gentile -------------------------------------------------------------

func runGenTile(args []string) error {
	fs := flag.NewFlagSet("gentile", flag.ContinueOnError)
	opts := addTileQueryFlags(fs)
	out := fs.String("out", "",
		`output path; "" means tile-<z>-<x>-<y>.png, "-" writes to stdout`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if opts.mongoURI == "" || len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("usage: gentile --mongo <uri> [flags] <z>/<x>/<y>")
	}
	z, x, y, err := parseTileCoord(rest[0])
	if err != nil {
		return err
	}
	if *out == "" {
		*out = fmt.Sprintf("tile-%d-%d-%d.png", z, x, y)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(opts.mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	colls := osmtiler.OpenOSMCollections(client.Database(opts.dbName))

	// Pad the bbox so the renderer has the cross-tile label-overdraw
	// features it expects (LabelBuffer pixels worth on each side).
	q := opts.QueryOptions
	q.PadBuffer = true
	queryStart := time.Now()
	features, stats, err := osmtiler.FetchTileFeaturesMulti(ctx, colls, z, x, y, q)
	if err != nil {
		return err
	}
	queryElapsed := time.Since(queryStart)

	renderStart := time.Now()
	png, err := osmtiler.RenderTileFromFeatures(features, z, x, y)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	renderElapsed := time.Since(renderStart)

	if *out == "-" {
		if _, err := os.Stdout.Write(png); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(*out, png, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
	}

	fmt.Fprintf(os.Stderr, "tile z=%d x=%d y=%d\n", z, x, y)
	fmt.Fprintf(os.Stderr, "  query:    %d docs (%s of BSON, %d decode-skipped) in %s\n",
		stats.Docs, humanBytes(stats.BytesRead), stats.DecodeFail, queryElapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "  features: %d kept after decode\n", len(features))
	fmt.Fprintf(os.Stderr, "  render:   %s PNG in %s\n",
		humanBytes(int64(len(png))), renderElapsed.Round(time.Millisecond))
	if *out != "-" {
		fmt.Fprintf(os.Stderr, "  out:      %s\n", *out)
	}
	return nil
}

// humanBytes formats a byte count with a single-character unit
// suffix (B, KiB, MiB, …) — used for the tile transfer log lines so
// "47 MB" is more readable than "49283147".
func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(KiB))
	}
	return fmt.Sprintf("%d B", n)
}

// parseTileCoord parses a "z/x/y" string into integer components.
func parseTileCoord(s string) (z, x, y int, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("tile coord must be z/x/y, got %q", s)
	}
	zi, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad z: %w", err)
	}
	xi, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad x: %w", err)
	}
	yi, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad y: %w", err)
	}
	return zi, xi, yi, nil
}

// indentJSON re-prefixes every line after the first with `prefix`
// so a multi-line JSON object lines up under the leading "db.x.find("
// when rendered as a single Printf.
func indentJSON(s, prefix string) string {
	return strings.ReplaceAll(s, "\n", "\n"+prefix)
}

func roadKindName(k osmtiler.RoadKind) string {
	switch k {
	case osmtiler.RoadMotorway:
		return "motorway"
	case osmtiler.RoadTrunk:
		return "trunk"
	case osmtiler.RoadPrimary:
		return "primary"
	case osmtiler.RoadSecondary:
		return "secondary"
	case osmtiler.RoadTertiary:
		return "tertiary"
	case osmtiler.RoadResidential:
		return "residential"
	case osmtiler.RoadService:
		return "service"
	case osmtiler.RoadPath:
		return "path"
	}
	return ""
}

// ----- backfill-lowzoom ----------------------------------------------------

// runBackfillLowZoom (re)builds the curated osm_lowzoom collection from the
// existing osm_overview / osm_coastal docs, without re-ingesting any PBF. It
// copies in only the features that QualifyForLowZoom (band classes; point
// labels or big enough to be visible at z8), storing the pre-simplified geomLow
// geometry so the z7/z8 query touches a small, fast collection instead of the
// 41M-doc overview bucket. Idempotent: upserts by _id, so re-runs refresh.
func runBackfillLowZoom(args []string) error {
	fs := flag.NewFlagSet("backfill-lowzoom", flag.ContinueOnError)
	mongoURI := fs.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dbName := fs.String("db", "osm", "MongoDB database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if v := os.Getenv("MONGO_DB"); v != "" && !flagSet(fs, "db") {
		*dbName = v
	}
	if *mongoURI == "" {
		fs.Usage()
		return fmt.Errorf("--mongo (or $MONGO_URI) required")
	}

	ctx := context.Background()
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()
	if err := client.Ping(connectCtx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	db := client.Database(*dbName)
	low := db.Collection(osmtiler.CollLowZoom)

	// Merge into the (unindexed) collection first — building the 2dsphere index
	// per-insert during $merge is far slower than a single bulk index build at
	// the end. Start clean so the build can't trip over stale partial data.
	if err := low.Drop(ctx); err != nil {
		return fmt.Errorf("drop %s: %w", osmtiler.CollLowZoom, err)
	}
	for _, src := range []string{osmtiler.CollOverview, osmtiler.CollCoastal} {
		if err := backfillLowZoomFrom(ctx, db.Collection(src), src); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
	}

	// 2dsphere render index so the z7/z8 $geoIntersects query is fast. Built once
	// over the finished collection (the copied geometry is the valid original).
	fmt.Printf("building %s index ...\n", osmtiler.CollLowZoom)
	if _, err := low.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "geometry", Value: "2dsphere"},
			{Key: "minZoom", Value: 1},
			{Key: "class", Value: 1},
		},
		Options: options.Index().SetName("geo_minZoom_class"),
	}); err != nil {
		return fmt.Errorf("lowzoom index: %w", err)
	}
	fmt.Printf("%s ready: %d docs\n", osmtiler.CollLowZoom, func() int64 {
		n, _ := low.EstimatedDocumentCount(ctx)
		return n
	}())
	return nil
}

// backfillLowZoomFrom copies the qualifying docs from one source bucket into
// osm_lowzoom using a server-side aggregation $merge — no 51M-doc round-trip to
// the client. The $match replicates osmtiler.QualifiesForLowZoom (band class;
// point, or bbox longer side >= lowZoomMinSpanDeg); docs are copied VERBATIM
// (valid original geometry stays indexed, geomLow kept for the render's
// UseLowGeom path). whenMatched=replace makes re-runs idempotent.
func backfillLowZoomFrom(ctx context.Context, src *mongo.Collection, name string) error {
	band := bson.A{"natural", "landuse", "leisure", "road", "railway", "aeroway", "admin", "place"}
	span := bson.M{"$max": bson.A{
		bson.M{"$subtract": bson.A{
			bson.M{"$arrayElemAt": bson.A{"$bbox", 2}},
			bson.M{"$arrayElemAt": bson.A{"$bbox", 0}},
		}},
		bson.M{"$subtract": bson.A{
			bson.M{"$arrayElemAt": bson.A{"$bbox", 3}},
			bson.M{"$arrayElemAt": bson.A{"$bbox", 1}},
		}},
	}}
	match := bson.M{
		"class": bson.M{"$in": band},
		"$or": bson.A{
			bson.M{"kind": "point"},
			bson.M{"$expr": bson.M{"$gte": bson.A{span, osmtiler.LowZoomMinSpanDeg}}},
		},
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$merge", Value: bson.M{
			"into":           osmtiler.CollLowZoom,
			"on":             "_id",
			"whenMatched":    "replace",
			"whenNotMatched": "insert",
		}}},
	}
	start := time.Now()
	fmt.Printf("  %-16s merging qualifying docs into %s ...\n", name, osmtiler.CollLowZoom)
	cur, err := src.Aggregate(ctx, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return fmt.Errorf("aggregate $merge: %w", err)
	}
	// $merge yields no documents; just drain + close.
	_ = cur.All(ctx, &[]bson.M{})
	fmt.Printf("%-16s done in %s\n", name, time.Since(start).Round(time.Second))
	return nil
}

// ----- backfill-geomlow ----------------------------------------------------

// runBackfillGeomLow adds the pre-simplified geomLow geometry to existing
// osm_overview / osm_coastal documents (the buckets the z7..z11 land-cover band
// queries), without re-downloading or re-ingesting any PBF. It reads each
// line/polygon doc, Douglas–Peucker-simplifies the geometry at
// osmtiler.LowGeomTolerance, and $sets geomLow when simplification actually
// drops vertices. Idempotent: docs that already have geomLow are skipped, and
// docs that don't reduce are simply left as-is (the query path falls back to
// their full geometry).
func runBackfillGeomLow(args []string) error {
	fs := flag.NewFlagSet("backfill-geomlow", flag.ContinueOnError)
	mongoURI := fs.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dbName := fs.String("db", "osm", "MongoDB database")
	batchSize := fs.Int("batch", 2000, "bulk-update batch size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if v := os.Getenv("MONGO_DB"); v != "" && !flagSet(fs, "db") {
		*dbName = v
	}
	if *mongoURI == "" {
		fs.Usage()
		return fmt.Errorf("--mongo (or $MONGO_URI) required")
	}

	ctx := context.Background()
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()
	if err := client.Ping(connectCtx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	colls := osmtiler.OpenOSMCollections(client.Database(*dbName))

	// Only the overview + coastal buckets are queried at the overview band
	// (z7..z11); the detail bucket (z12+) always renders full geometry.
	for _, b := range []osmtiler.MinZoomBucket{osmtiler.BucketOverview, osmtiler.BucketCoastal} {
		coll := colls.For(b)
		if err := backfillGeomLowColl(ctx, coll, b.CollectionName(), *batchSize); err != nil {
			return fmt.Errorf("%s: %w", b.CollectionName(), err)
		}
	}
	return nil
}

// flagSet reports whether a flag was explicitly set on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func backfillGeomLowColl(ctx context.Context, coll *mongo.Collection, name string, batchSize int) error {
	// Only line/polygon features can simplify, and only those missing geomLow
	// need processing — so re-runs after a partial pass are cheap.
	filter := bson.M{
		"kind":    bson.M{"$in": bson.A{"line", "polygon"}},
		"geomLow": bson.M{"$exists": false},
	}
	// Pull just what we need to rebuild geomLow: id, kind, and geometry.
	proj := bson.M{"_id": 1, "kind": 1, "geometry": 1}
	cur, err := coll.Find(ctx, filter, options.Find().SetProjection(proj).SetNoCursorTimeout(true))
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	defer cur.Close(ctx)

	var scanned, updated int64
	var models []mongo.WriteModel
	flush := func() error {
		if len(models) == 0 {
			return nil
		}
		res, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
		if err != nil {
			return fmt.Errorf("bulk write: %w", err)
		}
		updated += res.ModifiedCount
		models = models[:0]
		return nil
	}

	start := time.Now()
	for cur.Next(ctx) {
		scanned++
		feat, derr := osmtiler.DecodeFeature(cur.Current)
		if derr != nil {
			continue // skip undecodable docs; the renderer skips them too
		}
		var idDoc struct {
			ID string `bson:"_id"`
		}
		if err := bson.Unmarshal(cur.Current, &idDoc); err != nil || idDoc.ID == "" {
			continue
		}
		kindStr := "line"
		if feat.Kind == osmtiler.GeomPolygon {
			kindStr = "polygon"
		}
		gl := lowGeomFor(kindStr, feat.Coords)
		if gl == nil {
			continue // simplification wouldn't reduce — leave it to full-geom fallback
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": idDoc.ID}).
			SetUpdate(bson.M{"$set": bson.M{"geomLow": gl}}))
		if len(models) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
			fmt.Printf("  %-16s scanned=%d updated=%d (%s)\n", name, scanned, updated, time.Since(start).Round(time.Second))
		}
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Printf("%-16s done: scanned=%d updated=%d in %s\n", name, scanned, updated, time.Since(start).Round(time.Second))
	return nil
}
