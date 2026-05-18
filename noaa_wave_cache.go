package vc

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Wave forecasts come from any OPeNDAP-served THREDDS dataset that
// publishes significant-wave-height + primary-wave-direction grids.
// PacIOOS hosts NOAA WaveWatch III in friendly binary DODS form (no
// GRIB / no JPEG2000), and the same pattern works for NOAA NOMADS'
// GrADS DODS server, NCEI's archive, ERDDAP, etc. — every dataset
// just plugs different field names / dims / time-encoding into
// waveDatasetConfig and reuses the fetch + transform pipeline.

// waveDatasetConfig captures everything fetchWaveDataset needs to know
// about one OPeNDAP wave product: where to fetch, what the variables
// are called, and how to interpret the grid.
type waveDatasetConfig struct {
	// URL is the dataset's DODS root (we'll suffix .dods + a query).
	URL string
	// Nlat / Nlon are the grid dimensions. Pre-known so we can size
	// the slice query right and validate the response shape.
	Nlat, Nlon int
	// LatS / LatN / LonW / LonE bound the grid in degrees. LonW may
	// be 0 (datasets in 0..360 convention) or negative (-180..180);
	// the output header advertises it as-is so ol-wind's wrapX deals
	// with antimeridian crossings the same way it does for GFS wind.
	LatS, LatN, LonW, LonE float64
	// Dlat / Dlon are the cell spacing — derived from the bounds but
	// stored explicitly so headers don't need re-derivation.
	Dlat, Dlon float64
	// HeightVar / DirVar name the height + "direction-from" arrays in
	// the dataset. PacIOOS uses Thgt/Tdir, NCEP NOMADS uses
	// htsgwsfc/dirpwsfc, etc.
	HeightVar, DirVar string
	// TimeVar is the time-axis variable name. Almost always "time".
	TimeVar string
	// TimeBase + TimeUnit describe the time axis. TimeUnit is either
	// "hours" or "days" — we multiply axis values by it to get a
	// duration from TimeBase. We don't try to be a full udunits
	// implementation; THREDDS publishers in this space stick to
	// these two units.
	TimeBase time.Time
	TimeUnit time.Duration
	// LatBottomUp = true when the dataset's row 0 is the *southern*
	// edge (PacIOOS does this). We row-reverse during transform so
	// the output header can always advertise La1=north / La2=south
	// regardless of the upstream's convention.
	LatBottomUp bool
	// SlicePrefix is the variable-index prefix between the time
	// dimension and the lat/lon dimensions. PacIOOS WW3 has a
	// `z` level dimension between time and lat — its slice query
	// reads `Thgt[t:t][0:0][...][...]`. NCEP NOMADS gfswave is
	// surface-only and has no extra dim, so its slice is just
	// `htsgwsfc[t:t][...][...]`. Encode the in-between brackets
	// here (e.g. "[0:0]" or "").
	SlicePrefix string
	// MapsPerVar is the number of MAP arrays after each Grid's main
	// ARRAY in the DODS response. PacIOOS WW3 has 4 (time, z, lat,
	// lon); a surface-only dataset has 3 (time, lat, lon). We skip
	// these between consecutive variables in fetchWaveSlice.
	MapsPerVar int
	// DirIsToward = true when the direction variable encodes
	// "direction waves are propagating toward" (e.g. some SWAN
	// products). Default false = "direction from which waves come"
	// (WMO standard, what most WW3 outputs use). The u/v formula
	// inverts the sign when this is true so particle motion always
	// flows toward (current + 180° of "from-dir").
	DirIsToward bool
	// MissingMax: values above this are treated as missing (PacIOOS
	// uses ~9.999e20). 0 disables the check.
	MissingMax float64
}

// pacioosGlobalConfig is the original WW3 0.5° global dataset we've
// been serving — the baseline wave forecast.
func pacioosGlobalConfig() waveDatasetConfig {
	return waveDatasetConfig{
		URL:         "https://pae-paha.pacioos.hawaii.edu/thredds/dodsC/ww3_global/WaveWatch_III_Global_Wave_Model_best.ncd",
		Nlat:        311,
		Nlon:        720,
		LatS:        -77.5,
		LatN:        77.5,
		LonW:        0,
		LonE:        359.5,
		Dlat:        0.5,
		Dlon:        0.5,
		HeightVar:   "Thgt",
		DirVar:      "Tdir",
		TimeVar:     "time",
		TimeBase:    time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC),
		TimeUnit:    time.Hour,
		LatBottomUp: true,
		SlicePrefix: "[0:0]", // PacIOOS WW3 has a z dimension
		MapsPerVar:  4,
		MissingMax:  1e6,
	}
}

// fetchPacIOOSWave is a thin alias for the global WW3 dataset — kept
// for callers that haven't migrated to fetchWaveDataset yet.
func fetchPacIOOSWave(ctx context.Context, client *http.Client, target time.Time) ([]windRecord, error) {
	return fetchWaveDataset(ctx, client, pacioosGlobalConfig(), target)
}

// fetchWaveDataset pulls Hgt + Dir for one (config, target time) and
// returns 2 windRecords (u + v) ready to JSON-encode for ol-wind.
// `target` is a hint: we pick the dataset slice closest to it.
func fetchWaveDataset(ctx context.Context, client *http.Client, cfg waveDatasetConfig, target time.Time) ([]windRecord, error) {
	timeAxis, err := fetchWaveTimeAxis(ctx, client, cfg)
	if err != nil {
		return nil, fmt.Errorf("wave time axis: %w", err)
	}
	if len(timeAxis) == 0 {
		return nil, fmt.Errorf("wave dataset has no time values")
	}
	timeIdx, refTime := nearestWaveTimeIndex(timeAxis, target, cfg)

	hgt, dir, err := fetchWaveSlice(ctx, client, cfg, timeIdx)
	if err != nil {
		return nil, err
	}

	n := cfg.Nlat * cfg.Nlon
	if len(hgt) != n || len(dir) != n {
		return nil, fmt.Errorf("wave slice shape mismatch: hgt=%d dir=%d expected=%d",
			len(hgt), len(dir), n)
	}

	// Convert (height, direction) → (u, v) vectors. Row-reverse when
	// the dataset stores rows bottom-up so the output header can
	// always advertise La1=north / La2=south regardless of upstream
	// convention — that keeps the frontend's flipY:true uniform
	// across datasets. Without the reversal, every northern-hemisphere
	// lookup samples the southern hemisphere (the bug that hit the
	// Roaring Forties at 40°N).
	//
	// ol-wind reads particle motion as flowing in the +u/+v direction.
	// For "from" direction (WMO standard): u = -h·sin(d·π/180),
	// v = -h·cos(d·π/180) so particles drift toward (d + 180°).
	// For "toward" direction: just sin/cos.
	uData := make([]float64, n)
	vData := make([]float64, n)
	sign := -1.0
	if cfg.DirIsToward {
		sign = 1.0
	}
	for srcRow := 0; srcRow < cfg.Nlat; srcRow++ {
		dstRow := srcRow
		if cfg.LatBottomUp {
			dstRow = cfg.Nlat - 1 - srcRow
		}
		for col := 0; col < cfg.Nlon; col++ {
			srcIdx := srcRow*cfg.Nlon + col
			dstIdx := dstRow*cfg.Nlon + col
			h := float64(hgt[srcIdx])
			d := float64(dir[srcIdx])
			if math.IsNaN(h) || h < 0 ||
				(cfg.MissingMax > 0 && h > cfg.MissingMax) ||
				math.IsNaN(d) || d < 0 || d > 360 {
				continue
			}
			rad := d * math.Pi / 180
			uData[dstIdx] = sign * h * math.Sin(rad)
			vData[dstIdx] = sign * h * math.Cos(rad)
		}
	}

	hdr := windHeader{
		Discipline:                 0,
		Center:                     7,
		RefTime:                    refTime.Format("2006-01-02T15:04:05.000Z"),
		ForecastTime:               0,
		ParameterCategory:          gribParamCatMomentum,
		ParameterNumber:            gribParamUGRD,
		ParameterUnit:              "m",
		Surface1Type:               1, // ground or water surface
		Surface1Value:              0,
		GridDefinitionTemplateName: "regular_ll",
		Nx:                         cfg.Nlon,
		Ny:                         cfg.Nlat,
		Lo1:                        cfg.LonW,
		La1:                        cfg.LatN, // north on top, post-flip
		Lo2:                        cfg.LonE,
		La2:                        cfg.LatS,
		Dx:                         cfg.Dlon,
		Dy:                         cfg.Dlat,
		ScanMode:                   0,
	}
	uHdr := hdr
	vHdr := hdr
	vHdr.ParameterNumber = gribParamVGRD
	return []windRecord{
		{Header: uHdr, Data: uData},
		{Header: vHdr, Data: vData},
	}, nil
}

// nearestWaveTimeIndex picks the time-axis slot closest to `target`,
// returning that index and the resolved real-world time. The dataset's
// time encoding (units + base) is supplied via cfg so callers don't
// have to know whether a particular axis is "hours since 2017-01-01"
// or "days since 1-1-1".
func nearestWaveTimeIndex(axis []float64, target time.Time, cfg waveDatasetConfig) (int, time.Time) {
	if len(axis) == 0 {
		return 0, cfg.TimeBase
	}
	unitH := float64(cfg.TimeUnit) / float64(time.Hour)
	want := target.Sub(cfg.TimeBase).Hours() / unitH
	best := 0
	bestDelta := math.Abs(axis[0] - want)
	for i := 1; i < len(axis); i++ {
		d := math.Abs(axis[i] - want)
		if d < bestDelta {
			best = i
			bestDelta = d
		}
	}
	chosen := cfg.TimeBase.Add(time.Duration(axis[best] * float64(cfg.TimeUnit)))
	return best, chosen
}

// fetchWaveTimeAxis pulls the dataset's 1-D time array via OPeNDAP
// DODS. Small payload that we use to map the user's requested forecast
// time to an actual model time slice.
func fetchWaveTimeAxis(ctx context.Context, client *http.Client, cfg waveDatasetConfig) ([]float64, error) {
	url := cfg.URL + ".dods?" + cfg.TimeVar
	body, err := opendapGet(ctx, client, url)
	if err != nil {
		return nil, err
	}
	dataOff, err := dodsDataOffset(body)
	if err != nil {
		return nil, err
	}
	br := newDODSReader(body[dataOff:])
	return br.readF64Array()
}

// fetchWaveSlice pulls Hgt + Dir grids for a single time index at every
// lat/lon. Skips cfg.MapsPerVar trailing MAP arrays between the two
// variables (each Grid is ARRAY then maps for time / [z] / lat / lon).
func fetchWaveSlice(ctx context.Context, client *http.Client, cfg waveDatasetConfig, timeIdx int) ([]float32, []float32, error) {
	slice := fmt.Sprintf("[%d:%d]%s[0:%d][0:%d]",
		timeIdx, timeIdx, cfg.SlicePrefix, cfg.Nlat-1, cfg.Nlon-1)
	q := fmt.Sprintf("%s%s,%s%s", cfg.HeightVar, slice, cfg.DirVar, slice)
	url := cfg.URL + ".dods?" + urlEncodeBrackets(q)
	body, err := opendapGet(ctx, client, url)
	if err != nil {
		return nil, nil, err
	}
	dataOff, err := dodsDataOffset(body)
	if err != nil {
		return nil, nil, err
	}
	br := newDODSReader(body[dataOff:])
	hgt, err := br.readF32Array()
	if err != nil {
		return nil, nil, fmt.Errorf("%s array: %w", cfg.HeightVar, err)
	}
	// Skip Hgt's MAP arrays before reaching Dir's ARRAY. Maps are
	// either Float32 (lat / lon / z) or Float64 (time); always one
	// Float64 first (the time axis) then Float32 for the rest.
	if cfg.MapsPerVar > 0 {
		if _, err := br.readF64Array(); err != nil {
			return nil, nil, err
		}
		for i := 1; i < cfg.MapsPerVar; i++ {
			if _, err := br.readF32Array(); err != nil {
				return nil, nil, err
			}
		}
	}
	dir, err := br.readF32Array()
	if err != nil {
		return nil, nil, fmt.Errorf("%s array: %w", cfg.DirVar, err)
	}
	return hgt, dir, nil
}

// opendapGet fetches an OPeNDAP URL and returns the raw body. THREDDS
// 4xx responses embed the actual error message in the body so we
// surface that instead of just the HTTP code.
func opendapGet(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(body)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("opendap %s: %d %s", url, resp.StatusCode, msg)
	}
	return body, nil
}

// dodsDataOffset returns the index of the first XDR-encoded byte in a
// `.dods` response, i.e. the byte after the "Data:\n" marker.
func dodsDataOffset(body []byte) (int, error) {
	marker := []byte("\nData:\n")
	i := indexOf(body, marker)
	if i < 0 {
		// Some servers use CRLF.
		marker = []byte("\nData:\r\n")
		i = indexOf(body, marker)
	}
	if i < 0 {
		return 0, fmt.Errorf("missing Data: marker in DODS response")
	}
	return i + len(marker), nil
}

func indexOf(haystack, needle []byte) int {
	return strings.Index(string(haystack), string(needle))
}

// urlEncodeBrackets percent-encodes `[`, `]`, `,` so curl-friendly
// query strings work via Go's HTTP client (which doesn't auto-encode
// these for raw URLs).
func urlEncodeBrackets(s string) string {
	r := strings.NewReplacer("[", "%5B", "]", "%5D", ",", "%2C")
	return r.Replace(s)
}

// dodsReader is a tiny helper that knows how to walk OPeNDAP XDR data.
// Each top-level array in a DODS payload starts with two big-endian
// uint32 length fields (length-and-length, weird but documented) and
// then `len` values; floats are 4 bytes, doubles 8 bytes, both
// big-endian. Arrays don't get any padding because their element widths
// already align to 4 bytes.
type dodsReader struct {
	b   []byte
	pos int
}

func newDODSReader(b []byte) *dodsReader { return &dodsReader{b: b} }

func (r *dodsReader) readUint32() (uint32, error) {
	if r.pos+4 > len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.BigEndian.Uint32(r.b[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *dodsReader) readF32Array() ([]float32, error) {
	n1, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	n2, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	if n1 != n2 {
		return nil, fmt.Errorf("dods float32 array length mismatch: %d vs %d", n1, n2)
	}
	out := make([]float32, n1)
	if r.pos+int(n1)*4 > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	for i := uint32(0); i < n1; i++ {
		bits := binary.BigEndian.Uint32(r.b[r.pos : r.pos+4])
		out[i] = math.Float32frombits(bits)
		r.pos += 4
	}
	return out, nil
}

func (r *dodsReader) readF64Array() ([]float64, error) {
	n1, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	n2, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	if n1 != n2 {
		return nil, fmt.Errorf("dods float64 array length mismatch: %d vs %d", n1, n2)
	}
	out := make([]float64, n1)
	if r.pos+int(n1)*8 > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	for i := uint32(0); i < n1; i++ {
		bits := binary.BigEndian.Uint64(r.b[r.pos : r.pos+8])
		out[i] = math.Float64frombits(bits)
		r.pos += 8
	}
	return out, nil
}

// Sanity: strconv import keeps Go happy even if a future edit drops
// the only caller from this file.
var _ = strconv.Itoa
