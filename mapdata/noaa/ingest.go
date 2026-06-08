package noaa

import (
	"context"
	"fmt"
	"strconv"

	"go.mongodb.org/mongo-driver/mongo"
)

// IngestStats summarises an ingest run for logging.
type IngestStats struct {
	Cells        int // cells parsed and upserted
	CellsSkipped int // cells skipped (already current, or no .000 on disk)
	Docs         int // feature documents applied
	WriteErrors  int // per-document write errors tolerated (bad geometry etc.)
	GeomSkipped  int // features dropped for empty/degenerate geometry
}

// IngestCellFile parses a single .000 file and upserts its features into the
// noaa collection, then writes the per-cell dedup metadata. It always parses
// (no edition check) — callers wanting dedup should use IngestBBox or check
// LookupMeta first. cellName may be "" to derive it from the path.
func IngestCellFile(ctx context.Context, coll *mongo.Collection, cellName, path string) (IngestStats, error) {
	res, err := ParseCell(cellName, path)
	if err != nil {
		return IngestStats{}, err
	}
	applied, writeErrs, err := UpsertDocs(ctx, coll, res.Docs, 0)
	if err != nil {
		return IngestStats{}, err
	}
	if err := WriteMeta(ctx, coll, res.Meta); err != nil {
		return IngestStats{}, fmt.Errorf("write meta %s: %w", res.Meta.Cell, err)
	}
	return IngestStats{
		Cells:       1,
		Docs:        applied,
		WriteErrors: writeErrs,
		GeomSkipped: res.Skipped,
	}, nil
}

// IngestAll runs the full NOAA pipeline for the ENTIRE published catalog — every
// active ENC cell, worldwide — refreshing the catalog (so newly-published cells
// and editions are picked up) and ingesting each. It's IngestBBox over a global
// box; since the catalog overlap test returns every cell for world bounds, this
// is exactly "all files." Cells already at the current edition+update in Mongo
// are skipped, so periodic re-runs are cheap. minScale/maxScale still apply
// (pass 0 for no bound) for the rare case of restricting by chart scale.
func IngestAll(
	ctx context.Context,
	coll *mongo.Collection,
	store *Store,
	minScale, maxScale, parallel int,
	logf func(string, ...any),
) (IngestStats, error) {
	return IngestBBox(ctx, coll, store, -180, -90, 180, 90, minScale, maxScale, parallel, logf)
}

// IngestBBox is the full NOAA pipeline for a lon/lat box: it ensures every ENC
// cell overlapping the box is on disk at NOAA's latest edition (Store.SyncBBox),
// then parses and upserts each overlapping cell into the noaa collection. A
// cell whose edition+update already match what's recorded in the collection is
// skipped without re-parsing.
//
// logf, if non-nil, receives a progress line per cell; pass nil for silence.
func IngestBBox(
	ctx context.Context,
	coll *mongo.Collection,
	store *Store,
	minLon, minLat, maxLon, maxLat float64,
	minScale, maxScale, parallel int,
	logf func(string, ...any),
) (IngestStats, error) {
	if coll == nil {
		return IngestStats{}, fmt.Errorf("noaa: nil collection")
	}
	if store == nil {
		return IngestStats{}, fmt.Errorf("noaa: nil store")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if _, _, err := store.SyncBBox(ctx, minLon, minLat, maxLon, maxLat, minScale, maxScale, parallel); err != nil {
		return IngestStats{}, fmt.Errorf("noaa sync: %w", err)
	}

	cells := store.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, minScale, maxScale)
	var stats IngestStats
	for _, c := range cells {
		// Dedup: skip if the collection already holds this exact edition+update.
		if meta, ok, err := LookupMeta(ctx, coll, c.Name); err == nil && ok &&
			meta.Edition == strconv.Itoa(c.Edition) && meta.UpdateNumber == strconv.Itoa(c.Update) {
			stats.CellsSkipped++
			continue
		}
		path := store.S57Path(c.Name)
		if path == "" {
			// SyncBBox couldn't fetch it (download failed / inactive); skip.
			stats.CellsSkipped++
			continue
		}
		cs, err := IngestCellFile(ctx, coll, c.Name, path)
		if err != nil {
			logf("noaa cell %s: %v", c.Name, err)
			stats.CellsSkipped++
			continue
		}
		stats.Cells += cs.Cells
		stats.Docs += cs.Docs
		stats.WriteErrors += cs.WriteErrors
		stats.GeomSkipped += cs.GeomSkipped
		logf("noaa cell %s: %d features (%d geom-skipped, %d write-errs)",
			c.Name, cs.Docs, cs.GeomSkipped, cs.WriteErrors)
	}
	return stats, nil
}
