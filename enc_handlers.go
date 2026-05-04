package vc

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ENCHandlers exposes the ENC catalog/store/renderer via HTTP under /noaa-enc/.
type ENCHandlers struct {
	catalog  *ENCCatalog
	store    *ENCStore
	renderer *ENCRenderer
}

func NewENCHandlers(catalog *ENCCatalog, store *ENCStore, renderer *ENCRenderer) *ENCHandlers {
	return &ENCHandlers{catalog: catalog, store: store, renderer: renderer}
}

func (h *ENCHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("/noaa-enc/prefetch", h.handlePrefetch)
	mux.HandleFunc("/noaa-enc/stats", h.handleStats)
	mux.HandleFunc("/noaa-enc/tile/", h.handleTile)
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
	png, err := h.renderer.RenderTile(z, x, y)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(png)
}
