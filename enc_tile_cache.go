package vc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ENCTileCache is an unbounded write-through on-disk cache for rendered NOAA ENC
// PNG tiles. Tiles are stored at <rootDir>/sd-{N}/{z}/{x}/{y}.png so different
// safety-depth buckets stay isolated — a sailboat with safeDepth=10ft and a
// skiff with safeDepth=3ft each see their own gradient without colliding.
//
// TODO: add an eviction policy / size cap. Today this grows without bound; a
// follow-up should either prune by LRU/age or enforce a max-bytes budget like
// the WMS proxy cache does.
type ENCTileCache struct {
	rootDir string
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
