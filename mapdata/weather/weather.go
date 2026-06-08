// Package weather is the MongoDB store for decoded weather forecast data
// (wind / wave ol-wind JSON, isobar GeoJSON, …). The populate side
// (weathersync) decodes GRIB and Upserts the already-serialised payload here;
// the serve side reads it back with Get, so the payload is opaque to this
// package and the frontend's data shape is unchanged.
package weather

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CollWeather is the single collection holding decoded forecast payloads. It
// lives in the same database as the osm_* and noaa collections.
const CollWeather = "weather"

// Grid is one decoded forecast slice: a model + forecast hour, with the exact
// bytes the frontend consumes (ol-wind records JSON, isobar GeoJSON, …) stored
// opaquely so the read path needs no GRIB code.
type Grid struct {
	ID        string `bson:"_id"`       // "<model>:f<fh>"
	Model     string `bson:"model"`     // e.g. "gfs", "ecmwf"
	FH        int    `bson:"fh"`        // forecast hour
	Kind      string `bson:"kind"`      // "wind" | "wave" | "isobars" | ...
	Cycle     string `bson:"cycle"`     // model cycle, e.g. "20260607T12"
	UpdatedAt int64  `bson:"updatedAt"` // unix seconds (caller-stamped)
	Gzip      bool   `bson:"gzip"`      // true if Payload is gzip-compressed
	Payload   []byte `bson:"payload"`   // serialised JSON/GeoJSON (gzip per Gzip)
}

// ID builds the document _id for a (model, fh).
func ID(model string, fh int) string { return fmt.Sprintf("%s:f%03d", model, fh) }

// OpenCollection returns the weather collection from a database handle.
func OpenCollection(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(CollWeather)
}

// EnsureIndexes creates the lookup index on (model, fh).
func EnsureIndexes(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("weather: nil collection")
	}
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "model", Value: 1}, {Key: "fh", Value: 1}},
		Options: options.Index().SetName("model_fh"),
	})
	if err != nil {
		return fmt.Errorf("weather: create index: %w", err)
	}
	return nil
}

// Upsert writes (or replaces) one forecast slice.
func Upsert(ctx context.Context, coll *mongo.Collection, g Grid) error {
	if coll == nil {
		return fmt.Errorf("weather: nil collection")
	}
	g.ID = ID(g.Model, g.FH)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": g.ID},
		bson.M{"$set": g},
		options.Update().SetUpsert(true))
	return err
}

// Get returns the stored payload for (model, fh), ok=false if absent.
func Get(ctx context.Context, coll *mongo.Collection, model string, fh int) (Grid, bool, error) {
	if coll == nil {
		return Grid{}, false, fmt.Errorf("weather: nil collection")
	}
	var g Grid
	err := coll.FindOne(ctx, bson.M{"_id": ID(model, fh)}).Decode(&g)
	if err == mongo.ErrNoDocuments {
		return Grid{}, false, nil
	}
	if err != nil {
		return Grid{}, false, err
	}
	return g, true, nil
}
