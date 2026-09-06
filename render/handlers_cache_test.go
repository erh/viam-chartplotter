package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.viam.com/rdk/logging"
)

// noisyTilePNG encodes a 256x256 PNG that won't compress down to the
// transparent-tile size, so the handler treats it as a real render rather than
// an empty one. Deterministic: a fixed seed keeps the byte count stable.
func noisyTilePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() < 1024 {
		t.Fatalf("test tile is %d bytes; the handler would treat it as empty", buf.Len())
	}
	return buf.Bytes()
}

// TestTileCacheHeaders verifies chart tiles carry Cache-Control + ETag and that
// a matching If-None-Match short-circuits to 304 with no body.
//
// The tiles are seeded into the disk cache rather than rendered: without a
// Mongo collection attached the renderer draws nothing, and a visually-empty
// tile is deliberately served uncacheable (see TestEmptyTileIsNotCacheable).
// Seeding exercises the cache-hit path, which is the one that actually serves
// tiles in production.
func TestTileCacheHeaders(t *testing.T) {
	cache, err := NewENCTileCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewENCTileCache: %v", err)
	}
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), cache, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	const path = "/noaa-enc/tile/14/4855/6161.png"
	const z, x, y = 14, 4855, 6161
	cacheKey := tileCacheKey(StyleWMS, false, false, nil)
	tile := noisyTilePNG(t)
	// Both depth buckets the test asks for: the 6 ft default and the 12 ft
	// override below.
	for _, bucket := range []int{6, 12} {
		if err := cache.Put(cacheKey, bucket, z, x, y, tile); err != nil {
			t.Fatalf("seed bucket %d: %v", bucket, err)
		}
	}

	// First request: full 200 with cache headers.
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, path, nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status %d", w1.Code)
	}
	etag := w1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header")
	}
	if cc := w1.Header().Get("Cache-Control"); cc == "" {
		t.Fatalf("no Cache-Control header")
	}
	if ct := w1.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !bytes.Equal(w1.Body.Bytes(), tile) {
		t.Fatalf("body = %d bytes, want the cached tile's %d", w1.Body.Len(), len(tile))
	}

	// Conditional request with the matching ETag → 304, empty body, ETag echoed.
	req2 := httptest.NewRequest(http.MethodGet, path, nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Fatalf("conditional request: status %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Fatalf("304 body should be empty, got %d bytes", w2.Body.Len())
	}
	if w2.Header().Get("ETag") != etag {
		t.Fatalf("304 ETag = %q, want %q", w2.Header().Get("ETag"), etag)
	}

	// A different render option (sd) must yield a different ETag (distinct cache
	// shard) so variants don't share a 304.
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, path+"?sd=12", nil))
	if w3.Code != http.StatusOK {
		t.Fatalf("sd=12 request: status %d", w3.Code)
	}
	if e := w3.Header().Get("ETag"); e == etag {
		t.Fatalf("sd=12 shares ETag with default: %q", e)
	}
}

// A visually-empty tile means the renderer had nothing to draw — most often
// because the cells behind those coords aren't ingested yet. Letting a browser
// or CDN cache that would make "blank" the answer for a day even after the
// data lands, so an empty tile carries no-store and no ETag.
func TestEmptyTileIsNotCacheable(t *testing.T) {
	cache, err := NewENCTileCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewENCTileCache: %v", err)
	}
	// No collections attached, so the render is transparent.
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), cache, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	const path = "/noaa-enc/tile/14/4855/6161.png"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if e := w.Header().Get("ETag"); e != "" {
		t.Errorf("ETag = %q, want none — an empty tile must not be revalidatable", e)
	}
	if _, cached := cache.Get(tileCacheKey(StyleWMS, false, false, nil), 6, 14, 4855, 6161); cached {
		t.Errorf("empty tile was written to the disk cache")
	}
}
