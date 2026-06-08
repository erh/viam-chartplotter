package publish

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/erh/viam-chartplotter/weather"
	"go.viam.com/rdk/logging"
	"net/http"
	"os"
	"sort"
	"time"
)

// PublishedCycle is one self-contained slice of work the publisher
// hands off: a single (model, cycle) pair, decoded once and crammed
// into the tile grid for every forecast hour. The publisher uploads
// the per-(fh, tile) blobs plus a manifest, then atomically updates
// latest.json to point here.
type PublishedCycle struct {
	Model       string
	CycleTime   time.Time               // reference time of the run
	FHs         []int                   // forecast hours in publish order
	Tiles       []weather.Tile          // the fixed grid; copied into the manifest
	TileBlobs   map[publishKey]TileBlob // (fh, tile) → gzipped JSON + content metadata
	PublishedAt time.Time
}

// publishKey identifies one tile-payload within a PublishedCycle.
type publishKey struct {
	FH      int
	TileKey string
}

// TileBlob is one (fh, tile) payload ready for upload: already
// gzipped, with the byte count cached so the uploader can populate
// Content-Length without re-reading.
type TileBlob struct {
	GzippedJSON []byte
	UncompBytes int
}

// LatestPointer is the schema published at wind/<model>/latest.json.
// Clients fetch this (short max-age) to discover the current cycle and
// the tile grid, then derive per-(fh, tile) URLs deterministically.
type LatestPointer struct {
	Model          string         `json:"model"`
	Cycle          string         `json:"cycle"` // "20060102T15" UTC
	PublishedAt    time.Time      `json:"publishedAt"`
	FHs            []int          `json:"fhs"`
	Tiles          []TileManifest `json:"tiles"`
	PreviousCycles []string       `json:"previousCycles,omitempty"`
}

// TileManifest is the lightweight view of a weather.Tile we serialise into the
// latest pointer. The full weather.Tile struct's Col/Row are useful internally
// but redundant on the wire — clients only need (Key, Bbox) to do
// viewport-to-tile lookup.
type TileManifest struct {
	Key           string     `json:"key"`
	NominalBbox   [4]float64 `json:"nominalBbox"`   // [w, s, e, n]
	PublishedBbox [4]float64 `json:"publishedBbox"` // [w, s, e, n]
}

// BuildECMWFCycle runs one full publish prep: picks one ECMWF Open
// Data cycle that has every required forecast hour available, decodes
// 10u + 10v at each fh, crops into the tile grid, gzips each tile's
// ol-wind JSON, and returns everything ready for the uploader.
//
// "Picks one cycle" is the key invariant: a published archive must be
// internally consistent, never mixing fhs from different runs. ECMWF
// publishes a cycle incrementally — fh=0 lands first, fh=144 lands
// last — so the freshest cycle is often partial. We probe each
// candidate cycle in turn (newest first) and only ship one where
// every fh in [MinFh..MaxFh] is present.
//
// Slow path: per cycle, ~49 fhs × (GRIB fetch + decode + 36 crops +
// 36 gzips). Expect ~5-15 minutes on a small VM for a successful
// cycle. Run inside a goroutine; cancel via ctx to abort.
func BuildECMWFCycle(ctx context.Context, client *http.Client, m *weather.WeatherModel, logger logging.Logger) (*PublishedCycle, error) {
	if m == nil || m.Fetch == nil {
		return nil, fmt.Errorf("wind-publisher: model nil or has no Fetch")
	}
	if len(m.CycleHours) == 0 {
		return nil, fmt.Errorf("wind-publisher: model %s has no cycle hours", m.Name)
	}

	now := time.Now().UTC().Add(-time.Duration(m.PublishLagH) * time.Hour)
	candidate := weather.MostRecentCycle(now, m.CycleHours)
	var lastErr error
	// Same 4-back budget weather.WalkLatestCycle uses on the on-demand path —
	// that's two full days for a 6h-cycle model, more than enough for
	// any realistic publish-lag scenario.
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cycle, err := buildOneCycle(ctx, client, m, candidate, logger)
		if err == nil {
			return cycle, nil
		}
		logger.Infof("publisher: cycle %s incomplete (%v) — trying previous cycle",
			candidate.Format("20060102T15"), err)
		lastErr = err
		candidate = weather.PreviousCycle(candidate, m.CycleHours)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no fully-published cycle for %s in last %d attempts", m.Name, 4)
	}
	return nil, lastErr
}

// buildOneCycle attempts every forecast hour against ONE specific
// cycle. Returns an error on the first fh that isn't available in
// that cycle so the caller can step back. The error message includes
// the failing fh so an operator can tell "the cycle just hasn't
// finished publishing yet" from "ECMWF is broken".
//
// Uses two cache layers (project-wide rule: every external fetch +
// every expensive recomputation goes through a disk cache):
//
//   - Raw-GRIB cache via weather.CachedFetchECMWFWind10m — skips ECMWF
//   - weather.Tile-blob cache via readTileBlobCache — skips CCSDS decode +
//     crop + JSON encode + gzip when we already produced this
//     exact (cycle, fh, tile) tuple in a prior (perhaps interrupted)
//     run. Saves ~60 s CPU per cycle on full retry.
//
// When every tile for an fh is cached, we don't even need to decode
// the GRIB for that fh — saves the ~150 ms CCSDS decode too. We
// still call weather.CachedFetchECMWFWind10m to confirm the GRIB is
// retrievable (a hit-from-disk is essentially free), so a future
// "all-tiles-cached-but-the-raw-cache-was-wiped" scenario still
// produces a coherent error rather than silently skipping a missing
// fh.
func buildOneCycle(ctx context.Context, client *http.Client, m *weather.WeatherModel, cycleT time.Time, logger logging.Logger) (*PublishedCycle, error) {
	tiles := weather.AllTiles()
	cycle := &PublishedCycle{
		Model:     m.Name,
		CycleTime: cycleT,
		Tiles:     tiles,
		TileBlobs: make(map[publishKey]TileBlob),
	}

	startFh := m.MinFh
	step := m.StepFh
	if step <= 0 {
		step = 1
	}
	if startFh%step != 0 {
		startFh += step - (startFh % step)
	}

	for fh := startFh; fh <= m.MaxFh; fh += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Fast path: every tile for this fh already cached on disk?
		// Read them back and skip the decode entirely.
		cached, allHit := loadAllTileBlobsForFh(m.Name, cycleT, fh, tiles)
		if allHit {
			cycle.FHs = append(cycle.FHs, fh)
			for k, blob := range cached {
				cycle.TileBlobs[k] = blob
			}
			logger.Debugf("tileCache: ALL-HIT cycle=%s fh=%d tiles=%d (skipped decode + encode)",
				cycleT.UTC().Format("20060102T15"), fh, len(tiles))
			continue
		}

		records, err := fetchAndDecodeForPublish(ctx, client, cycleT, fh, logger)
		if err != nil {
			return nil, fmt.Errorf("fh=%d: %w", fh, err)
		}
		cycle.FHs = append(cycle.FHs, fh)
		for _, tile := range tiles {
			pk := publishKey{FH: fh, TileKey: tile.Key}
			if blob, ok := cached[pk]; ok {
				// Partial hit from the loadAll above — already on
				// disk, just reuse.
				cycle.TileBlobs[pk] = blob
				continue
			}
			cropped := cropPair(records, tile.PublishedBbox)
			blob, err := encodeTileBlob(cropped)
			if err != nil {
				return nil, fmt.Errorf("fh=%d tile=%s encode: %w", fh, tile.Key, err)
			}
			if werr := writeTileBlobCache(m.Name, cycleT, fh, tile.Key, blob); werr != nil {
				// Cache write is best-effort; the in-memory blob
				// still gets uploaded.
				logger.Warnf("tileCache: write %s: %v", tileCachePath(m.Name, cycleT, fh, tile.Key), werr)
			}
			cycle.TileBlobs[pk] = blob
		}
	}

	cycle.PublishedAt = time.Now().UTC()
	sort.Ints(cycle.FHs)
	return cycle, nil
}

// fetchAndDecodeForPublish pulls one (cycle, fh) pair through the
// project-wide raw-bytes disk cache (weather.CachedFetchECMWFWind10m). No
// per-fh walkback — the cycle-locked walkback lives in
// BuildECMWFCycle so a missing fh aborts the whole-cycle attempt
// cleanly instead of silently mixing runs. Cache means a resumed
// publish after a mid-build crash only re-fetches the fhs that
// weren't yet cached, which matters because ECMWF rate-limits
// aggressively and a full cycle is ~85 MB of unnecessary traffic on
// re-fetch.
func fetchAndDecodeForPublish(ctx context.Context, client *http.Client, cycleT time.Time, fh int, logger logging.Logger) ([]weather.WindRecord, error) {
	grib, err := weather.CachedFetchECMWFWind10m(ctx, client, cycleT, fh, logger)
	if err != nil {
		return nil, err
	}
	records, err := weather.ParseECMWFWind10m(grib, cycleT, fh)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return records, nil
}

// cropPair runs weather.CropWindRecord across a 2-record (10u, 10v) pair so
// both fields land in the same tile bbox.
func cropPair(recs []weather.WindRecord, bbox [4]float64) []weather.WindRecord {
	out := make([]weather.WindRecord, len(recs))
	for i, r := range recs {
		out[i] = weather.CropWindRecord(r, bbox)
	}
	return out
}

// encodeTileBlob serialises one tile's (u, v) record pair as ol-wind
// JSON and gzips it. Gzipped at maximum compression: the publisher's
// CPU is cheaper than R2 storage + chartplotter download bytes.
func encodeTileBlob(recs []weather.WindRecord) (TileBlob, error) {
	rawJSON, err := json.Marshal(recs)
	if err != nil {
		return TileBlob{}, err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return TileBlob{}, err
	}
	if _, err := zw.Write(rawJSON); err != nil {
		return TileBlob{}, err
	}
	if err := zw.Close(); err != nil {
		return TileBlob{}, err
	}
	return TileBlob{
		GzippedJSON: buf.Bytes(),
		UncompBytes: len(rawJSON),
	}, nil
}

// TileBlobFor returns the (gzipped) blob for one (fh, tile) pair plus
// a presence flag. Used by the CLI and the uploader to walk the cycle
// in a deterministic order.
func (pc *PublishedCycle) TileBlobFor(fh int, tileKey string) (TileBlob, bool) {
	blob, ok := pc.TileBlobs[publishKey{FH: fh, TileKey: tileKey}]
	return blob, ok
}

// FindWeatherModelForPublish exposes the in-tree weather model
// registry so the cmd/wind-publisher CLI can pick a model by name
// without importing the whole noaa_weather_cache stack. Returns nil
// for an unknown name.
func FindWeatherModelForPublish(name string) *weather.WeatherModel {
	return weather.FindModel(name)
}

// WriteManifest serialises a cycle's latest-pointer / per-cycle
// manifest to `path` as pretty-printed JSON, creating parent dirs as
// needed. Used by the dry-run CLI and the in-module publisher to drop
// the same shape into local disk and into R2.
func WriteManifest(path string, cycle *PublishedCycle) error {
	manifest := cycle.Manifest()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, body)
}

// writeFileAtomic writes via a sibling .tmp file + rename so a
// crashed publisher can't leave a half-written manifest that a
// chartplotter would parse as truncated JSON. Rename is atomic on
// POSIX filesystems and good-enough on macOS APFS.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Manifest renders this cycle's latest-pointer schema.
func (pc *PublishedCycle) Manifest() LatestPointer {
	tm := make([]TileManifest, len(pc.Tiles))
	for i, t := range pc.Tiles {
		tm[i] = TileManifest{Key: t.Key, NominalBbox: t.NominalBbox, PublishedBbox: t.PublishedBbox}
	}
	return LatestPointer{
		Model:       pc.Model,
		Cycle:       pc.CycleTime.UTC().Format("20060102T15"),
		PublishedAt: pc.PublishedAt,
		FHs:         append([]int(nil), pc.FHs...),
		Tiles:       tm,
	}
}
