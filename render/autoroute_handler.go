package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// SetDefaultIdealDepthFt sets the ideal (preferred) depth in feet used when a
// request omits ?ideal=. Zero restores the default of twice the safe depth.
func (h *ENCHandlers) SetDefaultIdealDepthFt(ft float64) { h.defaultIdealDepth = ft }

// handleOptimize re-plans an existing waypoint list over the charted data.
//
//	POST /noaa-enc/optimize
//	{"waypoints":[{"lat":..,"lng":..}, ...],
//	 "safe_depth_ft":6, "ideal_depth_ft":20, "keep_waypoints":true,
//	 "clearance_m":30, "avoid":["restricted"]}
//
// Every leg is re-planned around land, shoals and obstructions, on one grid
// over one chart query. With keep_waypoints (the default) each point the
// operator placed stays put and only the water between them changes — a
// waypoint is usually there for a reason the chart doesn't record. Set it
// false to let the smoother straighten through them and drop the redundant
// ones.
//
// The response is the same shape as /autoroute; direct_meters is the original
// route's length, so the caller can show what the optimisation cost or saved.
func (h *ENCHandlers) handleOptimize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Waypoints []struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		} `json:"waypoints"`
		SafeDepthFt   float64  `json:"safe_depth_ft"`
		IdealDepthFt  float64  `json:"ideal_depth_ft"`
		ClearanceM    *float64 `json:"clearance_m"`
		PadM          float64  `json:"pad_m"`
		MaxCellM      float64  `json:"max_cell_m"`
		MaxWaypoints  int      `json:"max_waypoints"`
		KeepWaypoints *bool    `json:"keep_waypoints"`
		Avoid         []string `json:"avoid"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, optimizeMaxBody)).Decode(&req); err != nil {
		http.Error(w, "bad JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Waypoints) < 2 {
		http.Error(w, "need at least two waypoints", http.StatusBadRequest)
		return
	}
	if len(req.Waypoints) > optimizeMaxWaypoints {
		http.Error(w, fmt.Sprintf("at most %d waypoints", optimizeMaxWaypoints), http.StatusBadRequest)
		return
	}

	points := make([]RoutePoint, 0, len(req.Waypoints))
	for _, w := range req.Waypoints {
		points = append(points, RoutePoint{Lat: w.Lat, Lng: w.Lng})
	}

	safeDepthFt := h.defaultSafeDepth
	if req.SafeDepthFt > 0 {
		safeDepthFt = req.SafeDepthFt
	}
	opts := DefaultAutoRouteOptions(safeDepthFt / feetPerMetre)
	idealFt := h.defaultIdealDepth
	if req.IdealDepthFt > 0 {
		idealFt = req.IdealDepthFt
	}
	if idealFt > 0 {
		opts.IdealDepthM = idealFt / feetPerMetre
	}
	if req.ClearanceM != nil && *req.ClearanceM >= 0 {
		opts.HardClearanceM = *req.ClearanceM
	}
	if req.PadM > 0 {
		opts.CorridorPadM = req.PadM
	}
	if req.MaxCellM > 0 {
		opts.MaxCellM = req.MaxCellM
	}
	if req.MaxWaypoints > 1 {
		opts.MaxWaypoints = req.MaxWaypoints
	}
	if req.KeepWaypoints != nil {
		opts.KeepWaypoints = *req.KeepWaypoints
	}
	for _, name := range req.Avoid {
		if strings.EqualFold(strings.TrimSpace(name), "restricted") {
			opts.Avoid = append(opts.Avoid, RestrictedAreaAvoid(3.0))
		}
	}

	res, err := h.renderer.AutoRouteVia(points, opts)
	writeRouteResult(w, res, err)
}

// optimizeMaxBody and optimizeMaxWaypoints bound what one request can ask for.
// A route is a handful of points; anything larger is a mistake or an attack.
const optimizeMaxBody = 1 << 20
const optimizeMaxWaypoints = 200

// writeRouteResult renders an AutoRouteResult or its error, shared by the
// two-point and waypoint-list endpoints so their responses can't drift.
func writeRouteResult(w http.ResponseWriter, res *AutoRouteResult, err error) {
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrNoRoute):
			status = http.StatusNotFound
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
	w.Header().Set("Cache-Control", "public, max-age=30")
	_ = json.NewEncoder(w).Encode(res)
}

// handleAutoRoute plans a route between two points over the charted ENC data.
//
//	GET /noaa-enc/autoroute?startLat=&startLon=&endLat=&endLon=
//	     [&sd=<safe depth, ft>] [&ideal=<preferred depth, ft>]
//	     [&clearance=<m>] [&soft_clearance=<m>] [&pad=<corridor pad, m>]
//	     [&max_cell=<coarsest grid cell, m>]
//	     [&avoid=restricted] [&max_waypoints=<n>]
//
// `sd` is the hard constraint — the route never crosses water charted shoaler
// than this — and defaults to the module's configured draft. `ideal` is the
// soft one: among safe routes, prefer the one that stays this deep. It
// defaults to the module's ideal_depth attribute, or twice the safe depth.
//
// Responds 200 with an AutoRouteResult, 400 for a bad request, 404 when no
// safe route exists, 503 when charts aren't available.
func (h *ENCHandlers) handleAutoRoute(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(name string) (float64, bool) {
		v := q.Get(name)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}

	startLat, ok1 := num("startLat")
	startLon, ok2 := num("startLon")
	endLat, ok3 := num("endLat")
	endLon, ok4 := num("endLon")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		http.Error(w, "need ?startLat&startLon&endLat&endLon as floats", http.StatusBadRequest)
		return
	}

	safeDepthFt := h.defaultSafeDepth
	if v, ok := num("sd"); ok && v > 0 {
		safeDepthFt = v
	}
	opts := DefaultAutoRouteOptions(safeDepthFt / feetPerMetre)

	idealFt := h.defaultIdealDepth
	if v, ok := num("ideal"); ok && v > 0 {
		idealFt = v
	}
	if idealFt > 0 {
		opts.IdealDepthM = idealFt / feetPerMetre
	}
	if v, ok := num("clearance"); ok && v >= 0 {
		opts.HardClearanceM = v
	}
	if v, ok := num("soft_clearance"); ok && v >= 0 {
		opts.SoftClearanceM = v
	}
	if v, ok := num("pad"); ok && v > 0 {
		opts.CorridorPadM = v
	}
	if v, ok := num("max_cell"); ok && v > 0 {
		opts.MaxCellM = v
	}
	if v, ok := num("max_waypoints"); ok && v > 1 {
		opts.MaxWaypoints = int(v)
	}
	for _, name := range strings.Split(q.Get("avoid"), ",") {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case "restricted":
			opts.Avoid = append(opts.Avoid, RestrictedAreaAvoid(3.0))
		}
	}

	res, err := h.renderer.AutoRoute(
		RoutePoint{Lat: startLat, Lng: startLon},
		RoutePoint{Lat: endLat, Lng: endLon},
		opts,
	)
	writeRouteResult(w, res, err)
}
