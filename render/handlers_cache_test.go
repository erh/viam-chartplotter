package render

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.viam.com/rdk/logging"
)

// TestTileCacheHeaders verifies chart tiles carry Cache-Control + ETag and that
// a matching If-None-Match short-circuits to 304 with no body.
func TestTileCacheHeaders(t *testing.T) {
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	const path = "/noaa-enc/tile/14/4855/6161.png"

	// First request: full 200 with cache headers (renderer has no collections,
	// so it returns a blank tile — the headers are what we're checking).
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
	if e := w3.Header().Get("ETag"); e == etag {
		t.Fatalf("sd=12 shares ETag with default: %q", e)
	}
}
