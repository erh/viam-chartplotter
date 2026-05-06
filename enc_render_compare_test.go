package vc

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/fogleman/gg"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/font/basicfont"
)

// TestCompareWithWMS renders our tile next to NOAA's WMS tile for the same
// XYZ and writes a 3-panel comparison PNG ([ours | WMS | diff]) per tile, plus
// a quantitative diff metric. The point is to be able to iterate on the
// renderer without the human in the loop: tweak something, run the test,
// look at the metric and the new comparison images, repeat.
//
// Run:
//
//	go test -count=1 -run TestCompareWithWMS -v ./...
//
// Output (default /tmp/noaa-compare/):
//
//	z{Z}-x{X}-y{Y}.png   3-panel image, 768x256
//
// Tunable via env vars:
//
//	CMP_OUT_DIR        /tmp/noaa-compare
//	CMP_CACHE_DIR      $UserCacheDir/viam-chartplotter/noaa-enc
//	CMP_WMS_CACHE_DIR  $UserCacheDir/viam-chartplotter/noaa-wms
//	CMP_SAFE_DEPTH_FT  6
//	CMP_TILES          15:9405:13010,14:4702:6505,16:18811:26021
//	CMP_PREFETCH       1   (set to 0 to skip ENC prefetch — assumes cells on disk)
func TestCompareWithWMS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wms-compare in short mode")
	}

	outDir := envOr("CMP_OUT_DIR", "/tmp/noaa-compare")
	cacheDir := envOr("CMP_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-enc"))
	wmsCacheDir := envOr("CMP_WMS_CACHE_DIR", filepath.Join(mustUserCacheDir(t), "viam-chartplotter", "noaa-wms"))
	// Match the module's default safe_depth_ft so test renders agree with
	// what the live /noaa-enc/tile/... URL produces. depthFill clamps
	// effective safety up to SHALLOW+1 m (= 3 m), which lines up with
	// NOAA's WMS effective rendering after sampling.
	safeDepthFt := envOrFloat(t, "CMP_SAFE_DEPTH_FT", 6)
	doPrefetch := os.Getenv("CMP_PREFETCH") != "0"
	// Curated regression set — five user-flagged tiles spanning z=12..16
	// across different chart-content scenarios:
	//   z=12 Chesapeake approach (mostly water, soundings, channel)
	//   z=13 offshore Virginia (deep water, depth contour, label-heavy)
	//   z=14 offshore (DEPMD-dominant, sparse features)
	//   z=15 Beaufort Harbor (mixed land/water/structures)
	//   z=16 Norfolk Harbor (BUAARE land + BUISGL buildings)
	// Override with CMP_TILES to test other tiles.
	tiles := envOrTiles(t, "CMP_TILES", []tileXYZ{
		{z: 12, x: 1184, y: 1593},
		{z: 13, x: 2368, y: 3187},
		{z: 14, x: 4737, y: 6375},
		{z: 15, x: 9405, y: 13010},
		{z: 16, x: 18897, y: 25526},
	})

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	t.Logf("cache:      %s", cacheDir)
	t.Logf("wms cache:  %s", wmsCacheDir)
	t.Logf("out:        %s", outDir)
	t.Logf("safe depth: %.1f ft", safeDepthFt)
	t.Logf("tiles:      %v", tiles)

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
	wmsCache, err := NewNoaaCache(wmsCacheDir, 0, logger)
	if err != nil {
		t.Fatalf("wms cache: %v", err)
	}

	ctx := context.Background()
	if err := catalog.EnsureFresh(ctx); err != nil {
		t.Logf("catalog refresh: %v (continuing with whatever is on disk)", err)
	}

	safeDepthM := safeDepthFt / feetPerMetre

	// One render per tile: prefetch its bbox, render ours, fetch WMS, save the
	// 3-panel image, log the metric.
	var rows []gridRow
	for _, tile := range tiles {
		if doPrefetch {
			minLon, minLat, maxLon, maxLat := tileLonLatBBox(tile)
			dl, sk, err := store.SyncBBox(ctx, minLon, minLat, maxLon, maxLat, 0, 0, 4)
			if err != nil {
				t.Logf("z=%d x=%d y=%d prefetch: %v", tile.z, tile.x, tile.y, err)
			} else if dl > 0 || sk > 0 {
				t.Logf("z=%d x=%d y=%d prefetch: %d downloaded, %d skipped", tile.z, tile.x, tile.y, dl, sk)
			}
		}

		// Render in WMS style — the compare metric only makes sense against
		// the WMS-matched output. ECDIS-style rendering deliberately diverges
		// from NOAA WMS (bold safety contour, two-tone soundings, topmarks)
		// and would skew the diff numbers.
		ourBytes, err := renderer.RenderTile(tile.z, tile.x, tile.y, safeDepthM, StyleWMS)
		if err != nil {
			t.Errorf("z=%d x=%d y=%d render: %v", tile.z, tile.x, tile.y, err)
			continue
		}
		ourImg, err := png.Decode(bytes.NewReader(ourBytes))
		if err != nil {
			t.Errorf("z=%d x=%d y=%d decode our: %v", tile.z, tile.x, tile.y, err)
			continue
		}

		canonical := wmsCanonicalForTile(tile, "")
		wmsBytes, _, _, err := wmsCache.fetch(ctx, canonical, "image/png")
		if err != nil {
			t.Errorf("z=%d x=%d y=%d wms fetch: %v", tile.z, tile.x, tile.y, err)
			continue
		}
		wmsImg, err := png.Decode(bytes.NewReader(wmsBytes))
		if err != nil {
			t.Errorf("z=%d x=%d y=%d decode wms: %v", tile.z, tile.x, tile.y, err)
			continue
		}

		panel := buildComparePanel(ourImg, wmsImg)
		outPath := filepath.Join(outDir, fmt.Sprintf("z%d-x%d-y%d.png", tile.z, tile.x, tile.y))
		if err := writePNG(outPath, panel); err != nil {
			t.Errorf("write %s: %v", outPath, err)
			continue
		}
		// Also save the raw inputs so it's easy to sample colours/text from
		// either side without parsing the panel out of the comparison image.
		_ = os.WriteFile(filepath.Join(outDir, fmt.Sprintf("z%d-x%d-y%d-ours.png", tile.z, tile.x, tile.y)), ourBytes, 0o644)
		_ = os.WriteFile(filepath.Join(outDir, fmt.Sprintf("z%d-x%d-y%d-wms.png", tile.z, tile.x, tile.y)), wmsBytes, 0o644)
		m := compareMetric(ourImg, wmsImg)
		t.Logf("z=%-2d x=%-6d y=%-6d  avg_rgb_diff=%6.2f  pct_diff>30=%5.2f%%  pct_diff>60=%5.2f%%  saved=%s",
			tile.z, tile.x, tile.y, m.avgDelta, m.pctOver30, m.pctOver60, outPath)
		rows = append(rows, gridRow{tile: tile, panel: panel, metric: m})
	}

	if len(rows) > 0 {
		gridPath := filepath.Join(outDir, "grid.png")
		if err := writeGrid(gridPath, rows); err != nil {
			t.Errorf("write grid: %v", err)
		} else {
			t.Logf("grid:    %s (%d tiles)", gridPath, len(rows))
		}
		var sumAvg, sumPct30, sumPct60 float64
		for _, r := range rows {
			sumAvg += r.metric.avgDelta
			sumPct30 += r.metric.pctOver30
			sumPct60 += r.metric.pctOver60
		}
		n := float64(len(rows))
		t.Logf("summary: tiles=%d  avg_rgb_diff=%.2f  pct_diff>30=%.2f%%  pct_diff>60=%.2f%%",
			len(rows), sumAvg/n, sumPct30/n, sumPct60/n)
	}
}

// gridRow holds one tile's compare panel plus its metric so we can stack
// them into a single review image at the end of the test.
type gridRow struct {
	tile   tileXYZ
	panel  *image.RGBA
	metric cmpMetric
}

// writeGrid stacks the compare panels vertically with a header strip per row
// containing the tile coords and metrics. The output is a single large PNG
// good for a quick visual review of all five tiles at once.
func writeGrid(path string, rows []gridRow) error {
	const (
		panelW    = 768
		panelH    = 256
		headerH   = 28 // tile coords + metric line
		paddingH  = 4
	)
	rowH := panelH + headerH + paddingH
	totalH := rowH*len(rows) + paddingH
	out := image.NewRGBA(image.Rect(0, 0, panelW, totalH))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.RGBA{R: 0xF4, G: 0xF4, B: 0xF4, A: 0xFF}}, image.Point{}, draw.Src)

	dc := gg.NewContextForRGBA(out)
	dc.SetFontFace(basicfont.Face7x13)
	for i, r := range rows {
		y := paddingH + i*rowH
		// Header bar.
		dc.SetColor(color.RGBA{R: 0x18, G: 0x1B, B: 0x21, A: 0xFF})
		dc.DrawRectangle(0, float64(y), panelW, headerH)
		dc.Fill()
		// Header text — tile coords on the left, metrics on the right.
		left := fmt.Sprintf("z=%d  x=%d  y=%d", r.tile.z, r.tile.x, r.tile.y)
		right := fmt.Sprintf("avg=%.1f   pct>30=%.1f%%   pct>60=%.1f%%",
			r.metric.avgDelta, r.metric.pctOver30, r.metric.pctOver60)
		dc.SetColor(color.RGBA{R: 0xE6, G: 0xE6, B: 0xE6, A: 0xFF})
		dc.Push()
		dc.ScaleAbout(1.5, 1.5, 8, float64(y)+8)
		dc.DrawStringAnchored(left, 8, float64(y)+8, 0, 0)
		dc.Pop()
		dc.Push()
		rightX := float64(panelW - 8)
		dc.ScaleAbout(1.5, 1.5, rightX, float64(y)+8)
		dc.DrawStringAnchored(right, rightX, float64(y)+8, 1, 0)
		dc.Pop()
		// Panel.
		draw.Draw(out, image.Rect(0, y+headerH, panelW, y+headerH+panelH),
			r.panel, image.Point{}, draw.Src)
	}

	return writePNG(path, out)
}

// compareMetric quantifies how different `our` is from `wms` on a 256x256
// tile. Both images are first composited over white so transparent pixels
// (which both renderers produce — WMS notably leaves land transparent) don't
// register as "different" against each other; they register against opaque
// pixels in the other image. avgDelta is the mean sum-of-channel-abs-diffs
// (range 0..765). pctOverN is the share of pixels whose channel-sum diff
// exceeds N — useful as a "fraction of the tile that visibly disagrees"
// metric without being thrown off by tiny anti-aliasing differences.
type cmpMetric struct {
	avgDelta            float64
	pctOver30, pctOver60 float64
}

func compareMetric(our, wms image.Image) cmpMetric {
	const w, h = 256, 256
	var sum, n, n30, n60 int
	for y := range h {
		for x := range w {
			a := flattenWhite(color.RGBAModel.Convert(our.At(x, y)).(color.RGBA))
			b := flattenWhite(color.RGBAModel.Convert(wms.At(x, y)).(color.RGBA))
			d := absInt(int(a.R)-int(b.R)) + absInt(int(a.G)-int(b.G)) + absInt(int(a.B)-int(b.B))
			sum += d
			n++
			if d > 30 {
				n30++
			}
			if d > 60 {
				n60++
			}
		}
	}
	if n == 0 {
		return cmpMetric{}
	}
	return cmpMetric{
		avgDelta:  float64(sum) / float64(n),
		pctOver30: 100 * float64(n30) / float64(n),
		pctOver60: 100 * float64(n60) / float64(n),
	}
}

// flattenWhite alpha-composites `c` over an opaque white background, so a
// fully-transparent input becomes pure white. This puts both our (which fills
// water but leaves uncharted areas transparent) and NOAA's (which leaves land
// transparent) on a level base for diffing.
func flattenWhite(c color.RGBA) color.RGBA {
	if c.A == 255 {
		return c
	}
	a := float64(c.A) / 255
	r := float64(c.R)*a + 255*(1-a)
	g := float64(c.G)*a + 255*(1-a)
	b := float64(c.B)*a + 255*(1-a)
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

func buildComparePanel(our, wms image.Image) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, 256*3, 256))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(0, 0, 256, 256), our, image.Point{}, draw.Over)
	draw.Draw(out, image.Rect(256, 0, 512, 256), wms, image.Point{}, draw.Over)
	for y := range 256 {
		for x := range 256 {
			a := flattenWhite(color.RGBAModel.Convert(out.At(x, y)).(color.RGBA))
			b := flattenWhite(color.RGBAModel.Convert(out.At(256+x, y)).(color.RGBA))
			d := absInt(int(a.R)-int(b.R)) + absInt(int(a.G)-int(b.G)) + absInt(int(a.B)-int(b.B))
			if d > 255 {
				d = 255
			}
			out.SetRGBA(512+x, y, color.RGBA{R: uint8(d), G: uint8(d), B: uint8(d), A: 255})
		}
	}
	return out
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// tileLonLatBBox returns the lon/lat bbox covered by a single XYZ tile. Used
// for ENC prefetch — we only need cells covering the tile we're about to
// render, plus a hair of slop.
func tileLonLatBBox(t tileXYZ) (minLon, minLat, maxLon, maxLat float64) {
	xmin, ymin, xmax, ymax := tileBBoxMercator(t)
	minLon, maxLat = mercToLonLat(xmin, ymax)
	maxLon, minLat = mercToLonLat(xmax, ymin)
	return
}

func envOrTiles(t *testing.T, key string, def []tileXYZ) []tileXYZ {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var out []tileXYZ
	for _, part := range splitCSV(raw) {
		var z, x, y int
		_, err := fmt.Sscanf(part, "%d:%d:%d", &z, &x, &y)
		if err != nil {
			t.Fatalf("%s=%q: %q is not z:x:y", key, raw, part)
		}
		out = append(out, tileXYZ{z: z, x: x, y: y})
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

