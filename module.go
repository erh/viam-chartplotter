package vc

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/erh/viam-chartplotter/mapdata/noaa"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/erh/vmodutils"

	"go.mongodb.org/mongo-driver/mongo"
	mongoopts "go.mongodb.org/mongo-driver/mongo/options"

	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
	"github.com/erh/viam-chartplotter/mapdata/weather"
	"github.com/erh/viam-chartplotter/render"
)

//go:embed dist
var staticFS embed.FS

func DistFS() (fs.FS, error) {
	return fs.Sub(staticFS, "dist")
}

var Model = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("chartplotter")

func init() {
	resource.RegisterComponent(
		generic.API,
		Model,
		resource.Registration[resource.Resource, resource.NoNativeConfig]{
			Constructor: newServer,
		})
}

func newServer(ctx context.Context, deps resource.Dependencies, config resource.Config, logger logging.Logger) (resource.Resource, error) {
	dist, err := DistFS()
	if err != nil {
		return nil, err
	}
	port := config.Attributes.Int("port", 8888)
	cacheDir := config.Attributes.String("noaa_cache_dir")
	cacheMaxBytes := int64(config.Attributes.Int("noaa_cache_max_bytes", 0))
	// "draft" (feet) drives the depth-shading bands. DEPMS covers
	// 3.3 ft → draft, DEPMD covers draft → 2×draft, DEPDW (safe water,
	// white) is ≥ 2×draft. Fall back to legacy "safe_depth_ft" name so
	// older configs keep working.
	draftFt := config.Attributes.Float64("draft", config.Attributes.Float64("safe_depth_ft", 6))
	myBoatIcon := config.Attributes.String("myboat_icon_path")
	// Public base URL of the wind-publisher's R2/CDN bucket. Empty
	// (or unset) falls back to DefaultWindCDNBaseURL inside
	// SetWindCDNBaseURL so every chartplotter gets fan-out behaviour
	// out of the box. Override with a different URL to point at a
	// staging mirror.
	windCDNBaseURL := config.Attributes.String("wind_cdn_base_url")
	// Mongo connection for the OSM tile underlay. Config attribute
	// wins, env var (MONGO_URI) is the dev-friendly fallback so you
	// don't have to wire it into config every time. Same idea for
	// db/coll names; both default to "osm"/"features" when neither
	// config nor env is set.
	mongoURI := firstNonEmpty(config.Attributes.String("mongo_uri"), os.Getenv("MONGO_URI"))
	mongoDB := firstNonEmpty(config.Attributes.String("mongo_db"), os.Getenv("MONGO_DB"), "osm")
	mongoColl := firstNonEmpty(config.Attributes.String("mongo_coll"), os.Getenv("MONGO_COLL"), "features")
	// Base URL of the map+weather (tile) server the frontend should fetch
	// tiles/weather from. Set via the tile_server_base_url config attribute.
	// When unset but Mongo is configured we assume a render/data server is
	// reachable on the conventional port and default to DefaultTileServerURL.
	// Empty means same-origin (this server serves its own tiles). Exposed to
	// the frontend via /app-config.
	tileServerBaseURL := config.Attributes.String("tile_server_base_url")
	if tileServerBaseURL == "" && mongoURI != "" {
		tileServerBaseURL = DefaultTileServerURL
	}
	return StartChartplotterServer(config.ResourceName(), dist, logger, port, cacheDir, cacheMaxBytes, draftFt, myBoatIcon, windCDNBaseURL, mongoURI, mongoDB, mongoColl, tileServerBaseURL)
}

// DefaultTileServerURL is the map+weather server address the frontend falls
// back to when Mongo is configured but no explicit tile_server_base_url /
// TILE_SERVER_URL is given. It points at the conventional render server port on
// the same host; override per deployment.
const DefaultTileServerURL = "http://localhost:8989"

// firstNonEmpty returns the first non-empty string in vals, or "".
// Used for the layered "config → env → default" knob resolution.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveCacheRoot picks the parent directory under which both the WMS proxy cache
// (noaa-wms/) and the ENC store (noaa-enc/) live. An explicit path wins; otherwise
// we use the OS user cache dir, falling back to the temp dir if HOME is unset.
func resolveCacheRoot(configured string) string {
	if configured != "" {
		return configured
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "viam-chartplotter")
}

// withCORS adds permissive CORS headers so a chartplotter app served from one
// origin can fetch tiles/weather from a separate map+weather server. The data
// is public, read-only GETs; OPTIONS preflights are answered 204 immediately.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withCookiePathRoot wraps an http.Handler so any Set-Cookie headers
// it writes that don't already specify a Path get `Path=/` appended.
// Required because vmodutils's cookie middleware doesn't set Path
// and Go's default-Path-from-request-URL behaviour fans out the same
// cookie into a copy per tile path.
func withCookiePathRoot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&cookiePathRootWriter{ResponseWriter: w}, r)
	})
}

type cookiePathRootWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *cookiePathRootWriter) fixCookies() {
	cookies := w.Header().Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	w.Header().Del("Set-Cookie")
	for _, c := range cookies {
		if !strings.Contains(strings.ToLower(c), "path=") {
			c = c + "; Path=/"
		}
		w.Header().Add("Set-Cookie", c)
	}
}

func (w *cookiePathRootWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.fixCookies()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cookiePathRootWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// StartChartplotterServer wires the static frontend, the NOAA WMS caching proxy, and
// the ENC catalog/store handlers, and starts an HTTP server on the given port.
// draftFt is the boat's draft in feet — drives the depth-shading bands at
// chart-detail zoom (DEPMS up to draft, DEPMD up to 2×draft, DEPDW above).
// The per-request `?sd=` query param overrides it.
func StartChartplotterServer(
	name resource.Name,
	dist fs.FS,
	logger logging.Logger,
	port int,
	cacheRoot string,
	cacheMaxBytes int64,
	draftFt float64,
	myBoatIconPath string,
	windCDNBaseURL string,
	mongoURI string,
	mongoDB string,
	mongoColl string,
	tileServerBaseURL string,
) (resource.Resource, error) {
	// Stand up tracing before anything else so even the early-init
	// errors get captured. Shutdown is wired through chartplotterResource
	// so spans buffered in the BatchSpanProcessor flush on module unload.
	tracerShutdown, err := initTracer(logger.Sublogger("tracing"))
	if err != nil {
		logger.Warnf("tracing init failed: %v — continuing without spans", err)
		tracerShutdown = func(context.Context) error { return nil }
	}

	mux, server, err := vmodutils.PrepInModuleServer(dist, logger.Sublogger("accessLog"))
	if err != nil {
		_ = tracerShutdown(context.Background())
		return nil, err
	}
	// vmodutils.PrepInModuleServer installs a cookie middleware that
	// calls http.SetCookie(w, &http.Cookie{Name, Value}) without
	// setting Path — Go then fills in the request URL's directory as
	// the default Path. That means every tile URL gets its own copy of
	// `api-key` / `api-key-id` / `host` cookies, fanning out into
	// hundreds of duplicates per session. Wrap the server handler so
	// any outgoing Set-Cookie gets a global Path=/ if it doesn't
	// already specify one.
	server.Handler = withCookiePathRoot(server.Handler)
	// Tracing + slow-request logging wraps the outermost handler so the
	// span / timing covers cookie middleware too. otelhttp creates a
	// span per request; the slow-log middleware emits a WARN line for
	// anything over CHARTPLOTTER_SLOW_LOG_MS (default 500 ms).
	server.Handler = withTracing(logger.Sublogger("slowReq"), server.Handler)
	// Permissive CORS on the read-only tile/weather/data API so a chartplotter
	// app on one origin (e.g. :8888) can fetch tiles and weather from a
	// separate map+weather server on another (e.g. :8989). Tiles/weather are
	// public GETs, so a blanket allow-origin is fine; preflights are answered
	// 204. Outermost wrapper so OPTIONS short-circuits before anything else.
	server.Handler = withCORS(server.Handler)

	root := resolveCacheRoot(cacheRoot)

	wmsCache, err := NewNoaaCache(filepath.Join(root, "noaa-wms"), cacheMaxBytes, logger.Sublogger("noaaCache"))
	if err != nil {
		return nil, err
	}
	wmsCache.Register(mux)
	logger.Infof("noaa wms cache: %s (max %d bytes, stale after %s)",
		wmsCache.cacheDir, wmsCache.maxBytes, wmsCache.staleAfter)

	encDir := filepath.Join(root, "noaa-enc")
	catalog, err := noaa.NewCatalog(encDir, logger.Sublogger("encCatalog"))
	if err != nil {
		return nil, err
	}
	encStore, err := noaa.NewStore(encDir, catalog, logger.Sublogger("encStore"))
	if err != nil {
		return nil, err
	}
	encRenderer := render.NewENCRenderer(logger.Sublogger("encRender"))
	encTileCache, err := render.NewENCTileCache(filepath.Join(encStore.RootDir(), "tiles"))
	if err != nil {
		return nil, err
	}
	// The /compare debug endpoint fetches a NOAA WMS tile via the WMS cache.
	// render/ doesn't import the WMS cache (would be an import cycle), so we
	// inject the fetch: build the canonical query here and hit the cache.
	wmsFetch := func(ctx context.Context, z, x, y int, layers string) ([]byte, error) {
		canonical := wmsCanonicalForTile(tileXYZ{x: x, y: y, z: z}, layers)
		b, _, _, err := wmsCache.fetch(ctx, canonical, "image/png")
		return b, err
	}
	encHandlers := render.NewENCHandlers(catalog, encStore, encRenderer, encTileCache, wmsFetch, draftFt)
	// OSM underlay layer — render by querying a MongoDB collection
	// populated offline by `osmtools ingest`. The URI is the only
	// required piece; if unset, the /noaa-enc/osm-tile/ endpoint
	// refuses to render (transparent fallback only) and the frontend
	// layer is effectively a no-op.
	// weatherColl is set when Mongo connects below, then wired into the weather
	// cache so it serves forecasts from Mongo (populated by weathersync) before
	// falling back to disk/upstream.
	var weatherColl *mongo.Collection
	if mongoURI != "" {
		mctx, mcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer mcancel()
		mclient, err := mongo.Connect(mctx, mongoopts.Client().ApplyURI(mongoURI))
		if err != nil {
			logger.Warnf("osm underlay disabled: mongo connect: %v", err)
		} else if err := mclient.Ping(mctx, nil); err != nil {
			logger.Warnf("osm underlay disabled: mongo ping: %v", err)
		} else {
			// mongoColl is no longer a single collection — features are
			// partitioned across per-minZoom-bucket collections (see
			// osmtiler.OpenOSMCollections). The configured "collection
			// name" is kept in StartChartplotterServer's signature for
			// back-compat with existing module configs, but ignored here;
			// the bucket collections have fixed names within the chosen
			// database.
			_ = mongoColl
			db := mclient.Database(mongoDB)
			colls := osmtiler.OpenOSMCollections(db)
			encRenderer.SetOSMCollections(colls)
			// ENC chart now renders from the noaa feature collection in the
			// same database (populated by `mapsync noaa-ingest`), instead of
			// parsing .000 files off disk at request time.
			encRenderer.SetNOAACollection(noaa.OpenCollection(db))
			weatherColl = weather.OpenCollection(db)
			logger.Infof("osm underlay: mongo db=%s buckets=%s/%s/%s; enc=%s; weather=%s",
				mongoDB, osmtiler.CollOverview, osmtiler.CollCoastal, osmtiler.CollDetail, noaa.CollNOAA, weather.CollWeather)
		}
	} else {
		logger.Info("osm underlay disabled (set mongo_uri config or MONGO_URI env to enable)")
	}
	encHandlers.Register(mux)
	logger.Infof("noaa enc store: %s (default draft=%.1f ft)", encDir, draftFt)

	// NOAA GFS weather cache. Serves /noaa-weather/gfs/latest.json which
	// the frontend wind layer (ol-wind) consumes. Disk cache lives under
	// <root>/noaa-weather/.
	weatherDir := filepath.Join(root, "noaa-weather")
	weatherCache, err := NewWeatherCache(weatherDir, logger.Sublogger("weather"))
	if err != nil {
		logger.Warnf("weather cache disabled: %v", err)
	} else {
		weatherCache.SetWindCDNBaseURL(windCDNBaseURL)
		if weatherColl != nil {
			weatherCache.SetWeatherCollection(weatherColl)
			logger.Infof("weather: serving from Mongo collection %q (disk/upstream fallback)", weather.CollWeather)
		}
		weatherCache.Register(mux)
		// Background prewarm of every model's forecast hours so the
		// first user scrub to any hour hits the disk cache instead of
		// blocking on a ~30-60 s NOMADS fetch. Uses its own context so
		// resource.Close can cancel it on module unload.
		weatherCache.Prewarm(context.Background())
		// Periodic cache cleaner: delete any file under
		// <root>/noaa-weather/ older than 60 days. Covers stale
		// per-version JSON (orphaned by weatherCacheVersion bumps),
		// raw-ecmwf/ raw-GRIB blobs that haven't been touched in
		// months, and any leftover .gz siblings. Runs once on
		// startup, then daily. ECMWF data is immutable per (cycle,
		// fh) so a delete-then-refetch is just one wasted upstream
		// pull on the next request — at 60 days that's essentially
		// never on an active install.
		weatherCache.StartCleaner(60*24*time.Hour, 24*time.Hour)
		logger.Infof("noaa weather cache: %s (cdn=%q)", weatherDir, windCDNBaseURL)
	}

	// Per-process instance ID. The frontend polls /version and reloads when it
	// changes, so the browser picks up a new build/restart without manual refresh.
	instanceID := newInstanceID()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"instance": instanceID})
	})

	// /app-config exposes runtime settings the frontend needs but can't know at
	// build time. tileServerBaseURL tells the app where to fetch map tiles and
	// weather: empty = same-origin (this server), otherwise a separate
	// map+weather server (e.g. http://host:8989). See DefaultTileServerURL.
	mux.HandleFunc("/app-config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"tileServerBaseURL": tileServerBaseURL})
	})
	if tileServerBaseURL != "" {
		logger.Infof("tile/weather server base URL: %s (frontend will fetch tiles+weather from here)", tileServerBaseURL)
	}

	// Optional override for the user's-own-boat marker icon. Resolved once at
	// startup; if the file is missing or unreadable we log and fall back to the
	// frontend's bundled default. AIS markers are unaffected.
	if myBoatIconPath != "" {
		abs, err := filepath.Abs(myBoatIconPath)
		if err != nil {
			logger.Warnf("myboat_icon_path %q: %v — falling back to default", myBoatIconPath, err)
		} else if info, err := os.Stat(abs); err != nil || info.IsDir() {
			logger.Warnf("myboat_icon_path %q not a readable file — falling back to default", abs)
		} else {
			mux.HandleFunc("/myboat-icon", func(w http.ResponseWriter, r *http.Request) {
				// Match the file's mtime in the ETag/If-Modified-Since flow that
				// http.ServeFile already implements, but no long-lived cache —
				// the user can swap the file and a reload picks it up.
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, abs)
			})
			logger.Infof("myboat icon: %s", abs)
		}
	}

	server.Addr = fmt.Sprintf(":%d", port)
	logger.Infof("going to listen on %v", server.Addr)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Errorf("error ListenAndServe: %v", err)
		}
	}()

	return &chartplotterResource{
		name:           name,
		server:         server,
		weatherCache:   weatherCache,
		tracerShutdown: tracerShutdown,
	}, nil
}

type chartplotterResource struct {
	resource.AlwaysRebuild

	name           resource.Name
	server         *http.Server
	weatherCache   *WeatherCache
	tracerShutdown func(context.Context) error
}

func (r *chartplotterResource) Name() resource.Name { return r.name }

func (r *chartplotterResource) Close(ctx context.Context) error {
	// Cancel the prewarm goroutine first so it doesn't keep hammering
	// NOMADS after the HTTP server is gone.
	if r.weatherCache != nil {
		r.weatherCache.Close()
	}
	err := r.server.Close()
	// Flush buffered spans last — the slow-log middleware emits one on
	// every request and the batch processor would otherwise drop the
	// in-flight batch when the process exits.
	if r.tracerShutdown != nil {
		_ = r.tracerShutdown(ctx)
	}
	return err
}

func (r *chartplotterResource) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	return nil, nil
}

func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails on a broken OS RNG; fall back to a fixed
		// string so the endpoint still responds (and reload-on-change still
		// works on the next successful start).
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}
