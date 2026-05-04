package vc

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ENCHandlers exposes the ENC catalog/store via HTTP under /noaa-enc/. The tile
// renderer is a stub; rendering S-57 to PNG is the next task.
type ENCHandlers struct {
	catalog *ENCCatalog
	store   *ENCStore
}

func NewENCHandlers(catalog *ENCCatalog, store *ENCStore) *ENCHandlers {
	return &ENCHandlers{catalog: catalog, store: store}
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

// handleTile is intentionally a stub: rendering S-57 to PNG requires GDAL/MapServer
// integration, which is the next milestone. Returning 501 keeps the URL shape stable
// so the frontend can be wired ahead of time.
func (h *ENCHandlers) handleTile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/noaa-enc/tile/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.Error(w, "bad path: expected /noaa-enc/tile/{z}/{x}/{y}.png", http.StatusBadRequest)
		return
	}
	yp := strings.TrimSuffix(parts[2], ".png")
	if _, err := strconv.Atoi(parts[0]); err != nil {
		http.Error(w, "bad z", http.StatusBadRequest)
		return
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		http.Error(w, "bad x", http.StatusBadRequest)
		return
	}
	if _, err := strconv.Atoi(yp); err != nil {
		http.Error(w, "bad y", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte("noaa-enc renderer not yet implemented"))
}
