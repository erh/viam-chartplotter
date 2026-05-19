package osmtiler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
		// Expand by one tile in each direction so the inner-perimeter
		// tiles have rendered neighbours to stitch labels with. Without
		// this, labels whose anchor sits just outside the bbox draw the
		// "our side" half inside the perimeter tile and the rest is
		// missing because the neighbour was never rendered.
		maxIdx := (1 << z) - 1
		xMin = clampInt(xMin-1, 0, maxIdx)
		yMin = clampInt(yMin-1, 0, maxIdx)
		xMax = clampInt(xMax+1, 0, maxIdx)
		yMax = clampInt(yMax+1, 0, maxIdx)
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

	// Fetch tile.openstreetmap.org reference tiles for side-by-side
	// comparison. Cached on disk by filename so subsequent runs don't
	// re-fetch (OSM's usage policy requires "heavy" caching).
	osmFetched, osmCached, osmFailed := fetchOSMReferenceTiles(t, outDir, zooms)
	t.Logf("OSM reference: fetched=%d cached=%d failed=%d",
		osmFetched, osmCached, osmFailed)

	indexPath := filepath.Join(outDir, "index.html")
	if err := writeZoomIndexHTML(indexPath, zooms); err != nil {
		t.Errorf("write index.html: %v", err)
	} else {
		t.Logf("index: %s", indexPath)
	}
}

// fetchOSMReferenceTiles populates osm_zNN_x_y.png next to each of our
// rendered tiles. Already-cached files are skipped. Concurrency is
// capped at 4 so we stay polite under OSM's tile-usage policy; even
// across all zooms this is a few hundred tiles, one-time.
func fetchOSMReferenceTiles(t *testing.T, outDir string, zooms []zoomMeta) (fetched, cached, failed int64) {
	type job struct{ z, x, y int }
	jobs := make(chan job, 32)
	var wg sync.WaitGroup
	var fetchedA, cachedA, failedA atomic.Int64

	client := &http.Client{Timeout: 15 * time.Second}
	const userAgent = "viam-chartplotter-tests (+https://github.com/erh/viam-chartplotter)"

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				path := filepath.Join(outDir,
					fmt.Sprintf("osm_z%02d_%d_%d.png", j.z, j.x, j.y))
				if _, err := os.Stat(path); err == nil {
					cachedA.Add(1)
					continue
				}
				url := fmt.Sprintf("https://tile.openstreetmap.org/%d/%d/%d.png", j.z, j.x, j.y)
				req, err := http.NewRequest(http.MethodGet, url, nil)
				if err != nil {
					failedA.Add(1)
					continue
				}
				req.Header.Set("User-Agent", userAgent)
				resp, err := client.Do(req)
				if err != nil {
					t.Logf("osm fetch %s: %v", url, err)
					failedA.Add(1)
					continue
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					t.Logf("osm fetch %s: http %d", url, resp.StatusCode)
					failedA.Add(1)
					continue
				}
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					failedA.Add(1)
					continue
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					failedA.Add(1)
					continue
				}
				fetchedA.Add(1)
			}
		}()
	}

	for _, zm := range zooms {
		for x := zm.XMin; x <= zm.XMax; x++ {
			for y := zm.YMin; y <= zm.YMax; y++ {
				jobs <- job{zm.Z, x, y}
			}
		}
	}
	close(jobs)
	wg.Wait()
	return fetchedA.Load(), cachedA.Load(), failedA.Load()
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
  .pair { display: flex; gap: 1em; align-items: flex-start; }
  .pane { display: flex; flex-direction: column; }
  .pane .caption { font-size: 12px; color: #444; margin-bottom: .25em; }
  .grid {
    display: grid;
    grid-auto-rows: 256px;
    gap: 0;
    background: #e5e5e5;
    width: max-content;
    border: 1px solid #999;
  }
  .grid img { display: block; width: 256px; height: 256px; image-rendering: pixelated; }
  .grid .missing { width: 256px; height: 256px; background: repeating-linear-gradient(45deg, #e5e5e5 0 10px, #d8d8d8 10px 20px); }
</style>
</head><body>
<h1>ZIP 10024 &mdash; OSM tile checkpoint</h1>
<p>Left: our renderer. Right: tile.openstreetmap.org reference at the same (z, x, y). Our background is transparent (gray here approximates what an image viewer paints behind transparent PNGs); the OSM tiles have their own water/land fills baked in.</p>
`)
	for _, zm := range zooms {
		cols := zm.XMax - zm.XMin + 1
		rows := zm.YMax - zm.YMin + 1
		fmt.Fprintf(w, `<div class="zoom">
<h2>z = %d</h2>
<div class="label">x=[%d..%d] y=[%d..%d] &mdash; %d&times;%d = %d tiles</div>
<div class="pair">
<div class="pane"><div class="caption">ours</div>
<div class="grid" style="grid-template-columns: repeat(%d, 256px);">
`, zm.Z, zm.XMin, zm.XMax, zm.YMin, zm.YMax, cols, rows, cols*rows, cols)
		for y := zm.YMin; y <= zm.YMax; y++ {
			for x := zm.XMin; x <= zm.XMax; x++ {
				fmt.Fprintf(w, `<img src="z%02d_%d_%d.png" title="z=%d x=%d y=%d">`+"\n",
					zm.Z, x, y, zm.Z, x, y)
			}
		}
		fmt.Fprintf(w, `</div></div>
<div class="pane"><div class="caption">tile.openstreetmap.org</div>
<div class="grid" style="grid-template-columns: repeat(%d, 256px);">
`, cols)
		for y := zm.YMin; y <= zm.YMax; y++ {
			for x := zm.XMin; x <= zm.XMax; x++ {
				osmName := fmt.Sprintf("osm_z%02d_%d_%d.png", zm.Z, x, y)
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), osmName)); err == nil {
					fmt.Fprintf(w, `<img src="%s" title="osm z=%d x=%d y=%d">`+"\n",
						osmName, zm.Z, x, y)
				} else {
					fmt.Fprintln(w, `<div class="missing"></div>`)
				}
			}
		}
		fmt.Fprintln(w, "</div></div></div></div>")
	}
	fmt.Fprintln(w, "</body></html>")
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
