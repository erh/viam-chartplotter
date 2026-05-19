package vc

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tile-blob disk cache: the second cache layer behind raw-GRIB.
// While the raw-GRIB cache prevents re-fetching from ECMWF, this one
// prevents re-doing the CCSDS decode + crop + JSON encode + gzip
// when an interrupted publish run is re-attempted.
//
// Layout: <ECMWFRawCacheDir>/../tiles/<model>/<cycle>/f<fh>/<tile>.json.gz
// Sits next to raw-ecmwf/ so the 60-day cache cleaner in
// noaa_weather_cache.go's StartCleaner picks it up automatically.
//
// Files are byte-identical to what the publisher uploads to R2, so
// the cache also lets us skip re-encoding on every retry — the
// loadAllTileBlobsForFh fast path reads them straight back into the
// PublishedCycle without ever needing to touch the raw GRIB.
//
// All paths route through tileCacheRoot() which derives off
// ECMWFRawCacheDir; if no raw cache is configured (test / dev
// only), the tile cache is also disabled and the publisher falls
// back to recomputing every time. Same "fail open" stance the raw
// cache uses.

// tileCacheRoot returns the directory under which per-(model, cycle,
// fh, tile) blobs live. Empty string when caching is disabled.
func tileCacheRoot() string {
	if ECMWFRawCacheDir == "" {
		return ""
	}
	// raw-ecmwf/ and tiles/ are siblings under the noaa-weather
	// cache root so the cleaner walks both.
	return filepath.Join(filepath.Dir(ECMWFRawCacheDir), "tiles")
}

// tileCachePath builds the deterministic on-disk path for one
// (model, cycle, fh, tile) blob. Stable across processes so a
// publisher restart can find files left by the previous run.
func tileCachePath(model string, cycleT time.Time, fh int, tileKey string) string {
	root := tileCacheRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root,
		model,
		cycleT.UTC().Format("20060102T15"),
		fmt.Sprintf("f%03d", fh),
		tileKey+".json.gz")
}

// readTileBlobCache returns the cached gzipped blob for one tile if
// present, or (TileBlob{}, false). The size is read from the file
// header (gzipped length), so UncompBytes is left zero — the
// publisher doesn't look at it after the cache layer, so this is
// fine.
func readTileBlobCache(model string, cycleT time.Time, fh int, tileKey string) (TileBlob, bool) {
	path := tileCachePath(model, cycleT, fh, tileKey)
	if path == "" {
		return TileBlob{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return TileBlob{}, false
	}
	return TileBlob{GzippedJSON: b}, true
}

// writeTileBlobCache persists one tile's gzipped JSON via the
// standard .tmp + rename pattern (so a crash mid-write never
// leaves a half-truncated blob a future run would try to upload).
// Creates parent directories on demand. Returns the first error
// from any step; callers should log and continue (cache is
// best-effort).
func writeTileBlobCache(model string, cycleT time.Time, fh int, tileKey string, blob TileBlob) error {
	path := tileCachePath(model, cycleT, fh, tileKey)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob.GzippedJSON, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadAllTileBlobsForFh probes the cache for every tile of one
// (model, cycle, fh) tuple and returns the map of hits plus a flag
// telling the caller "every tile is cached, you can skip decode."
// On partial cache (some tiles cached, others not), the returned map
// contains only the hits — the caller still has to decode the GRIB
// to produce the missing ones, but it can reuse what's already on
// disk for the rest. Per-tile cache log lines emitted at debug
// verbosity (one per fh's all-hit case logged at info from the
// caller) so a full ALL-HIT scan doesn't flood the logs.
func loadAllTileBlobsForFh(model string, cycleT time.Time, fh int, tiles []Tile) (map[publishKey]TileBlob, bool) {
	out := make(map[publishKey]TileBlob, len(tiles))
	for _, tile := range tiles {
		blob, ok := readTileBlobCache(model, cycleT, fh, tile.Key)
		if !ok {
			// Don't bother continuing if we miss even one tile —
			// the caller will decode anyway, and the loaded blobs
			// already in `out` will still be reused per-tile.
			return out, false
		}
		out[publishKey{FH: fh, TileKey: tile.Key}] = blob
	}
	return out, true
}

