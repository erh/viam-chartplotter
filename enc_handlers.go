package vc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/erh/viam-chartplotter/mapdata/noaa"

	"github.com/fogleman/gg"
	"golang.org/x/image/font/basicfont"
)

// ENCHandlers exposes the ENC catalog/store/renderer via HTTP under /noaa-enc/.
type ENCHandlers struct {
	catalog          *noaa.Catalog
	store            *noaa.Store
	renderer         *ENCRenderer
	tileCache        *ENCTileCache
	wmsCache         *NoaaCache // for the /compare endpoint; may be nil
	defaultSafeDepth float64    // feet; used when ?sd= is absent
}

func NewENCHandlers(catalog *noaa.Catalog, store *noaa.Store, renderer *ENCRenderer, tileCache *ENCTileCache, wmsCache *NoaaCache, defaultSafeDepthFt float64) *ENCHandlers {
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
	mux.HandleFunc("/noaa-enc/debug-tile/", h.handleDebugTile)
	mux.HandleFunc("/noaa-enc/compare/test", h.handleCompareTest)
	mux.HandleFunc("/noaa-enc/compare/", h.handleCompare)
	mux.HandleFunc("/noaa-enc/navaids", h.handleNavaids)
	mux.HandleFunc("/noaa-enc/structures", h.handleStructures)
	mux.HandleFunc("/noaa-enc/osm-tile/", h.handleOSMTile)
}

// handleOSMTile serves a 256×256 PNG containing only the OSM vector
// underlay (highways + buildings) for the given XYZ tile. Used as a
// stand-alone OL TileLayer so the frontend can toggle OSM detail on/off
// without re-rendering the chart, and so a cold Overpass fetch only
// blocks the OSM layer's tiles — the chart layer keeps painting at full
// speed regardless.
//
//	GET /noaa-enc/osm-tile/{z}/{x}/{y}.png
func (h *ENCHandlers) handleOSMTile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/noaa-enc/osm-tile/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.Error(w, "bad path: expected /noaa-enc/osm-tile/{z}/{x}/{y}.png", http.StatusBadRequest)
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

	// Reuse the chart tile cache with a distinct style key so OSM tiles
	// share eviction policy and disk layout with chart tiles. The depth
	// bucket is irrelevant for OSM — pin it to 0. The version suffix is
	// driven by OSMRenderRulesVersion (enc_render.go); bump that const
	// whenever the OSM rasteriser logic changes so stale PNGs from a
	// prior implementation get auto-superseded.
	cacheKey := "osm-raster-v" + strconv.Itoa(OSMRenderRulesVersion)
	if h.tileCache != nil {
		if cached, ok := h.tileCache.Get(cacheKey, 0, z, x, y); ok {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(cached)
			return
		}
	}

	pngBytes, rendered, err := h.renderer.RenderOSMTile(z, x, y)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if rendered {
		// Real feature-backed render — disk-cache and tell the
		// browser to hold onto it for a day.
		const minCacheableTileBytes = 1024
		if h.tileCache != nil && len(pngBytes) >= minCacheableTileBytes {
			_ = h.tileCache.Put(cacheKey, 0, z, x, y, pngBytes)
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		// Fallback transparent PNG (region still loading, or no
		// covering extract). Cache neither side — the next request
		// after the region finishes parsing should produce a real
		// tile and we don't want the browser pinning the blank.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
	}
	_, _ = w.Write(pngBytes)
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

// handleDebugTile is a convenience wrapper around /debug that takes XYZ tile
// coords directly. Computes the tile's lon/lat bbox and forwards to
// DebugBBox so the response shape is identical.
//
//	GET /noaa-enc/debug-tile/{z}/{x}/{y}
func (h *ENCHandlers) handleDebugTile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/noaa-enc/debug-tile/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.Error(w, "bad path: expected /noaa-enc/debug-tile/{z}/{x}/{y}", http.StatusBadRequest)
		return
	}
	// Strip an optional trailing extension so debug-tile/14/4822/6159.png
	// works the same as ...6159 — easier to retype from a tile URL.
	yp := strings.TrimSuffix(parts[2], ".png")
	z, errZ := strconv.Atoi(parts[0])
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(yp)
	if errZ != nil || errX != nil || errY != nil {
		http.Error(w, "bad coords", http.StatusBadRequest)
		return
	}
	tileXmin, tileYmin, tileXmax, tileYmax := tileBBoxMercator(tileXYZ{x: x, y: y, z: z})
	minLon, maxLat := mercToLonLat(tileXmin, tileYmax)
	maxLon, minLat := mercToLonLat(tileXmax, tileYmin)
	report, err := h.renderer.DebugBBox(minLon, minLat, maxLon, maxLat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tile":  map[string]int{"z": z, "x": x, "y": y},
		"bbox":  []float64{minLon, minLat, maxLon, maxLat},
		"cells": report,
	})
}

// handleNavaids returns navaid features (buoys, beacons, lights, daymarks)
// inside the bbox as GeoJSON. Used by the frontend to render an interactive
// vector layer with hover popups instead of baking the symbols and labels
// into the chart tile PNG.
//
//	GET /noaa-enc/navaids?minLon=...&minLat=...&maxLon=...&maxLat=...
func (h *ENCHandlers) handleNavaids(w http.ResponseWriter, r *http.Request) {
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
	navaids, err := h.renderer.Navaids(minLon, minLat, maxLon, maxLat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// GeoJSON FeatureCollection with point geometries — OL's GeoJSON format
	// reads this directly into Feature objects with a "properties" bag.
	type geoFeature struct {
		Type       string         `json:"type"`
		Geometry   map[string]any `json:"geometry"`
		Properties map[string]any `json:"properties"`
	}
	feats := make([]geoFeature, 0, len(navaids))
	for _, n := range navaids {
		props := map[string]any{
			"class": n.Class,
		}
		for k, v := range n.Properties {
			props[k] = v
		}
		feats = append(feats, geoFeature{
			Type:       "Feature",
			Geometry:   map[string]any{"type": "Point", "coordinates": []float64{n.Lon, n.Lat}},
			Properties: props,
		})
	}
	w.Header().Set("Content-Type", "application/geo+json")
	// Short cache — navaids don't change often, but the cell set on disk
	// might (background SyncBBox), and the frontend re-fetches per pan.
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     "FeatureCollection",
		"features": feats,
	})
}

// handleStructures returns bridge / overhead-cable / overhead-pipe /
// conveyor features inside the bbox as GeoJSON. Used by the frontend
// to render an interactive vector layer with hover popups (clearance
// attributes, names, remarks) instead of baking the labels into the
// chart tile PNG.
//
//	GET /noaa-enc/structures?minLon=...&minLat=...&maxLon=...&maxLat=...
func (h *ENCHandlers) handleStructures(w http.ResponseWriter, r *http.Request) {
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
	structures, err := h.renderer.Structures(minLon, minLat, maxLon, maxLat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type geoFeature struct {
		Type       string         `json:"type"`
		Geometry   StructureGeom  `json:"geometry"`
		Properties map[string]any `json:"properties"`
	}
	feats := make([]geoFeature, 0, len(structures))
	for _, s := range structures {
		props := map[string]any{"class": s.Class}
		for k, v := range s.Properties {
			props[k] = v
		}
		feats = append(feats, geoFeature{
			Type:       "Feature",
			Geometry:   s.Geometry,
			Properties: props,
		})
	}
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     "FeatureCollection",
		"features": feats,
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

	style := ParseRenderStyle(r.URL.Query().Get("style"))
	skipNavaids := r.URL.Query().Get("navaids") == "0"
	transparentLand := r.URL.Query().Get("landfill") == "0"
	skipClasses := parseSkipClasses(r.URL.Query().Get("skip"))
	// Each render-option variant gets its own cache shard so URLs that
	// differ only in those params don't stomp on each other's cached PNGs.
	// The "vN-" prefix comes from ENCRenderRulesVersion (enc_render.go) and
	// is bumped whenever code changes alter the rendered pixels without a
	// URL change — old vN directories then become inert and re-rendering
	// happens on the next request.
	cacheKey := "v" + strconv.Itoa(ENCRenderRulesVersion) + "-" + style.String()
	if skipNavaids {
		cacheKey += "-nonavaids"
	}
	if transparentLand {
		cacheKey += "-noland"
	}
	if len(skipClasses) > 0 {
		cacheKey += "-skip:" + skipKey(skipClasses)
	}

	if h.tileCache != nil {
		if cached, ok := h.tileCache.Get(cacheKey, bucket, z, x, y); ok {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(cached)
			return
		}
	}

	pngBytes, err := h.renderer.RenderTile(z, x, y, RenderOptions{
		SafeDepthM:      safeDepthM,
		Style:           style,
		SkipNavaids:     skipNavaids,
		TransparentLand: transparentLand,
		SkipClasses:     skipClasses,
	})
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
		if err := h.tileCache.Put(cacheKey, bucket, z, x, y, pngBytes); err != nil {
			// Cache write failures shouldn't fail the request; the next render
			// will just have to redo the work.
			_ = err
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(pngBytes)
}

// handleCompareTest serves an HTML page that stacks /compare panels for the
// same lat/lon at z=7..16, so you can eyeball shading consistency across
// zooms without juggling tile coords by hand.
//
//	GET /noaa-enc/compare/test[?lat=32.77&lon=-79.93]
func (h *ENCHandlers) handleCompareTest(w http.ResponseWriter, r *http.Request) {
	// Default: Charleston Harbor — the area we've been iterating on.
	lat := 32.7700
	lon := -79.8800
	if v := r.URL.Query().Get("lat"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lat = f
		}
	}
	if v := r.URL.Query().Get("lon"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lon = f
		}
	}
	q := r.URL.Query()
	q.Del("lat")
	q.Del("lon")
	suffix := ""
	if e := q.Encode(); e != "" {
		suffix = "?" + e
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<!doctype html>
<html><head><meta charset="utf-8"><title>compare z=7..16 @ %.4f,%.4f</title>
<style>
 body{font-family:system-ui,sans-serif;margin:1em;background:#222;color:#ddd}
 .row{margin-bottom:1em;border-bottom:1px solid #444;padding-bottom:.5em}
 .row h2{margin:.2em 0;font-size:14px;font-weight:normal;color:#aaa}
 .row img{display:block;background:#000;image-rendering:pixelated}
 input{font-family:inherit;padding:2px 6px}
 form{margin-bottom:1em}
</style></head><body>
<form method="get">
 lat <input name="lat" value="%.4f" size="10">
 lon <input name="lon" value="%.4f" size="10">
 <button>go</button>
 <span style="color:#888;margin-left:1em">panels: ours | WMS | diff | OSM</span>
</form>
`, lat, lon, lat, lon)
	for z := 7; z <= 16; z++ {
		x, y := lonLatToTile(lon, lat, z)
		fmt.Fprintf(&buf, `<div class="row"><h2>z=%d  x=%d y=%d &middot; <a href="/noaa-enc/osm-tile/%d/%d/%d.png%s" style="color:#7af">osm</a></h2><img src="/noaa-enc/compare/%d/%d/%d.png%s"></div>`+"\n",
			z, x, y, z, x, y, suffix, z, x, y, suffix)
	}
	buf.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(buf.Bytes())
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

	// Forward the same render-option params as /tile so the compare image
	// shows exactly what the noaa-local layer is composing in the
	// browser. Default-on (?navaids=, ?osm=, ?landfill= absent) gives the
	// raw "match NOAA WMS" render the compare endpoint was originally
	// designed for; pass any of them to see the live-layer variant.
	ourPNG, err := h.renderer.RenderTile(z, x, y, RenderOptions{
		SafeDepthM:      safeDepthM,
		Style:           ParseRenderStyle(r.URL.Query().Get("style")),
		SkipNavaids:     r.URL.Query().Get("navaids") == "0",
		TransparentLand: r.URL.Query().Get("landfill") == "0",
		SkipClasses:     parseSkipClasses(r.URL.Query().Get("skip")),
	})
	if err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ourImg, err := png.Decode(bytes.NewReader(ourPNG))
	if err != nil {
		http.Error(w, "decode our: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Match the LAYERS the frontend's noaa TileWMS layer uses so this
	// request shares the cache with whatever the user already browsed —
	// otherwise every /compare hit MISSes and fetches NOAA upstream.
	canonical := wmsCanonicalForTile(tileXYZ{z: z, x: x, y: y}, "0,1,2,3,4,5,6")
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

	// Four panels side by side: ours | WMS | diff | OSM.
	const panelW = 256
	const numPanels = 4
	out := image.NewRGBA(image.Rect(0, 0, panelW*numPanels, 256))
	// White background so transparent areas of any tile are visible.
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(0, 0, panelW, 256), ourImg, image.Point{}, draw.Over)
	draw.Draw(out, image.Rect(panelW, 0, 2*panelW, 256), wmsImg, image.Point{}, draw.Over)

	// Diff panel: per-pixel RGB distance between ours and WMS, grayscale.
	for py := range 256 {
		for px := range 256 {
			o := color.RGBAModel.Convert(out.At(px, py)).(color.RGBA)
			n := color.RGBAModel.Convert(out.At(panelW+px, py)).(color.RGBA)
			d := absInt(int(o.R)-int(n.R)) + absInt(int(o.G)-int(n.G)) + absInt(int(o.B)-int(n.B))
			if d > 255 {
				d = 255
			}
			out.SetRGBA(2*panelW+px, py, color.RGBA{R: uint8(d), G: uint8(d), B: uint8(d), A: 255})
		}
	}

	// OSM tile from our self-hosted renderer (water already transparent
	// by design). Failures are non-fatal so the rest of the compare
	// image still renders.
	if osmPNG, _, err := h.renderer.RenderOSMTile(z, x, y); err == nil {
		if osmImg, derr := png.Decode(bytes.NewReader(osmPNG)); derr == nil {
			draw.Draw(out, image.Rect(3*panelW, 0, 4*panelW, 256), osmImg, image.Point{}, draw.Over)
		}
	}

	// Panel labels so anyone looking at /tmp/foo.png or the network response
	// knows which is which without checking the handler source.
	annotatePanel(out, 4, 0, "ours")
	annotatePanel(out, panelW+4, 0, "WMS")
	annotatePanel(out, 2*panelW+4, 0, fmt.Sprintf("diff z=%d x=%d y=%d", z, x, y))
	annotatePanel(out, 3*panelW+4, 0, "OSM")

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// annotatePanel draws a label at the top-left of the given pixel position.
// Solid black background (slightly transparent) with bold white text scaled
// 2× for readability — basicfont is 7×13 which is tiny otherwise.
func annotatePanel(img *image.RGBA, x, y int, label string) {
	const scale = 2.0
	dc := gg.NewContextForRGBA(img)
	dc.SetFontFace(basicfont.Face7x13)
	rawW, rawH := dc.MeasureString(label)
	w, h := rawW*scale, rawH*scale
	pad := 4.0
	dc.SetColor(color.RGBA{R: 0, G: 0, B: 0, A: 0xCC})
	dc.DrawRectangle(float64(x), float64(y), w+2*pad, h+2*pad)
	dc.Fill()
	dc.SetColor(color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
	tx := float64(x) + pad
	ty := float64(y) + pad + h*0.85
	dc.Push()
	dc.ScaleAbout(scale, scale, tx, ty)
	dc.DrawStringAnchored(label, tx, ty, 0, 0)
	dc.Pop()
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// parseSkipClasses turns a comma-separated S-57 class list ("COALNE,LIGHTS")
// into a set the renderer consults to drop those classes entirely. Empty /
// missing input → nil (no skipping). Values are upper-cased and trimmed.
func parseSkipClasses(s string) map[string]bool {
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.ToUpper(p))
		if p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// skipKey returns a stable, sorted, comma-joined string for a skip set.
// Used as part of the tile-cache key so two URLs with the same skip set in
// any order share a cache entry but distinct skip sets don't collide.
func skipKey(m map[string]bool) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
