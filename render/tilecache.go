package render

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.viam.com/rdk/logging"
)

// ENCTileCache is a write-through on-disk cache for rendered NOAA ENC PNG tiles.
// Tiles are stored at <rootDir>/{style}-sd-{N}/{z}/{x}/{y}.png so different
// safety-depth buckets and render-rule versions (the version is part of the
// style key) stay isolated. A background cleaner (StartCleaner) ages tiles out:
// stale-version directories stop being written after a version bump and age off
// on their own, which also bounds growth.
type ENCTileCache struct {
	rootDir string

	logger        logging.Logger
	cleanerCancel context.CancelFunc
}

// NewENCTileCache creates a tile cache rooted at rootDir. The directory is
// created if it doesn't exist.
func NewENCTileCache(rootDir string) (*ENCTileCache, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("enc tile cache: mkdir %q: %w", rootDir, err)
	}
	return &ENCTileCache{rootDir: rootDir}, nil
}

// Path returns the on-disk path for a tile, regardless of whether it exists.
// Exposed for tests. safeDepthBucket is in integer feet. `style` is "wms" or
// "ecdis" — keyed separately so two layers don't collide.
func (c *ENCTileCache) Path(style string, safeDepthBucket, z, x, y int) string {
	if style == "" {
		style = "wms"
	}
	return filepath.Join(c.rootDir,
		style+"-sd-"+strconv.Itoa(safeDepthBucket),
		strconv.Itoa(z),
		strconv.Itoa(x),
		strconv.Itoa(y)+".png")
}

// Get returns the cached PNG bytes for the tile if present. The bool is false
// on any miss (including transient read errors); callers should treat that as
// "render and Put".
func (c *ENCTileCache) Get(style string, safeDepthBucket, z, x, y int) ([]byte, bool) {
	data, err := os.ReadFile(c.Path(style, safeDepthBucket, z, x, y))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Put writes the tile to disk atomically (write to a sibling .tmp file then
// rename) so a concurrent renderer of the same tile can't observe a partial
// PNG.
func (c *ENCTileCache) Put(style string, safeDepthBucket, z, x, y int, png []byte) error {
	final := c.Path(style, safeDepthBucket, z, x, y)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("enc tile cache: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), filepath.Base(final)+".*.tmp")
	if err != nil {
		return fmt.Errorf("enc tile cache: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(png); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("enc tile cache: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("enc tile cache: close: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("enc tile cache: rename: %w", err)
	}
	return nil
}

// StartCleaner runs a background goroutine that deletes tiles older than maxAge
// from disk every `interval`, so the cache doesn't grow without bound and
// stale-version tiles (left behind after an ENCRenderRulesVersion bump) get
// reclaimed. Age is by file mtime, which Put sets on (re)render; a hot tile
// served only from cache for longer than maxAge is deleted and re-rendered once
// on the next request. Cleans once eagerly on start. Call StopCleaner to stop.
func (c *ENCTileCache) StartCleaner(logger logging.Logger, maxAge, interval time.Duration) {
	if maxAge <= 0 || interval <= 0 {
		return
	}
	c.logger = logger
	ctx, cancel := context.WithCancel(context.Background())
	c.cleanerCancel = cancel
	go func() {
		runOnce := func() {
			n, freed := c.cleanOlderThan(maxAge)
			if n > 0 && c.logger != nil {
				c.logger.Infof("enc tile cache: removed %d tiles (%.1f MB freed, older than %s)",
					n, float64(freed)/(1024*1024), maxAge)
			}
		}
		runOnce()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}

// StopCleaner stops the background cleaner started by StartCleaner. Safe to call
// when no cleaner is running.
func (c *ENCTileCache) StopCleaner() {
	if c.cleanerCancel != nil {
		c.cleanerCancel()
		c.cleanerCancel = nil
	}
}

// cleanOlderThan walks the cache and removes every regular file whose mtime is
// older than maxAge, returning the count removed and bytes freed. Empty parent
// directories left behind are pruned best-effort on a second pass.
func (c *ENCTileCache) cleanOlderThan(maxAge time.Duration) (removed int, freed int64) {
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.WalkDir(c.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			return nil
		}
		size := info.Size()
		if rmErr := os.Remove(path); rmErr == nil {
			removed++
			freed += size
		}
		return nil
	})
	c.pruneEmptyDirs()
	return removed, freed
}

// pruneEmptyDirs removes now-empty directories under rootDir (e.g. a fully-aged-
// out stale-version tree), deepest first. rootDir itself is kept.
func (c *ENCTileCache) pruneEmptyDirs() {
	var dirs []string
	_ = filepath.WalkDir(c.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != c.rootDir {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Remove deepest first so a parent can become empty after its children go.
	for i := len(dirs) - 1; i >= 0; i-- {
		if entries, derr := os.ReadDir(dirs[i]); derr == nil && len(entries) == 0 {
			_ = os.Remove(dirs[i])
		}
	}
}
