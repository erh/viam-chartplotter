// datasync periodically syncs the entire NOAA ENC catalog (worldwide) into
// MongoDB (the loop form of the NOAA ingest, over all cells). Run it as a
// standalone daemon when you're not using the Viam datasync model. Re-runs are
// cheap — cells already current in Mongo are skipped.
//
//	datasync --mongo mongodb://db:27017 --interval 24h
//
// Targeted re-ingest: pass .000 cell files (or directories of them) as
// positional args to re-parse exactly those cells in place — no catalog
// refresh, no downloads, and NO edition dedup (so it picks up a parser change
// like a new stored geometry field even though the NOAA edition is unchanged):
//
//	datasync --mongo mongodb://db:27017 ./noaa-enc/cells/US5NYC*  # one region
//	find ./noaa-enc -name '*.000' -print0 | xargs -0 datasync --mongo … # all on disk
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.viam.com/rdk/logging"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI (required)")
	dbName := flag.String("db", envOr("MONGO_DB", "osm"), "MongoDB database")
	encDir := flag.String("enc-dir", "./noaa-enc", "ENC cell download directory")
	minScale := flag.Int("minscale", 0, "min cell scale (0=no bound)")
	maxScale := flag.Int("maxscale", 0, "max cell scale (0=no bound)")
	parallel := flag.Int("parallel", 4, "concurrent cell downloads")
	interval := flag.Duration("interval", 24*time.Hour, "sync interval; 0 = run once and exit")
	flag.Parse()

	if *mongoURI == "" {
		return fmt.Errorf("--mongo (or MONGO_URI) is required")
	}

	logger := logging.NewLogger("datasync")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		cancel()
	}()

	connectCtx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	defer ccancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	coll := noaa.OpenCollection(client.Database(*dbName))
	if err := noaa.EnsureIndexes(ctx, coll); err != nil {
		return err
	}

	// Targeted re-ingest mode: positional args are cell files / dirs to re-parse
	// in place. Skips the catalog/store machinery entirely (no downloads, no
	// dedup) and exits when done.
	if paths := flag.Args(); len(paths) > 0 {
		return reingestPaths(ctx, coll, paths, logger)
	}

	catalog, err := noaa.NewCatalog(*encDir, logger.Sublogger("catalog"))
	if err != nil {
		return err
	}
	store, err := noaa.NewStore(*encDir, catalog, logger.Sublogger("store"))
	if err != nil {
		return err
	}

	logf := func(format string, a ...any) { logger.Infof(format, a...) }
	once := func() {
		stats, err := noaa.IngestAll(ctx, coll, store, *minScale, *maxScale, *parallel, logf)
		if err != nil {
			logger.Warnf("sync: %v", err)
			return
		}
		logger.Infof("sync done: %d cells, %d features, %d skipped, %d write-errors",
			stats.Cells, stats.Docs, stats.CellsSkipped, stats.WriteErrors)
	}

	once()
	if *interval <= 0 {
		return nil
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			once()
		}
	}
}

// reingestPaths re-parses the given .000 cell files (directories are walked for
// *.000) and upserts their features unconditionally — no edition dedup — so a
// parser change is reflected for exactly the cells you already have on disk.
func reingestPaths(ctx context.Context, coll *mongo.Collection, paths []string, logger logging.Logger) error {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			logger.Warnf("skip %s: %v", p, err)
			continue
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".000") {
				files = append(files, path)
			}
			return nil
		})
	}
	if len(files) == 0 {
		return fmt.Errorf("no .000 cell files found in %v", paths)
	}
	logger.Infof("reingest: %d cell file(s)", len(files))

	var total noaa.IngestStats
	for i, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := noaa.IngestCellFile(ctx, coll, "", f)
		if err != nil {
			logger.Warnf("reingest %s: %v", filepath.Base(f), err)
			continue
		}
		total.Cells += st.Cells
		total.Docs += st.Docs
		total.WriteErrors += st.WriteErrors
		total.GeomSkipped += st.GeomSkipped
		logger.Infof("reingest %d/%d %s: %d features (%d geom-skipped, %d write-errs)",
			i+1, len(files), noaa.CellNameFromPath(f), st.Docs, st.GeomSkipped, st.WriteErrors)
	}
	logger.Infof("reingest done: %d cells, %d features, %d write-errors, %d geom-skipped",
		total.Cells, total.Docs, total.WriteErrors, total.GeomSkipped)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
