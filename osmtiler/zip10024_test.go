package osmtiler

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// ZIP 10024 (Upper West Side, Manhattan) — bounding box used as the
// "where are we" visual checkpoint while v0.1 is being built out. The
// box is intentionally a bit generous: West 79th to West 91st Street
// between the Hudson and Central Park West.
const (
	zip10024MinLon = -73.990
	zip10024MaxLon = -73.965
	zip10024MinLat = 40.778
	zip10024MaxLat = 40.796
)

// TestRenderZip10024 renders one PNG per (z, x, y) tile that covers
// the ZIP 10024 bbox at every zoom 0..18. Output goes to OSM_TILES_OUT
// (default /tmp/osm-zip10024-tiles/zNN_x_y.png) so you can scroll a
// directory of PNGs and eyeball what the renderer currently produces.
//
// Requires a Geofabrik- or BBBike-style .osm.pbf that covers
// Manhattan, set via OSM_NYC_PBF. Skipped in -short mode and when no
// PBF is configured (with a download hint).
func TestRenderZip10024(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping zip10024 visual render in short mode")
	}
	pbfPath := os.Getenv("OSM_NYC_PBF")
	if pbfPath == "" {
		t.Skip("OSM_NYC_PBF not set. Grab a NYC extract and re-run, e.g.:\n" +
			"  curl -L -o /tmp/NewYork.osm.pbf \\\n" +
			"    https://download.bbbike.org/osm/bbbike/NewYork/NewYork.osm.pbf\n" +
			"  OSM_NYC_PBF=/tmp/NewYork.osm.pbf go test ./osmtiler -run TestRenderZip10024 -v")
	}
	if _, err := os.Stat(pbfPath); err != nil {
		t.Fatalf("OSM_NYC_PBF=%s: %v", pbfPath, err)
	}

	outDir := envOr("OSM_TILES_OUT", "/tmp/osm-zip10024-tiles")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("pbf: %s", pbfPath)
	t.Logf("out: %s", outDir)

	fs, err := LoadPBF(context.Background(), pbfPath)
	if err != nil {
		t.Fatalf("LoadPBF: %v", err)
	}
	t.Logf("loaded %d features", len(fs.Features))

	classCounts := make(map[Class]int)
	for i := range fs.Features {
		classCounts[fs.Features[i].Class]++
	}
	for c := Class(1); c < ClassCount; c++ {
		if n := classCounts[c]; n > 0 {
			t.Logf("  %-9s %d", c, n)
		}
	}

	// Worker pool — RenderTile only reads from fs (which is sorted
	// and immutable after LoadPBF), so concurrent calls are safe.
	type job struct{ z, x, y int }
	jobs := make(chan job, 64)
	var (
		wg       sync.WaitGroup
		rendered atomic.Int64
		failed   atomic.Int64
	)
	workers := runtime.GOMAXPROCS(0)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				data, err := RenderTile(fs, j.z, j.x, j.y)
				if err != nil {
					t.Errorf("render z%d/%d/%d: %v", j.z, j.x, j.y, err)
					failed.Add(1)
					continue
				}
				path := filepath.Join(outDir, fmt.Sprintf("z%02d_%d_%d.png", j.z, j.x, j.y))
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Errorf("write %s: %v", path, err)
					failed.Add(1)
					continue
				}
				rendered.Add(1)
			}
		}()
	}

	var zooms []zoomMeta
	for z := 0; z <= 18; z++ {
		xMin, yMin, xMax, yMax := TilesCoveringBBox(
			zip10024MinLon, zip10024MinLat,
			zip10024MaxLon, zip10024MaxLat, z)
		for x := xMin; x <= xMax; x++ {
			for y := yMin; y <= yMax; y++ {
				jobs <- job{z, x, y}
			}
		}
		zooms = append(zooms, zoomMeta{Z: z, XMin: xMin, XMax: xMax, YMin: yMin, YMax: yMax})
		t.Logf("z=%2d: queued %d tiles (x=[%d..%d] y=[%d..%d])",
			z, (xMax-xMin+1)*(yMax-yMin+1), xMin, xMax, yMin, yMax)
	}
	close(jobs)
	wg.Wait()
	t.Logf("total: %d tiles written to %s (failed=%d, workers=%d)",
		rendered.Load(), outDir, failed.Load(), workers)

	indexPath := filepath.Join(outDir, "index.html")
	if err := writeZoomIndexHTML(indexPath, zooms); err != nil {
		t.Errorf("write index.html: %v", err)
	} else {
		t.Logf("index: %s", indexPath)
	}
}

// zoomMeta is the per-zoom data the HTML index needs to lay tiles out
// in their true (x, y) grid position so adjacent tiles stitch up.
type zoomMeta struct {
	Z                      int
	XMin, YMin, XMax, YMax int
}

// writeZoomIndexHTML produces an index.html in outDir showing every
// rendered tile laid out in its true (x, y) grid position, grouped by
// zoom. The underlay color matches the neutral gray that most image
// viewers paint behind a transparent PNG, so opening a tile standalone
// vs. in the index looks the same.
func writeZoomIndexHTML(path string, zooms []zoomMeta) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	fmt.Fprint(w, `<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<title>ZIP 10024 &mdash; OSM tile checkpoint</title>
<style>
  body { font-family: sans-serif; background: #f0f0f0; margin: 0; padding: 1em; }
  h1 { font-size: 18px; }
  .zoom { margin-bottom: 2em; }
  .zoom h2 { margin: 0 0 .25em; font-size: 14px; }
  .label { color: #888; font-size: 12px; font-family: monospace; margin-bottom: .25em; }
  .grid {
    display: grid;
    grid-auto-rows: 256px;
    gap: 0;
    background: #e5e5e5;
    width: max-content;
    border: 1px solid #999;
  }
  .grid img { display: block; width: 256px; height: 256px; image-rendering: pixelated; }
</style>
</head><body>
<h1>ZIP 10024 &mdash; OSM tile checkpoint</h1>
<p>Tiles are rendered with transparent water so the chart's water layer can show through in production. The gray underlay here approximates what an image viewer paints behind transparent PNGs.</p>
`)
	for _, zm := range zooms {
		cols := zm.XMax - zm.XMin + 1
		rows := zm.YMax - zm.YMin + 1
		fmt.Fprintf(w, `<div class="zoom">
<h2>z = %d</h2>
<div class="label">x=[%d..%d] y=[%d..%d] &mdash; %d&times;%d = %d tiles</div>
<div class="grid" style="grid-template-columns: repeat(%d, 256px);">
`, zm.Z, zm.XMin, zm.XMax, zm.YMin, zm.YMax, cols, rows, cols*rows, cols)
		// Row-major: top-to-bottom (small y to large y), left-to-right.
		for y := zm.YMin; y <= zm.YMax; y++ {
			for x := zm.XMin; x <= zm.XMax; x++ {
				fmt.Fprintf(w, `<img src="z%02d_%d_%d.png" title="z=%d x=%d y=%d">`+"\n",
					zm.Z, x, y, zm.Z, x, y)
			}
		}
		fmt.Fprintln(w, "</div></div>")
	}
	fmt.Fprintln(w, "</body></html>")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
