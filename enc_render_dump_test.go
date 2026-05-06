package vc

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"go.viam.com/rdk/logging"
)

// TestDumpGreatSouthBay renders a small handful of stitched mosaics over Great
// South Bay so you can flip through them in Preview and see what the renderer
// is actually producing at each zoom. This is intentionally a few large images,
// not thousands of tiles, so it's easy to spot what's there and what's missing.
//
// Run:
//
//	go test -count=1 -run TestDumpGreatSouthBay -v ./...
//
// Output (default /tmp/gsb-tiles/):
//
//	gsb-z12.png   broad view of the bay   (8x8 tiles -> 2048x2048)
//	gsb-z14.png   mid zoom                (8x8 tiles -> 2048x2048)
//	gsb-z16.png   detail / channels       (8x8 tiles -> 2048x2048)
//
// Tunable via env vars:
//
//	GSB_OUT_DIR    /tmp/gsb-tiles
//	GSB_ZOOMS      12,14,16
//	GSB_GRID       8                       (NxN tiles per mosaic)
//	GSB_CENTER_LON -73.05
//	GSB_CENTER_LAT 40.66
//	GSB_CACHE_DIR  $HOME/.cache/viam-chartplotter/noaa-enc
//	GSB_PREFETCH   1                       (set to 0 to skip prefetch)
func TestDumpGreatSouthBay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping debug tile dump in short mode")
	}

	outDir := envOr("GSB_OUT_DIR", "/tmp/gsb-tiles")
	zooms := envOrInts(t, "GSB_ZOOMS", []int{12, 14, 16})
	grid := envOrInt(t, "GSB_GRID", 8)
	// Hollywood / Dania Beach area, FL — Port Everglades approach.
	centerLon := envOrFloat(t, "GSB_CENTER_LON", -80.10809)
	centerLat := envOrFloat(t, "GSB_CENTER_LAT", 26.11245)
	cacheDir := envOr("GSB_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
	doPrefetch := os.Getenv("GSB_PREFETCH") != "0"

	t.Logf("cache:  %s", cacheDir)
	t.Logf("out:    %s", outDir)
	t.Logf("zooms:  %v", zooms)
	t.Logf("grid:   %dx%d tiles per mosaic", grid, grid)
	t.Logf("center: %.4f, %.4f", centerLon, centerLat)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	logger := logging.NewTestLogger(t)
	catalog, err := NewENCCatalog(cacheDir, logger)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store, err := NewENCStore(cacheDir, catalog, logger)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer := NewENCRenderer(catalog, store, logger)

	ctx := context.Background()
	if err := catalog.EnsureFresh(ctx); err != nil {
		t.Logf("catalog refresh: %v (continuing with whatever is on disk)", err)
	}

	// Prefetch using the bbox of the largest zoom's grid so all relevant cells
	// are on disk before rendering.
	if doPrefetch {
		biggestZ := zooms[0]
		for _, z := range zooms {
			if z > biggestZ {
				biggestZ = z
			}
		}
		minLon, minLat, maxLon, maxLat := gridBBox(centerLon, centerLat, biggestZ, grid)
		dl, sk, err := store.SyncBBox(ctx, minLon, minLat, maxLon, maxLat, 0, 0, 4)
		if err != nil {
			t.Logf("prefetch: %v", err)
		} else {
			t.Logf("prefetch: %d downloaded, %d skipped", dl, sk)
		}
	}

	// Surface which cells actually overlap so we can rule out coverage gaps.
	for _, z := range zooms {
		minLon, minLat, maxLon, maxLat := gridBBox(centerLon, centerLat, z, grid)
		cells := catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, 0, 0)
		t.Logf("z=%d bbox=[%.3f,%.3f,%.3f,%.3f] -> %d cells overlap",
			z, minLon, minLat, maxLon, maxLat, len(cells))
		for _, c := range cells {
			s57 := store.S57Path(c.Name)
			t.Logf("  %-12s scale=1:%-7d on_disk=%v", c.Name, c.CScale, s57 != "")
		}
	}

	for _, z := range zooms {
		path := filepath.Join(outDir, fmt.Sprintf("gsb-z%d.png", z))
		if err := renderMosaic(t, renderer, centerLon, centerLat, z, grid, path); err != nil {
			t.Errorf("z=%d mosaic: %v", z, err)
			continue
		}
		t.Logf("wrote %s", path)
	}
	t.Logf("open output: open %s", outDir)
}

// gridBBox returns the lon/lat box covered by an NxN tile grid centered on the
// given lon/lat at the given zoom.
func gridBBox(centerLon, centerLat float64, z, n int) (float64, float64, float64, float64) {
	cx, cy := lonLatToTile(centerLon, centerLat, z)
	half := n / 2
	x0, y0 := cx-half, cy-half
	x1, y1 := x0+n-1, y0+n-1
	xminMerc, yminMerc, _, _ := tileBBoxMercator(tileXYZ{x: x0, y: y1, z: z})
	_, _, xmaxMerc, ymaxMerc := tileBBoxMercator(tileXYZ{x: x1, y: y0, z: z})
	minLon, maxLat := mercToLonLat(xminMerc, ymaxMerc)
	maxLon, minLat := mercToLonLat(xmaxMerc, yminMerc)
	return minLon, minLat, maxLon, maxLat
}

// renderMosaic renders an NxN tile grid centered on the given lon/lat and
// stitches it into a single PNG so it's easy to scan visually.
func renderMosaic(t *testing.T, renderer *ENCRenderer, centerLon, centerLat float64, z, n int, outPath string) error {
	cx, cy := lonLatToTile(centerLon, centerLat, z)
	half := n / 2
	x0, y0 := cx-half, cy-half

	const tileSize = 256
	mosaic := image.NewRGBA(image.Rect(0, 0, n*tileSize, n*tileSize))

	type job struct{ row, col int }
	jobs := make(chan job, n*n)
	for row := range n {
		for col := range n {
			jobs <- job{row: row, col: col}
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	var rendered, empty, failed int64
	workers := runtime.NumCPU()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				tx := x0 + j.col
				ty := y0 + j.row
				pngBytes, err := renderer.RenderTile(z, tx, ty, 8.0/feetPerMetre, StyleWMS)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					t.Logf("  z=%d/%d/%d: %v", z, tx, ty, err)
					continue
				}
				if len(pngBytes) < 200 {
					atomic.AddInt64(&empty, 1)
				}
				img, err := png.Decode(bytes.NewReader(pngBytes))
				if err != nil {
					atomic.AddInt64(&failed, 1)
					t.Logf("  z=%d/%d/%d decode: %v", z, tx, ty, err)
					continue
				}
				dst := image.Rect(j.col*tileSize, j.row*tileSize, (j.col+1)*tileSize, (j.row+1)*tileSize)
				draw.Draw(mosaic, dst, img, image.Point{}, draw.Over)
				atomic.AddInt64(&rendered, 1)
			}
		}()
	}
	wg.Wait()
	t.Logf("z=%d: rendered=%d empty=%d failed=%d", z, rendered, empty, failed)

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, mosaic)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrInt(t *testing.T, k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		t.Fatalf("env %s=%q: %v", k, v, err)
	}
	return n
}

func envOrInts(t *testing.T, k string, def []int) []int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var out []int
	for _, s := range splitComma(v) {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			t.Fatalf("env %s=%q: %v", k, v, err)
		}
		out = append(out, n)
	}
	return out
}

func envOrFloat(t *testing.T, k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil {
		t.Fatalf("env %s=%q: %v", k, v, err)
	}
	return f
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// mustUserCacheDir returns the same cache root the running Go server uses
// (~/Library/Caches on macOS, $XDG_CACHE_HOME or ~/.cache on Linux). Using a
// matching path means the test reads the cells the server has already
// downloaded instead of re-fetching into a parallel directory.
func mustUserCacheDir(t *testing.T) string {
	d, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("user cache dir: %v", err)
	}
	return d
}
