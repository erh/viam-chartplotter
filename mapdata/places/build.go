package places

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Building the gazetteer.
//
// The source collections total hundreds of millions of documents, so this is
// written to be interruptible and re-runnable rather than to complete in one
// heroic pass: every write is an idempotent upsert keyed on the place's
// identity, and the work can be scoped to a bounding box. Build the water you
// sail in first; it is useful immediately, and the rest can follow overnight.

// BuildOptions configures one build pass.
type BuildOptions struct {
	// BBox restricts the build to features intersecting [minLon, minLat,
	// maxLon, maxLat]. Nil builds everything.
	BBox *[4]float64
	// BatchSize is how many places to accumulate before writing.
	BatchSize int
	// Progress, when set, is called periodically with what has been read and
	// written so far.
	Progress func(collection string, read, written int)
}

// BuildStats reports what a pass did.
type BuildStats struct {
	Read    int
	Written int
	Elapsed time.Duration
}

const defaultBatchSize = 2000

// namedFilter selects documents with a usable name, optionally inside a bbox.
// The bbox test is on the stored bbox array rather than the geometry, so it
// needs no geo index and can ride the name index scan.
func namedFilter(bbox *[4]float64) bson.M {
	f := bson.M{"name": bson.M{"$exists": true, "$gt": ""}}
	if bbox != nil {
		f["bbox.0"] = bson.M{"$lte": bbox[2]}
		f["bbox.2"] = bson.M{"$gte": bbox[0]}
		f["bbox.1"] = bson.M{"$lte": bbox[3]}
		f["bbox.3"] = bson.M{"$gte": bbox[1]}
	}
	return f
}

// SourceDoc is the shape both the chart and OSM collections share for our
// purposes. OSM carries its type in tags; the chart carries it in objectClass.
type SourceDoc struct {
	Name        string            `bson:"name"`
	ObjectClass string            `bson:"objectClass"`
	Class       string            `bson:"class"`
	Cell        string            `bson:"cell"`
	BBox        []float64         `bson:"bbox"`
	Tags        map[string]string `bson:"tags"`
}

// BuildFrom reads one source collection into the gazetteer. source is
// SourceChart or SourceOSM; kindOf turns a document into its class string.
func BuildFrom(ctx context.Context, dst, src *mongo.Collection, source string, kindOf func(SourceDoc) string, opts BuildOptions) (BuildStats, error) {
	began := time.Now()
	var stats BuildStats
	if dst == nil || src == nil {
		return stats, nil
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}

	// Hinted to the name index so this is a scan of named features rather than
	// of the whole collection; the projection keeps geometry off the wire,
	// which for a named coastline is the entire document.
	find := options.Find().
		SetProjection(bson.M{"name": 1, "objectClass": 1, "class": 1, "cell": 1, "bbox": 1, "tags": 1}).
		SetHint("name_search").
		SetBatchSize(1000).
		SetNoCursorTimeout(true)
	cur, err := src.Find(ctx, namedFilter(opts.BBox), find)
	if err != nil {
		return stats, fmt.Errorf("places: read %s: %w", src.Name(), err)
	}
	defer cur.Close(ctx)

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

	lastReport := time.Now()
	for cur.Next(ctx) {
		var d SourceDoc
		if err := cur.Decode(&d); err != nil {
			continue // one unreadable document shouldn't end the pass
		}
		stats.Read++
		if d.Name == "" || len(d.BBox) != 4 {
			continue
		}
		lat := (d.BBox[1] + d.BBox[3]) / 2
		lng := (d.BBox[0] + d.BBox[2]) / 2
		class := kindOf(d)
		batch = append(batch, Place{
			ID:     ID(source, class, d.Name, lat, lng),
			Name:   d.Name,
			Source: source,
			Class:  class,
			Lat:    lat,
			Lng:    lng,
			BBox:   [4]float64{d.BBox[0], d.BBox[1], d.BBox[2], d.BBox[3]},
			Cell:   d.Cell,
		})
		if len(batch) >= opts.BatchSize {
			if err := flush(); err != nil {
				return stats, err
			}
		}
		if opts.Progress != nil && time.Since(lastReport) > 5*time.Second {
			opts.Progress(src.Name(), stats.Read, stats.Written)
			lastReport = time.Now()
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if err := cur.Err(); err != nil {
		return stats, fmt.Errorf("places: read %s: %w", src.Name(), err)
	}
	stats.Elapsed = time.Since(began)
	return stats, nil
}

// ChartKind is the class of a chart place: its S-57 object class.
func ChartKind(d SourceDoc) string { return d.ObjectClass }

// OSMKindFunc builds a kindOf for OSM documents from a tag-priority function,
// falling back to the ingest-time class when no interesting tag is present.
func OSMKindFunc(kindFromTags func(map[string]string) string) func(SourceDoc) string {
	return func(d SourceDoc) string {
		if k := kindFromTags(d.Tags); k != "" {
			return k
		}
		return d.Class
	}
}
