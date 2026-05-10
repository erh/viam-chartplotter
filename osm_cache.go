package vc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

// OSMTileCache fetches and disk-caches OSM raster tiles from the standard
// tile.openstreetmap.org service. Used as a base layer under our chart
// renderer when the user enables the OSM-underlay option, so harbour
// detail (roads, buildings, parks) shows under the chart's water/navaid
// overlay without the frontend having to composite two layers.
//
// OSM's usage policy requires a meaningful User-Agent and forbids "heavy"
// traffic — the on-disk cache means each (z,x,y) tile is fetched at most
// once per cache lifetime (until `Clear` is called or the file is removed),
// which keeps a single chartplotter instance well within fair use.
type OSMTileCache struct {
	cacheDir string
	client   *http.Client
	ua       string
	logger   logging.Logger

	mu       sync.Mutex
	inflight map[string]*sync.WaitGroup
}

// NewOSMTileCache creates a tile cache rooted at cacheDir. The directory is
// created if it doesn't exist. ua is the User-Agent header sent on upstream
// requests; OSM's policy requires a meaningful identifier, so callers
// should pass something like "viam-chartplotter/1.0 (+https://...)".
func NewOSMTileCache(cacheDir, ua string, logger logging.Logger) (*OSMTileCache, error) {
	if cacheDir == "" {
		return nil, errors.New("osm cache: cacheDir must be set")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("osm cache: mkdir %q: %w", cacheDir, err)
	}
	if ua == "" {
		ua = "viam-chartplotter (+https://github.com/viamrobotics/viam-chartplotter)"
	}
	return &OSMTileCache{
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 12 * time.Second},
		ua:       ua,
		logger:   logger,
		inflight: map[string]*sync.WaitGroup{},
	}, nil
}

// Fetch returns the PNG bytes of the OSM tile at (z, x, y), cache-first.
// Concurrent callers for the same tile coalesce on a single upstream
// request via inflight wait groups so a burst of tile renders doesn't
// fan out to multiple OSM requests for the same image.
func (c *OSMTileCache) Fetch(ctx context.Context, z, x, y int) ([]byte, error) {
	path := c.tilePath(z, x, y)
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	}

	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	c.mu.Lock()
	if wg, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		wg.Wait()
		// Whoever did the work has either populated the cache file or
		// failed — try the disk read again either way.
		return os.ReadFile(path)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.inflight[key] = wg
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		wg.Done()
	}()

	url := fmt.Sprintf("https://tile.openstreetmap.org/%d/%d/%d.png", z, x, y)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("osm fetch %d/%d/%d: %w", z, x, y, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("osm fetch %d/%d/%d: http %d", z, x, y, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		// Don't fail the request just because we couldn't cache — return
		// the bytes so the render still succeeds; next call will retry.
		if c.logger != nil {
			c.logger.Warnf("osm cache mkdir %q: %v", filepath.Dir(path), err)
		}
		return data, nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		if c.logger != nil {
			c.logger.Warnf("osm cache write %q: %v", path, err)
		}
	}
	return data, nil
}

func (c *OSMTileCache) tilePath(z, x, y int) string {
	return filepath.Join(c.cacheDir, fmt.Sprintf("%d", z), fmt.Sprintf("%d", x), fmt.Sprintf("%d.png", y))
}
