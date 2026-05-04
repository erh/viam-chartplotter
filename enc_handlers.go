package vc

import (
	"encoding/json"
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
	defaultSafeDepth float64 // feet; used when ?sd= is absent
}

func NewENCHandlers(catalog *ENCCatalog, store *ENCStore, renderer *ENCRenderer, tileCache *ENCTileCache, defaultSafeDepthFt float64) *ENCHandlers {
	if defaultSafeDepthFt <= 0 {
		defaultSafeDepthFt = 6
	}
	return &ENCHandlers{
		catalog:          catalog,
		store:            store,
		renderer:         renderer,
		tileCache:        tileCache,
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

	png, err := h.renderer.RenderTile(z, x, y, safeDepthM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.tileCache != nil {
		if err := h.tileCache.Put(bucket, z, x, y, png); err != nil {
			// Cache write failures shouldn't fail the request; the next render
			// will just have to redo the work.
			_ = err
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(png)
}
