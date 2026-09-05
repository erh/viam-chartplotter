// chartdiag reports why a chart query is slow, against a real database.
//
//	chartdiag --mongo mongodb://host:27017 --db osm route --start 41.47,-71.33 --end 41.55,-71.39
//	chartdiag --mongo ... search --q brenton
//
// It prints the indexes present on the collection, the exact filter the server
// would run, that query's explain() plan (index used, keys and docs examined),
// and a timed live run with the payload size. That is enough to tell a missing
// index from a corridor that is simply too big, which the driver's
// "context deadline exceeded" on its own is not.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
	"go.viam.com/rdk/logging"

	"github.com/erh/viam-chartplotter/render"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func realMain() error {
	// Go's flag package stops at the first non-flag argument, so a mode-first
	// invocation ("chartdiag route --start …") would silently ignore every
	// flag after the mode. Parse, take the mode, then parse what follows it.
	fs := flag.NewFlagSet("chartdiag", flag.ExitOnError)
	flag := fs // shadow so the declarations below read unchanged
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI")
	dbName := flag.String("db", envOr("MONGO_DB", "osm"), "MongoDB database")
	startArg := flag.String("start", "", "route start as lat,lon")
	endArg := flag.String("end", "", "route end as lat,lon")
	query := flag.String("q", "", "search text (search mode)")
	safeDepthFt := flag.Float64("sd", 6, "safe depth, feet")
	padM := flag.Float64("pad", 0, "corridor pad in metres (0 = the default 40% of the leg)")
	timeout := flag.Duration("timeout", 120*time.Second, "how long to let the live query run")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	mode := ""
	if rest := fs.Args(); len(rest) > 0 {
		mode = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
	}

	if *mongoURI == "" {
		return fmt.Errorf("need --mongo (or MONGO_URI)")
	}
	if mode == "" {
		return fmt.Errorf("need a mode: route or search")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+30*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	coll := noaa.OpenCollection(client.Database(*dbName))

	if err := reportIndexes(ctx, coll); err != nil {
		return err
	}

	switch mode {
	case "route":
		start, err := parsePoint(*startArg)
		if err != nil {
			return fmt.Errorf("--start: %w", err)
		}
		end, err := parsePoint(*endArg)
		if err != nil {
			return fmt.Errorf("--end: %w", err)
		}
		return diagRoute(ctx, coll, start, end, *safeDepthFt, *padM, *timeout)
	case "prewarm":
		start, err := parsePoint(*startArg)
		if err != nil {
			return fmt.Errorf("--start: %w", err)
		}
		end, err := parsePoint(*endArg)
		if err != nil {
			return fmt.Errorf("--end: %w", err)
		}
		return prewarm(ctx, client.Database(*dbName), start, end, *timeout)
	case "osm":
		if *query == "" {
			return fmt.Errorf("need --q")
		}
		return diagOSM(ctx, client.Database(*dbName), *query, *timeout)
	case "search":
		if *query == "" {
			return fmt.Errorf("need --q")
		}
		return diagSearch(ctx, coll, *query, *timeout)
	}
	return fmt.Errorf("unknown mode %q (want route, search, osm or prewarm)", mode)
}

// reportIndexes lists what's actually on the collection, flagging the ones the
// router and search depend on. A missing index here explains most timeouts.
func reportIndexes(ctx context.Context, coll *mongo.Collection) error {
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("list indexes: %w", err)
	}
	var specs []bson.M
	if err := cur.All(ctx, &specs); err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	have := map[string]bool{}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		n, _ := s["name"].(string)
		have[n] = true
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("indexes:", strings.Join(names, ", "))
	for _, want := range []string{"class_geo", "name_search"} {
		if !have[want] {
			fmt.Printf("  !! %s MISSING — run `datasync --indexes-only` (or `make ensure-indexes`)\n", want)
		}
	}
	fmt.Println()
	return nil
}

func diagRoute(ctx context.Context, coll *mongo.Collection, start, end render.RoutePoint, safeDepthFt, padM float64, timeout time.Duration) error {
	opts := render.DefaultAutoRouteOptions(safeDepthFt / 3.28084)
	if padM > 0 {
		opts.CorridorPadM = padM
	}
	plan := render.PlanAutoRoute(start, end, opts)

	widthKm := (plan.BBox[2] - plan.BBox[0]) * 111.32
	heightKm := (plan.BBox[3] - plan.BBox[1]) * 111.32
	fmt.Printf("corridor : %.4f,%.4f .. %.4f,%.4f  (~%.0f x %.0f km)\n",
		plan.BBox[1], plan.BBox[0], plan.BBox[3], plan.BBox[2], widthKm, heightKm)
	fmt.Printf("grid     : %d x %d cells at %.0f m\n", plan.GridW, plan.GridH, plan.CellM)
	fmt.Printf("geometry : %s\n", map[bool]string{true: "geomLow (simplified)", false: "full resolution"}[plan.UseLowGeom])
	fmt.Printf("classes  : %d (%s)\n\n", len(plan.Classes), strings.Join(plan.Classes, " "))

	filter := noaa.BBoxClassFilter(plan.BBox[0], plan.BBox[1], plan.BBox[2], plan.BBox[3], plan.Classes)
	return explainAndRun(ctx, coll, filter, "class_geo", timeout)
}

// prewarm builds the navigability tiles covering a box, so the first route
// through that water doesn't pay for them. --start and --end are opposite
// corners of the region, not a route.
func prewarm(ctx context.Context, db *mongo.Database, a, b render.RoutePoint, timeout time.Duration) error {
	minLon, maxLon := math.Min(a.Lng, b.Lng), math.Max(a.Lng, b.Lng)
	minLat, maxLat := math.Min(a.Lat, b.Lat), math.Max(a.Lat, b.Lat)

	r := render.NewENCRenderer(logging.NewLogger("prewarm"))
	r.SetNOAACollection(noaa.OpenCollection(db))
	navColl := noaa.OpenNavGridCollection(db)
	if err := noaa.EnsureNavGridIndexes(ctx, navColl); err != nil {
		return err
	}
	r.SetNavGridCollection(navColl)

	for z := render.NavMinZoom; z <= render.NavMaxZoom; z++ {
		keys := render.NavTilesForBBox(z, minLon, minLat, maxLon, maxLat)
		fmt.Printf("z%-3d %5d tiles (%.0f m cells) ", z, len(keys), render.NavCellSizeM(z, (minLat+maxLat)/2))
		began := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err := r.NavTiles(runCtx, z, minLon, minLat, maxLon, maxLat)
		cancel()
		if err != nil {
			fmt.Printf("FAILED after %s: %v\n", time.Since(began).Round(time.Second), err)
			continue
		}
		fmt.Printf("done in %s\n", time.Since(began).Round(time.Second))
	}
	return nil
}

// diagOSM reports the OSM side: which collections carry the name index, and
// how long a search of each actually takes. The collections differ hugely in
// size (osm_detail holds every building and street), so a search that is fine
// on one can be hopeless on another.
func diagOSM(ctx context.Context, db *mongo.Database, q string, timeout time.Duration) error {
	colls := osmtiler.OpenOSMCollections(db)
	for _, coll := range []*mongo.Collection{colls.Overview, colls.Coastal, colls.Detail, colls.Skip} {
		if coll == nil {
			continue
		}
		cur, err := coll.Indexes().List(ctx)
		if err != nil {
			fmt.Printf("%-14s index list failed: %v\n", coll.Name(), err)
			continue
		}
		var specs []bson.M
		_ = cur.All(ctx, &specs)
		names := make([]string, 0, len(specs))
		hasSearch := false
		for _, sp := range specs {
			n, _ := sp["name"].(string)
			names = append(names, n)
			if n == osmtiler.SearchIndexName {
				hasSearch = true
			}
		}
		est, _ := coll.EstimatedDocumentCount(ctx)
		mark := "ok"
		if !hasSearch {
			mark = "!! " + osmtiler.SearchIndexName + " MISSING"
		}
		fmt.Printf("%-14s %10d docs  %-28s [%s]\n", coll.Name(), est, strings.Join(names, ","), mark)
	}
	fmt.Println()

	for _, tc := range []struct {
		label string
		kinds map[string][]string
		limit int
	}{
		{"marine kinds, 20", osmtiler.MarineKinds(), 20},
		{"marine kinds, 200", osmtiler.MarineKinds(), 200},
		{"any kind, 20", nil, 20},
		{"any kind, 200", nil, 200},
	} {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		began := time.Now()
		hits, err := osmtiler.SearchByName(runCtx, colls, q, tc.limit, nil, tc.kinds)
		cancel()
		if err != nil {
			fmt.Printf("  %-18s FAILED after %s: %v\n", tc.label, time.Since(began).Round(time.Millisecond), err)
			continue
		}
		fmt.Printf("  %-18s %d hits in %s\n", tc.label, len(hits), time.Since(began).Round(time.Millisecond))
		for i, h := range hits {
			if i >= 2 {
				break
			}
			fmt.Printf("     %-34s %s\n", h.Name, h.Kind)
		}
	}
	return nil
}

func diagSearch(ctx context.Context, coll *mongo.Collection, q string, timeout time.Duration) error {
	fmt.Printf("search   : %q\n", q)
	filter := noaa.NameSearchFilter(q)
	blob, _ := json.Marshal(filter)
	fmt.Printf("filter   : %s\n\n", blob)

	// Show what actually came back — a search bug is usually visible in the
	// names, not in the plan.
	names, err := noaa.SearchByName(ctx, coll, q, "", 12, nil)
	if err != nil {
		fmt.Printf("  (search failed: %v)\n", err)
	} else {
		fmt.Printf("--- top %d names ---\n", len(names))
		for _, d := range names {
			fmt.Printf("  %-40s %-8s %s\n", d.Name, d.ObjectClass, d.Cell)
		}
		fmt.Println()
	}
	return explainAndRun(ctx, coll, filter, "name_search", timeout)
}

// explainAndRun reports the planner's choice for a filter, then actually runs
// it so we see wall time and payload rather than an estimate.
func explainAndRun(ctx context.Context, coll *mongo.Collection, filter bson.M, hint string, timeout time.Duration) error {
	for _, attempt := range []struct {
		label string
		hint  string
	}{{"planner's choice", ""}, {"hinted " + hint, hint}} {
		fmt.Printf("--- explain: %s ---\n", attempt.label)
		if err := explain(ctx, coll, filter, attempt.hint); err != nil {
			fmt.Printf("  (failed: %v)\n", err)
		}
	}

	fmt.Printf("\n--- live runs (limit %s each) ---\n", timeout)
	// Two runs: everything the old code fetched, and only the fields the
	// consumer actually reads. Over a remote link the difference between them
	// is the whole story — Mongo's own execution time is milliseconds.
	for _, run := range []struct {
		label      string
		projection bson.M
	}{
		{"whole documents", bson.M{"geomLow": 0}},
		{"routing projection", render.RoutingProjection()},
	} {
		if err := timedRun(ctx, coll, filter, run.projection, run.label, timeout); err != nil {
			return err
		}
	}
	// A usage-band ceiling drops the fine harbour/berthing cells, whose detail
	// is finer than a coarse routing grid can represent anyway. This is the
	// measurement that says whether that is worth doing.
	for _, band := range []int{4, 3} {
		banded := bson.M{}
		for k, v := range filter {
			banded[k] = v
		}
		banded["usageBand"] = bson.M{"$lte": band}
		if err := timedRun(ctx, coll, banded, render.RoutingProjection(),
			fmt.Sprintf("+ usageBand <= %d", band), timeout); err != nil {
			return err
		}
	}
	return nil
}

func timedRun(ctx context.Context, coll *mongo.Collection, filter, projection bson.M, label string, timeout time.Duration) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	began := time.Now()
	cur, err := coll.Find(runCtx, filter, options.Find().SetProjection(projection))
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	defer cur.Close(runCtx)
	docs, bytes := 0, 0
	for cur.Next(runCtx) {
		docs++
		bytes += len(cur.Current)
	}
	if err := cur.Err(); err != nil {
		fmt.Printf("  %-20s FAILED after %s at %d docs / %.1f MB: %v\n",
			label, time.Since(began).Round(time.Millisecond), docs, float64(bytes)/1e6, err)
		return nil
	}
	fmt.Printf("  %-20s %d docs, %.1f MB, %s\n", label, docs, float64(bytes)/1e6, time.Since(began).Round(time.Millisecond))
	return nil
}

func explain(ctx context.Context, coll *mongo.Collection, filter bson.M, hint string) error {
	cmd := bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "find", Value: coll.Name()},
			{Key: "filter", Value: filter},
			{Key: "hint", Value: hintValue(hint)},
		}},
		{Key: "verbosity", Value: "executionStats"},
	}
	if hint == "" {
		cmd = bson.D{
			{Key: "explain", Value: bson.D{
				{Key: "find", Value: coll.Name()},
				{Key: "filter", Value: filter},
			}},
			{Key: "verbosity", Value: "executionStats"},
		}
	}
	var out bson.M
	if err := coll.Database().RunCommand(ctx, cmd).Decode(&out); err != nil {
		return err
	}
	stats, _ := out["executionStats"].(bson.M)
	if stats == nil {
		return fmt.Errorf("no executionStats in explain output")
	}
	fmt.Printf("  index          : %s\n", indexName(stats))
	fmt.Printf("  keys examined  : %v\n", stats["totalKeysExamined"])
	fmt.Printf("  docs examined  : %v\n", stats["totalDocsExamined"])
	fmt.Printf("  docs returned  : %v\n", stats["nReturned"])
	fmt.Printf("  execution ms   : %v\n", stats["executionTimeMillis"])
	return nil
}

func hintValue(hint string) any {
	if hint == "" {
		return nil
	}
	return hint
}

// indexName digs the winning plan's index out of the explain tree, reporting
// COLLSCAN plainly — that is the answer we are usually looking for.
func indexName(stats bson.M) string {
	raw, err := json.Marshal(stats["executionStages"])
	if err != nil {
		return "?"
	}
	s := string(raw)
	if strings.Contains(s, `"COLLSCAN"`) {
		return "COLLSCAN (no index — this is the problem)"
	}
	const key = `"indexName":"`
	if i := strings.Index(s, key); i >= 0 {
		rest := s[i+len(key):]
		if j := strings.Index(rest, `"`); j >= 0 {
			return rest[:j]
		}
	}
	return "?"
}

func parsePoint(s string) (render.RoutePoint, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) != 2 {
		return render.RoutePoint{}, fmt.Errorf("want lat,lon (got %q)", s)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return render.RoutePoint{}, err
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return render.RoutePoint{}, err
	}
	return render.RoutePoint{Lat: lat, Lng: lon}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
