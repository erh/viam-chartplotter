// render-cmd renders chartplotter tiles straight from MongoDB to disk for a
// fast, server-free debug loop. Give it a lat/lon (and optional zoom); it
// writes our ENC render, the OSM underlay, the merged tile, and — unless
// --wms=false — NOAA's WMS tile plus a side-by-side compare panel, then prints
// the per-tile feature breakdown the renderer saw.
//
//	go run ./cmd/render --lat 32.79 --lon -79.86 --zoom 13
//	go build -o render-cmd ./cmd/render && ./render-cmd --lat 41.7 --lon -70.3
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.viam.com/rdk/logging"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
	"github.com/erh/viam-chartplotter/render"
)

const mercatorMax = 20037508.342789244

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	// Default location: Charleston, SC harbour (Mount Pleasant) — a mixed
	// land/water/harbour scene that's a good rendering stress test.
	lat := flag.Float64("lat", 32.79, "latitude (degrees)")
	lon := flag.Float64("lon", -79.86, "longitude (degrees)")
	zoom := flag.Int("zoom", 13, "tile zoom level")
	mongoURI := flag.String("mongo", "mongodb://erh-23big.local:27017", "MongoDB URI")
	dbName := flag.String("db", "osm", "MongoDB database")
	outDir := flag.String("out", "/tmp/render", "output directory")
	sd := flag.Float64("sd", 6, "safe depth (feet)")
	fetchWMS := flag.Bool("wms", true, "also fetch NOAA WMS and build a compare panel")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	x, y := lonLatToTile(*lon, *lat, *zoom)
	fmt.Printf("lat=%.5f lon=%.5f -> z=%d x=%d y=%d\n", *lat, *lon, *zoom, x, y)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(*mongoURI))
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	db := client.Database(*dbName)

	r := render.NewENCRenderer(nil, nil, logging.NewLogger("render-cmd"))
	r.SetNOAACollection(noaa.OpenCollection(db))
	r.SetOSMCollections(osmtiler.OpenOSMCollections(db))

	report, _ := r.TileFeatureReport(*zoom, x, y)
	fmt.Printf("features=%d query=%.0fms\n  byKind=%v\n  byScale=%v\n  byClass=%v\n",
		report.Features, report.QueryMS, report.ByKind, report.ByScale, report.ByClass)

	opts := render.RenderOptions{SafeDepthM: *sd / 3.28084, Style: render.StyleWMS}

	enc, encT, err := r.RenderTile(*zoom, x, y, opts)
	if err != nil {
		return fmt.Errorf("render enc: %w", err)
	}
	write(*outDir, "enc", *zoom, x, y, enc)
	osmPNG, _, _ := r.RenderOSMTile(*zoom, x, y)
	write(*outDir, "osm", *zoom, x, y, osmPNG)
	// MERGED uses the frontend's per-zoom params so it matches what the app
	// actually composites (transparent land so OSM streets show, etc.).
	merged, _, mt, err := r.RenderMergedTile(*zoom, x, y, render.BrowserMergedOptions(*zoom, *sd/3.28084))
	if err != nil {
		return fmt.Errorf("render merged: %w", err)
	}
	write(*outDir, "merged", *zoom, x, y, merged)
	fmt.Printf("timing: enc-db=%.0fms enc-draw=%.0fms osm=%.0fms composite=%.0fms\n",
		encT.QueryMS, encT.DrawMS, mt.OSMMS, mt.CompositeMS)

	encImg, _ := png.Decode(bytes.NewReader(enc))
	osmImg, _ := png.Decode(bytes.NewReader(osmPNG))
	mergedImg, _ := png.Decode(bytes.NewReader(merged))
	var wmsImg image.Image
	if *fetchWMS {
		wms, err := fetchWMSPNG(ctx, *zoom, x, y)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wms fetch failed (continuing):", err)
		} else {
			write(*outDir, "wms", *zoom, x, y, wms)
			wmsImg, _ = png.Decode(bytes.NewReader(wms))
		}
	}

	// ENC | OSM | MERGED | WMS — MERGED is exactly what the app shows.
	panel := buildPanel(encImg, osmImg, mergedImg, wmsImg)
	panelPath := filepath.Join(*outDir, fmt.Sprintf("compare-z%d-x%d-y%d.png", *zoom, x, y))
	if err := writePNG(panelPath, panel); err != nil {
		return err
	}
	fmt.Printf("wrote %s  (panels: ENC | OSM | MERGED | WMS)\n", panelPath)
	return nil
}

// buildPanel lays out ENC | OSM | MERGED | WMS over white, 256px each. MERGED
// (OSM under ENC) is the tile the live app composites and serves.
func buildPanel(panels ...image.Image) image.Image {
	const w = 256
	out := image.NewRGBA(image.Rect(0, 0, w*len(panels), w))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	for i, p := range panels {
		if p != nil {
			draw.Draw(out, image.Rect(i*w, 0, (i+1)*w, w), p, image.Point{}, draw.Over)
		}
	}
	return out
}

func fetchWMSPNG(ctx context.Context, z, x, y int) ([]byte, error) {
	size := 2 * mercatorMax / float64(int(1)<<z)
	xmin := -mercatorMax + float64(x)*size
	xmax := xmin + size
	ymax := mercatorMax - float64(y)*size
	ymin := ymax - size
	url := fmt.Sprintf("https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/ENCOnline/MapServer/exts/MaritimeChartService/WMSServer"+
		"?SERVICE=WMS&VERSION=1.3.0&REQUEST=GetMap&FORMAT=image/png&TRANSPARENT=TRUE"+
		"&LAYERS=0,1,2,3,4,5,6&CRS=EPSG:3857&WIDTH=256&HEIGHT=256&STYLES=&BBOX=%f,%f,%f,%f",
		xmin, ymin, xmax, ymax)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wms status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func lonLatToTile(lon, lat float64, z int) (int, int) {
	n := math.Exp2(float64(z))
	x := int((lon + 180.0) / 360.0 * n)
	latRad := lat * math.Pi / 180.0
	y := int((1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * n)
	return x, y
}

func write(dir, kind string, z, x, y int, b []byte) {
	if len(b) == 0 {
		return
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-z%d-x%d-y%d.png", kind, z, x, y))
	_ = os.WriteFile(p, b, 0o644)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
