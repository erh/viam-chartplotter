package render

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// handleSearch finds named ENC features by name.
//
//	GET /noaa-enc/search?q=<text>[&class=<S-57 acronym>][&limit=<n>][&lat=&lon=]
//
// With lat/lon the results come back nearest-first from that point, which is
// what makes a search for a common name ("north channel", "middle ground")
// useful — there are dozens nationwide and the one you want is the near one.
// Without it they're alphabetical.
//
// 200 with a (possibly empty) result list, 400 on a missing query, 503 when
// this instance has no charts attached.
func (h *ENCHandlers) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		http.Error(w, "need ?q=<search text>", http.StatusBadRequest)
		return
	}

	limit := 0
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = v
	}

	var origin *RoutePoint
	lat, latErr := strconv.ParseFloat(q.Get("lat"), 64)
	lon, lonErr := strconv.ParseFloat(q.Get("lon"), 64)
	if latErr == nil && lonErr == nil {
		p := RoutePoint{Lat: lat, Lng: lon}
		if validLatLng(p) {
			origin = &p
		}
	}

	results, matched, err := h.renderer.Search(query, q.Get("class"), limit, origin)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errNoCharts):
			status = http.StatusServiceUnavailable
		case errors.Is(err, ErrChartQueryTimeout):
			status = http.StatusGatewayTimeout
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Names don't move; a short cache absorbs the keystroke-by-keystroke
	// repeats of a type-ahead without holding a stale chart ingest for long.
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"query": query,
		// The terms that actually matched. When it differs from query, the
		// full phrase found nothing and this is a narrower answer — the UI is
		// expected to say so rather than pass it off as an exact match.
		"matched_query": matched,
		"results":       results,
	})
}
