// osmtools is a multi-purpose CLI for the chartplotter's OSM data
// pipeline. Today it has one subcommand:
//
//	osmtools ingest --pbf <path> --mongo <uri> --region <key>
//
// More subcommands (e.g. region inspection, tile prerender, mongo
// query helpers) can be added to the dispatch table in main().
//
// Usage examples:
//
//	osmtools ingest --pbf /tmp/NewYork.osm.pbf \
//	                --mongo mongodb://localhost:27017 \
//	                --region us-new-york
//
//	osmtools ingest --pbf /tmp/NewYork.osm.pbf \
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
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/osmtiler"
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
	case "downloadpbfs":
		if err := runDownloadPBFs(args); err != nil {
			fmt.Fprintf(os.Stderr, "downloadpbfs: %v\n", err)
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
	fmt.Fprintln(os.Stderr, "Usage: osmtools <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  ingest        Read a .osm.pbf and upsert kept features into MongoDB")
	fmt.Fprintln(os.Stderr, "  query         Show + count the features a given tile would query for")
	fmt.Fprintln(os.Stderr, "  gentile       Render a tile PNG by querying the MongoDB collection")
	fmt.Fprintln(os.Stderr, "  downloadpbfs  Fetch every Geofabrik .osm.pbf for a continent key")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, `Run "osmtools <subcommand> --help" for subcommand flags.`)
}

// ----- ingest --------------------------------------------------------------

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	pbfPath := fs.String("pbf", "", "path to input .osm.pbf (required)")
	mongoURI := fs.String("mongo", "", "MongoDB connection URI (required)")
	dbName := fs.String("db", "osm", "MongoDB database name")
	collName := fs.String("coll", "features", "MongoDB collection name")
	region := fs.String("region", "", "region key recorded on every document (defaults to PBF basename)")
	batchSize := fs.Int("batch", 1000, "bulk upsert batch size")
	procs := fs.Int("procs", runtime.NumCPU(), "PBF decoder workers")
	force := fs.Bool("force", false,
		"re-ingest even when an ingest-meta doc says this region's PBF hash already matches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pbfPath == "" || *mongoURI == "" {
		fs.Usage()
		return fmt.Errorf("--pbf and --mongo are required")
	}
	if *region == "" {
		base := filepath.Base(*pbfPath)
		base = strings.TrimSuffix(base, ".osm.pbf")
		base = strings.TrimSuffix(base, ".pbf")
		// Geofabrik names downloads `<leaf>-latest.osm.pbf`; if the
		// file still has that trailing -latest we don't want it
		// leaking into the region key recorded in MongoDB.
		base = strings.TrimSuffix(base, "-latest")
		*region = base
		fmt.Fprintf(os.Stderr, "no --region given, using %q derived from filename\n", *region)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	return ingest(ctx, ingestOpts{
		pbfPath:   *pbfPath,
		mongoURI:  *mongoURI,
		dbName:    *dbName,
		collName:  *collName,
		region:    *region,
		batchSize: *batchSize,
		procs:     *procs,
		force:     *force,
	})
}

type ingestOpts struct {
	pbfPath   string
	mongoURI  string
	dbName    string
	collName  string
	region    string
	batchSize int
	procs     int
	force     bool
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
	Tags         map[string]string `bson:"tags,omitempty"`
}

func ingest(ctx context.Context, opts ingestOpts) error {
	// 1. Connect to MongoDB up front so a bad URI fails before we
	//    spend minutes parsing a PBF.
	connectCtx, connectCancel := context.WithTimeout(ctx, 15*time.Second)
	defer connectCancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(opts.mongoURI))
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
	coll := client.Database(opts.dbName).Collection(opts.collName)
	fmt.Fprintf(os.Stderr, "connected to %s.%s\n", opts.dbName, opts.collName)

	if err := ensureFeatureIndexes(ctx, coll); err != nil {
		return fmt.Errorf("ensure indexes: %w", err)
	}

	// 1b. Hash the PBF and check ingest-meta — if a previous ingest
	//     of this region recorded the same hash and the collection
	//     still has roughly the expected number of feature docs,
	//     there's nothing new to write. Saves ~minutes per re-run.
	hashStart := time.Now()
	pbfHash, pbfSize, err := hashFile(opts.pbfPath)
	if err != nil {
		return fmt.Errorf("hash pbf: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pbf hash: sha256:%s… (%s, %s)\n",
		pbfHash[:12], humanBytes(pbfSize), time.Since(hashStart).Round(time.Millisecond))

	if !opts.force {
		if skip, why := shouldSkipIngest(ctx, coll, opts.region, pbfHash); skip {
			fmt.Fprintf(os.Stderr, "skip: %s\n", why)
			return nil
		} else if why != "" {
			fmt.Fprintf(os.Stderr, "re-ingest: %s\n", why)
		}
	}

	// 2. Pass 1 — relations only. Identify the multipolygons we'll
	//    later emit, plus the way IDs they reference so we know to
	//    keep their geometry in pass 2.
	relPass, memberWays, err := scanRelations(ctx, opts.pbfPath, opts.procs)
	if err != nil {
		return fmt.Errorf("relations pass: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pass 1: %d multipolygon relations, %d member way ids\n",
		len(relPass), len(memberWays))

	// 3. Pass 2 — nodes + ways. Emit node POIs / way features
	//    directly; stash coords for relation members. After pass 2
	//    we stitch and emit the multipolygon features.
	w := newUpserter(coll, opts.batchSize)
	memberCoords, err := scanNodesAndWays(ctx, opts, memberWays, w)
	if err != nil {
		return fmt.Errorf("nodes/ways pass: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pass 2: emitted %d node/way features, kept %d member way geometries\n",
		w.emitted, len(memberCoords))

	if err := emitRelations(ctx, opts, relPass, memberCoords, w); err != nil {
		return fmt.Errorf("emit relations: %w", err)
	}
	fmt.Fprintf(os.Stderr, "emitted %d total features\n", w.emitted)

	if err := w.flush(ctx); err != nil {
		return fmt.Errorf("final flush: %w", err)
	}
	if w.bulkErrors > 0 {
		fmt.Fprintf(os.Stderr, "done: %d upserts (%d batches, %d docs rejected by server)\n",
			w.upserted, w.batches, w.bulkErrors)
	} else {
		fmt.Fprintf(os.Stderr, "done: %d upserts (%d batches)\n", w.upserted, w.batches)
	}

	// Persist the ingest-meta so a future run on the same PBF can
	// short-circuit. Count the actual region docs in the collection
	// (BulkWrite Ordered=false can have left a few behind) so the
	// skip-tolerance check has a true baseline.
	actualCount, err := coll.CountDocuments(ctx, bson.M{"region": opts.region})
	if err != nil {
		return fmt.Errorf("post-count: %w", err)
	}
	if err := writeIngestMeta(ctx, coll, opts.region, pbfHash, pbfSize, actualCount); err != nil {
		return fmt.Errorf("write ingest meta: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ingest-meta: region=%s hash=%s… docs=%d\n",
		opts.region, pbfHash[:12], actualCount)
	return nil
}

// ----- ingest-meta helpers -------------------------------------------------

// ingestMetaID returns the _id we use for the per-region meta doc.
// Lives in the same collection as the features so we don't have to
// configure / index a second collection; the `_ingest_meta:` prefix
// guarantees no collision with feature documents (whose ids look
// like "<region>:<osmType>:<osmID>[:<ring>]").
func ingestMetaID(region string) string { return "_ingest_meta:" + region }

// shouldSkipIngest returns (true, why) when the recorded meta says
// this PBF has already been fully ingested and the collection still
// has ~the expected number of region docs. Returns (false, why)
// when we recognize the region but think it needs re-ingest, with
// `why` describing the reason. (false, "") means no prior meta.
func shouldSkipIngest(ctx context.Context, coll *mongo.Collection, region, pbfHash string) (bool, string) {
	var meta struct {
		PBFHash  string `bson:"pbfHash"`
		DocCount int64  `bson:"docCount"`
	}
	err := coll.FindOne(ctx, bson.M{"_id": ingestMetaID(region)}).Decode(&meta)
	if err != nil {
		return false, ""
	}
	if meta.PBFHash != pbfHash {
		return false, fmt.Sprintf("PBF hash changed (was %s… now %s…)",
			truncHash(meta.PBFHash), truncHash(pbfHash))
	}
	actual, err := coll.CountDocuments(ctx, bson.M{"region": region})
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
	return true, fmt.Sprintf("already ingested: hash sha256:%s… matches, %d docs in collection (recorded %d)",
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
		// Keep every multipolygon. We used to gate on the class
		// classifier here, which is what made adding a new render
		// rule a re-ingest event — every refinement we've ever done
		// needed the relation pass re-run. Storing all multipolygons
		// is the "ingest cleanly, no filtering" rule from
		// OSM_TILES_PLAN.md; classification happens at render time
		// from the stored tag map.
		return r.Tags.Find("type") == "multipolygon"
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

func scanNodesAndWays(ctx context.Context, opts ingestOpts, memberWays map[osm.WayID]struct{}, w *upserter) (map[osm.WayID][]osmtiler.LonLat, error) {
	f, err := os.Open(opts.pbfPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil {
		fmt.Fprintf(os.Stderr, "pass 2: scanning %.1f MB of PBF\n", float64(info.Size())/(1<<20))
	}

	sc := osmpbf.New(ctx, f, opts.procs)
	sc.SkipRelations = true
	// Wire bytes-read into the upserter's periodic log so the
	// operator gets a percentage-of-file progress without us having
	// to pre-scan for an element count.
	if info, err := f.Stat(); err == nil {
		w.setProgress(sc.FullyScannedBytes, info.Size())
	}
	sc.FilterWay = func(wy *osm.Way) bool {
		if _, ok := memberWays[wy.ID]; ok {
			return true
		}
		// Keep every tagged way. The classifier no longer gates here
		// (was: drop if ClassSkip); see the FilterRelation comment for
		// the rationale.
		return len(wy.Tags) > 0
	}
	defer sc.Close()

	nodes := map[osm.NodeID]osmtiler.LonLat{}
	memberCoords := map[osm.WayID][]osmtiler.LonLat{}

	for sc.Scan() {
		switch e := sc.Object().(type) {
		case *osm.Node:
			nodes[e.ID] = osmtiler.LonLat{Lon: e.Lon, Lat: e.Lat}
			if len(e.Tags) == 0 {
				continue
			}
			// Every tagged node is stored (was: dropped if ClassSkip).
			// Class is still pre-computed so the renderer fast-path
			// doesn't have to re-classify from tags per query.
			doc := nodeDoc(opts.region, e, osmtiler.Classify(e.Tags))
			if err := w.upsert(ctx, doc); err != nil {
				return nil, err
			}

		case *osm.Way:
			coords := make([]osmtiler.LonLat, 0, len(e.Nodes))
			for _, n := range e.Nodes {
				p, ok := nodes[n.ID]
				if !ok {
					continue
				}
				coords = append(coords, p)
			}
			if _, want := memberWays[e.ID]; want && len(coords) >= 2 {
				memberCoords[e.ID] = coords
			}
			if len(e.Tags) == 0 || len(coords) < 2 {
				continue
			}
			// Every tagged way with valid geometry is stored.
			doc := wayDoc(opts.region, e, osmtiler.Classify(e.Tags), coords)
			if err := w.upsert(ctx, doc); err != nil {
				return nil, err
			}
		}
	}
	return memberCoords, sc.Err()
}

func emitRelations(ctx context.Context, opts ingestOpts, rels []relDesc, memberCoords map[osm.WayID][]osmtiler.LonLat, w *upserter) error {
	for _, rd := range rels {
		rings := osmtiler.AssembleOuterRings(rd.OuterWays, memberCoords)
		for i, ring := range rings {
			doc := relRingDoc(opts.region, rd, i, ring)
			if err := w.upsert(ctx, doc); err != nil {
				return err
			}
		}
	}
	return nil
}

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
	return featureDoc{
		ID:           fmt.Sprintf("%s:rel:%d:%d", region, rd.ID, ringIdx),
		Region:       region,
		OSMType:      "rel",
		OSMID:        int64(rd.ID),
		RingIndex:    ringIdx,
		Class:        rd.Class.String(),
		Kind:         "polygon",
		Name:         rd.Name,
		MinZoom:      int(osmtiler.GeomMinZoom(rd.Class, rd.Tags)),
		MinLabelZoom: int(osmtiler.LabelMinZoom(rd.Class, rd.Tags)),
		BBox:         bboxOf(coords),
		Geometry:     geometryForRing("polygon", coords),
		Tags:         tagsAsMap(rd.Tags),
	}
}

// ensureFeatureIndexes creates the indexes the tile-render query
// path expects. Called at the start of every ingest run so a fresh
// collection comes up correctly without a separate mongo-shell step.
// Idempotent — re-running the same spec is a no-op; a clashing
// pre-existing index returns an error so the operator can decide.
func ensureFeatureIndexes(ctx context.Context, coll *mongo.Collection) error {
	models := []mongo.IndexModel{
		{
			// Primary render query: $geoIntersects on a tile bbox,
			// optionally filtered by minZoom (range) and class. The
			// compound key serves bbox-only, bbox+minZoom, and the
			// full bbox+minZoom+class via index-prefix matching.
			Keys: bson.D{
				{Key: "geometry", Value: "2dsphere"},
				{Key: "minZoom", Value: 1},
				{Key: "class", Value: 1},
			},
			Options: options.Index().SetName("geo_minZoom_class"),
		},
		{
			// Admin/inspection queries that don't pin geography
			// (e.g. "all docs in this region", border-dedup work).
			Keys:    bson.D{{Key: "region", Value: 1}},
			Options: options.Index().SetName("region_1"),
		},
	}
	created, err := coll.Indexes().CreateMany(ctx, models)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "indexes ready: %v\n", created)
	return nil
}

// ----- bulk upsert ---------------------------------------------------------

type upserter struct {
	coll      *mongo.Collection
	batchSize int
	pending   []mongo.WriteModel
	emitted   int
	upserted  int
	batches   int
	lastLog   time.Time

	// Progress reporting: if set, the 5-second log line tacks on
	// "N.N% of file" so the operator has a sense of wall-clock
	// remaining. PBF has no header element-count, so bytes-read is
	// the most honest signal we can give.
	progressBytes func() int64
	progressTotal int64

	// Per-doc-failure tracking. With Ordered=false BulkWrite, the
	// 2dsphere validator can reject individual docs without us
	// losing the rest of the batch — we count them and log a sample
	// instead of aborting.
	bulkErrors   int
	loggedSample bool
}

func newUpserter(coll *mongo.Collection, batchSize int) *upserter {
	return &upserter{coll: coll, batchSize: batchSize}
}

func (u *upserter) setProgress(bytesFn func() int64, total int64) {
	u.progressBytes = bytesFn
	u.progressTotal = total
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
	if time.Since(u.lastLog) > 5*time.Second {
		msg := fmt.Sprintf("  ... %d emitted (%d upserted)", u.emitted, u.upserted)
		if u.progressBytes != nil && u.progressTotal > 0 {
			read := u.progressBytes()
			pct := float64(read) / float64(u.progressTotal) * 100
			msg = fmt.Sprintf("%s, %.1f%% of file", msg, pct)
		}
		fmt.Fprintln(os.Stderr, msg)
		u.lastLog = time.Now()
	}
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
				fmt.Fprintf(os.Stderr, "  warn: %d docs rejected by server in batch; sample [code=%d]: %s\n",
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

// ----- downloadpbfs --------------------------------------------------------

func runDownloadPBFs(args []string) error {
	fs := flag.NewFlagSet("downloadpbfs", flag.ContinueOnError)
	continent := fs.String("continent", "",
		"continent key from the static catalog; one of: "+strings.Join(continentNames(), ", "))
	all := fs.Bool("all", false,
		"fetch Geofabrik's index-v1.json and download every leaf extract (very large; combine with --parent/--filter to narrow)")
	parent := fs.String("parent", "",
		"with --all, restrict to descendants of this Geofabrik id (e.g. 'europe', 'us')")
	filter := fs.String("filter", "",
		"with --all, only download extracts whose id contains this substring")
	includeParents := fs.Bool("include-parents", false,
		"with --all, also download non-leaf extracts (parent regions are redundant when their children are downloaded)")
	dir := fs.String("dir", "", "destination directory for the .osm.pbf files (required)")
	force := fs.Bool("force", false, "re-download files that already exist on disk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		fs.Usage()
		return fmt.Errorf("--dir is required")
	}
	if (*continent == "" && !*all) || (*continent != "" && *all) {
		fs.Usage()
		return fmt.Errorf("exactly one of --continent or --all is required")
	}

	var sources []pbfSource
	if *all {
		got, err := loadGeofabrikIndex(*parent, *filter, *includeParents)
		if err != nil {
			return fmt.Errorf("geofabrik index: %w", err)
		}
		sources = got
		label := "geofabrik"
		if *parent != "" {
			label += " parent=" + *parent
		}
		if *filter != "" {
			label += " filter=" + *filter
		}
		fmt.Fprintf(os.Stderr, "matched %d extracts in %s\n", len(sources), label)
	} else {
		got, ok := pbfContinents[*continent]
		if !ok {
			return fmt.Errorf("unknown continent %q; valid: %s",
				*continent, strings.Join(continentNames(), ", "))
		}
		sources = got
	}
	if len(sources) == 0 {
		return fmt.Errorf("no PBFs matched the filter")
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", *dir, err)
	}

	client := &http.Client{
		// State / country PBFs can be 1–3 GB on a slow link; keep
		// the overall budget generous instead of relying on default
		// keepalive timing.
		Timeout: 30 * time.Minute,
	}
	const userAgent = "viam-chartplotter osmtools (+https://github.com/erh/viam-chartplotter)"

	fmt.Fprintf(os.Stderr, "downloading %d PBFs for %s → %s\n", len(sources), *continent, *dir)
	var fetched, skipped, failed int
	totalStart := time.Now()

	for i, src := range sources {
		// Save as `<geofabrik-id>.osm.pbf` (e.g. us-new-york.osm.pbf)
		// so the filename matches the region key recorded in MongoDB
		// — `ingest` then defaults --region from the filename and
		// the doc trail stays consistent end-to-end.
		dst := filepath.Join(*dir, src.Name+".osm.pbf")
		if !*force {
			if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
				fmt.Fprintf(os.Stderr, "  [%2d/%d] %-30s cached %s (%s)\n",
					i+1, len(sources), src.Name, filepath.Base(dst), humanBytes(info.Size()))
				skipped++
				continue
			}
		}

		fmt.Fprintf(os.Stderr, "  [%2d/%d] %-30s downloading… ", i+1, len(sources), src.Name)
		n, err := downloadPBF(client, userAgent, src.URL, dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
			failed++
			continue
		}
		fmt.Fprintf(os.Stderr, "%s\n", humanBytes(n))
		fetched++
	}

	fmt.Fprintf(os.Stderr, "done: %d fetched, %d cached, %d failed in %s\n",
		fetched, skipped, failed, time.Since(totalStart).Round(time.Second))
	if failed > 0 {
		return fmt.Errorf("%d download(s) failed", failed)
	}
	return nil
}

// downloadPBF streams the URL to <dst>.part and renames on success
// so an interrupted run never leaves a half-PBF on disk that looks
// valid to the next ingest. Returns the byte count written.
func downloadPBF(client *http.Client, userAgent, url, dst string) (int64, error) {
	partPath := dst + ".part"
	_ = os.Remove(partPath)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}

	out, err := os.Create(partPath)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		return 0, fmt.Errorf("read body: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return 0, fmt.Errorf("close: %w", closeErr)
	}
	if err := os.Rename(partPath, dst); err != nil {
		_ = os.Remove(partPath)
		return 0, fmt.Errorf("rename: %w", err)
	}
	return n, nil
}

// loadGeofabrikIndex fetches index-v1.json from Geofabrik, parses the
// FeatureCollection, and returns the .osm.pbf URL for every entry
// matching the given filters. By default we keep only leaves
// (entries no other entry lists as `parent`) so a download run that
// asks for "europe" doesn't also pull france and bavaria — that
// would be wire bytes spent on data already inside the larger file.
func loadGeofabrikIndex(parent, substr string, includeParents bool) ([]pbfSource, error) {
	const indexURL = "https://download.geofabrik.de/index-v1.json"
	req, err := http.NewRequest(http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "viam-chartplotter osmtools (+https://github.com/erh/viam-chartplotter)")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d fetching index-v1.json", resp.StatusCode)
	}

	var idx struct {
		Features []struct {
			Properties struct {
				ID     string            `json:"id"`
				Parent string            `json:"parent"`
				Name   string            `json:"name"`
				URLs   map[string]string `json:"urls"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	// Build the set of ids that have at least one child so we can
	// identify leaves: a leaf is any id that no other id lists as
	// `parent`. We treat the top-level continents as non-leaves —
	// they're always parents of one or more countries.
	hasChild := map[string]bool{}
	for _, f := range idx.Features {
		if f.Properties.Parent != "" {
			hasChild[f.Properties.Parent] = true
		}
	}

	// Descendant check: walk the parent chain up from id, return
	// true if we hit the requested ancestor.
	parentOf := map[string]string{}
	for _, f := range idx.Features {
		parentOf[f.Properties.ID] = f.Properties.Parent
	}
	isDescendant := func(id, ancestor string) bool {
		if ancestor == "" {
			return true
		}
		for cur := id; cur != ""; cur = parentOf[cur] {
			if cur == ancestor {
				return true
			}
		}
		return false
	}

	var out []pbfSource
	for _, f := range idx.Features {
		p := f.Properties
		pbfURL := p.URLs["pbf"]
		if pbfURL == "" {
			continue
		}
		if !includeParents && hasChild[p.ID] {
			continue
		}
		if parent != "" && !isDescendant(p.ID, parent) {
			continue
		}
		if substr != "" && !strings.Contains(p.ID, substr) {
			continue
		}
		out = append(out, pbfSource{Name: p.ID, URL: pbfURL})
	}
	return out, nil
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
	collName string
	osmtiler.QueryOptions
}

func addTileQueryFlags(fs *flag.FlagSet) *tileQueryOpts {
	o := &tileQueryOpts{}
	fs.StringVar(&o.mongoURI, "mongo", "", "MongoDB connection URI (required)")
	fs.StringVar(&o.dbName, "db", "osm", "MongoDB database name")
	fs.StringVar(&o.collName, "coll", "features", "MongoDB collection name")
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
	fmt.Println("filter (paste into mongosh):")
	fmt.Printf("  db.%s.find(%s).count()\n", opts.collName, indentJSON(string(pretty), "  "))
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	coll := client.Database(opts.dbName).Collection(opts.collName)

	start := time.Now()
	count, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	fmt.Printf("count       %d features in %s\n", count, time.Since(start).Round(time.Millisecond))
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
	coll := client.Database(opts.dbName).Collection(opts.collName)

	// Pad the bbox so the renderer has the cross-tile label-overdraw
	// features it expects (LabelBuffer pixels worth on each side).
	q := opts.QueryOptions
	q.PadBuffer = true
	queryStart := time.Now()
	features, stats, err := osmtiler.FetchTileFeatures(ctx, coll, z, x, y, q)
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
