package vc

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
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
	CycleTime   time.Time          // reference time of the run
	FHs         []int              // forecast hours in publish order
	Tiles       []Tile             // the fixed grid; copied into the manifest
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
	Model           string         `json:"model"`
	Cycle           string         `json:"cycle"` // "20060102T15" UTC
	PublishedAt     time.Time      `json:"publishedAt"`
	FHs             []int          `json:"fhs"`
	Tiles           []TileManifest `json:"tiles"`
	PreviousCycles  []string       `json:"previousCycles,omitempty"`
}

// TileManifest is the lightweight view of a Tile we serialise into the
// latest pointer. The full Tile struct's Col/Row are useful internally
// but redundant on the wire — clients only need (Key, Bbox) to do
// viewport-to-tile lookup.
type TileManifest struct {
	Key           string     `json:"key"`
	NominalBbox   [4]float64 `json:"nominalBbox"`   // [w, s, e, n]
	PublishedBbox [4]float64 `json:"publishedBbox"` // [w, s, e, n]
}

// BuildECMWFCycle runs one full publish prep: walks the latest ECMWF
// Open Data cycle, decodes 10u + 10v at every forecast hour in
// `m.MinFh..m.MaxFh` with step `m.StepFh`, crops each into the tile
// grid, gzips each tile's ol-wind JSON, and returns everything ready
// for the uploader. No network I/O happens beyond the ECMWF fetch
// itself — the returned PublishedCycle is pure bytes.
//
// This is the slow path: ~49 fhs × (1 GRIB fetch + decode + 36 crops
// + 36 gzips). Expect ~5-15 minutes on a small VM. Run inside a
// goroutine; cancel via ctx to abort mid-cycle.
func BuildECMWFCycle(ctx context.Context, client *http.Client, m *WeatherModel) (*PublishedCycle, error) {
	if m == nil || m.Fetch == nil {
		return nil, fmt.Errorf("wind-publisher: model nil or has no Fetch")
	}
	tiles := AllTiles()
	cycle := &PublishedCycle{
		Model:     m.Name,
		Tiles:     tiles,
		TileBlobs: make(map[publishKey]TileBlob),
	}

	// Walk all fhs in step order. Stop on the first cycle-resolution
	// error (fetch returned no body for ANY cycle in walkLatestCycle's
	// 4-back window); a single-fh failure mid-walk is fatal too so we
	// don't ship a partial cycle with holes the client would surface
	// as 404s.
	startFh := m.MinFh
	step := m.StepFh
	if step <= 0 {
		step = 1
	}
	if startFh%step != 0 {
		startFh += step - (startFh % step)
	}

	var anyCycleTime time.Time
	for fh := startFh; fh <= m.MaxFh; fh += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		records, cycleT, err := fetchAndDecodeForPublish(ctx, client, m, fh)
		if err != nil {
			return nil, fmt.Errorf("fh=%d: %w", fh, err)
		}
		if anyCycleTime.IsZero() {
			anyCycleTime = cycleT
		} else if !cycleT.Equal(anyCycleTime) {
			// A walkLatestCycle that hit a different cycle for this fh
			// (because the latest one isn't fully published yet) would
			// mix cycles in one publish — refuse rather than ship
			// inconsistent fields.
			return nil, fmt.Errorf("fh=%d resolved to cycle %s, expected %s",
				fh, cycleT.Format("20060102T15"), anyCycleTime.Format("20060102T15"))
		}
		cycle.FHs = append(cycle.FHs, fh)

		for _, tile := range tiles {
			cropped := cropPair(records, tile.PublishedBbox)
			blob, err := encodeTileBlob(cropped)
			if err != nil {
				return nil, fmt.Errorf("fh=%d tile=%s encode: %w", fh, tile.Key, err)
			}
			cycle.TileBlobs[publishKey{FH: fh, TileKey: tile.Key}] = blob
		}
	}

	cycle.CycleTime = anyCycleTime
	cycle.PublishedAt = time.Now().UTC()
	sort.Ints(cycle.FHs)
	return cycle, nil
}

// fetchAndDecodeForPublish wraps the existing m.Fetch path so the
// publisher reuses the same decoder, cycle-walkback, and raw-cache
// behaviour as the on-demand server. Returns the decoded windRecords
// and the reference time of the cycle that satisfied the fetch.
func fetchAndDecodeForPublish(ctx context.Context, client *http.Client, m *WeatherModel, fh int) ([]windRecord, time.Time, error) {
	// Capture the cycle time by intercepting walkLatestCycle's choice.
	// The standard m.Fetch contract doesn't return the cycle time, so
	// we duplicate the walkback here (the ECMWF model's Fetch does the
	// same internally) to get it back.
	var pickedCycle time.Time
	walker := func(ctx context.Context, t time.Time) ([]byte, error) {
		bytes, err := fetchECMWFWind10m(ctx, client, t, fh)
		if err == nil {
			pickedCycle = t
		}
		return bytes, err
	}
	grib, cycleT, err := walkLatestCycle(ctx, m, fh, walker)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !pickedCycle.IsZero() {
		cycleT = pickedCycle
	}
	records, err := parseECMWFWind10m(grib, cycleT, fh)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse: %w", err)
	}
	return records, cycleT, nil
}

// cropPair runs CropWindRecord across a 2-record (10u, 10v) pair so
// both fields land in the same tile bbox.
func cropPair(recs []windRecord, bbox [4]float64) []windRecord {
	out := make([]windRecord, len(recs))
	for i, r := range recs {
		out[i] = CropWindRecord(r, bbox)
	}
	return out
}

// encodeTileBlob serialises one tile's (u, v) record pair as ol-wind
// JSON and gzips it. Gzipped at maximum compression: the publisher's
// CPU is cheaper than R2 storage + chartplotter download bytes.
func encodeTileBlob(recs []windRecord) (TileBlob, error) {
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
func FindWeatherModelForPublish(name string) *WeatherModel {
	return findModel(name)
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
