package render

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fogleman/gg"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.viam.com/rdk/logging"
	"golang.org/x/image/font/basicfont"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

// TestGoldenTiles renders the ENC and MERGED tiles for the default compare
// location at every zoom, stacks them into ONE tall image (a labeled
// "z | ENC | MERGED" row per zoom), and pixel-compares it to a single golden
// PNG on disk. It's a change-detector: any render change makes it fail and
// writes golden/actual/diff full images to /tmp/golden-fail/ so the change can
// be eyeballed before being accepted. Re-seed after an intended change with:
//
//	UPDATE_GOLDEN=1 MONGO_URI=mongodb://erh-23big.local:27017 \
//	  go test ./render -run TestGoldenTiles -count=1
//
// Requires a seeded Mongo; skips cleanly if one isn't reachable.
func TestGoldenTiles(t *testing.T) {
	if testing.Short() {
		t.Skip("golden render test skipped in short mode")
	}
	const (
		lat, lon = 32.79, -79.86 // render-cmd default (Charleston, SC)
		minZoom  = 7
		maxZoom  = 16
	)
	update := os.Getenv("UPDATE_GOLDEN") != ""
	goldenPath := filepath.Join("testdata", "golden", "compare.png")

	mongoURI := envOrDefault("MONGO_URI", "mongodb://erh-23big.local:27017")
	mongoDB := envOrDefault("MONGO_DB", "osm")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Skipf("mongo connect (%s): %v — skipping golden test", mongoURI, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("mongo ping (%s): %v — skipping golden test", mongoURI, err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	db := client.Database(mongoDB)

	r := NewENCRenderer(logging.NewTestLogger(t))
	r.SetNOAACollection(noaa.OpenCollection(db))
	r.SetOSMCollections(osmtiler.OpenOSMCollections(db))

	var rows []image.Image
	for z := minZoom; z <= maxZoom; z++ {
		x, y := lonLatToTile(lon, lat, z)
		// The MERGED app tile — exactly what the frontend composites. WMS and the
		// standalone OSM panel are excluded (external / nondeterministic / times
		// out at low zoom).
		mergedPNG, _, _, err := r.RenderMergedTile(z, x, y, BrowserMergedOptions(z, 6/feetPerMetre))
		if err != nil {
			t.Fatalf("z=%d render merged: %v", z, err)
		}
		rows = append(rows, goldenRow(z, mustDecode(t, mergedPNG)))
	}
	got := vstack(rows...)

	if update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writePNGFile(goldenPath, got); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("seeded golden: %s (z%d..z%d, panel: MERGED)", goldenPath, minZoom, maxZoom)
		return
	}

	goldenBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("no golden (%v); run with UPDATE_GOLDEN=1 to seed", err)
	}
	golden := mustDecode(t, goldenBytes)
	if diff, ndiff := pixelDiff(golden, got); ndiff > 0 {
		outDir := "/tmp/golden-fail"
		_ = os.MkdirAll(outDir, 0o755)
		_ = writePNGFile(filepath.Join(outDir, "golden.png"), golden)
		_ = writePNGFile(filepath.Join(outDir, "actual.png"), got)
		_ = writePNGFile(filepath.Join(outDir, "diff.png"), diff)
		t.Errorf("%d pixels differ from golden — see %s/{golden,actual,diff}.png (rows z%d..z%d, panel: MERGED)",
			ndiff, outDir, minZoom, maxZoom)
	} else {
		t.Logf("golden matches (z%d..z%d)", minZoom, maxZoom)
	}
}

// goldenRow builds one labeled row: [z-label | MERGED].
func goldenRow(z int, merged image.Image) image.Image {
	const labelW = 44
	h := 256
	w := labelW + merged.Bounds().Dx()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(labelW, 0, w, h), merged, merged.Bounds().Min, draw.Over)
	dc := gg.NewContextForRGBA(out)
	dc.SetFontFace(basicfont.Face7x13)
	dc.SetColor(color.Black)
	dc.DrawString("z"+itoa(z), 6, 20)
	return out
}

// pixelDiff returns a diff image (white=same, red=different) and the number of
// differing pixels. Mismatched sizes count as fully different.
func pixelDiff(a, b image.Image) (image.Image, int) {
	w, h := maxi(a.Bounds().Dx(), b.Bounds().Dx()), maxi(a.Bounds().Dy(), b.Bounds().Dy())
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	n := 0
	for y := range h {
		for x := range w {
			ar, ag, ab, aa := safeAt(a, x, y)
			br, bg, bb, ba := safeAt(b, x, y)
			if ar != br || ag != bg || ab != bb || aa != ba {
				n++
				out.Set(x, y, color.RGBA{R: 0xFF, A: 0xFF})
			}
		}
	}
	return out, n
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func safeAt(im image.Image, x, y int) (uint32, uint32, uint32, uint32) {
	if !image.Pt(x, y).In(im.Bounds()) {
		return 0, 0, 0, 0
	}
	return im.At(x, y).RGBA()
}

func vstack(imgs ...image.Image) image.Image {
	w, h := 0, 0
	for _, im := range imgs {
		if im.Bounds().Dx() > w {
			w = im.Bounds().Dx()
		}
		h += im.Bounds().Dy()
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	y := 0
	for _, im := range imgs {
		draw.Draw(out, image.Rect(0, y, im.Bounds().Dx(), y+im.Bounds().Dy()), im, im.Bounds().Min, draw.Over)
		y += im.Bounds().Dy()
	}
	return out
}

func mustDecode(t *testing.T, b []byte) image.Image {
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("png decode: %v", err)
	}
	return im
}

func writePNGFile(path string, im image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, im)
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
