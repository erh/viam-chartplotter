package vc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	mongoopts "go.mongodb.org/mongo-driver/mongo/options"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// DataSync Viam resource: keeps the `noaa` MongoDB collection current for a
// configured lon/lat box by periodically syncing NOAA ENC cells to disk and
// parsing them into Mongo (the same pipeline as `mapsync noaa-ingest`). NOAA
// publishes new cell editions weekly; this runs on an interval so a deployed
// fleet's chart data stays fresh without anyone running the CLI.
//
// OSM data changes slowly and its state-sized extracts are big one-off batches,
// so OSM ingest stays a manual `make ingest-osm-*` step rather than living here.
//
// Model: erh:viam-chartplotter:datasync.
var DataSyncModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("datasync")

func init() {
	resource.RegisterComponent(
		generic.API,
		DataSyncModel,
		resource.Registration[resource.Resource, *DataSyncConfig]{
			Constructor: newDataSync,
		})
}

// DataSyncConfig configures one datasync instance.
type DataSyncConfig struct {
	MongoURI string `json:"mongo_uri"`
	MongoDB  string `json:"mongo_db,omitempty"` // default "osm"
	ENCDir   string `json:"enc_dir,omitempty"`  // cell download dir; default OS cache

	// Bounding box of ENC coverage to keep populated.
	MinLon float64 `json:"min_lon"`
	MinLat float64 `json:"min_lat"`
	MaxLon float64 `json:"max_lon"`
	MaxLat float64 `json:"max_lat"`

	MinScale int `json:"min_scale,omitempty"` // 0 = no bound
	MaxScale int `json:"max_scale,omitempty"` // 0 = no bound
	Parallel int `json:"parallel,omitempty"`  // concurrent cell downloads (default 4)

	// IntervalHours between sync passes (default 24). Re-runs are cheap — cells
	// whose edition+update already match in Mongo are skipped.
	IntervalHours int `json:"interval_hours,omitempty"`
}

// Validate enforces a usable Mongo URI and a non-degenerate bbox.
func (c *DataSyncConfig) Validate(path string) ([]string, error) {
	if c.MongoURI == "" {
		return nil, fmt.Errorf("%s: mongo_uri required", path)
	}
	if c.MaxLon <= c.MinLon || c.MaxLat <= c.MinLat {
		return nil, fmt.Errorf("%s: a valid bbox is required (min_lon<max_lon, min_lat<max_lat)", path)
	}
	return nil, nil
}

func (c *DataSyncConfig) db() string {
	if c.MongoDB != "" {
		return c.MongoDB
	}
	return "osm"
}

func (c *DataSyncConfig) interval() time.Duration {
	if c.IntervalHours > 0 {
		return time.Duration(c.IntervalHours) * time.Hour
	}
	return 24 * time.Hour
}

func (c *DataSyncConfig) encDir() string {
	if c.ENCDir != "" {
		return c.ENCDir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "viam-chartplotter", "noaa-enc")
}

type dataSync struct {
	resource.AlwaysRebuild
	resource.Named

	logger logging.Logger
	cfg    *DataSyncConfig

	client *mongo.Client
	coll   *mongo.Collection
	store  *noaa.Store

	mu       sync.Mutex
	cancel   context.CancelFunc
	lastRun  time.Time
	lastStat noaa.IngestStats
	lastErr  string
}

func newDataSync(ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*DataSyncConfig](conf)
	if err != nil {
		return nil, err
	}

	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(connectCtx, mongoopts.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		return nil, fmt.Errorf("datasync: mongo connect: %w", err)
	}
	if err := client.Ping(connectCtx, nil); err != nil {
		return nil, fmt.Errorf("datasync: mongo ping: %w", err)
	}
	coll := noaa.OpenCollection(client.Database(cfg.db()))
	if err := noaa.EnsureIndexes(ctx, coll); err != nil {
		return nil, fmt.Errorf("datasync: ensure indexes: %w", err)
	}

	catalog, err := noaa.NewCatalog(cfg.encDir(), logger.Sublogger("catalog"))
	if err != nil {
		return nil, err
	}
	store, err := noaa.NewStore(cfg.encDir(), catalog, logger.Sublogger("store"))
	if err != nil {
		return nil, err
	}

	d := &dataSync{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
		cfg:    cfg,
		client: client,
		coll:   coll,
		store:  store,
	}
	loopCtx, loopCancel := context.WithCancel(context.Background())
	d.cancel = loopCancel
	go d.runLoop(loopCtx)
	return d, nil
}

func (d *dataSync) runLoop(ctx context.Context) {
	// First pass immediately so a freshly-configured instance populates Mongo
	// without waiting a full interval.
	wake := time.NewTimer(0)
	defer wake.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake.C:
		}
		d.runOnce(ctx)
		wake.Reset(d.cfg.interval())
	}
}

func (d *dataSync) runOnce(ctx context.Context) {
	d.logger.Infof("datasync: syncing NOAA ENC for bbox=[%.4f,%.4f,%.4f,%.4f]",
		d.cfg.MinLon, d.cfg.MinLat, d.cfg.MaxLon, d.cfg.MaxLat)
	stats, err := noaa.IngestBBox(ctx, d.coll, d.store,
		d.cfg.MinLon, d.cfg.MinLat, d.cfg.MaxLon, d.cfg.MaxLat,
		d.cfg.MinScale, d.cfg.MaxScale, d.cfg.Parallel,
		func(format string, a ...any) { d.logger.Infof(format, a...) })
	d.mu.Lock()
	d.lastRun = time.Now()
	d.lastStat = stats
	if err != nil {
		d.lastErr = err.Error()
	} else {
		d.lastErr = ""
	}
	d.mu.Unlock()
	if err != nil {
		d.logger.Warnf("datasync: %v", err)
		return
	}
	d.logger.Infof("datasync: done — %d cells, %d features, %d skipped, %d write-errors",
		stats.Cells, stats.Docs, stats.CellsSkipped, stats.WriteErrors)
}

func (d *dataSync) Close(_ context.Context) error {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.mu.Unlock()
	if d.client != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = d.client.Disconnect(dctx)
	}
	return nil
}

// DoCommand: {"command":"status"} reports the last run; {"command":"sync_now"}
// triggers an immediate sync in the background.
func (d *dataSync) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	switch op, _ := cmd["command"].(string); op {
	case "", "status":
		d.mu.Lock()
		defer d.mu.Unlock()
		return map[string]interface{}{
			"lastRun":      d.lastRun.Format(time.RFC3339),
			"cells":        d.lastStat.Cells,
			"features":     d.lastStat.Docs,
			"cellsSkipped": d.lastStat.CellsSkipped,
			"writeErrors":  d.lastStat.WriteErrors,
			"lastError":    d.lastErr,
		}, nil
	case "sync_now":
		go d.runOnce(context.Background())
		return map[string]interface{}{"triggered": true}, nil
	default:
		return nil, fmt.Errorf("unknown command %q", op)
	}
}
