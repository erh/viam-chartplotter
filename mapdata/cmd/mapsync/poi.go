package main

// ----- ingest-poi ----------------------------------------------------------
//
// Ingest a non-ENC point-of-interest dataset (wrecks, artificial reefs,
// offshore platforms) into the `poi` collection. See mapdata/poi for what the
// datasets are and how their fields are mapped.
//
//	# what fields does this feed actually publish?
//	mapsync ingest-poi --dataset ocs-wrecks --url <layer> --inspect
//
//	# load it
//	mapsync ingest-poi --mongo mongodb://db:27017 --dataset ocs-wrecks --url <layer>
//
//	# or from a file someone already downloaded and converted
//	mapsync ingest-poi --mongo mongodb://db:27017 --dataset fl-reefs --file reefs.geojson
//
// The fetch is deliberately not automatic and not scheduled: these are public
// services with no SLA, the layer number inside a MapServer is a choice, and a
// chartplotter on a boat should not be discovering at sea that a feed moved.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/mapdata/poi"
)

func runIngestPOI(args []string) error {
	fs := flag.NewFlagSet("ingest-poi", flag.ContinueOnError)
	mongoURI := fs.String("mongo", os.Getenv("MONGO_URI"), "MongoDB connection URI (required unless --inspect)")
	dbName := fs.String("db", "osm", "MongoDB database name")
	dataset := fs.String("dataset", "", "dataset key: "+strings.Join(poi.DatasetKeys(), ", "))
	layerURL := fs.String("url", "", "ArcGIS feature layer to fetch (paged, f=geojson)")
	file := fs.String("file", "", "read a GeoJSON FeatureCollection from disk instead of fetching")
	bboxArg := fs.String("bbox", "", "keep only features inside minLon,minLat,maxLon,maxLat")
	pageSize := fs.Int("page-size", 1000, "records per ArcGIS request")
	inspect := fs.Bool("inspect", false, "print the property keys the feed publishes, then exit (no writes)")
	prune := fs.Bool("prune", false, "after a full-feed ingest, delete rows this run didn't write (rows gone upstream)")
	drop := fs.Bool("drop", false, "delete the dataset and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mapsync ingest-poi --dataset <key> [--url <layer> | --file <geojson>] [flags]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Datasets:")
		for _, k := range poi.DatasetKeys() {
			d := poi.Datasets[k]
			fmt.Fprintf(os.Stderr, "  %-18s %s\n", d.Key, d.Description)
			fmt.Fprintf(os.Stderr, "  %-18s   %s\n", "", d.URL)
		}
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ds, ok := poi.Datasets[*dataset]
	if !ok {
		fs.Usage()
		return fmt.Errorf("--dataset must be one of: %s", strings.Join(poi.DatasetKeys(), ", "))
	}
	var bbox *[4]float64
	if *bboxArg != "" {
		parsed, err := parsePOIBBox(*bboxArg)
		if err != nil {
			return err
		}
		bbox = &parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// --drop needs the database and nothing else.
	if *drop {
		if *mongoURI == "" {
			return fmt.Errorf("--mongo (or MONGO_URI) is required")
		}
		client, disconnect, err := connectMongo(ctx, *mongoURI)
		if err != nil {
			return err
		}
		defer disconnect()
		n, err := poi.Drop(ctx, poi.Open(client.Database(*dbName)), ds.Key)
		if err != nil {
			return err
		}
		fmt.Printf("dropped %d %s documents\n", n, ds.Key)
		return nil
	}

	features, err := readFeed(ctx, *file, *layerURL, *pageSize, bbox)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d features\n", ds.Key, len(features))

	// --inspect exists because field names are the one thing about these feeds
	// that can't be looked up reliably: agencies rename columns between
	// vintages, and a mapping written against the docs can silently produce
	// unnamed rows. Print what actually arrived.
	if *inspect {
		fmt.Println("property keys:")
		for _, k := range poi.SampleKeys(features, 50) {
			fmt.Println("  " + k)
		}
		if len(features) > 0 {
			p, ok := ds.Adapter(features[0])
			fmt.Printf("first feature maps to: ok=%v name=%q class=%s at %.4f,%.4f detail=%q\n",
				ok, p.Name, p.Class, p.Lat, p.Lng, p.Detail)
		}
		return nil
	}

	if *mongoURI == "" {
		return fmt.Errorf("--mongo (or MONGO_URI) is required")
	}
	client, disconnect, err := connectMongo(ctx, *mongoURI)
	if err != nil {
		return err
	}
	defer disconnect()

	coll := poi.Open(client.Database(*dbName))
	if err := poi.EnsureIndexes(ctx, coll); err != nil {
		return err
	}
	stats, began, err := poi.Ingest(ctx, coll, ds.Key, features, ds.Adapter, bbox)
	if err != nil {
		return err
	}
	fmt.Printf("%s: read=%d written=%d dropped=%d\n", ds.Key, stats.Read, stats.Written, stats.Dropped)

	// Pruning is opt-in and refuses a partial run: deleting "everything this
	// run didn't write" after a bbox-scoped or interrupted ingest would delete
	// the rest of the country.
	if *prune {
		if bbox != nil {
			return fmt.Errorf("--prune needs a full-feed ingest; it would delete everything outside --bbox")
		}
		n, err := poi.Prune(ctx, coll, ds.Key, began)
		if err != nil {
			return err
		}
		fmt.Printf("%s: pruned %d stale documents\n", ds.Key, n)
	}
	fmt.Println("rebuild the gazetteer to make these searchable: datasync --build-places")
	return nil
}

// readFeed loads the dataset from disk or fetches it from an ArcGIS layer.
func readFeed(ctx context.Context, file, layerURL string, pageSize int, bbox *[4]float64) ([]poi.Feature, error) {
	switch {
	case file != "" && layerURL != "":
		return nil, fmt.Errorf("pass --file or --url, not both")
	case file != "":
		return poi.ReadFile(file)
	case layerURL != "":
		return poi.FetchArcGIS(ctx, layerURL, poi.FetchOptions{
			PageSize: pageSize,
			BBox:     bbox,
			Progress: func(n int) { fmt.Printf("  fetched %d…\n", n) },
		})
	default:
		return nil, fmt.Errorf("need --url (fetch) or --file (local GeoJSON)")
	}
}

func connectMongo(ctx context.Context, uri string) (*mongo.Client, func(), error) {
	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(connectCtx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := client.Ping(connectCtx, nil); err != nil {
		return nil, nil, fmt.Errorf("mongo ping: %w", err)
	}
	return client, func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}, nil
}

func parsePOIBBox(s string) ([4]float64, error) {
	var out [4]float64
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return out, fmt.Errorf("--bbox wants minLon,minLat,maxLon,maxLat")
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return out, fmt.Errorf("--bbox: %w", err)
		}
		out[i] = v
	}
	return out, nil
}
