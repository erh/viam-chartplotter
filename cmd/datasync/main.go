// datasync periodically syncs NOAA ENC cells for a lon/lat box into MongoDB
// (the loop form of `mapsync noaa-ingest`). Run it as a standalone daemon when
// you're not using the Viam datasync model. Re-runs are cheap — cells already
// current in Mongo are skipped.
//
//	datasync --mongo mongodb://db:27017 --minlon -80.2 --minlat 32.6 \
//	         --maxlon -79.6 --maxlat 33.0 --interval 24h
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	minLon := flag.Float64("minlon", 0, "bbox min longitude")
	minLat := flag.Float64("minlat", 0, "bbox min latitude")
	maxLon := flag.Float64("maxlon", 0, "bbox max longitude")
	maxLat := flag.Float64("maxlat", 0, "bbox max latitude")
	minScale := flag.Int("minscale", 0, "min cell scale (0=no bound)")
	maxScale := flag.Int("maxscale", 0, "max cell scale (0=no bound)")
	parallel := flag.Int("parallel", 4, "concurrent cell downloads")
	interval := flag.Duration("interval", 24*time.Hour, "sync interval; 0 = run once and exit")
	flag.Parse()

	if *mongoURI == "" || *maxLon <= *minLon || *maxLat <= *minLat {
		return fmt.Errorf("--mongo and a valid bbox (--minlon --minlat --maxlon --maxlat) are required")
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
		stats, err := noaa.IngestBBox(ctx, coll, store, *minLon, *minLat, *maxLon, *maxLat, *minScale, *maxScale, *parallel, logf)
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
