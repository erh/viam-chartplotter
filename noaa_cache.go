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
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

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
		client:     &http.Client{Timeout: 30 * time.Second},
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
}

// canonicalQuery returns a stable representation of the WMS query string so different
// orderings/cases of the same logical request hash to the same cache key.
func canonicalQuery(raw string) (string, string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", "", err
	}
	keys := make([]string, 0, len(values))
	upper := url.Values{}
	for k, v := range values {
		uk := strings.ToUpper(k)
		upper[uk] = append(upper[uk], v...)
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

func (c *NoaaCache) cachePath(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	hex := hex.EncodeToString(sum[:])
	return filepath.Join(c.cacheDir, hex[:2], hex+".bin")
}

// fetch returns the cached bytes and content-type for the given canonical
// query. Behaviour:
//   - Fresh on disk (mtime within staleAfter): serve from disk, no upstream call.
//   - Stale on disk: serve the stale bytes immediately and kick a background
//     refresh. If upstream is down we keep serving the old copy rather than
//     leaving the client with nothing.
//   - Nothing on disk: block on upstream and write through to disk.
func (c *NoaaCache) fetch(ctx context.Context, canonical, format string) ([]byte, string, error) {
	path := c.cachePath(canonical)
	if info, err := os.Stat(path); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			stale := c.staleAfter > 0 && time.Since(info.ModTime()) >= c.staleAfter
			if stale {
				go c.refreshAsync(canonical, format)
			}
			return data, format, nil
		}
	}
	// No usable disk copy — must wait for upstream.
	return c.fetchAndStore(ctx, canonical, format)
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err == nil {
			_ = os.Rename(tmp, path)
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
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	data, ct, err := c.fetch(r.Context(), canonical, format)
	if err != nil {
		c.logger.Warnf("noaa proxy fetch failed: %v", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
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
			if _, _, err := c.fetch(r.Context(), canonical, "image/png"); err != nil {
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"files":      files,
		"bytes":      bytes,
		"maxBytes":   c.maxBytes,
		"staleAfter": c.staleAfter.String(),
		"cacheDir":   c.cacheDir,
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
