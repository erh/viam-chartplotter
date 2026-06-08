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

	"github.com/erh/viam-chartplotter/mapdata/weather"
)

// WeatherSync Viam resource: decodes weather forecasts (GRIB → ol-wind JSON /
// isobar GeoJSON via the existing WeatherCache pipeline) and writes them to the
// `weather` MongoDB collection on an interval. A render/tile server can then
// serve weather from Mongo instead of every instance re-fetching GRIB.
//
// Model: erh:viam-chartplotter:weathersync.
var WeatherSyncModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("weathersync")

func init() {
	resource.RegisterComponent(
		generic.API,
		WeatherSyncModel,
		resource.Registration[resource.Resource, *WeatherSyncConfig]{
			Constructor: newWeatherSync,
		})
}

// WeatherSyncConfig configures one weathersync instance.
type WeatherSyncConfig struct {
	MongoURI string `json:"mongo_uri"`
	MongoDB  string `json:"mongo_db,omitempty"`  // default "osm"
	CacheDir string `json:"cache_dir,omitempty"` // raw/decoded weather cache; default OS cache

	// Models restricts which model names to sync; empty = every enabled,
	// fetchable model in the registry.
	Models []string `json:"models,omitempty"`

	// MaxFH caps the forecast hour synced per model (0 = the model's own MaxFh).
	MaxFH int `json:"max_fh,omitempty"`

	// IntervalHours between sync passes (default 6).
	IntervalHours int `json:"interval_hours,omitempty"`
}

func (c *WeatherSyncConfig) Validate(path string) ([]string, error) {
	if c.MongoURI == "" {
		return nil, fmt.Errorf("%s: mongo_uri required", path)
	}
	return nil, nil
}

func (c *WeatherSyncConfig) db() string {
	if c.MongoDB != "" {
		return c.MongoDB
	}
	return "osm"
}

func (c *WeatherSyncConfig) interval() time.Duration {
	if c.IntervalHours > 0 {
		return time.Duration(c.IntervalHours) * time.Hour
	}
	return 6 * time.Hour
}

type weatherSync struct {
	resource.AlwaysRebuild
	resource.Named

	logger logging.Logger
	cfg    *WeatherSyncConfig

	client *mongo.Client
	coll   *mongo.Collection
	cache  *WeatherCache

	mu      sync.Mutex
	cancel  context.CancelFunc
	lastRun time.Time
	lastN   int
	lastErr string
}

func newWeatherSync(ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*WeatherSyncConfig](conf)
	if err != nil {
		return nil, err
	}

	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		base, derr := os.UserCacheDir()
		if derr != nil {
			base = os.TempDir()
		}
		cacheDir = filepath.Join(base, "viam-chartplotter", "noaa-weather")
	}
	cache, err := NewWeatherCache(cacheDir, logger.Sublogger("weather"))
	if err != nil {
		return nil, fmt.Errorf("weathersync: weather cache: %w", err)
	}

	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(connectCtx, mongoopts.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		return nil, fmt.Errorf("weathersync: mongo connect: %w", err)
	}
	if err := client.Ping(connectCtx, nil); err != nil {
		return nil, fmt.Errorf("weathersync: mongo ping: %w", err)
	}
	coll := weather.OpenCollection(client.Database(cfg.db()))
	if err := weather.EnsureIndexes(ctx, coll); err != nil {
		return nil, fmt.Errorf("weathersync: ensure indexes: %w", err)
	}

	w := &weatherSync{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
		cfg:    cfg,
		client: client,
		coll:   coll,
		cache:  cache,
	}
	loopCtx, loopCancel := context.WithCancel(context.Background())
	w.cancel = loopCancel
	go w.runLoop(loopCtx)
	return w, nil
}

func (w *weatherSync) runLoop(ctx context.Context) {
	wake := time.NewTimer(0)
	defer wake.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake.C:
		}
		w.runOnce(ctx)
		wake.Reset(w.cfg.interval())
	}
}

func (w *weatherSync) runOnce(ctx context.Context) {
	total, firstErr := SyncWeatherOnce(ctx, w.cache, w.coll, w.cfg.Models, w.cfg.MaxFH, w.logger)
	w.mu.Lock()
	w.lastRun = time.Now().UTC()
	w.lastN = total
	if firstErr != nil {
		w.lastErr = firstErr.Error()
	} else {
		w.lastErr = ""
	}
	w.mu.Unlock()
	w.logger.Infof("weathersync: stored %d forecast slices to Mongo", total)
}

// SyncWeatherOnce decodes every enabled, fetchable weather model's forecast
// hours through the cache and upserts the served JSON into the weather
// collection. modelsFilter restricts to those model names (nil/empty = all);
// maxFH caps the forecast hour per model (0 = each model's own MaxFh). Returns
// the number of slices stored and the first error encountered. Exported so both
// the weathersync Viam model and the cmd/weathersync binary share one path.
func SyncWeatherOnce(ctx context.Context, cache *WeatherCache, coll *mongo.Collection, modelsFilter []string, maxFH int, logger logging.Logger) (int, error) {
	want := func(name string) bool {
		if len(modelsFilter) == 0 {
			return true
		}
		for _, m := range modelsFilter {
			if m == name {
				return true
			}
		}
		return false
	}
	now := time.Now().UTC()
	total := 0
	var firstErr error
	for _, m := range listModels() {
		if m.Disabled || (m.Fetch == nil && m.FetchBytes == nil) || !want(m.Name) {
			continue // WMS-only / not-yet-decodable / filtered-out models
		}
		cycle := mostRecentCycle(now.Add(-time.Duration(m.PublishLagH)*time.Hour), m.CycleHours).Format("20060102T15")
		step := m.StepFh
		if step < 1 {
			step = 1
		}
		maxFh := m.MaxFh
		if maxFH > 0 && maxFH < maxFh {
			maxFh = maxFH
		}
		stored := 0
		for fh := m.MinFh; fh <= maxFh; fh += step {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			// refreshNow decodes (or hits the disk cache) and writes the served
			// JSON + a .gz sibling. We store the GZIPPED bytes in Mongo: the raw
			// global JSON (e.g. GFS ~35 MB) exceeds Mongo's 16 MB document cap,
			// but the gzip (~4 MB) fits and is served with Content-Encoding gzip.
			if err := cache.refreshNow(ctx, m, fh); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			body, gz, err := readWeatherPayload(cache, m.Name, fh)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := weather.Upsert(ctx, coll, weather.Grid{
				Model: m.Name, FH: fh, Kind: m.Kind, Cycle: cycle, UpdatedAt: now.Unix(), Gzip: gz, Payload: body,
			}); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			stored++
			total++
		}
		if stored > 0 && logger != nil {
			logger.Infof("weathersync: %s cycle=%s — %d hours stored", m.Name, cycle, stored)
		}
	}
	return total, firstErr
}

// readWeatherPayload returns the bytes to store for (model, fh): the gzipped
// sibling (gz=true) when present — needed so global models fit Mongo's 16 MB
// doc cap — else the raw JSON (gz=false).
func readWeatherPayload(cache *WeatherCache, model string, fh int) ([]byte, bool, error) {
	if b, err := os.ReadFile(cache.cacheGzPath(model, fh)); err == nil {
		return b, true, nil
	}
	b, err := os.ReadFile(cache.cachePath(model, fh))
	return b, false, err
}

func (w *weatherSync) Close(_ context.Context) error {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	w.mu.Unlock()
	if w.client != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = w.client.Disconnect(dctx)
	}
	return nil
}

func (w *weatherSync) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	switch op, _ := cmd["command"].(string); op {
	case "", "status":
		w.mu.Lock()
		defer w.mu.Unlock()
		return map[string]interface{}{
			"lastRun":   w.lastRun.Format(time.RFC3339),
			"stored":    w.lastN,
			"lastError": w.lastErr,
		}, nil
	case "sync_now":
		go w.runOnce(context.Background())
		return map[string]interface{}{"triggered": true}, nil
	default:
		return nil, fmt.Errorf("unknown command %q", op)
	}
}
