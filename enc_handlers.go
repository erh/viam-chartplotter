package vc

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"strconv"
	"strings"
)

// ENCHandlers exposes the ENC catalog/store/renderer via HTTP under /noaa-enc/.
type ENCHandlers struct {
	catalog          *ENCCatalog
	store            *ENCStore
	renderer         *ENCRenderer
	tileCache        *ENCTileCache
	wmsCache         *NoaaCache // for the /compare endpoint; may be nil
	defaultSafeDepth float64    // feet; used when ?sd= is absent
}

func NewENCHandlers(catalog *ENCCatalog, store *ENCStore, renderer *ENCRenderer, tileCache *ENCTileCache, wmsCache *NoaaCache, defaultSafeDepthFt float64) *ENCHandlers {
	if defaultSafeDepthFt <= 0 {
		defaultSafeDepthFt = 6
	}
	return &ENCHandlers{
		catalog:          catalog,
		store:            store,
		renderer:         renderer,
		tileCache:        tileCache,
		wmsCache:         wmsCache,
		defaultSafeDepth: defaultSafeDepthFt,
	}
}

const feetPerMetre = 3.28084

// safeDepthBucket rounds a safety depth (feet) to an integer-foot bucket so we
// don't generate one tile-cache shard per floating-point variant.
func safeDepthBucket(safeDepthFt float64) int {
	b := int(safeDepthFt + 0.5)
	if b < 1 {
		b = 1
	}
	return b
}

func (h *ENCHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("/noaa-enc/prefetch", h.handlePrefetch)
	mux.HandleFunc("/noaa-enc/stats", h.handleStats)
	mux.HandleFunc("/noaa-enc/tile/", h.handleTile)
	mux.HandleFunc("/noaa-enc/debug", h.handleDebug)
	mux.HandleFunc("/noaa-enc/compare/", h.handleCompare)
}

type encPrefetchRequest struct {
	MinLon   float64 `json:"minLon"`
	MinLat   float64 `json:"minLat"`
	MaxLon   float64 `json:"maxLon"`
	MaxLat   float64 `json:"maxLat"`
	MinScale int     `json:"minScale,omitempty"`
	MaxScale int     `json:"maxScale,omitempty"`
}

type encPrefetchResponse struct {
	Downloaded int `json:"downloaded"`
	Skipped    int `json:"skipped"`
}

func (h *ENCHandlers) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req encPrefetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	dl, sk, err := h.store.SyncBBox(
		r.Context(),
		req.MinLon, req.MinLat, req.MaxLon, req.MaxLat,
		req.MinScale, req.MaxScale, 4,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(encPrefetchResponse{Downloaded: dl, Skipped: sk})
}

func (h *ENCHandlers) handleStats(w http.ResponseWriter, r *http.Request) {
	cellsLocal, bytes := h.store.Stats()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"catalog_fetched":  h.catalog.FetchedAt(),
		"catalog_cells":    h.catalog.CellCount(),
		"cells_downloaded": cellsLocal,
		"store_bytes":      bytes,
		"store_dir":        h.store.RootDir(),
	})
}

// handleDebug reports, for the bbox in the query string, every overlapping cell
// and the count of features broken down by S-57 object class and geometry type.
// Useful to confirm whether the s57 lib is actually producing polygons/lines for
// a given area or just points.
//
//	GET /noaa-enc/debug?minLon=-73.4&minLat=40.6&maxLon=-72.6&maxLat=40.8
func (h *ENCHandlers) handleDebug(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parse := func(name string) (float64, bool) {
		v, err := strconv.ParseFloat(q.Get(name), 64)
		return v, err == nil
	}
	minLon, ok1 := parse("minLon")
	minLat, ok2 := parse("minLat")
	maxLon, ok3 := parse("maxLon")
	maxLat, ok4 := parse("maxLat")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		http.Error(w, "need ?minLon&minLat&maxLon&maxLat as floats", http.StatusBadRequest)
		return
	}
	report, err := h.renderer.DebugBBox(minLon, minLat, maxLon, maxLat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// handleTile renders a 256x256 PNG for the given XYZ tile from any ENC cells we
// have on disk that overlap. Tiles outside our coverage come back as transparent
// PNGs so the layer composes cleanly with the basemap.
func (h *ENCHandlers) handleTile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/noaa-enc/tile/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.Error(w, "bad path: expected /noaa-enc/tile/{z}/{x}/{y}.png", http.StatusBadRequest)
		return
	}
	yp := strings.TrimSuffix(parts[2], ".png")
	z, errZ := strconv.Atoi(parts[0])
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(yp)
	if errZ != nil || errX != nil || errY != nil {
		http.Error(w, "bad coords", http.StatusBadRequest)
		return
	}

	// safeDepth is in feet. Per-request `?sd=` overrides the module default so
	// each boat (or test) can render its own threshold without touching config.
	safeDepthFt := h.defaultSafeDepth
	if v := r.URL.Query().Get("sd"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			safeDepthFt = parsed
		}
	}
	bucket := safeDepthBucket(safeDepthFt)
	safeDepthM := float64(bucket) / feetPerMetre

	if h.tileCache != nil {
		if cached, ok := h.tileCache.Get(bucket, z, x, y); ok {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(cached)
			return
		}
	}

	pngBytes, err := h.renderer.RenderTile(z, x, y, safeDepthM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Don't cache visually-empty tiles. A fully-transparent 256x256 PNG is
	// ~768 bytes; anything that small means the renderer had nothing to draw,
	// most likely because the underlying cells haven't been downloaded yet.
	// Caching that empty result would make it the permanent answer for these
	// coords even after a later prefetch fills in the cells. Re-rendering an
	// empty tile is cheap, so it's safe to skip caching them.
	const minCacheableTileBytes = 1024
	if h.tileCache != nil && len(pngBytes) >= minCacheableTileBytes {
		if err := h.tileCache.Put(bucket, z, x, y, pngBytes); err != nil {
			// Cache write failures shouldn't fail the request; the next render
			// will just have to redo the work.
			_ = err
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(pngBytes)
}

// handleCompare renders a side-by-side debug image — our render | NOAA WMS |
// pixel diff — for the given XYZ tile, so we can iterate the renderer toward
// visual parity with NOAA's S-52 output. The diff panel is grayscale: black
// = byte-identical RGB, white = maximally different. The WMS half is fetched
// through the existing /noaa-wms cache so repeated comparisons are free.
//
//	GET /noaa-enc/compare/{z}/{x}/{y}.png[?sd=N]
func (h *ENCHandlers) handleCompare(w http.ResponseWriter, r *http.Request) {
	if h.wmsCache == nil {
		http.Error(w, "wms cache not configured", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/noaa-enc/compare/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.Error(w, "bad path: expected /noaa-enc/compare/{z}/{x}/{y}.png", http.StatusBadRequest)
		return
	}
	yp := strings.TrimSuffix(parts[2], ".png")
	z, errZ := strconv.Atoi(parts[0])
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(yp)
	if errZ != nil || errX != nil || errY != nil {
		http.Error(w, "bad coords", http.StatusBadRequest)
		return
	}

	safeDepthFt := h.defaultSafeDepth
	if v := r.URL.Query().Get("sd"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			safeDepthFt = parsed
		}
	}
	safeDepthM := float64(safeDepthBucket(safeDepthFt)) / feetPerMetre

	ourPNG, err := h.renderer.RenderTile(z, x, y, safeDepthM)
	if err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ourImg, err := png.Decode(bytes.NewReader(ourPNG))
	if err != nil {
		http.Error(w, "decode our: "+err.Error(), http.StatusInternalServerError)
		return
	}

	canonical := wmsCanonicalForTile(tileXYZ{z: z, x: x, y: y}, "")
	wmsBytes, _, _, err := h.wmsCache.fetch(r.Context(), canonical, "image/png")
	if err != nil {
		http.Error(w, "wms: "+err.Error(), http.StatusBadGateway)
		return
	}
	wmsImg, err := png.Decode(bytes.NewReader(wmsBytes))
	if err != nil {
		http.Error(w, "decode wms: "+err.Error(), http.StatusInternalServerError)
		return
	}

	out := image.NewRGBA(image.Rect(0, 0, 256*3, 256))
	// White background so transparent areas of either tile are visible.
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(0, 0, 256, 256), ourImg, image.Point{}, draw.Over)
	draw.Draw(out, image.Rect(256, 0, 512, 256), wmsImg, image.Point{}, draw.Over)

	for py := range 256 {
		for px := range 256 {
			o := color.RGBAModel.Convert(out.At(px, py)).(color.RGBA)
			n := color.RGBAModel.Convert(out.At(256+px, py)).(color.RGBA)
			d := absInt(int(o.R)-int(n.R)) + absInt(int(o.G)-int(n.G)) + absInt(int(o.B)-int(n.B))
			if d > 255 {
				d = 255
			}
			out.SetRGBA(512+px, py, color.RGBA{R: uint8(d), G: uint8(d), B: uint8(d), A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
