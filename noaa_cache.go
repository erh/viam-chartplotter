package vc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.viam.com/rdk/logging"
)

// cacheStatus describes what happened on a single proxy request, surfaced
// to the client via the X-Cache response header so each tile request
// self-reports in browser devtools.
type cacheStatus int

const (
	cacheStatusMiss  cacheStatus = iota // had to wait for upstream
	cacheStatusHit                      // fresh disk copy, zero upstream
	cacheStatusStale                    // served stale disk copy + bg refresh kicked
)

func (s cacheStatus) String() string {
	switch s {
	case cacheStatusHit:
		return "HIT"
	case cacheStatusStale:
		return "STALE"
	case cacheStatusMiss:
		return "MISS"
	}
	return "UNKNOWN"
}

const (
	defaultUpstreamWMS = "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer"

	mercatorMax = 20037508.342789244

	prefetchConcurrency = 4

	defaultMaxCacheBytes int64 = 20 * 1024 * 1024 * 1024 // 20 GiB
	defaultStaleAfter          = 14 * 24 * time.Hour     // 2 weeks
)

// NoaaCache is a caching reverse proxy for the NOAA Maritime Chart Service WMS endpoint.
// Tile responses are stored on disk keyed by the canonicalized query string so subsequent
// requests for the same tile are served locally without hitting NOAA.
type NoaaCache struct {
	cacheDir   string
	upstream   string
	client     *http.Client
	logger     logging.Logger
	maxBytes   int64
	staleAfter time.Duration

	// in-flight de-duplication so concurrent requests for the same tile only fetch once
	mu       sync.Mutex
	inflight map[string]*sync.WaitGroup

	// evictMu serializes eviction passes; TryLock means a write that arrives mid-pass
	// just skips kicking off another pass rather than queuing.
	evictMu sync.Mutex

	// Cumulative request outcomes, surfaced via /noaa-wms/stats.
	hits   atomic.Uint64
	misses atomic.Uint64
	stales atomic.Uint64
	errs   atomic.Uint64
}

func NewNoaaCache(cacheDir string, maxBytes int64, logger logging.Logger) (*NoaaCache, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("noaa cache: cacheDir must be set")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("noaa cache: mkdir %q: %w", cacheDir, err)
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxCacheBytes
	}
	return &NoaaCache{
		cacheDir:   cacheDir,
		upstream:   defaultUpstreamWMS,
		client:     &http.Client{Timeout: 15 * time.Second},
		logger:     logger,
		maxBytes:   maxBytes,
		staleAfter: defaultStaleAfter,
		inflight:   map[string]*sync.WaitGroup{},
	}, nil
}

// Register attaches the proxy and prefetch endpoints to mux.
func (c *NoaaCache) Register(mux *http.ServeMux) {
	mux.HandleFunc("/noaa-wms/proxy", c.handleProxy)
	mux.HandleFunc("/noaa-wms/prefetch", c.handlePrefetch)
	mux.HandleFunc("/noaa-wms/stats", c.handleStats)
	mux.HandleFunc("/noaa-wms/canon", c.handleCanon)
}

// handleCanon is a diagnostic: paste any /noaa-wms/proxy URL's query string
// (or even a full upstream URL's query) and get back the canonical form, the
// SHA256 cache key, the on-disk path, and whether that file exists. Two
// requests for the "same tile" should return identical canon/sha values; if
// they don't, the cache will never hit.
func (c *NoaaCache) handleCanon(w http.ResponseWriter, r *http.Request) {
	canonical, format, err := canonicalQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := c.cachePath(canonical)
	exists := false
	var size int64
	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		exists = true
		size = info.Size()
		mtime = info.ModTime()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"canonical": canonical,
		"format":    format,
		"path":      path,
		"exists":    exists,
		"size":      size,
		"mtime":     mtime,
	})
}

// canonicalQuery returns a stable representation of the WMS query string so different
// orderings/cases of the same logical request hash to the same cache key.
//
// Params whose name starts with `_` are treated as client-only (e.g. an
// OpenLayers cache-buster `_v=<build hash>`). They're dropped here so a new
// build doesn't invalidate every previously-cached tile, and so we don't
// forward them upstream to NOAA where they're meaningless.
//
// BBOX values are also numerically normalized: OpenLayers formats tile
// extents with full JS number precision, so the same tile can come over
// the wire as `22.5` one frame and `22.499999999999996` the next due to
// floating-point intermediate ops. Without normalization those hash to
// different cache keys and the cache effectively never hits.
func canonicalQuery(raw string) (string, string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", "", err
	}
	keys := make([]string, 0, len(values))
	upper := url.Values{}
	for k, v := range values {
		if strings.HasPrefix(k, "_") {
			continue
		}
		uk := strings.ToUpper(k)
		normalized := v
		if uk == "BBOX" {
			normalized = make([]string, len(v))
			for i, s := range v {
				normalized[i] = normalizeBBox(s)
			}
		}
		upper[uk] = append(upper[uk], normalized...)
		keys = append(keys, uk)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		vs := upper[k]
		sort.Strings(vs)
		for j, v := range vs {
			if j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	canon := b.String()
	format := upper.Get("FORMAT")
	if format == "" {
		format = "image/png"
	}
	return canon, format, nil
}

// normalizeBBox parses "minx,miny,maxx,maxy" and re-emits each coordinate
// as a fixed 6-decimal string. 6 places gives ~10 cm precision in degrees
// and ~1 µm in metres — both well past meaningful tile precision and
// safely past any FP intermediate-op drift OpenLayers emits. Values that
// don't parse as numbers are passed through unchanged so this is safe for
// arbitrary BBOX-like inputs.
func normalizeBBox(s string) string {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return s
	}
	out := make([]string, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return s
		}
		out[i] = strconv.FormatFloat(f, 'f', 6, 64)
	}
	return strings.Join(out, ",")
}

func (c *NoaaCache) cachePath(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	hex := hex.EncodeToString(sum[:])
	return filepath.Join(c.cacheDir, hex[:2], hex+".bin")
}

// fetch returns the cached bytes, content-type, and a status describing
// where the bytes came from (so callers can surface HIT/STALE/MISS to
// clients). Behaviour:
//   - Fresh on disk (mtime within staleAfter): serve from disk, no upstream call.
//   - Stale on disk: serve the stale bytes immediately and kick a background
//     refresh. If upstream is down we keep serving the old copy rather than
//     leaving the client with nothing.
//   - Nothing on disk: block on upstream and write through to disk.
func (c *NoaaCache) fetch(ctx context.Context, canonical, format string) ([]byte, string, cacheStatus, error) {
	path := c.cachePath(canonical)
	if info, err := os.Stat(path); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			stale := c.staleAfter > 0 && time.Since(info.ModTime()) >= c.staleAfter
			if stale {
				go c.refreshAsync(canonical, format)
				return data, format, cacheStatusStale, nil
			}
			return data, format, cacheStatusHit, nil
		}
	}
	// No usable disk copy — must wait for upstream.
	data, ct, err := c.fetchAndStore(ctx, canonical, format)
	return data, ct, cacheStatusMiss, err
}

// refreshAsync triggers a fire-and-forget upstream refresh for `canonical`.
// Skips if a fetch (sync or async) for the same key is already in flight, so
// repeated requests against a stale tile only generate one upstream attempt
// at a time. Errors are logged at Debug — the caller has already been served
// the stale bytes.
func (c *NoaaCache) refreshAsync(canonical, format string) {
	c.mu.Lock()
	if _, busy := c.inflight[canonical]; busy {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	// Detached from any request context: a client closing its socket must not
	// abort an in-progress refresh, since the *next* request still wants the
	// fresh bytes on disk.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, err := c.fetchAndStore(ctx, canonical, format); err != nil {
		c.logger.Debugf("noaa cache bg refresh: %v", err)
	}
}

// fetchAndStore performs a synchronous upstream fetch and persists a 200
// response to disk. Concurrent calls for the same key are de-duped via
// `inflight` — followers wait, then read the file the leader wrote.
func (c *NoaaCache) fetchAndStore(ctx context.Context, canonical, format string) ([]byte, string, error) {
	path := c.cachePath(canonical)

	c.mu.Lock()
	if wg, ok := c.inflight[canonical]; ok {
		c.mu.Unlock()
		wg.Wait()
		if data, err := os.ReadFile(path); err == nil {
			return data, format, nil
		}
		// Leader's fetch failed and didn't write anything — fall through and
		// try upstream ourselves.
	} else {
		wg := &sync.WaitGroup{}
		wg.Add(1)
		c.inflight[canonical] = wg
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			delete(c.inflight, canonical)
			c.mu.Unlock()
			wg.Done()
		}()
	}

	upstreamURL := c.upstream + "?" + canonical
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream %d: %s", resp.StatusCode, string(body))
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = format
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.logger.Warnf("noaa cache: mkdir %q: %v", filepath.Dir(path), err)
	} else {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			c.logger.Warnf("noaa cache: write %q: %v", tmp, err)
		} else if err := os.Rename(tmp, path); err != nil {
			c.logger.Warnf("noaa cache: rename %q -> %q: %v", tmp, path, err)
		}
	}

	go c.maybeEvict()

	return body, ct, nil
}

// maybeEvict trims the cache directory to maxBytes by removing the least-recently-modified
// files first. It targets 90% of the limit so we don't run another pass for every write.
func (c *NoaaCache) maybeEvict() {
	if !c.evictMu.TryLock() {
		return
	}
	defer c.evictMu.Unlock()

	type entry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var entries []entry
	var total int64
	_ = filepath.Walk(c.cacheDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		entries = append(entries, entry{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})

	if total <= c.maxBytes {
		return
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })

	target := c.maxBytes - c.maxBytes/10 // evict down to 90% of the limit
	removed := 0
	var freed int64
	for _, e := range entries {
		if total <= target {
			break
		}
		if err := os.Remove(e.path); err == nil {
			total -= e.size
			freed += e.size
			removed++
		}
	}
	if removed > 0 {
		c.logger.Infof("noaa cache evicted %d files, freed %d bytes (now %d/%d)", removed, freed, total, c.maxBytes)
	}
}

func (c *NoaaCache) handleProxy(w http.ResponseWriter, r *http.Request) {
	canonical, format, err := canonicalQuery(r.URL.RawQuery)
	if err != nil {
		c.errs.Add(1)
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	data, ct, status, err := c.fetch(r.Context(), canonical, format)
	if err != nil {
		c.errs.Add(1)
		c.logger.Warnf("noaa proxy fetch failed: %v", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	switch status {
	case cacheStatusHit:
		c.hits.Add(1)
	case cacheStatusStale:
		c.stales.Add(1)
	case cacheStatusMiss:
		c.misses.Add(1)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// X-Cache lets the browser devtools Network panel show, per-request,
	// whether the proxy served from disk or hit upstream. Far less invasive
	// than file-counting or logging — every tile self-reports.
	w.Header().Set("X-Cache", status.String())
	// X-Cache-Key (sha) and X-Cache-Canonical (the full canonicalized
	// query that gets hashed) let you diff two "same tile" requests in
	// devtools to see exactly which param is drifting. If two requests
	// for the same tile show different keys, the canonicals will show
	// which param differs.
	sum := sha256.Sum256([]byte(canonical))
	w.Header().Set("X-Cache-Key", hex.EncodeToString(sum[:]))
	w.Header().Set("X-Cache-Canonical", canonical)
	// X-Tile is just the BBOX (post-normalization). Two requests for the
	// "same tile" — the thing your eye groups together — share a BBOX, so
	// this header makes the grouping trivial in devtools: filter the
	// Network panel for X-Tile=<value> and you see every request that
	// should have hashed identically.
	if cv, err := url.ParseQuery(canonical); err == nil {
		if bbox := cv.Get("BBOX"); bbox != "" {
			w.Header().Set("X-Tile", bbox)
		}
	}
	_, _ = w.Write(data)
}

type prefetchRequest struct {
	MinLon  float64 `json:"minLon"`
	MinLat  float64 `json:"minLat"`
	MaxLon  float64 `json:"maxLon"`
	MaxLat  float64 `json:"maxLat"`
	MinZoom int     `json:"minZoom"`
	MaxZoom int     `json:"maxZoom"`
	Layers  string  `json:"layers"`
}

type prefetchResponse struct {
	Tiles int `json:"tiles"`
}

// handlePrefetch enumerates the XYZ tile grid covering the requested bbox/zoom range and
// requests each WMS tile so it lands in the disk cache. It runs synchronously; tiles
// already cached return immediately. Callers wanting fire-and-forget behaviour can simply
// not await the response.
func (c *NoaaCache) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req prefetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.MinZoom == 0 && req.MaxZoom == 0 {
		req.MinZoom, req.MaxZoom = 9, 13
	}
	if req.MaxZoom < req.MinZoom {
		http.Error(w, "maxZoom < minZoom", http.StatusBadRequest)
		return
	}

	tiles := tilesForBBox(req.MinLon, req.MinLat, req.MaxLon, req.MaxLat, req.MinZoom, req.MaxZoom)

	sem := make(chan struct{}, prefetchConcurrency)
	var wg sync.WaitGroup
	for _, t := range tiles {
		t := t
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			canonical := wmsCanonicalForTile(t, req.Layers)
			if _, _, _, err := c.fetch(r.Context(), canonical, "image/png"); err != nil {
				c.logger.Debugf("prefetch tile %v: %v", t, err)
			}
		}()
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prefetchResponse{Tiles: len(tiles)})
}

func (c *NoaaCache) handleStats(w http.ResponseWriter, r *http.Request) {
	var files int
	var bytes int64
	_ = filepath.Walk(c.cacheDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	hits := c.hits.Load()
	stales := c.stales.Load()
	misses := c.misses.Load()
	errs := c.errs.Load()
	served := hits + stales + misses
	var hitRate float64
	if served > 0 {
		hitRate = float64(hits+stales) / float64(served)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"files":      files,
		"bytes":      bytes,
		"maxBytes":   c.maxBytes,
		"staleAfter": c.staleAfter.String(),
		"cacheDir":   c.cacheDir,
		"hits":       hits,
		"stales":     stales,
		"misses":     misses,
		"errors":     errs,
		"hitRate":    hitRate,
	})
}

// ---- tile math ----

type tileXYZ struct{ x, y, z int }

func tilesForBBox(minLon, minLat, maxLon, maxLat float64, minZ, maxZ int) []tileXYZ {
	var out []tileXYZ
	for z := minZ; z <= maxZ; z++ {
		x0, y1 := lonLatToTile(minLon, minLat, z)
		x1, y0 := lonLatToTile(maxLon, maxLat, z)
		if x0 > x1 {
			x0, x1 = x1, x0
		}
		if y0 > y1 {
			y0, y1 = y1, y0
		}
		max := 1 << z
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				if x < 0 || y < 0 || x >= max || y >= max {
					continue
				}
				out = append(out, tileXYZ{x, y, z})
			}
		}
	}
	return out
}

func lonLatToTile(lon, lat float64, z int) (int, int) {
	n := float64(int(1) << z)
	x := int((lon + 180.0) / 360.0 * n)
	latRad := lat * math.Pi / 180.0
	y := int((1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * n)
	return x, y
}

func tileBBoxMercator(t tileXYZ) (float64, float64, float64, float64) {
	size := 2 * mercatorMax / float64(int(1)<<t.z)
	xmin := -mercatorMax + float64(t.x)*size
	xmax := xmin + size
	ymax := mercatorMax - float64(t.y)*size
	ymin := ymax - size
	return xmin, ymin, xmax, ymax
}

func wmsCanonicalForTile(t tileXYZ, layers string) string {
	xmin, ymin, xmax, ymax := tileBBoxMercator(t)
	v := url.Values{}
	v.Set("SERVICE", "WMS")
	v.Set("VERSION", "1.3.0")
	v.Set("REQUEST", "GetMap")
	v.Set("FORMAT", "image/png")
	v.Set("TRANSPARENT", "TRUE")
	v.Set("LAYERS", layers)
	v.Set("STYLES", "")
	v.Set("WIDTH", "256")
	v.Set("HEIGHT", "256")
	v.Set("CRS", "EPSG:3857")
	v.Set("BBOX", fmt.Sprintf("%f,%f,%f,%f", xmin, ymin, xmax, ymax))
	canon, _, _ := canonicalQuery(v.Encode())
	return canon
}
