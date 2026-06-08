package weather

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.viam.com/rdk/logging"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/erh/viam-chartplotter/weather/store"
)

// WeatherCache spans both weather concerns. POPULATE side (used by weathersync):
// fetch GRIB/GRIB-like data from upstream, decode into ol-wind JSON / isobar
// GeoJSON, stage on disk (refreshNow) for upsert into Mongo. SERVE side (used by
// the chartplotter/tileserver HTTP mux): serveModel reads the decoded payload
// straight from the Mongo weather collection — Mongo-only, no disk read or
// upstream fetch in the request path.
type WeatherCache struct {
	cacheDir string
	client   *http.Client
	logger   logging.Logger

	// windCDNBaseURL, when non-empty, is the public base URL the
	// frontend should prefer for tile-published wind data (currently
	// ECMWF only). e.g. "https://chartwx.example.com". The chartplotter
	// frontend reads this via /noaa-weather/config and, if set, fetches
	// `<base>/wind/ecmwf/...` tile blobs instead of hammering this
	// module for global JSON. Empty means "no CDN; serve everything
	// locally" (single-instance dev mode, the default).
	windCDNBaseURL string

	// refresh is the soft TTL — past this, a request triggers a
	// background refresh (the cached copy is still served immediately).
	refresh time.Duration

	// fetchMu de-dupes concurrent refreshes; second caller just waits.
	fetchMu sync.Mutex

	// cleanerCancel stops the periodic disk-cache cleaner goroutine
	// (see StartCleaner). nil when no cleaner is running.
	cleanerCancel context.CancelFunc

	// mongoColl is the weather collection a weathersync service populates.
	// serveModel reads ONLY from it — a render/tile server serves forecasts
	// straight from Mongo. Nil means serving is unconfigured (503).
	mongoColl *mongo.Collection

	hits      atomic.Uint64
	refreshes atomic.Uint64
	errs      atomic.Uint64
}

// SetWeatherCollection wires the Mongo weather collection serveModel reads from.
// Required for serving — when nil, /noaa-weather/data returns 503 (serve is
// Mongo-only; a weathersync service must populate the collection).
func (wc *WeatherCache) SetWeatherCollection(c *mongo.Collection) { wc.mongoColl = c }

const (
	weatherStaleAfter = 90 * time.Minute
	// GFS 0.25° runs are typically available ~3.5 h after the cycle hour.
	gfsPublishLagHours = 4
	// Hard cap on forecast hour so a typo in `?fh=` can't make us fetch
	// the 16-day-out run forever (GFS publishes f000..f384 at 3 h steps).
	gfsMaxForecastHour = 240
	// NOMADS filter endpoint that lets us subset by variable + level
	// without downloading the full ~500 MB GRIB2.
	nomadsGFSURLTemplate = "https://nomads.ncep.noaa.gov/cgi-bin/filter_gfs_0p25.pl" +
		"?file=gfs.t%02dz.pgrb2.0p25.f%03d" +
		"&lev_10_m_above_ground=on&var_UGRD=on&var_VGRD=on" +
		"&dir=%%2Fgfs.%s%%2F%02d%%2Fatmos"
	// GFSWAVE 0.25° global, HTSGW (significant wave height, m) +
	// DIRPW (primary wave direction, degrees "from") at the surface.
	nomadsWaveURLTemplate = "https://nomads.ncep.noaa.gov/cgi-bin/filter_gfswave.pl" +
		"?file=gfswave.t%02dz.global.0p25.f%03d.grib2" +
		"&var_HTSGW=on&var_DIRPW=on" +
		"&dir=%%2Fgfs.%s%%2F%02d%%2Fwave%%2Fgridded"
)

// NewWeatherCache wires a WeatherCache pointing at cacheDir. The
// directory is created if it doesn't exist.
// cdnServedModels lists every model whose data the chartplotter
// fleet should fetch from the wind-publisher's R2 bucket rather than
// from upstream directly. Centralised so a future "add GFS to the
// publisher" change is a one-line edit here plus a publisher tweak —
// no scattered string comparisons.
var cdnServedModels = map[string]bool{
	"ecmwf": true,
}

func isCDNServedModel(name string) bool { return cdnServedModels[name] }

// DefaultWindCDNBaseURL is the public r2.dev URL the viam-chartplotter-ecmwf
// bucket is exposed at. Every chartplotter in the fleet defaults to fetching
// ECMWF wind tiles from here so only the one machine running the wind-publisher
// hits ECMWF Open Data, not 10K chartplotters. The serve /config handler reports
// it to the frontend; the publisher (weather/publish) uploads to the matching
// bucket. Override via the `wind_cdn_base_url` config attribute.
const DefaultWindCDNBaseURL = "https://pub-6ae2d2a870f74799a963dbc892ea400b.r2.dev"

// SetWindCDNBaseURL configures the public CDN base for tile-published
// wind data. Called by the module's Register-time wiring; the cache
// holds it just so the frontend can read it back via
// /noaa-weather/config (the cache itself doesn't use the CDN — it
// remains the on-demand fallback).
//
// Empty input maps to DefaultWindCDNBaseURL so a `make run` /
// minimal-config deployment still gets fan-out behaviour out of the
// box. There's no first-class "disable the CDN" mode any more — the
// only practical reason to bypass it is local dev against a module
// with no publisher running, and even then the local fallback still
// kicks in when latest.json is unreachable.
func (wc *WeatherCache) SetWindCDNBaseURL(u string) {
	if u == "" {
		u = DefaultWindCDNBaseURL
	}
	wc.windCDNBaseURL = strings.TrimRight(u, "/")
}

func NewWeatherCache(cacheDir string, logger logging.Logger) (*WeatherCache, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("weather cache: cacheDir required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("weather cache: mkdir %q: %w", cacheDir, err)
	}
	// Wire ECMWF's raw-GRIB2 stash to a subdirectory of the cache so
	// a CCSDS-decoder failure leaves the wire blob on disk for the
	// cmd/ecmwf-probe -file replay path. Best-effort: if the mkdir
	// fails we just don't enable the raw cache (the model still
	// works, you just can't replay).
	rawDir := filepath.Join(cacheDir, "raw-ecmwf")
	if err := os.MkdirAll(rawDir, 0o755); err == nil {
		ECMWFRawCacheDir = rawDir
	}
	return &WeatherCache{
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 120 * time.Second},
		logger:   logger,
		refresh:  weatherStaleAfter,
	}, nil
}

// Register attaches the weather handlers to mux. Endpoints:
//
//	/noaa-weather/models                       — JSON model registry
//	/noaa-weather/data/{model}/latest.json     — decoded GRIB as ol-wind JSON
//	/noaa-weather/stats                        — cache stats
//	/noaa-weather/gfs/latest.json              — legacy alias for {model}=gfs
//
// The /data/{model}/ route lets the frontend switch models with a URL
// change rather than swapping endpoints. The legacy alias keeps any
// existing bookmarked tab working through the rename.
func (wc *WeatherCache) Register(mux *http.ServeMux) {
	mux.HandleFunc("/noaa-weather/models", wc.handleModels)
	mux.HandleFunc("/noaa-weather/data/", wc.handleData)
	mux.HandleFunc("/noaa-weather/stats", wc.handleStats)
	mux.HandleFunc("/noaa-weather/config", wc.handleConfig)
	// Legacy alias — pre-multi-model code path. Cheap to keep around.
	mux.HandleFunc("/noaa-weather/gfs/latest.json", func(w http.ResponseWriter, r *http.Request) {
		wc.serveModel(w, r, FindModel("gfs"))
	})
}

func (wc *WeatherCache) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(modelMetaList())
}

// handleConfig exposes the small bits of server config the frontend
// needs at runtime — currently just the wind CDN base URL the tile
// fetcher should use. Lives next to /noaa-weather/models so the
// frontend's existing config fetch can be extended with one more
// request without inventing a new namespace.
func (wc *WeatherCache) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	body := map[string]interface{}{
		"windCDNBaseURL": wc.windCDNBaseURL,
		// Tile grid params — must match the publisher's constants
		// (wind_publisher_tiler.go). Surfacing them lets a frontend
		// recompute tile keys without hard-coding the grid; if we
		// ever change tileGridCols / tileGridRows / tileOverlapDeg
		// the frontend follows automatically.
		"tileGrid": map[string]interface{}{
			"cols":        tileGridCols,
			"rows":        tileGridRows,
			"overlapDeg":  tileOverlapDeg,
			"nominalLonW": tileNominalLonW,
			"nominalLatS": tileNominalLatS,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// handleData parses /noaa-weather/data/{model}/latest.json and dispatches
// to serveModel. Anything that doesn't match returns 404 with a helpful
// hint rather than the generic ServeMux "page not found".
func (wc *WeatherCache) handleData(w http.ResponseWriter, r *http.Request) {
	const prefix = "/noaa-weather/data/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "latest.json" {
		http.Error(w, "expected /noaa-weather/data/{model}/latest.json", http.StatusNotFound)
		return
	}
	m := FindModel(parts[0])
	if m == nil {
		http.Error(w, "unknown weather model: "+parts[0], http.StatusNotFound)
		return
	}
	wc.serveModel(w, r, m)
}

// weatherCacheVersion is bumped whenever the decoder logic changes in a
// way that would invalidate previously-cached JSON. Old files (without
// the version suffix, or with a lower version) are simply ignored —
// the cache layer treats them as a MISS and refetches. This keeps a
// stale buggy run (e.g. the pre-fix HRRR "57 kt uniformly" output)
// from haunting the deployment until the soft TTL expires.
//
// v2: sign+magnitude decode for signed GRIB2 integer fields (binary /
//
//	decimal scale factors, surface + earth-radius scales).
//
// v3: ECMWF CCSDS/AEC decoder — partial libaec fidelity through the
//
//	ID-before-ref, k=id-1, zero-block, FLUSH-postprocess, and
//	xMax clamp fixes. Bumped so the production server stops serving
//	the impossible-1000-m/s-wind JSON some earlier ECMWF runs
//	produced.
//
// v4: gfs-isobars now emits 2 hPa contour spacing (was 4 hPa) plus
//
//	Point features for local H/L pressure extrema. Old v3 GeoJSON
//	files don't carry the kind="H"/"L" properties the frontend now
//	expects, so they'd render the existing 4 hPa lines without
//	any extremum labels.
const weatherCacheVersion = "v4"

// cachePath returns the on-disk cache location for one (model, fh)
// combination. Model names are validated URL-safe at init time so we
// can drop them straight into a filename.
func (wc *WeatherCache) cachePath(modelName string, fh int) string {
	return filepath.Join(wc.cacheDir, fmt.Sprintf("%s-%s-f%03d.json", modelName, weatherCacheVersion, fh))
}

// cacheGzPath is the precompressed sibling next to the .json cache file.
// Written atomically by refreshNow and served (Content-Encoding: gzip)
// to clients that advertise Accept-Encoding: gzip so we don't pay 35 MB
// of wire transfer per forecast hour.
func (wc *WeatherCache) cacheGzPath(modelName string, fh int) string {
	return wc.cachePath(modelName, fh) + ".gz"
}

// clientAcceptsGzip is true if the request advertises gzip in its
// Accept-Encoding header. Lower-cased compare so "GZIP" / "gzip,
// deflate" both match.
func clientAcceptsGzip(r *http.Request) bool {
	enc := r.Header.Get("Accept-Encoding")
	if enc == "" {
		return false
	}
	for _, tok := range strings.Split(enc, ",") {
		// "gzip;q=0" disables gzip explicitly; skip in that case.
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, params, _ := strings.Cut(tok, ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			if strings.Contains(params, "q=0") && !strings.Contains(params, "q=0.") {
				return false
			}
			return true
		}
	}
	return false
}

// parseForecastHour pulls `?fh=N` off the request and snaps it to the
// model's StepFh / [MinFh, MaxFh] window. Defaults to MinFh on missing
// / malformed input.
func parseForecastHour(r *http.Request, m *WeatherModel) int {
	v := r.URL.Query().Get("fh")
	if v == "" {
		return m.MinFh
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return m.MinFh
	}
	return m.snapFh(n)
}

// serveModel serves a model's forecast for the requested hour straight from the
// Mongo weather collection. Serve is Mongo-ONLY: there is no on-demand GRIB
// fetch or disk read in the request path — the serve side is a clean reader. A
// weathersync service owns download→decode→store (see SyncWeatherOnce); if a
// forecast isn't in the store yet that's a 404, not a synchronous upstream
// fetch. Used by both /noaa-weather/data/{model}/latest.json and the legacy
// GFS alias.
func (wc *WeatherCache) serveModel(w http.ResponseWriter, r *http.Request, m *WeatherModel) {
	if m.Disabled {
		// Surface the registered Reason verbatim — the picker reads it
		// out of the error so the user knows why the option is greyed.
		http.Error(w, "model disabled: "+m.Reason, http.StatusNotImplemented)
		return
	}
	if m.Fetch == nil && m.FetchBytes == nil {
		// Frontend-rendered model (e.g. PacIOOS WMS heatmap) — there's
		// no JSON to serve, the picker uses a different code path.
		http.Error(w, "model is frontend-rendered, no data endpoint", http.StatusNotImplemented)
		return
	}
	if wc.mongoColl == nil {
		http.Error(w, "weather store not configured (no weathersync populating Mongo)", http.StatusServiceUnavailable)
		return
	}
	fh := parseForecastHour(r, m)
	g, ok, err := store.Get(r.Context(), wc.mongoColl, m.Name, fh)
	if err != nil {
		wc.errs.Add(1)
		http.Error(w, "weather store: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !ok || len(g.Payload) == 0 {
		http.Error(w, fmt.Sprintf("forecast not in store yet (model=%s fh=%d)", m.Name, fh), http.StatusNotFound)
		return
	}
	wc.hits.Add(1)
	w.Header().Set("X-Cache", "MONGO")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Add("Vary", "Accept-Encoding")
	switch {
	case !g.Gzip:
		_, _ = w.Write(g.Payload)
	case clientAcceptsGzip(r):
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(g.Payload)
	default:
		// Stored gzipped but the client can't take it — inflate on the way out.
		if gz, gerr := gzip.NewReader(bytes.NewReader(g.Payload)); gerr == nil {
			_, _ = io.Copy(w, gz)
			_ = gz.Close()
		}
	}
}

func (wc *WeatherCache) handleStats(w http.ResponseWriter, r *http.Request) {
	// Walk the cache dir so we surface every forecast hour we have on
	// disk rather than just f000.
	entries, _ := os.ReadDir(wc.cacheDir)
	files := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]any{
			"name":  e.Name(),
			"size":  info.Size(),
			"mtime": info.ModTime(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cache_dir":   wc.cacheDir,
		"files":       files,
		"stale_after": wc.refresh.String(),
		"hits":        wc.hits.Load(),
		"refreshes":   wc.refreshes.Load(),
		"errs":        wc.errs.Load(),
	})
}

// refreshNow fetches + decodes the model's GRIB at forecast hour `fh`
// and atomically replaces the cache file. De-duped via fetchMu so
// concurrent callers for the same (model, fh) wait for the first
// one's result.
func (wc *WeatherCache) refreshNow(ctx context.Context, m *WeatherModel, fh int) error {
	// Outer span covers everything including the time spent waiting on
	// fetchMu, since that's a real source of latency when prewarm and a
	// user request collide.
	ctx, span := tracer().Start(ctx, "weather.refreshNow",
		trace.WithAttributes(
			attribute.String("weather.model", m.Name),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()

	lockStart := time.Now()
	wc.fetchMu.Lock()
	span.SetAttributes(attribute.Int64("weather.lock_wait_ms", time.Since(lockStart).Milliseconds()))
	defer wc.fetchMu.Unlock()
	// Re-check freshness after acquiring the lock — another caller may
	// have just refreshed this same (model, fh).
	if info, err := os.Stat(wc.cachePath(m.Name, fh)); err == nil && time.Since(info.ModTime()) <= wc.refresh {
		span.SetAttributes(attribute.Bool("weather.short_circuit", true))
		return nil
	}

	// Models split into two output shapes: ol-wind WindRecord JSON for
	// the particle layers (wind / waves), and pre-serialised bytes
	// (GeoJSON for isobars, …) for the vector overlays. FetchBytes wins
	// when set so a model can short-circuit the WindRecord encoder.
	if m.FetchBytes != nil {
		body, err := wc.spanFetchBytes(ctx, m, fh)
		if err != nil {
			wc.errs.Add(1)
			span.RecordError(err)
			span.SetStatus(codes.Error, "fetch")
			return fmt.Errorf("%s fetch: %w", m.Name, err)
		}
		span.SetAttributes(attribute.Int("weather.bytes_in", len(body)))
		if err := wc.spanWriteBytes(ctx, m, fh, body); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "encode")
			return err
		}
	} else {
		records, err := wc.spanFetch(ctx, m, fh)
		if err != nil {
			wc.errs.Add(1)
			span.RecordError(err)
			span.SetStatus(codes.Error, "fetch")
			return fmt.Errorf("%s fetch: %w", m.Name, err)
		}
		span.SetAttributes(attribute.Int("weather.records", len(records)))
		if err := wc.spanEncode(ctx, m, fh, records); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "encode")
			return err
		}
	}
	// Write the .gz sibling so requests that advertise gzip can skip the
	// 35 MB raw transfer. Failures here are non-fatal — the handler
	// transparently falls back to the .json file.
	if err := wc.spanWriteGzip(ctx, m.Name, fh); err != nil {
		wc.logger.Warnf("weather: gzip %s fh=%d: %v", m.Name, fh, err)
		span.RecordError(err)
	}
	wc.refreshes.Add(1)
	wc.logger.Infof("weather: refreshed %s fh=%d", m.Name, fh)
	return nil
}

// spanFetch wraps m.Fetch (NOMADS / DWD / PacIOOS round-trip + GRIB
// decode) so the trace shows how much of refreshNow is the upstream
// fetch vs. local I/O. This is usually the bulk of slow refreshes —
// NOMADS commonly takes 30-60 s for a single GFS hour.
func (wc *WeatherCache) spanFetch(ctx context.Context, m *WeatherModel, fh int) ([]WindRecord, error) {
	ctx, span := tracer().Start(ctx, "weather.fetch",
		trace.WithAttributes(
			attribute.String("weather.model", m.Name),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()
	records, err := m.Fetch(ctx, wc.client, time.Time{}, fh)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return records, err
}

// spanEncode wraps the JSON encode + atomic rename. Encoding a single
// GFS hour is ~35 MB of JSON, dominated by float-to-string conversion
// — typically 100-300 ms.
func (wc *WeatherCache) spanEncode(ctx context.Context, m *WeatherModel, fh int, records []WindRecord) error {
	_, span := tracer().Start(ctx, "weather.encode",
		trace.WithAttributes(
			attribute.String("weather.model", m.Name),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()
	tmp := wc.cachePath(m.Name, fh) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		span.RecordError(err)
		return err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(records); err != nil {
		f.Close()
		os.Remove(tmp)
		span.RecordError(err)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		span.RecordError(err)
		return err
	}
	if err := os.Rename(tmp, wc.cachePath(m.Name, fh)); err != nil {
		span.RecordError(err)
		return err
	}
	if info, err := os.Stat(wc.cachePath(m.Name, fh)); err == nil {
		span.SetAttributes(attribute.Int64("weather.bytes", info.Size()))
	}
	return nil
}

// spanFetchBytes mirrors spanFetch but for the FetchBytes path used by
// vector overlays (isobars). The model returns already-serialised bytes
// (GeoJSON) so the cache layer doesn't go through WindRecord encoding.
func (wc *WeatherCache) spanFetchBytes(ctx context.Context, m *WeatherModel, fh int) ([]byte, error) {
	ctx, span := tracer().Start(ctx, "weather.fetch",
		trace.WithAttributes(
			attribute.String("weather.model", m.Name),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()
	body, err := m.FetchBytes(ctx, wc.client, time.Time{}, fh)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return body, err
}

// spanWriteBytes is the FetchBytes equivalent of spanEncode: writes the
// raw bytes atomically (.tmp + rename) into the model's cache file.
func (wc *WeatherCache) spanWriteBytes(ctx context.Context, m *WeatherModel, fh int, body []byte) error {
	_, span := tracer().Start(ctx, "weather.encode",
		trace.WithAttributes(
			attribute.String("weather.model", m.Name),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()
	tmp := wc.cachePath(m.Name, fh) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		span.RecordError(err)
		return err
	}
	if err := os.Rename(tmp, wc.cachePath(m.Name, fh)); err != nil {
		span.RecordError(err)
		return err
	}
	span.SetAttributes(attribute.Int64("weather.bytes", int64(len(body))))
	return nil
}

// spanWriteGzip wraps writeGzip with a span recording the compressed
// size so traces show the wire-savings vs. the raw .json size.
func (wc *WeatherCache) spanWriteGzip(ctx context.Context, modelName string, fh int) error {
	_, span := tracer().Start(ctx, "weather.gzip",
		trace.WithAttributes(
			attribute.String("weather.model", modelName),
			attribute.Int("weather.fh", fh),
		),
	)
	defer span.End()
	err := wc.writeGzip(modelName, fh)
	if err != nil {
		span.RecordError(err)
		return err
	}
	if info, err := os.Stat(wc.cacheGzPath(modelName, fh)); err == nil {
		span.SetAttributes(attribute.Int64("weather.gz_bytes", info.Size()))
	}
	return nil
}

// writeGzip compresses the .json cache file for (model, fh) into a
// .json.gz sibling. Written atomically (.tmp + rename) so a half-
// written file can never be served. Caller holds fetchMu.
func (wc *WeatherCache) writeGzip(modelName string, fh int) error {
	src := wc.cachePath(modelName, fh)
	dst := wc.cacheGzPath(modelName, fh)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Close cancels the periodic disk-cache cleaner goroutine if one was started
// (see StartCleaner). The serve path holds no goroutines.
func (wc *WeatherCache) Close() {
	if wc.cleanerCancel != nil {
		wc.cleanerCancel()
	}
}

// cleanOldFiles walks the cache directory recursively and deletes
// every regular file whose mtime is older than `maxAge`. Returns
// the count + bytes freed so the caller can log a summary. Walk
// errors on individual files are swallowed (logged but not fatal)
// so a single permission issue doesn't strand the rest of the
// cleanup.
//
// Covers everything under wc.cacheDir: stale per-version JSON
// (e.g. orphaned ecmwf-v2-*.json after a version bump), the
// raw-ecmwf/ raw-GRIB cache, and any .gz siblings. ECMWF data is
// immutable per (cycle, fh) so re-fetching after a cleanup costs
// only the bytes we'd have to re-pull from upstream — at 60-day
// thresholds that's essentially never on a normally-active
// publisher.
func (wc *WeatherCache) cleanOldFiles(maxAge time.Duration) (filesDeleted int, bytesFreed int64) {
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.Walk(wc.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log + skip; one bad entry shouldn't abort the whole walk.
			wc.logger.Debugf("weather: cache cleaner walk %s: %v", path, err)
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil {
			wc.logger.Debugf("weather: cache cleaner rm %s: %v", path, rmErr)
			return nil
		}
		filesDeleted++
		bytesFreed += info.Size()
		return nil
	})
	return
}

// StartCleaner runs cleanOldFiles in a background goroutine: once
// immediately, then on each `interval` tick. Cancelled by Close.
// Safe to call more than once (subsequent calls replace the
// previous cleaner's cancel; the orphaned goroutine exits on its
// next tick).
//
// `maxAge` is how stale a file must be before deletion (e.g.
// 60 * 24 * time.Hour for two months). `interval` is how often the
// cleaner runs — once a day is plenty since the cache grows slowly
// and the cleanup itself is cheap (one Walk pass).
func (wc *WeatherCache) StartCleaner(maxAge, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	wc.cleanerCancel = cancel
	go func() {
		runOnce := func() {
			n, freed := wc.cleanOldFiles(maxAge)
			if n > 0 {
				wc.logger.Infof("weather: cache cleaner removed %d files (%.1f MB freed, older than %s)",
					n, float64(freed)/(1024*1024), maxAge)
			}
		}
		// Eagerly clean once on startup so a long-idle install with a
		// huge stale cache doesn't have to wait `interval` to reclaim.
		runOnce()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}

// --- ol-wind JSON shape ---------------------------------------------------

// windHeader matches the GRIB-as-JSON header that grib2json (and hence
// ol-wind) consumes. Field names are camelCase per the schema.
type windHeader struct {
	Discipline                 int     `json:"discipline"`
	Center                     int     `json:"center"`
	RefTime                    string  `json:"refTime"`
	ForecastTime               int     `json:"forecastTime"`
	ParameterCategory          int     `json:"parameterCategory"`
	ParameterNumber            int     `json:"parameterNumber"`
	ParameterUnit              string  `json:"parameterUnit"`
	Surface1Type               int     `json:"surface1Type"`
	Surface1Value              float64 `json:"surface1Value"`
	GridDefinitionTemplateName string  `json:"gridDefinitionTemplateName"`
	Nx                         int     `json:"nx"`
	Ny                         int     `json:"ny"`
	Lo1                        float64 `json:"lo1"`
	La1                        float64 `json:"la1"`
	Lo2                        float64 `json:"lo2"`
	La2                        float64 `json:"la2"`
	Dx                         float64 `json:"dx"`
	Dy                         float64 `json:"dy"`
	ScanMode                   int     `json:"scanMode"`
}

type WindRecord struct {
	Header windHeader `json:"header"`
	Data   []float64  `json:"data"`
}

// --- Minimal GRIB2 parser -------------------------------------------------
//
// Just enough to handle GFS 0.25° UGRD/VGRD at 10 m above ground:
//   - Section 3 grid template 0 (lat/lon, regular_ll)
//   - Section 5 data representation template 0 (simple packing)
//   - No bitmap (section 6 indicator = 255)
//
// Any other template returns an error so we don't silently mis-decode.

const (
	gribParamCatMomentum    = 2
	gribParamUGRD           = 2
	gribParamVGRD           = 3
	gribSurfaceHeightAboveG = 103 // "Specified height level above ground (m)"
	// Oceanography discipline (10) → category 0 (waves) → params 3
	// (HTSGW, sig wave height m) and 10 (DIRPW, primary wave dir deg).
	gribDisciplineOceanography = 10
	gribParamCatWaves          = 0
	gribParamHTSGW             = 3
	gribParamDIRPW             = 10
)

// parseGFSWind10m walks every message in the file, picks out the two
// matching 10 m UGRD/VGRD records, and emits the ol-wind JSON shape.
// `forecastHour` is stamped onto each record's header so the frontend
// can show which forecast hour is currently displayed.
func parseGFSWind10m(grib []byte, runTime time.Time, forecastHour int) ([]WindRecord, error) {
	wantWind := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
		return paramCat == gribParamCatMomentum &&
			(paramNum == gribParamUGRD || paramNum == gribParamVGRD) &&
			surfType == gribSurfaceHeightAboveG &&
			surfValue == 10
	}
	var ugrd, vgrd *WindRecord
	for off := 0; off < len(grib); {
		end, rec, err := parseGRIBMessage(grib[off:], runTime, forecastHour, wantWind)
		if err != nil {
			return nil, fmt.Errorf("message @ off=%d: %w", off, err)
		}
		if rec != nil {
			switch {
			case rec.Header.ParameterCategory == gribParamCatMomentum &&
				rec.Header.ParameterNumber == gribParamUGRD &&
				rec.Header.Surface1Type == gribSurfaceHeightAboveG &&
				rec.Header.Surface1Value == 10:
				ugrd = rec
			case rec.Header.ParameterCategory == gribParamCatMomentum &&
				rec.Header.ParameterNumber == gribParamVGRD &&
				rec.Header.Surface1Type == gribSurfaceHeightAboveG &&
				rec.Header.Surface1Value == 10:
				vgrd = rec
			}
		}
		off += end
		if end <= 0 {
			break // defensive against parser bug
		}
	}
	if ugrd == nil || vgrd == nil {
		return nil, fmt.Errorf("missing UGRD or VGRD@10m (ugrd=%v vgrd=%v)", ugrd != nil, vgrd != nil)
	}
	return []WindRecord{*ugrd, *vgrd}, nil
}

// parseGFSWaveSurface walks every message in a GFSWAVE file, picks out
// the HTSGW (significant wave height, m) and DIRPW (primary wave
// direction, degrees from-which-waves-come) records at the surface, and
// converts them into the same 2-record (u, v) JSON shape ol-wind
// consumes. ol-wind keys data records by parameterCategory=2 + number
// 2/3, so we re-stamp the wave records as "wind" (param 2/2 and 2/3)
// even though they represent wave motion.
//
// Encoding choice: u = -h · sin(dir·π/180), v = -h · cos(dir·π/180).
// That makes ol-wind's particle animation drift in the direction the
// waves are propagating (180° opposite of DIRPW), with magnitude equal
// to wave height in metres — so wave height drives the colour scale and
// wave direction drives the particle flow.
func parseGFSWaveSurface(grib []byte, runTime time.Time, forecastHour int) ([]WindRecord, error) {
	wantWave := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
		return discipline == gribDisciplineOceanography &&
			paramCat == gribParamCatWaves &&
			(paramNum == gribParamHTSGW || paramNum == gribParamDIRPW)
	}
	var hRec, dRec *WindRecord
	for off := 0; off < len(grib); {
		end, rec, err := parseGRIBMessage(grib[off:], runTime, forecastHour, wantWave)
		if err != nil {
			return nil, fmt.Errorf("wave message @ off=%d: %w", off, err)
		}
		if rec != nil {
			switch rec.Header.ParameterNumber {
			case gribParamHTSGW:
				hRec = rec
			case gribParamDIRPW:
				dRec = rec
			}
		}
		off += end
		if end <= 0 {
			break
		}
	}
	if hRec == nil || dRec == nil {
		return nil, fmt.Errorf("missing HTSGW or DIRPW (h=%v d=%v)", hRec != nil, dRec != nil)
	}
	if len(hRec.Data) != len(dRec.Data) {
		return nil, fmt.Errorf("wave grid mismatch: h=%d d=%d", len(hRec.Data), len(dRec.Data))
	}
	uData := make([]float64, len(hRec.Data))
	vData := make([]float64, len(hRec.Data))
	for i := range hRec.Data {
		h := hRec.Data[i]
		// GFSWAVE uses ~9.999e20 to encode "no data" (deep grid, no
		// waves over land). Zero it so ol-wind's colour scale doesn't
		// get pulled into the trillions.
		if math.IsNaN(h) || h > 1e6 || h < 0 {
			h = 0
		}
		d := dRec.Data[i]
		if math.IsNaN(d) || d < 0 || d > 360 {
			uData[i] = 0
			vData[i] = 0
			continue
		}
		rad := d * math.Pi / 180
		uData[i] = -h * math.Sin(rad)
		vData[i] = -h * math.Cos(rad)
	}
	// Synthesise the two ol-wind records. Copy the height record's
	// header (grid bounds etc.) and re-stamp param category/number to
	// the wind convention so wind-core's formatData picks them up.
	uHdr := hRec.Header
	uHdr.ParameterCategory = gribParamCatMomentum
	uHdr.ParameterNumber = gribParamUGRD
	uHdr.ParameterUnit = "m" // wave height in metres
	vHdr := uHdr
	vHdr.ParameterNumber = gribParamVGRD
	return []WindRecord{
		{Header: uHdr, Data: uData},
		{Header: vHdr, Data: vData},
	}, nil
}

// parseGRIBMessage parses one GRIB2 message starting at b[0] and returns
// the total bytes consumed plus the decoded record (or nil if the
// message isn't a parameter we care about). Returns an error only for
// malformed input or unsupported templates.
// gribMessageFilter is called with the parameter identification fields
// extracted from a GRIB2 message so the caller can decide whether to
// pay the data-unpacking cost. Returns true to keep, false to skip.
type gribMessageFilter func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool

func parseGRIBMessage(b []byte, runTime time.Time, forecastHour int, want gribMessageFilter) (int, *WindRecord, error) {
	if len(b) < 16 || string(b[:4]) != "GRIB" {
		return 0, nil, fmt.Errorf("missing GRIB magic")
	}
	discipline := int(b[6])
	edition := int(b[7])
	if edition != 2 {
		return 0, nil, fmt.Errorf("only GRIB2 supported, got edition %d", edition)
	}
	totalLen := int(binary.BigEndian.Uint64(b[8:16]))
	if totalLen <= 0 || totalLen > len(b) {
		return 0, nil, fmt.Errorf("bogus message length %d (have %d)", totalLen, len(b))
	}
	if string(b[totalLen-4:totalLen]) != "7777" {
		return 0, nil, fmt.Errorf("missing 7777 terminator")
	}

	var (
		center                    int
		paramCat, paramNum        int
		surfType                  int
		surfValue                 float64
		nx, ny                    int
		lo1, la1, lo2, la2        float64
		dx, dy                    float64
		scanMode                  int
		refValue                  float32
		binaryScale, decimalScale int
		bitsPerValue              int
		dataValuesCount           int
		packedData                []byte
		dataTemplate              int
		gridTemplate              int
		haveBitmap                bool
		// Template 5.3 (complex packing + spatial differencing) extras.
		// These are zero/unused for template 5.0.
		numGroups              int
		groupWidthRef          int
		groupWidthBits         int
		groupLengthRef         int
		groupLengthIncrement   int
		groupLengthLast        int
		groupLengthBits        int
		spatialDiffOrder       int
		spatialDiffExtraOctets int
		// Template 5.42 (CCSDS) extras.
		ccsdsFlags     byte
		ccsdsBlockSize int
		ccsdsRSI       int
	)

	off := 16
	for off < totalLen-4 {
		secLen := int(binary.BigEndian.Uint32(b[off : off+4]))
		secNum := int(b[off+4])
		s := b[off : off+secLen]
		switch secNum {
		case 1: // Identification
			center = int(binary.BigEndian.Uint16(s[5:7]))
			// Reference time begins at byte 12 (year), 14 (month) etc — we
			// already pass runTime from the caller, so skip parsing it.
		case 3: // Grid Definition
			gridTemplate = int(binary.BigEndian.Uint16(s[12:14]))
			dataValuesCount = int(binary.BigEndian.Uint32(s[6:10]))
			if gridTemplate != 0 {
				return 0, nil, fmt.Errorf("grid template %d not supported (need 0/regular_ll)", gridTemplate)
			}
			// Template 3.0 layout for the fields we need (1-indexed in
			// WMO Manual; offsets below are 0-indexed into the section
			// body, so byte 31 in the manual is s[30]):
			//   31-34: Ni (Nx)
			//   35-38: Nj (Ny)
			//   47-50: La1 (×1e6)
			//   51-54: Lo1 (×1e6)
			//   56-59: La2 (×1e6)
			//   60-63: Lo2 (×1e6)
			//   64-67: Di (Dx, ×1e6)
			//   68-71: Dj (Dy, ×1e6)
			//   72:    scan mode
			nx = int(binary.BigEndian.Uint32(s[30:34]))
			ny = int(binary.BigEndian.Uint32(s[34:38]))
			la1 = signedUint32(binary.BigEndian.Uint32(s[46:50])) / 1e6
			lo1 = signedUint32(binary.BigEndian.Uint32(s[50:54])) / 1e6
			la2 = signedUint32(binary.BigEndian.Uint32(s[55:59])) / 1e6
			lo2 = signedUint32(binary.BigEndian.Uint32(s[59:63])) / 1e6
			dx = signedUint32(binary.BigEndian.Uint32(s[63:67])) / 1e6
			dy = signedUint32(binary.BigEndian.Uint32(s[67:71])) / 1e6
			scanMode = int(s[71])
		case 4: // Product Definition
			// Template 4.0 fields (offsets into section body):
			//   9:  parameter category
			//   10: parameter number
			//   23: surface 1 type
			//   24: surface 1 scale factor
			//   25-28: surface 1 scaled value
			paramCat = int(s[9])
			paramNum = int(s[10])
			surfType = int(s[22])
			// Sign+magnitude per GRIB2 spec — see signedInt8 in
			// grib_sections.go for the rationale.
			surfScale := signedInt8(s[23])
			surfScaled := int(binary.BigEndian.Uint32(s[24:28]))
			surfValue = float64(surfScaled) * math.Pow10(-surfScale)
		case 5: // Data Representation
			dataTemplate = int(binary.BigEndian.Uint16(s[9:11]))
			// All packing templates share octets 12-21 — pull them once.
			// Binary + decimal scale factors are sign+magnitude per the
			// GRIB2 spec; see signedInt16's comment in grib_sections.go.
			refValue = math.Float32frombits(binary.BigEndian.Uint32(s[11:15]))
			binaryScale = signedInt16(binary.BigEndian.Uint16(s[15:17]))
			decimalScale = signedInt16(binary.BigEndian.Uint16(s[17:19]))
			bitsPerValue = int(s[19])
			switch dataTemplate {
			case 0:
				// Simple packing — no extra fields.
			case 2, 3:
				// Complex packing (template 2) and complex packing +
				// spatial differencing (template 3). NOMADS uses 3 for
				// modern GFS. Field layout — left column is the WMO
				// 1-indexed octet number, right column is the equivalent
				// 0-indexed offset into the section body `s`:
				//   octet 22    →  s[21]   group splitting method
				//   octet 23    →  s[22]   missing value management
				//   octets 24-27→  s[23:27] primary missing val substitute
				//   octets 28-31→  s[27:31] secondary missing val substitute
				//   octets 32-35→  s[31:35] number of groups (NG)
				//   octet 36    →  s[35]   reference for group widths
				//   octet 37    →  s[36]   bits used for group widths
				//   octets 38-41→  s[37:41] reference for group lengths
				//   octet 42    →  s[41]   length increment for group lengths
				//   octets 43-46→  s[42:46] true length of last group
				//   octet 47    →  s[46]   bits for scaled group lengths
				// Template 3 adds:
				//   octet 48    →  s[47]   order of spatial differencing
				//   octet 49    →  s[48]   octets for spatial-diff descriptors
				numGroups = int(binary.BigEndian.Uint32(s[31:35]))
				groupWidthRef = int(s[35])
				groupWidthBits = int(s[36])
				groupLengthRef = int(binary.BigEndian.Uint32(s[37:41]))
				groupLengthIncrement = int(s[41])
				groupLengthLast = int(binary.BigEndian.Uint32(s[42:46]))
				groupLengthBits = int(s[46])
				if dataTemplate == 3 {
					spatialDiffOrder = int(s[47])
					spatialDiffExtraOctets = int(s[48])
				}
			case 42:
				// CCSDS / AEC (used by ECMWF Open Data). Section 5
				// extras live at the same offsets as the table in
				// grib_sections.go's parsePackingSection:
				//   octet 22 → s[21] flags (mask per WMO Table 5.40)
				//   octet 23 → s[22] block size
				//   octets 24-25 → s[23:25] reference sample interval
				if len(s) < 25 {
					return 0, nil, fmt.Errorf("ccsds section too short (%d bytes)", len(s))
				}
				ccsdsFlags = s[21]
				ccsdsBlockSize = int(s[22])
				ccsdsRSI = int(binary.BigEndian.Uint16(s[23:25]))
			default:
				return totalLen, nil, nil // unsupported template (e.g. JPEG2000)
			}
		case 6: // Bit Map
			haveBitmap = s[5] != 0xFF
			if haveBitmap {
				return totalLen, nil, nil // unsupported for now
			}
		case 7: // Data
			packedData = s[5:]
		}
		off += secLen
		if secLen <= 0 {
			return 0, nil, fmt.Errorf("zero-length section at %d", off)
		}
	}

	// If the caller doesn't want this parameter, skip the (potentially
	// expensive) value unpacking.
	if want != nil && !want(discipline, paramCat, paramNum, surfType, surfValue) {
		return totalLen, nil, nil
	}

	if dataValuesCount == 0 {
		dataValuesCount = nx * ny
	}
	var values []float64
	var err error
	switch dataTemplate {
	case 0:
		values, err = unpackSimple(packedData, refValue, binaryScale, decimalScale, bitsPerValue, dataValuesCount)
	case 2, 3:
		values, err = unpackComplex(packedData, refValue, binaryScale, decimalScale, bitsPerValue,
			numGroups, groupWidthRef, groupWidthBits,
			groupLengthRef, groupLengthIncrement, groupLengthLast, groupLengthBits,
			spatialDiffOrder, spatialDiffExtraOctets, dataValuesCount)
	case 42:
		values, err = unpackCCSDS(packedData, refValue, binaryScale, decimalScale, bitsPerValue,
			ccsdsFlags, ccsdsBlockSize, ccsdsRSI, dataValuesCount)
	default:
		return totalLen, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("unpack template %d: %w", dataTemplate, err)
	}

	rec := &WindRecord{
		Header: windHeader{
			Discipline:                 discipline,
			Center:                     center,
			RefTime:                    runTime.Format("2006-01-02T15:04:05.000Z"),
			ForecastTime:               forecastHour,
			ParameterCategory:          paramCat,
			ParameterNumber:            paramNum,
			ParameterUnit:              "m s**-1",
			Surface1Type:               surfType,
			Surface1Value:              surfValue,
			GridDefinitionTemplateName: "regular_ll",
			Nx:                         nx,
			Ny:                         ny,
			Lo1:                        lo1,
			La1:                        la1,
			Lo2:                        lo2,
			La2:                        la2,
			Dx:                         dx,
			Dy:                         dy,
			ScanMode:                   scanMode,
		},
		Data: values,
	}
	return totalLen, rec, nil
}

// signedUint32 decodes GRIB2's "sign + magnitude" 32-bit value: high bit
// is sign, low 31 bits are magnitude.
func signedUint32(u uint32) float64 {
	if u&0x80000000 != 0 {
		return -float64(u & 0x7FFFFFFF)
	}
	return float64(u)
}

// unpackComplex implements GRIB2 data representation template 5.2 / 5.3
// (complex packing, with optional first/second-order spatial differencing).
// This is what NOMADS publishes GFS in. The corresponding data section
// (template 7.2 / 7.3) layout:
//
//  1. For template 3 only: spatial-diff descriptors — `spatialOrder`
//     first values plus an overall minimum, each `extraOctets * 8` bits
//     wide, stored as sign+magnitude.
//  2. NG group reference values, each `bitsPerValue` bits, byte-aligned
//     after the previous block.
//  3. NG group widths, each `groupWidthBits` bits.
//  4. NG group lengths, each `groupLengthBits` bits — actual length is
//     `groupLengthRef + length * groupLengthIncrement` (last group is
//     pinned to `groupLengthLast`).
//  5. The packed values themselves — for each group i with width Wi and
//     length Li, Li values each (`bitsPerValue + Wi`) bits.
//
// Reconstruction:
//
//	raw[k] = groupRef[group_of_k] + groupVal[k]            // packed → raw
//	if spatial-diff: undo the diff using the first values + min          // template 3 only
//	value = (R + raw * 2^E) * 10^-D                                       // scale
func unpackComplex(
	packed []byte,
	ref float32,
	binaryScale, decimalScale, bitsPerValue,
	numGroups, gwRef, gwBits int,
	glRef, glIncr, glLast, glBits,
	spatialOrder, extraOctets, nValues int,
) ([]float64, error) {
	br := newBitReader(packed)

	// Spatial-diff descriptors (only present for template 3).
	firstVals := make([]int, spatialOrder)
	var overallMin int
	if spatialOrder > 0 && extraOctets > 0 {
		bits := extraOctets * 8
		for i := 0; i < spatialOrder; i++ {
			firstVals[i] = signedFromBits(br.read(bits), bits)
		}
		overallMin = signedFromBits(br.read(bits), bits)
		br.align()
	}

	groupRef := make([]int, numGroups)
	for i := 0; i < numGroups; i++ {
		groupRef[i] = int(br.read(bitsPerValue))
	}
	br.align()
	groupWidth := make([]int, numGroups)
	for i := 0; i < numGroups; i++ {
		groupWidth[i] = int(br.read(gwBits)) + gwRef
	}
	br.align()
	groupLength := make([]int, numGroups)
	for i := 0; i < numGroups; i++ {
		groupLength[i] = glRef + int(br.read(glBits))*glIncr
	}
	if numGroups > 0 {
		groupLength[numGroups-1] = glLast
	}
	br.align()

	raw := make([]int, 0, nValues)
	for i := 0; i < numGroups; i++ {
		w := groupWidth[i]
		l := groupLength[i]
		r := groupRef[i]
		for j := 0; j < l; j++ {
			var v int
			if w == 0 {
				v = r
			} else {
				v = r + int(br.read(w))
			}
			raw = append(raw, v)
		}
	}
	if len(raw) > nValues {
		raw = raw[:nValues]
	}
	if br.err != nil {
		return nil, br.err
	}

	// Undo spatial differencing.
	if spatialOrder > 0 {
		// raw[0..spatialOrder-1] are the literal first values from the
		// descriptor block (already in the raw[] array as group-0
		// entries; we overwrite them).
		for i := 0; i < spatialOrder && i < len(raw); i++ {
			raw[i] = firstVals[i]
		}
		// Each subsequent value gets `overallMin` added back, then the
		// difference operator inverted.
		switch spatialOrder {
		case 1:
			for i := spatialOrder; i < len(raw); i++ {
				raw[i] = raw[i] + overallMin + raw[i-1]
			}
		case 2:
			for i := spatialOrder; i < len(raw); i++ {
				raw[i] = raw[i] + overallMin + 2*raw[i-1] - raw[i-2]
			}
		default:
			return nil, fmt.Errorf("spatial differencing order %d not supported", spatialOrder)
		}
	}

	scaleBin := math.Pow(2, float64(binaryScale))
	scaleDec := math.Pow10(-decimalScale)
	out := make([]float64, len(raw))
	for i, x := range raw {
		out[i] = (float64(ref) + float64(x)*scaleBin) * scaleDec
	}
	return out, nil
}

// bitReader pulls big-endian bit fields out of a byte stream. NOAA GRIB2
// data sections are tightly packed bit streams that we walk in order,
// with byte-alignment between distinct blocks (see template 7.2 / 7.3).
type bitReader struct {
	b      []byte
	bp     int
	buf    uint64
	bufLen uint
	err    error
}

func newBitReader(b []byte) *bitReader { return &bitReader{b: b} }

func (r *bitReader) read(nBits int) uint64 {
	if r.err != nil || nBits == 0 {
		return 0
	}
	if nBits < 0 || nBits > 64 {
		r.err = fmt.Errorf("bitReader: bad nBits %d", nBits)
		return 0
	}
	for r.bufLen < uint(nBits) {
		if r.bp >= len(r.b) {
			r.err = fmt.Errorf("bitReader: ran out of bytes at bp=%d", r.bp)
			return 0
		}
		r.buf = (r.buf << 8) | uint64(r.b[r.bp])
		r.bp++
		r.bufLen += 8
	}
	shift := r.bufLen - uint(nBits)
	mask := uint64(1)<<uint(nBits) - 1
	v := (r.buf >> shift) & mask
	r.bufLen -= uint(nBits)
	r.buf &= (uint64(1) << r.bufLen) - 1
	return v
}

func (r *bitReader) align() {
	// Drop any leftover bits so the next field starts byte-aligned.
	r.buf = 0
	r.bufLen = 0
}

// signedFromBits decodes a sign+magnitude bit field of arbitrary width.
// Used for the spatial-differencing descriptors in template 7.3.
func signedFromBits(u uint64, bits int) int {
	if bits <= 0 {
		return 0
	}
	signBit := uint64(1) << uint(bits-1)
	if u&signBit != 0 {
		return -int(u &^ signBit)
	}
	return int(u)
}

// unpackSimple implements GRIB2 data representation template 5.0
// (simple packing): value = R + (X * 2^E) * 10^-D, where X is the
// packed integer and bitsPerValue tells you how many bits per X. NOAA
// publishes GFS UGRD/VGRD with bitsPerValue=12; smaller bit widths still
// pack into byte-aligned streams.
func unpackSimple(packed []byte, ref float32, binaryScale, decimalScale, bitsPerValue, n int) ([]float64, error) {
	out := make([]float64, n)
	if bitsPerValue == 0 {
		// Constant field — every value equals the reference.
		v := float64(ref)
		for i := range out {
			out[i] = v
		}
		return out, nil
	}
	scaleBin := math.Pow(2, float64(binaryScale))
	scaleDec := math.Pow10(-decimalScale)
	mask := uint64(1)<<uint(bitsPerValue) - 1

	var bitBuf uint64
	var bitsInBuf uint
	bp := 0
	for i := 0; i < n; i++ {
		for bitsInBuf < uint(bitsPerValue) {
			if bp >= len(packed) {
				return nil, fmt.Errorf("simple unpack: ran out of bytes (i=%d/%d)", i, n)
			}
			bitBuf = (bitBuf << 8) | uint64(packed[bp])
			bp++
			bitsInBuf += 8
		}
		shift := bitsInBuf - uint(bitsPerValue)
		x := (bitBuf >> shift) & mask
		bitsInBuf -= uint(bitsPerValue)
		bitBuf &= (1 << bitsInBuf) - 1
		out[i] = (float64(ref) + float64(x)*scaleBin) * scaleDec
	}
	return out, nil
}
