// tileserver runs the chartplotter HTTP server as a standalone map + weather
// backend: it renders tiles and serves weather from MongoDB (and proxies NOAA
// WMS), with permissive CORS so app instances on other origins can fetch from
// it. No Viam robot needed — point chartplotter apps at it via the
// tile_server_base_url config attribute.
//
//	MONGO_URI=mongodb://db:27017 go run ./cmd/tileserver --port 8989
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"

	vc "github.com/erh/viam-chartplotter"
)

func main() {
	if err := realMain(); err != nil {
		panic(err)
	}
}

func realMain() error {
	port := flag.Int("port", envInt("PORT", 8989), "HTTP listen port")
	cacheDir := flag.String("cache-dir", os.Getenv("NOAA_CACHE_DIR"), "cache root (WMS/weather); default OS cache dir")
	mongoURI := flag.String("mongo", os.Getenv("MONGO_URI"), "MongoDB URI (tiles read from here)")
	mongoDB := flag.String("db", envOr("MONGO_DB", "osm"), "MongoDB database")
	// A standalone tile server has no boat/robot to connect to, so the frontend
	// it serves defaults to chart-extended (chart-only) mode. Pass --chart-only=false
	// to serve the full boat UI (e.g. when fronting a machine some other way).
	chartOnly := flag.Bool("chart-only", true, "serve the frontend in chart-only (no-boat) mode")
	flag.Parse()

	ctx := context.Background()
	logger := logging.NewLogger("tileserver")

	dist, err := vc.DistFS()
	if err != nil {
		return err
	}
	// tile_server_base_url is empty: this process IS the tile server, it serves
	// its own tiles same-origin.
	ws, err := vc.StartChartplotterServer(generic.Named("tileserver"), dist, logger,
		*port, *cacheDir, 0, 6, "", *mongoURI, *mongoDB, "features", "", *chartOnly)
	if err != nil {
		return err
	}
	defer ws.Close(ctx)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	logger.Infof("tileserver on :%d (mongo=%s db=%s); ctrl+c to exit", *port, *mongoURI, *mongoDB)
	<-sigs
	logger.Info("shutting down")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
