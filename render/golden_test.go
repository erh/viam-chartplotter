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

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.viam.com/rdk/logging"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

// TestGoldenTiles renders the ENC and MERGED tiles for the default compare
// location at every zoom and pixel-compares them to golden PNGs on disk. It's a
// change-detector: any render change makes it fail and writes a
// golden|actual|diff strip to /tmp/golden-fail/ so the change can be eyeballed
// before being accepted. Re-seed the goldens after an intended change with:
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
	goldenDir := filepath.Join("testdata", "golden")

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

	if update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var failed []int
	for z := minZoom; z <= maxZoom; z++ {
		x, y := lonLatToTile(lon, lat, z)
		// The two outputs we control: ENC-only and the MERGED app tile. WMS and
		// the standalone OSM panel are excluded (external / nondeterministic /
		// times out at low zoom).
		encPNG, _, err := r.RenderTile(z, x, y, RenderOptions{SafeDepthM: 6 / feetPerMetre, Style: StyleWMS})
		if err != nil {
			t.Fatalf("z=%d render enc: %v", z, err)
		}
		mergedPNG, _, _, err := r.RenderMergedTile(z, x, y, BrowserMergedOptions(z, 6/feetPerMetre))
		if err != nil {
			t.Fatalf("z=%d render merged: %v", z, err)
		}
		strip := hstack(mustDecode(t, encPNG), mustDecode(t, mergedPNG))

		goldenPath := filepath.Join(goldenDir, "z"+itoa(z)+".png")
		if update {
			if err := writePNGFile(goldenPath, strip); err != nil {
				t.Fatalf("z=%d write golden: %v", z, err)
			}
			t.Logf("seeded %s", goldenPath)
			continue
		}

		goldenBytes, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Errorf("z=%d: no golden (%v); run with UPDATE_GOLDEN=1 to seed", z, err)
			failed = append(failed, z)
			continue
		}
		golden := mustDecode(t, goldenBytes)
		if diff, ndiff := pixelDiff(golden, strip); ndiff > 0 {
			failed = append(failed, z)
			outDir := "/tmp/golden-fail"
			_ = os.MkdirAll(outDir, 0o755)
			_ = writePNGFile(filepath.Join(outDir, "z"+itoa(z)+"-golden.png"), golden)
			_ = writePNGFile(filepath.Join(outDir, "z"+itoa(z)+"-actual.png"), strip)
			_ = writePNGFile(filepath.Join(outDir, "z"+itoa(z)+"-diff.png"), diff)
			t.Errorf("z=%d: %d pixels differ from golden — see %s/z%d-{golden,actual,diff}.png (panels: ENC | MERGED)",
				z, ndiff, outDir, z)
		}
	}
	if update {
		t.Logf("seeded goldens z%d..z%d in %s", minZoom, maxZoom, goldenDir)
	} else if len(failed) == 0 {
		t.Logf("all zooms z%d..z%d match golden", minZoom, maxZoom)
	}
}

// pixelDiff returns a diff image (white=same, red=different) and the number of
// differing pixels. Mismatched sizes count as fully different.
func pixelDiff(a, b image.Image) (image.Image, int) {
	ba, bb := a.Bounds(), b.Bounds()
	w, h := ba.Dx(), ba.Dy()
	if bb.Dx() > w {
		w = bb.Dx()
	}
	if bb.Dy() > h {
		h = bb.Dy()
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	n := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ar, ag, ab, aa := safeAt(a, x, y)
			br, bg, bb2, ba2 := safeAt(b, x, y)
			if ar != br || ag != bg || ab != bb2 || aa != ba2 {
				n++
				out.Set(x, y, color.RGBA{R: 0xFF, A: 0xFF})
			}
		}
	}
	return out, n
}

func safeAt(im image.Image, x, y int) (uint32, uint32, uint32, uint32) {
	if !(image.Pt(x, y).In(im.Bounds())) {
		return 0, 0, 0, 0
	}
	return im.At(x, y).RGBA()
}

func hstack(imgs ...image.Image) image.Image {
	const pad = 2
	w, h := 0, 0
	for _, im := range imgs {
		w += im.Bounds().Dx() + pad
		if im.Bounds().Dy() > h {
			h = im.Bounds().Dy()
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	x := 0
	for _, im := range imgs {
		draw.Draw(out, image.Rect(x, 0, x+im.Bounds().Dx(), im.Bounds().Dy()), im, im.Bounds().Min, draw.Over)
		x += im.Bounds().Dx() + pad
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
