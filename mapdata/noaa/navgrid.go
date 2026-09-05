package noaa

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Precomputed navigability tiles.
//
// Routing reads "how deep is it here, and can I go there" over a wide area.
// Answering that from polygons means fetching tens of megabytes of coastline
// and depth areas and rasterising them on every request — measured at 31k
// documents and 58.6 MB for one 147x80 km corridor, to produce a grid that is
// half a megabyte. This collection stores the grid instead.
//
// A tile is boat-independent on purpose: it holds charted depth and hard
// physical facts (land, an obstruction with no charted depth over it), never a
// decision that depends on the boat's draft. The router applies its own safe
// depth, clearances and penalties to what it reads, so one set of tiles serves
// every boat.

// CollNavGrid holds the precomputed navigability tiles.
const CollNavGrid = "noaa_navgrid"

// NavTileSize is the cell count along each edge of a tile. 256 keeps a tile's
// payload small (a few hundred KB uncompressed, far less on the wire) while
// covering enough ground that a corridor needs a handful, not hundreds.
const NavTileSize = 256

// NavGridVersion is bumped whenever the meaning of a stored tile changes, so
// stale tiles are rebuilt rather than silently trusted. Part of the document
// id, so old and new coexist and a rollback still finds its own tiles.
const NavGridVersion = 1

// NavDepthUncharted is the depth sentinel for a cell no depth area covers.
// Distinct from "0 m", which on a chart means a drying area.
const NavDepthUncharted = math.MinInt16

// Cell flags. These are facts about the world, not judgements about a boat.
const (
	NavFlagLand        uint8 = 1 << iota // charted land or shoreline construction
	NavFlagObstruction                   // wreck/rock/pile with no charted depth over it
	NavFlagDredged                       // maintained channel
	NavFlagUnsurveyed                    // UNSARE
	NavFlagRestricted                    // RESARE — the router decides what to do about it
)

// NavTile is one precomputed tile: a NavTileSize x NavTileSize grid over a
// slippy-map tile, in row-major order from the tile's north-west corner.
type NavTile struct {
	Z, X, Y int
	// Depth is decimetres, so 0.1 m resolution over a +/-3276 m range — finer
	// than any charted sounding and far cheaper than float64.
	Depth []int16
	Flags []uint8
	Built time.Time
}

// navTileDoc is the stored form. The two arrays are zlib-compressed: a tile is
// mostly runs of the same value (open water, solid land), so they shrink by
// one to two orders of magnitude.
type navTileDoc struct {
	ID    string    `bson:"_id"`
	Z     int       `bson:"z"`
	X     int       `bson:"x"`
	Y     int       `bson:"y"`
	Size  int       `bson:"size"`
	Depth []byte    `bson:"depth"`
	Flags []byte    `bson:"flags"`
	Built time.Time `bson:"built"`
}

// NavTileID is the document id for a tile at the current version.
func NavTileID(z, x, y int) string {
	return fmt.Sprintf("v%d/%d/%d/%d", NavGridVersion, z, x, y)
}

// OpenNavGridCollection returns the navigability tile collection.
func OpenNavGridCollection(db *mongo.Database) *mongo.Collection {
	if db == nil {
		return nil
	}
	return db.Collection(CollNavGrid)
}

// EnsureNavGridIndexes creates the lookup index. Tiles are fetched by id in
// bulk, so _id is nearly enough; the (z,x,y) index serves range prewarming and
// coverage reporting.
func EnsureNavGridIndexes(ctx context.Context, coll *mongo.Collection) error {
	if coll == nil {
		return fmt.Errorf("noaa: nil navgrid collection")
	}
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "z", Value: 1}, {Key: "x", Value: 1}, {Key: "y", Value: 1}},
		Options: options.Index().SetName("zxy"),
	})
	if err != nil {
		return fmt.Errorf("noaa: create navgrid index: %w", err)
	}
	return nil
}

// GetNavTiles fetches the requested tiles, returning only those already built.
// Callers build and store what's missing.
func GetNavTiles(ctx context.Context, coll *mongo.Collection, keys [][3]int) (map[[3]int]*NavTile, error) {
	if coll == nil || len(keys) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, NavTileID(k[0], k[1], k[2]))
	}
	cur, err := coll.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, fmt.Errorf("noaa: navgrid find: %w", err)
	}
	defer cur.Close(ctx)

	out := make(map[[3]int]*NavTile, len(keys))
	for cur.Next(ctx) {
		var d navTileDoc
		if err := cur.Decode(&d); err != nil {
			continue // a corrupt tile is rebuilt, not fatal
		}
		tile, err := decodeNavTile(d)
		if err != nil {
			continue
		}
		out[[3]int{d.Z, d.X, d.Y}] = tile
	}
	return out, cur.Err()
}

// PutNavTiles stores tiles, replacing any existing ones. Unordered so one bad
// document doesn't abort the batch.
func PutNavTiles(ctx context.Context, coll *mongo.Collection, tiles []*NavTile) error {
	if coll == nil || len(tiles) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(tiles))
	for _, t := range tiles {
		depth, err := compressInt16(t.Depth)
		if err != nil {
			return err
		}
		flags, err := compressBytes(t.Flags)
		if err != nil {
			return err
		}
		doc := navTileDoc{
			ID: NavTileID(t.Z, t.X, t.Y), Z: t.Z, X: t.X, Y: t.Y,
			Size: NavTileSize, Depth: depth, Flags: flags, Built: time.Now().UTC(),
		}
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"_id": doc.ID}).SetReplacement(doc).SetUpsert(true))
	}
	_, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return fmt.Errorf("noaa: navgrid write: %w", err)
	}
	return nil
}

func decodeNavTile(d navTileDoc) (*NavTile, error) {
	if d.Size != NavTileSize {
		return nil, fmt.Errorf("noaa: navgrid tile %s has size %d, want %d", d.ID, d.Size, NavTileSize)
	}
	depth, err := decompressInt16(d.Depth, NavTileSize*NavTileSize)
	if err != nil {
		return nil, err
	}
	flags, err := decompressBytes(d.Flags, NavTileSize*NavTileSize)
	if err != nil {
		return nil, err
	}
	return &NavTile{Z: d.Z, X: d.X, Y: d.Y, Depth: depth, Flags: flags, Built: d.Built}, nil
}

func compressInt16(v []int16) ([]byte, error) {
	raw := make([]byte, len(v)*2)
	for i, n := range v {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(n))
	}
	return compressBytes(raw)
}

func compressBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("noaa: navgrid compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("noaa: navgrid compress: %w", err)
	}
	return buf.Bytes(), nil
}

func decompressBytes(b []byte, want int) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("noaa: navgrid decompress: %w", err)
	}
	defer zr.Close()
	// Bounded read: a corrupt or hostile document must not be able to make us
	// allocate without limit.
	out, err := io.ReadAll(io.LimitReader(zr, int64(want)+1))
	if err != nil {
		return nil, fmt.Errorf("noaa: navgrid decompress: %w", err)
	}
	if len(out) != want {
		return nil, fmt.Errorf("noaa: navgrid tile is %d bytes, want %d", len(out), want)
	}
	return out, nil
}

func decompressInt16(b []byte, want int) ([]int16, error) {
	raw, err := decompressBytes(b, want*2)
	if err != nil {
		return nil, err
	}
	out := make([]int16, want)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}
