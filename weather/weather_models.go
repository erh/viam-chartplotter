package weather

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ECMWFRawCacheDir is the directory backing the project-wide rule
// "every external fetch goes through a disk cache." ECMWF Open Data
// publishes each GRIB message at one immutable URL, so once we've
// pulled (cycle, fh) we never re-fetch — every subsequent caller
// (on-demand server, wind-publisher cron, cmd/ecmwf-probe) gets the
// cached bytes for free. This matters because ECMWF rate-limits
// aggressively (we hit a 429 during development) and a publisher
// that crashes 80% through a cycle build would otherwise re-fetch
// all 49 fhs on the next run.
//
// Production wiring: NewWeatherCache sets this to <cacheDir>/raw-ecmwf
// on the server, and the wind-publisher CLI / Viam resource set it
// via SetECMWFRawCacheDir. The on-demand model.Fetch and the
// publisher's fetchAndDecodeForPublish both go through
// CachedFetchECMWFWind10m, which is cache-first.
var ECMWFRawCacheDir string

// SetECMWFRawCacheDir lets non-server callers wire up the raw cache
// without going through NewWeatherCache. Creates the directory if
// missing; the empty-string form disables caching (only useful in
// tests).
func SetECMWFRawCacheDir(dir string) error {
	if dir == "" {
		ECMWFRawCacheDir = ""
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ecmwf raw cache mkdir %q: %w", dir, err)
	}
	ECMWFRawCacheDir = dir
	return nil
}

// ecmwfRawCachePath builds the deterministic file name for a
// (cycle, fh) blob. Stable across processes so the publisher CLI,
// the on-demand server, and the probe all hit the same cache entry.
func ecmwfRawCachePath(runTime time.Time, fh int) string {
	if ECMWFRawCacheDir == "" {
		return ""
	}
	return filepath.Join(ECMWFRawCacheDir,
		fmt.Sprintf("ecmwf-%s-f%03d.grib2", runTime.UTC().Format("20060102T15"), fh))
}

// CachedFetchECMWFWind10m is the cache-first entry point every caller
// should use. Checks the disk cache, falls back to fetchECMWFWind10m
// on miss, writes back to the cache on success. Write failures are
// non-fatal — caching is best-effort, the decoded data is what
// matters.
//
// Logs one line per call so an operator can confirm cache behaviour
// at a glance: a healthy publisher resume after a crash should show
// HIT for every fh it already processed and MISS only for the new
// ones.
func CachedFetchECMWFWind10m(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]byte, error) {
	cycleStr := runTime.UTC().Format("20060102T15")
	path := ecmwfRawCachePath(runTime, fh)
	if path == "" {
		log.Printf("ecmwfCache: DISABLED cycle=%s fh=%d (no raw cache dir set)", cycleStr, fh)
	} else if b, err := os.ReadFile(path); err == nil {
		log.Printf("ecmwfCache: HIT cycle=%s fh=%d bytes=%d path=%s", cycleStr, fh, len(b), path)
		return b, nil
	} else if !os.IsNotExist(err) {
		// Real I/O error reading the cache file — log it so it's
		// obvious rather than silently re-fetching. We still
		// proceed to fetch from ECMWF.
		log.Printf("ecmwfCache: ERR cycle=%s fh=%d path=%s read: %v (will refetch)", cycleStr, fh, path, err)
	} else {
		log.Printf("ecmwfCache: MISS cycle=%s fh=%d path=%s", cycleStr, fh, path)
	}

	grib, err := fetchECMWFWind10m(ctx, client, runTime, fh)
	if err != nil {
		return nil, err
	}
	if path != "" {
		// Write via .tmp + rename so a crash mid-write doesn't
		// leave a half-blob the next run would happily try to
		// decode (and fail in a confusing way).
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, grib, 0o644); werr == nil {
			if rerr := os.Rename(tmp, path); rerr != nil {
				log.Printf("ecmwfCache: write rename %s: %v", path, rerr)
			} else {
				log.Printf("ecmwfCache: STORE cycle=%s fh=%d bytes=%d path=%s", cycleStr, fh, len(grib), path)
			}
		} else {
			log.Printf("ecmwfCache: write %s: %v", path, werr)
		}
	}
	return grib, nil
}

// WeatherModel describes one upstream forecast that the cache can
// serve under /noaa-weather/data/{name}/latest.json. The cache layer
// (WeatherCache) handles the latest-cycle walk-back, on-disk
// debouncing, and serving — each model just declares its cadence and
// supplies a Fetch that downloads + decodes a single (run, fh) into
// the ol-wind JSON shape.
type WeatherModel struct {
	Name        string
	DisplayName string
	Kind        string // "wind" | "wave"
	Domain      string // "global" | "conus" | "europe"
	CycleHours  []int
	MinFh       int
	MaxFh       int
	StepFh      int
	PublishLagH int
	// Disabled + Reason are surfaced to the UI as a non-selectable entry
	// so the picker can advertise "this is coming, but the decoder isn't
	// here yet". Used for NAM (JPEG2000), ECMWF (CCSDS), ICON-Global
	// (icosahedral regrid) which all need decoder work we haven't done.
	Disabled bool
	Reason   string
	// Fetch downloads + decodes one (runTime, fh) into ol-wind records.
	// May return ErrUnsupportedPacking — the cache layer logs that
	// distinctly so a deploy can tell "upstream missing" from "decoder
	// missing".
	Fetch func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]WindRecord, error)
	// FetchBytes is the alternative path for models whose on-the-wire
	// shape isn't the ol-wind 2-record JSON — currently isobars (GeoJSON
	// LineString FeatureCollection) and (eventually) lightning strokes.
	// When set, the cache writes the returned bytes straight to disk
	// instead of json-encoding []WindRecord. Exactly one of Fetch /
	// FetchBytes must be set on an enabled model.
	FetchBytes func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]byte, error)
}

// snapFh snaps fh to [MinFh, MaxFh] aligned to StepFh.
func (m *WeatherModel) snapFh(fh int) int {
	if fh < m.MinFh {
		fh = m.MinFh
	}
	if fh > m.MaxFh {
		fh = m.MaxFh
	}
	if m.StepFh > 1 {
		fh = (fh / m.StepFh) * m.StepFh
	}
	return fh
}

// allModels is the registry the cache + the /noaa-weather/models JSON
// endpoint consume. Ordering controls the UI dropdown order.
var allModels = []*WeatherModel{
	gfsWindModel(),
	hrrrWindModel(),
	iconEUWindModel(),
	namWindStub(),
	ecmwfWindModel(),
	iconGlobalWindStub(),
	pacioosWaveModel(),
	pacioosHawaiiWaveModel(),
	gfsIsobarsModel(),
	noaaGLMLightningStub(),
	// nomads-gfswave is unregistered until we verify NOMADS' actual
	// current DODS URL pattern. Every variant I've tried (date dir
	// with/without `gfswave` prefix, gfsv16 suffix, etc.) lands on
	// NOMADS' '301 Welcome to NOMADS' page with no Location header,
	// which means the path just doesn't exist. The fetch code stays
	// in noaa_wave_cache.go + weather_models.go so re-enabling is a
	// one-line append once a working URL is confirmed.
}

// FindModel does an O(n) lookup — registry is fixed small (~10 entries).
func FindModel(name string) *WeatherModel {
	for _, m := range allModels {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// listModels returns the registry — used by the /noaa-weather/models
// endpoint so the frontend dropdown can populate itself.
func listModels() []*WeatherModel { return allModels }

// ----------------------------------------------------------------------
// Common helpers
// ----------------------------------------------------------------------

// WalkLatestCycle walks back through `m.CycleHours` (every 24h ÷ N
// hours apart) until `fetch` returns a non-error body for that
// candidate run. Returns the body + the run time we hit.
func WalkLatestCycle(
	ctx context.Context,
	m *WeatherModel,
	fh int,
	fetch func(ctx context.Context, runTime time.Time) ([]byte, error),
) ([]byte, time.Time, error) {
	if len(m.CycleHours) == 0 {
		return nil, time.Time{}, fmt.Errorf("model %s has no cycle hours", m.Name)
	}
	now := time.Now().UTC().Add(-time.Duration(m.PublishLagH) * time.Hour)
	// Start at the most recent cycle that should be published.
	candidate := MostRecentCycle(now, m.CycleHours)
	var lastErr error
	for i := 0; i < 4; i++ {
		start := time.Now()
		body, err := fetch(ctx, candidate)
		if err == nil {
			log.Printf("weather: %s WalkLatestCycle hit cycle=%s fh=%d attempt=%d dur=%s bytes=%d",
				m.Name, candidate.Format("20060102T15"), fh, i, time.Since(start), len(body))
			return body, candidate, nil
		}
		log.Printf("weather: %s WalkLatestCycle MISS cycle=%s fh=%d attempt=%d dur=%s err=%v",
			m.Name, candidate.Format("20060102T15"), fh, i, time.Since(start), err)
		lastErr = err
		// Step back to the prior cycle. CycleHours are sorted; find the
		// previous slot, jumping a day if we wrap.
		candidate = PreviousCycle(candidate, m.CycleHours)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no run available for %s in last 24h", m.Name)
	}
	return nil, time.Time{}, lastErr
}

func MostRecentCycle(now time.Time, cycles []int) time.Time {
	h := now.Hour()
	pick := cycles[0]
	for _, c := range cycles {
		if c <= h {
			pick = c
		}
	}
	if pick > h {
		// All cycles are in the future on this UTC day — roll back.
		now = now.Add(-24 * time.Hour)
		pick = cycles[len(cycles)-1]
	}
	return time.Date(now.Year(), now.Month(), now.Day(), pick, 0, 0, 0, time.UTC)
}

func PreviousCycle(t time.Time, cycles []int) time.Time {
	h := t.Hour()
	for i := len(cycles) - 1; i >= 0; i-- {
		if cycles[i] < h {
			return time.Date(t.Year(), t.Month(), t.Day(), cycles[i], 0, 0, 0, time.UTC)
		}
	}
	prev := t.Add(-24 * time.Hour)
	return time.Date(prev.Year(), prev.Month(), prev.Day(), cycles[len(cycles)-1], 0, 0, 0, time.UTC)
}

// fetchURL is the standard GET with the cache's HTTP client + context.
// Returns the body or an error including the HTTP status so the
// WalkLatestCycle backoff prints something useful.
func fetchURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(body) < 32 || string(body[:4]) != "GRIB" {
		return nil, fmt.Errorf("response is not GRIB2 (size=%d, first=%q)",
			len(body), string(body[:min(16, len(body))]))
	}
	return body, nil
}

// fetchURLAnyContent is the same as fetchURL but doesn't require the
// "GRIB" magic bytes — used for bz2-wrapped responses (ICON-EU) where
// the magic only shows up after decompression.
func fetchURLAnyContent(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// decodeRegularLLWind walks every GRIB2 message in `grib` and decodes
// the first message whose product matches `want` into a WindRecord.
// Used by every regular_ll model (GFS, ICON-EU, …) — they only differ
// in which (paramCat, paramNum) they ask for. Returns the first
// matching record or nil; pass twice with different filters to pull
// UGRD + VGRD separately.
func decodeRegularLLMessage(
	grib []byte,
	runTime time.Time,
	fh int,
	want func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool,
) (*WindRecord, error) {
	for off := 0; off < len(grib); {
		end, rec, err := parseGRIBMessage(grib[off:], runTime, fh, want)
		if err != nil {
			return nil, fmt.Errorf("message @ off=%d: %w", off, err)
		}
		if rec != nil {
			return rec, nil
		}
		if end <= 0 {
			break
		}
		off += end
	}
	return nil, nil
}

// ----------------------------------------------------------------------
// GFS (existing — keep behaviour identical, just behind the registry)
// ----------------------------------------------------------------------

func gfsWindModel() *WeatherModel {
	m := &WeatherModel{
		Name:        "gfs",
		DisplayName: "GFS (NOAA, 0.25° global)",
		Kind:        "wind",
		Domain:      "global",
		CycleHours:  []int{0, 6, 12, 18},
		MinFh:       0,
		MaxFh:       240,
		StepFh:      3,
		PublishLagH: 4,
	}
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]WindRecord, error) {
		body, runT, err := WalkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
			date := t.Format("20060102")
			cc := t.Hour()
			url := fmt.Sprintf(nomadsGFSURLTemplate, cc, fh, date, cc)
			return fetchURL(ctx, client, url)
		})
		if err != nil {
			return nil, err
		}
		return parseGFSWind10m(body, runT, fh)
	}
	return m
}

// ----------------------------------------------------------------------
// HRRR — Lambert Conformal source, complex packing (already supported).
// We reproject the 1799×1059 native LC grid down onto a regular lat/lon
// CONUS subgrid that ol-wind can render directly.
// ----------------------------------------------------------------------

const (
	// HRRR coverage rectangle, chosen to cover the native CONUS LC grid
	// without too much wasted ocean. 0.05° ≈ 5.5 km — coarser than HRRR's
	// 3 km native resolution but plenty for ol-wind's particle drift,
	// and keeps the JSON payload to ~4 MB instead of ~25 MB.
	hrrrLonW = -134.0
	hrrrLonE = -60.0
	hrrrLatS = 20.0
	hrrrLatN = 53.0
	hrrrDlon = 0.05
	hrrrDlat = 0.05
	// NOMADS HRRR filter — subsets to UGRD/VGRD at 10 m, ~150 KB GRIB2
	// instead of ~50 MB whole file.
	nomadsHRRRURLTemplate = "https://nomads.ncep.noaa.gov/cgi-bin/filter_hrrr_2d.pl" +
		"?file=hrrr.t%02dz.wrfsfcf%02d.grib2" +
		"&lev_10_m_above_ground=on&var_UGRD=on&var_VGRD=on" +
		"&dir=%%2Fhrrr.%s%%2Fconus"
)

func hrrrWindModel() *WeatherModel {
	m := &WeatherModel{
		Name:        "hrrr",
		DisplayName: "HRRR (NOAA, 3 km CONUS)",
		Kind:        "wind",
		Domain:      "conus",
		CycleHours:  []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23},
		MinFh:       0,
		MaxFh:       18,
		StepFh:      1,
		PublishLagH: 2,
	}
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]WindRecord, error) {
		body, runT, err := WalkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
			date := t.Format("20060102")
			cc := t.Hour()
			url := fmt.Sprintf(nomadsHRRRURLTemplate, cc, fh, date)
			return fetchURL(ctx, client, url)
		})
		if err != nil {
			return nil, err
		}
		return decodeHRRRWind(body, runT, fh)
	}
	return m
}

// decodeHRRRWind walks the HRRR GRIB2, parses UGRD and VGRD on the LC
// grid, reprojects each onto the regular lat/lon CONUS subgrid, and
// emits the ol-wind 2-record JSON shape.
func decodeHRRRWind(grib []byte, runTime time.Time, fh int) ([]WindRecord, error) {
	wantWind := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
		return paramCat == gribParamCatMomentum &&
			(paramNum == gribParamUGRD || paramNum == gribParamVGRD) &&
			surfType == gribSurfaceHeightAboveG &&
			surfValue == 10
	}
	var u, v []float64
	var grid *lambertGrid
	for off := 0; off < len(grib); {
		secs, err := walkGRIBMessage(grib[off:])
		if err != nil {
			return nil, fmt.Errorf("HRRR @ off=%d: %w", off, err)
		}
		prod, err := parseProductSection(secs.section4)
		if err != nil {
			return nil, err
		}
		if !wantWind(secs.discipline, prod.paramCat, prod.paramNum, prod.surfType, prod.surfValue) {
			off += secs.totalLen
			continue
		}
		g, err := parseLambertGrid(secs.section3)
		if err != nil {
			return nil, fmt.Errorf("HRRR grid: %w", err)
		}
		// We require all messages to share the same grid — HRRR does.
		grid = g
		pack, err := parsePackingSection(secs.section5, g.Nx*g.Ny)
		if err != nil {
			return nil, err
		}
		values, err := unpackData(secs.section7, pack)
		if err != nil {
			return nil, fmt.Errorf("HRRR unpack: %w", err)
		}
		switch prod.paramNum {
		case gribParamUGRD:
			u = values
		case gribParamVGRD:
			v = values
		}
		off += secs.totalLen
	}
	if u == nil || v == nil || grid == nil {
		return nil, fmt.Errorf("HRRR: missing UGRD or VGRD@10m")
	}
	// Diagnostic — print the parsed Lambert grid + a sample of the
	// unpacked source values so we can tell at runtime whether a
	// downstream "all wind same speed" symptom is a grid-parsing bug
	// (Latin1/Latin2/Dx zero), an unpack bug (constant values), or a
	// reprojection bug (varying source → constant output). Logged at
	// info level via the stdlib logger so it shows up alongside the
	// existing weather-cache log lines without needing to thread the
	// rdk logger through the model registry.
	uMin, uMax := minMax(u)
	vMin, vMax := minMax(v)
	log.Printf("HRRR decoded fh=%d: grid Nx=%d Ny=%d La1=%.3f Lo1=%.3f LaD=%.3f LoV=%.3f Latin1=%.3f Latin2=%.3f Dx=%.1fm Dy=%.1fm scan=%d R=%.0fm; u∈[%.2f,%.2f] v∈[%.2f,%.2f]",
		fh, grid.Nx, grid.Ny, grid.La1, grid.Lo1, grid.LaD, grid.LoV,
		grid.Latin1, grid.Latin2, grid.Dx, grid.Dy, grid.ScanMode, grid.EarthRadius,
		uMin, uMax, vMin, vMax)
	uLL, Nlon, Nlat, err := reprojectLambertToLatLon(u, grid, hrrrLonW, hrrrLonE, hrrrLatS, hrrrLatN, hrrrDlon, hrrrDlat)
	if err != nil {
		return nil, err
	}
	vLL, _, _, err := reprojectLambertToLatLon(v, grid, hrrrLonW, hrrrLonE, hrrrLatS, hrrrLatN, hrrrDlon, hrrrDlat)
	if err != nil {
		return nil, err
	}
	uLLmin, uLLmax := minMax(uLL)
	vLLmin, vLLmax := minMax(vLL)
	log.Printf("HRRR reprojected fh=%d: Nlon=%d Nlat=%d u∈[%.2f,%.2f] v∈[%.2f,%.2f]",
		fh, Nlon, Nlat, uLLmin, uLLmax, vLLmin, vLLmax)
	hdr := windHeader{
		Center:                     7, // NCEP
		RefTime:                    runTime.Format("2006-01-02T15:04:05.000Z"),
		ForecastTime:               fh,
		ParameterCategory:          gribParamCatMomentum,
		ParameterNumber:            gribParamUGRD,
		ParameterUnit:              "m s**-1",
		Surface1Type:               gribSurfaceHeightAboveG,
		Surface1Value:              10,
		GridDefinitionTemplateName: "regular_ll",
		Nx:                         Nlon,
		Ny:                         Nlat,
		Lo1:                        hrrrLonW,
		La1:                        hrrrLatN, // N→S row-major, La1 is the northern edge
		Lo2:                        hrrrLonE,
		La2:                        hrrrLatS,
		Dx:                         hrrrDlon,
		Dy:                         hrrrDlat,
		ScanMode:                   0,
	}
	uHdr := hdr
	vHdr := hdr
	vHdr.ParameterNumber = gribParamVGRD
	return []WindRecord{
		{Header: uHdr, Data: uLL},
		{Header: vHdr, Data: vLL},
	}, nil
}

// ----------------------------------------------------------------------
// ICON-EU (DWD opendata) — regular_ll 0.0625°, simple packing, bz2
// wrapped, U and V live in separate files.
// ----------------------------------------------------------------------

const iconEUURLTemplate = "https://opendata.dwd.de/weather/nwp/icon-eu/grib/%02d/%s/" +
	"icon-eu_europe_regular-lat-lon_single-level_%s%02d_%03d_%s.grib2.bz2"

func iconEUWindModel() *WeatherModel {
	m := &WeatherModel{
		Name:        "icon-eu",
		DisplayName: "ICON-EU (DWD, 0.0625° Europe)",
		Kind:        "wind",
		Domain:      "europe",
		CycleHours:  []int{0, 6, 12, 18},
		MinFh:       0,
		MaxFh:       120,
		StepFh:      1,
		PublishLagH: 4,
	}
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]WindRecord, error) {
		// ICON-EU ships U_10M and V_10M as two independent .grib2.bz2
		// files — we fetch them in series (cycle walk-back uses the
		// U_10M file as the probe, and assumes V_10M ships at the same
		// time).
		var bodyU, bodyV []byte
		_, runT, err := WalkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
			date := t.Format("2006010215")
			cc := t.Hour()
			urlU := fmt.Sprintf(iconEUURLTemplate, cc, "u_10m", date[:8], cc, fh, "U_10M")
			body, err := fetchURLAnyContent(ctx, client, urlU)
			if err != nil {
				return nil, err
			}
			decoded, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(body)))
			if err != nil {
				return nil, fmt.Errorf("bz2 decode: %w", err)
			}
			if len(decoded) < 4 || string(decoded[:4]) != "GRIB" {
				return nil, fmt.Errorf("ICON-EU U: bad GRIB after bz2")
			}
			bodyU = decoded
			return decoded, nil
		})
		if err != nil {
			return nil, err
		}
		// Now fetch V at the same run.
		date := runT.Format("2006010215")
		cc := runT.Hour()
		urlV := fmt.Sprintf(iconEUURLTemplate, cc, "v_10m", date[:8], cc, fh, "V_10M")
		rawV, err := fetchURLAnyContent(ctx, client, urlV)
		if err != nil {
			return nil, fmt.Errorf("ICON-EU V fetch: %w", err)
		}
		bodyV, err = io.ReadAll(bzip2.NewReader(bytes.NewReader(rawV)))
		if err != nil {
			return nil, fmt.Errorf("ICON-EU V bz2: %w", err)
		}
		wantAny := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
			return true
		}
		uRec, err := decodeRegularLLMessage(bodyU, runT, fh, wantAny)
		if err != nil || uRec == nil {
			return nil, fmt.Errorf("ICON-EU U decode: %v", err)
		}
		vRec, err := decodeRegularLLMessage(bodyV, runT, fh, wantAny)
		if err != nil || vRec == nil {
			return nil, fmt.Errorf("ICON-EU V decode: %v", err)
		}
		// DWD doesn't always stamp param category=2/num=2,3 the way ol-wind
		// expects — overwrite so wind-core picks the records up regardless.
		uRec.Header.ParameterCategory = gribParamCatMomentum
		uRec.Header.ParameterNumber = gribParamUGRD
		uRec.Header.Surface1Type = gribSurfaceHeightAboveG
		uRec.Header.Surface1Value = 10
		vRec.Header.ParameterCategory = gribParamCatMomentum
		vRec.Header.ParameterNumber = gribParamVGRD
		vRec.Header.Surface1Type = gribSurfaceHeightAboveG
		vRec.Header.Surface1Value = 10
		return []WindRecord{*uRec, *vRec}, nil
	}
	return m
}

// ----------------------------------------------------------------------
// Waves — PacIOOS WaveWatch III "best" forecast, fetched via OPeNDAP
// (binary DODS, not GRIB) so we avoid the JPEG2000-packed GFSWAVE
// upstream that the in-tree parser can't decode. The hard work
// (DODS parsing, time-axis lookup, height+direction → u/v vector
// conversion) lives in noaa_wave_cache.go; this registry entry just
// wires it into the model picker and forecast-hour slider.
// ----------------------------------------------------------------------

// waveModelFor wraps a waveDatasetConfig in a registry-shaped model.
// The (cycle, fh) → target-time mapping is identical across wave
// datasets — reconstruct the most recent GFS cycle, add fh hours,
// hand to fetchWaveDataset which picks the nearest available slice.
func waveModelFor(name, displayName, domain string, cfg waveDatasetConfig) *WeatherModel {
	m := &WeatherModel{
		Name:        name,
		DisplayName: displayName,
		Kind:        "wave",
		Domain:      domain,
		// All our wave datasets expose ~hourly slices and map fh to
		// the nearest one — we reuse the GFS cadence for the slider's
		// day-tick alignment and now-floor across models.
		CycleHours:  []int{0, 6, 12, 18},
		MinFh:       0,
		MaxFh:       240,
		StepFh:      1,
		PublishLagH: 5,
	}
	m.Fetch = func(ctx context.Context, client *http.Client, _ time.Time, fh int) ([]WindRecord, error) {
		now := time.Now().UTC().Truncate(time.Hour)
		gfsCycle := now.Add(-time.Duration(gfsPublishLagHours) * time.Hour)
		gfsCycle = time.Date(gfsCycle.Year(), gfsCycle.Month(), gfsCycle.Day(),
			(gfsCycle.Hour()/6)*6, 0, 0, 0, time.UTC)
		target := gfsCycle.Add(time.Duration(fh) * time.Hour)
		return fetchWaveDataset(ctx, client, cfg, target)
	}
	return m
}

func pacioosWaveModel() *WeatherModel {
	return waveModelFor("pacioos-ww3",
		"WaveWatch III (PacIOOS, 0.5° global)",
		"global", pacioosGlobalConfig())
}

func pacioosHawaiiWaveModel() *WeatherModel {
	return waveModelFor("pacioos-ww3-hawaii",
		"WaveWatch III (PacIOOS, 0.05° Hawaii)",
		"hawaii", pacioosHawaiiConfig())
}

func nomadsGFSWaveModel() *WeatherModel {
	return waveModelFor("nomads-gfswave",
		"GFS-Wave (NOAA NOMADS, 0.25° global)",
		"global", nomadsGFSWaveConfig())
}

// ----------------------------------------------------------------------
// Stubs — advertised in the UI but disabled until a decoder ships.
// Picking one shows the Reason in a tooltip and refuses to switch.
// ----------------------------------------------------------------------

func namWindStub() *WeatherModel {
	return &WeatherModel{
		Name:        "nam",
		DisplayName: "NAM (NOAA, 12 km CONUS)",
		Kind:        "wind",
		Domain:      "conus",
		Disabled:    true,
		Reason:      "needs JPEG2000 (GRIB2 template 5.40) decoder",
	}
}

// ----------------------------------------------------------------------
// ECMWF Open Data — 0.25° global IFS HRES forecast. CCSDS/AEC packing
// (template 5.42) is now decoded in-tree; the model registry calls into
// fetchECMWFWind which uses the .index sidecar to range-GET just the
// 10u/10v messages out of the run's monolithic GRIB2 file rather than
// pulling the whole ~50 MB blob across the wire for every cache miss.
//
// Layout:
//   https://data.ecmwf.int/forecasts/{YYYYMMDD}/{HH}z/ifs/0p25/oper/
//     {YYYYMMDD}{HH}0000-{step}h-oper-fc.grib2
//     {YYYYMMDD}{HH}0000-{step}h-oper-fc.index   ← JSON-lines, byte ranges
//
// The .index file lists each GRIB2 message inside the .grib2 file with
// `_offset` and `_length` keys; we read it, find the 10u and 10v entries
// at levtype=sfc, and issue two Range-bounded GETs against the data
// file to pull just those messages.
// ----------------------------------------------------------------------

const ecmwfBaseURL = "https://data.ecmwf.int/forecasts/%s/%02dz/ifs/0p25/oper/%s%02d0000-%dh-oper-fc"

// ecmwfUserAgent identifies us to ECMWF Open Data's WAF. The default
// Go http.Client UA ("Go-http-client/1.1") trips a 429 rate limit
// almost immediately; ECMWF Open Data's documentation explicitly
// asks automated clients to send a descriptive UA with a contact URL.
const ecmwfUserAgent = "viam-chartplotter/0.1 (+https://github.com/erh/viam-chartplotter)"

// ecmwfIndexEntry is one JSON line out of the .index sidecar. ECMWF
// stamps extra keys on each line (class, expver, stream, …); we only
// care about the ones that identify a wind-at-10m message and the byte
// range to pull. Numeric fields come through as strings in the actual
// file, except for `_offset`/`_length` which are JSON numbers.
type ecmwfIndexEntry struct {
	Param   string `json:"param"`
	LevType string `json:"levtype"`
	Step    string `json:"step"`
	Offset  int64  `json:"_offset"`
	Length  int64  `json:"_length"`
}

func ecmwfWindModel() *WeatherModel {
	m := &WeatherModel{
		Name:        "ecmwf",
		DisplayName: "ECMWF Open Data (0.25° global)",
		Kind:        "wind",
		Domain:      "global",
		CycleHours:  []int{0, 6, 12, 18},
		MinFh:       0,
		// ECMWF Open Data publishes 0-144 at 3h steps, 150-240 at 6h.
		// Cap at 144 so the slider's step alignment stays uniform and
		// fh values in the always-3h zone all resolve to a real message.
		MaxFh:  144,
		StepFh: 3,
		// ECMWF Open Data lands ~7 h after the cycle hour (longer than
		// NOMADS' GFS); back off accordingly so WalkLatestCycle starts
		// at a run that's actually published.
		PublishLagH: 7,
	}
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]WindRecord, error) {
		// CachedFetchECMWFWind10m handles the raw-bytes disk cache
		// (project-wide rule: every upstream fetch goes through a
		// cache). On a cache hit, ECMWF isn't touched at all.
		grib, runT, err := WalkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
			return CachedFetchECMWFWind10m(ctx, client, t, fh)
		})
		if err != nil {
			return nil, err
		}
		return ParseECMWFWind10m(grib, runT, fh)
	}
	return m
}

// fetchECMWFWind10m fetches the .index, picks the 10u and 10v entries
// at levtype=sfc, range-GETs each message out of the run's .grib2 file,
// and returns a concatenated buffer the GRIB walker can decode as if
// it were a normal multi-message GRIB2 stream.
func fetchECMWFWind10m(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]byte, error) {
	date := runTime.Format("20060102")
	cc := runTime.Hour()
	base := fmt.Sprintf(ecmwfBaseURL, date, cc, date, cc, fh)
	indexURL := base + ".index"
	gribURL := base + ".grib2"

	idxBody, err := ecmwfGet(ctx, client, indexURL, "")
	if err != nil {
		return nil, fmt.Errorf("ECMWF index: %w", err)
	}
	uEntry, vEntry, err := pickECMWFWindEntries(idxBody)
	if err != nil {
		return nil, fmt.Errorf("ECMWF index pick: %w", err)
	}

	uMsg, err := ecmwfGetRange(ctx, client, gribURL, uEntry.Offset, uEntry.Length)
	if err != nil {
		return nil, fmt.Errorf("ECMWF 10u range: %w", err)
	}
	vMsg, err := ecmwfGetRange(ctx, client, gribURL, vEntry.Offset, vEntry.Length)
	if err != nil {
		return nil, fmt.Errorf("ECMWF 10v range: %w", err)
	}

	// Concatenate so ParseECMWFWind10m can walk both messages in a
	// single pass — both start with the GRIB magic so the walker's
	// position-relative parsing works unchanged.
	return append(uMsg, vMsg...), nil
}

// pickECMWFWindEntries scans the .index JSON-lines body for the 10u and
// 10v messages at levtype=sfc. Returns descriptive errors if either is
// missing so the cycle walk-back can try the previous run.
func pickECMWFWindEntries(idx []byte) (uEntry, vEntry ecmwfIndexEntry, err error) {
	var uHave, vHave bool
	sc := bufio.NewScanner(bytes.NewReader(idx))
	// .index lines for surface fields are short, but pressure-level
	// runs can include very long lines (every level on one row in some
	// dumps) — bump the scanner buffer so we don't get token-too-long
	// errors on those.
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e ecmwfIndexEntry
		if jerr := json.Unmarshal(line, &e); jerr != nil {
			// One bad line shouldn't doom the whole walk — skip.
			continue
		}
		if e.LevType != "sfc" {
			continue
		}
		switch e.Param {
		case "10u":
			uEntry = e
			uHave = true
		case "10v":
			vEntry = e
			vHave = true
		}
		if uHave && vHave {
			break
		}
	}
	if serr := sc.Err(); serr != nil {
		return uEntry, vEntry, fmt.Errorf("scan: %w", serr)
	}
	if !uHave || !vHave {
		return uEntry, vEntry, fmt.Errorf("missing 10u/10v in index (u=%v v=%v)", uHave, vHave)
	}
	return uEntry, vEntry, nil
}

// ecmwfGet issues a polite GET against data.ecmwf.int. ECMWF Open
// Data's WAF responds 429 to the default Go User-Agent within a few
// hits per minute; setting a descriptive UA per their docs avoids
// the throttle and matches what we ask of users of cmd/ecmwf-probe.
func ecmwfGet(ctx context.Context, client *http.Client, url, rangeHdr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ecmwfUserAgent)
	req.Header.Set("Accept", "*/*")
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ecmwfGet: %s range=%q dur=%s err=%v", url, rangeHdr, time.Since(start), err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		log.Printf("ecmwfGet: %s range=%q dur=%s status=%d", url, rangeHdr, time.Since(start), resp.StatusCode)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	log.Printf("ecmwfGet: %s range=%q dur=%s status=%d bytes=%d",
		url, rangeHdr, time.Since(start), resp.StatusCode, len(body))
	return body, err
}

// ecmwfGetRange is the byte-range variant — same UA-shaping as
// ecmwfGet but slices a 200-with-full-body response down to the
// requested window if a misbehaving cache ignored the Range header.
func ecmwfGetRange(ctx context.Context, client *http.Client, url string, offset, length int64) ([]byte, error) {
	body, err := ecmwfGet(ctx, client, url, fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > length {
		body = body[offset : offset+length]
	}
	if len(body) < 4 || string(body[:4]) != "GRIB" {
		return nil, fmt.Errorf("range response is not GRIB2 (size=%d, first=%q)",
			len(body), string(body[:min(4, len(body))]))
	}
	return body, nil
}

// ParseECMWFWind10m walks the (10u, 10v) concatenated GRIB2 buffer and
// emits the ol-wind 2-record JSON shape. ECMWF stamps centre=98 and
// parameter category=2 / numbers 2 (UGRD) and 3 (VGRD) so we can use
// the same wantWind filter as GFS, but we re-stamp the headers after
// decode just in case a future revision renames the params.
func ParseECMWFWind10m(grib []byte, runTime time.Time, fh int) ([]WindRecord, error) {
	wantWind := func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool {
		return paramCat == gribParamCatMomentum &&
			(paramNum == gribParamUGRD || paramNum == gribParamVGRD) &&
			surfType == gribSurfaceHeightAboveG &&
			surfValue == 10
	}
	var u, v *WindRecord
	for off := 0; off < len(grib); {
		end, rec, err := parseGRIBMessage(grib[off:], runTime, fh, wantWind)
		if err != nil {
			return nil, fmt.Errorf("ECMWF message @ off=%d: %w", off, err)
		}
		if rec != nil {
			switch rec.Header.ParameterNumber {
			case gribParamUGRD:
				u = rec
			case gribParamVGRD:
				v = rec
			}
		}
		if end <= 0 {
			break
		}
		off += end
	}
	if u == nil || v == nil {
		return nil, fmt.Errorf("ECMWF: missing 10u/10v (u=%v v=%v)", u != nil, v != nil)
	}
	return []WindRecord{*u, *v}, nil
}

func iconGlobalWindStub() *WeatherModel {
	return &WeatherModel{
		Name:        "icon",
		DisplayName: "ICON (DWD, 0.125° global)",
		Kind:        "wind",
		Domain:      "global",
		Disabled:    true,
		Reason:      "needs icosahedral→latlon regrid (DWD ships native unstructured grid)",
	}
}

// noaaGLMLightningStub registers the GOES-East GLM lightning option in
// the picker so it surfaces in the layer panel under the weather
// group, but flips Disabled so toggling it on shows the Reason
// (matching the NAM/ECMWF/ICON pattern). The actual GLM L2 LCFA feed
// is NetCDF4/HDF5 over the NESDIS S3 mirror — decoding that is a
// separate work item.
func noaaGLMLightningStub() *WeatherModel {
	return &WeatherModel{
		Name:        "noaa-glm",
		DisplayName: "Lightning (GOES-East GLM, near-real-time)",
		Kind:        "lightning",
		Domain:      "americas",
		Disabled:    true,
		Reason:      "needs GOES-16/18 GLM NetCDF4 decoder (L2 LCFA strokes)",
	}
}

// ----------------------------------------------------------------------
// JSON shape served by /noaa-weather/models
// ----------------------------------------------------------------------

type modelMeta struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Kind        string `json:"kind"`
	Domain      string `json:"domain"`
	MinFh       int    `json:"minFh"`
	MaxFh       int    `json:"maxFh"`
	StepFh      int    `json:"stepFh"`
	Disabled    bool   `json:"disabled,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func modelMetaList() []modelMeta {
	out := make([]modelMeta, 0, len(allModels))
	for _, m := range allModels {
		out = append(out, modelMeta{
			Name:        m.Name,
			DisplayName: m.DisplayName,
			Kind:        m.Kind,
			Domain:      m.Domain,
			MinFh:       m.MinFh,
			MaxFh:       m.MaxFh,
			StepFh:      m.StepFh,
			Disabled:    m.Disabled,
			Reason:      m.Reason,
		})
	}
	return out
}

// minMax returns the min and max of a non-empty float slice. Used by
// the HRRR diagnostic logging to sanity-check that unpacked GRIB
// values land in the m/s range we expect (typically ±30).
func minMax(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	mn, mx := xs[0], xs[0]
	for _, x := range xs {
		if math.IsNaN(x) {
			continue
		}
		if x < mn {
			mn = x
		}
		if x > mx {
			mx = x
		}
	}
	return mn, mx
}

// Sanity check at init: model names must be URL-safe (no '/' or
// whitespace) since they slot directly into /noaa-weather/data/{name}/.
func init() {
	seen := map[string]bool{}
	for _, m := range allModels {
		if seen[m.Name] {
			panic(fmt.Sprintf("duplicate weather model name: %s", m.Name))
		}
		seen[m.Name] = true
		if strings.ContainsAny(m.Name, "/ \t") {
			panic(fmt.Sprintf("weather model name %q must be URL-safe", m.Name))
		}
	}
}
