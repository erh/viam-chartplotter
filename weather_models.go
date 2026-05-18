package vc

import (
	"bytes"
	"compress/bzip2"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

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
	Fetch func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]windRecord, error)
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
	ecmwfWindStub(),
	iconGlobalWindStub(),
	pacioosWaveModel(),
	pacioosHawaiiWaveModel(),
	nomadsGFSWaveModel(),
}

// findModel does an O(n) lookup — registry is fixed small (~10 entries).
func findModel(name string) *WeatherModel {
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

// walkLatestCycle walks back through `m.CycleHours` (every 24h ÷ N
// hours apart) until `fetch` returns a non-error body for that
// candidate run. Returns the body + the run time we hit.
func walkLatestCycle(
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
	candidate := mostRecentCycle(now, m.CycleHours)
	var lastErr error
	for i := 0; i < 4; i++ {
		body, err := fetch(ctx, candidate)
		if err == nil {
			return body, candidate, nil
		}
		lastErr = err
		// Step back to the prior cycle. CycleHours are sorted; find the
		// previous slot, jumping a day if we wrap.
		candidate = previousCycle(candidate, m.CycleHours)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no run available for %s in last 24h", m.Name)
	}
	return nil, time.Time{}, lastErr
}

func mostRecentCycle(now time.Time, cycles []int) time.Time {
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

func previousCycle(t time.Time, cycles []int) time.Time {
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
// walkLatestCycle backoff prints something useful.
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
// the first message whose product matches `want` into a windRecord.
// Used by every regular_ll model (GFS, ICON-EU, …) — they only differ
// in which (paramCat, paramNum) they ask for. Returns the first
// matching record or nil; pass twice with different filters to pull
// UGRD + VGRD separately.
func decodeRegularLLMessage(
	grib []byte,
	runTime time.Time,
	fh int,
	want func(discipline, paramCat, paramNum, surfType int, surfValue float64) bool,
) (*windRecord, error) {
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
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]windRecord, error) {
		body, runT, err := walkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
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
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]windRecord, error) {
		body, runT, err := walkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
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
func decodeHRRRWind(grib []byte, runTime time.Time, fh int) ([]windRecord, error) {
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
	return []windRecord{
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
	m.Fetch = func(ctx context.Context, client *http.Client, runTime time.Time, fh int) ([]windRecord, error) {
		// ICON-EU ships U_10M and V_10M as two independent .grib2.bz2
		// files — we fetch them in series (cycle walk-back uses the
		// U_10M file as the probe, and assumes V_10M ships at the same
		// time).
		var bodyU, bodyV []byte
		_, runT, err := walkLatestCycle(ctx, m, fh, func(ctx context.Context, t time.Time) ([]byte, error) {
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
		return []windRecord{*uRec, *vRec}, nil
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
	m.Fetch = func(ctx context.Context, client *http.Client, _ time.Time, fh int) ([]windRecord, error) {
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

func ecmwfWindStub() *WeatherModel {
	return &WeatherModel{
		Name:        "ecmwf",
		DisplayName: "ECMWF Open Data (0.25° global)",
		Kind:        "wind",
		Domain:      "global",
		Disabled:    true,
		Reason:      "needs CCSDS/AEC (GRIB2 template 5.42) decoder + .index byte-range subsetting",
	}
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
