package places

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go.mongodb.org/mongo-driver/mongo"
)

// Loading Overture Maps places into the gazetteer.
//
// Overture's places layer (CDLA-Permissive 2.0) carries the commercial marine
// POIs OSM lacks — Cobb's Marina exists there with its street address while
// OSM has never heard of it. The parquet-on-S3 distribution is filtered and
// flattened by DuckDB (see the ingest-overture Makefile target) into NDJSON,
// one place per line, which this reads and upserts. Splitting the work that
// way keeps a parquet reader out of the Go module and makes the subset a
// plain file you can inspect before loading.

// OvertureRow is one line of the DuckDB export.
type OvertureRow struct {
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	Street     string  `json:"street"`
	City       string  `json:"city"`
	State      string  `json:"state"`
	Lng        float64 `json:"lng"`
	Lat        float64 `json:"lat"`
	Confidence float64 `json:"confidence"`
}

// OvertureMinConfidence drops the low-confidence records — conflation noise
// like "Baypoint & Little Crk Marina" (0.70) sitting beside the clean "Bay
// Point Marina" (0.92). The export query filters too; this is the backstop
// for hand-edited files.
const OvertureMinConfidence = 0.5

// PlaceFromOverture maps one export row to a gazetteer Place, or nil for a
// row not worth keeping (no name, junk coordinates, low confidence).
func PlaceFromOverture(row OvertureRow) *Place {
	if row.Name == "" || row.Category == "" {
		return nil
	}
	if row.Confidence < OvertureMinConfidence {
		return nil
	}
	if row.Lat == 0 && row.Lng == 0 {
		return nil
	}
	if row.Lat < -90 || row.Lat > 90 || row.Lng < -180 || row.Lng > 180 {
		return nil
	}
	return &Place{
		ID:     ID(SourceOverture, row.Category, row.Name, row.Lat, row.Lng),
		Name:   row.Name,
		Source: SourceOverture,
		Class:  row.Category,
		Lat:    row.Lat,
		Lng:    row.Lng,
		BBox:   [4]float64{row.Lng, row.Lat, row.Lng, row.Lat},
		Street: row.Street,
		City:   row.City,
		State:  row.State,
	}
}

// BuildFromOverture reads NDJSON rows and upserts them. Unparseable lines are
// counted and skipped — one bad row shouldn't end an import — and everything
// is an idempotent upsert, so re-running a monthly refresh is safe.
func BuildFromOverture(ctx context.Context, dst *mongo.Collection, r io.Reader, opts BuildOptions) (BuildStats, error) {
	var stats BuildStats
	if dst == nil {
		return stats, fmt.Errorf("places: nil collection")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}

	batch := make([]Place, 0, opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := Upsert(ctx, dst, batch)
		if err != nil {
			return err
		}
		stats.Written += n
		batch = batch[:0]
		return nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		stats.Read++
		var row OvertureRow
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		p := PlaceFromOverture(row)
		if p == nil {
			continue
		}
		batch = append(batch, *p)
		if len(batch) >= opts.BatchSize {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return stats, fmt.Errorf("places: read overture export: %w", err)
	}
	return stats, flush()
}
