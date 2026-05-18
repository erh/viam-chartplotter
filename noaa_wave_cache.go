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

// PacIOOS hosts the NOAA WaveWatch III global "best" forecast over a
// THREDDS server. Its WMS endpoint serves heatmap tiles but the data
// values themselves come through OPeNDAP. We use the DODS binary
// endpoint (XDR-encoded) to pull Thgt (significant wave height, m) +
// Tdir (primary wave direction, degrees from-which-waves-come) at a
// single time slice, then convert to a u/v vector grid in the same
// JSON shape ol-wind consumes — so the frontend can render animated
// wave-direction particles just like it does for wind.
//
// Why not GFSWAVE from NOMADS? It's encoded with GRIB2 template 5.40
// (JPEG2000) which our pure-Go parser can't decode. PacIOOS's THREDDS
// also serves WaveWatch III, but in friendlier binary formats.

const (
	pacioosWaveDataset = "https://pae-paha.pacioos.hawaii.edu/thredds/dodsC/ww3_global/WaveWatch_III_Global_Wave_Model_best.ncd"
	pacioosWaveLat     = 311 // -77.5..77.5 step 0.5
	pacioosWaveLon     = 720 // 0..359.5 step 0.5
)

// fetchPacIOOSWave pulls the latest Thgt + Tdir slice and returns
// 2 windRecords (u + v) ready to JSON-encode for ol-wind.
//
// `forecastHour` is treated as a hint: PacIOOS exposes ~hourly slices
// of the "best" forecast, so we map (gfs run + fh) → nearest available
// time index. Returns the actual data refTime via the records' header.
func fetchPacIOOSWave(ctx context.Context, client *http.Client, target time.Time) ([]windRecord, error) {
	timeAxis, err := fetchWaveTimeAxis(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("wave time axis: %w", err)
	}
	if len(timeAxis) == 0 {
		return nil, fmt.Errorf("wave dataset has no time values")
	}
	timeIdx, refTime := nearestWaveTimeIndex(timeAxis, target)

	thgt, tdir, err := fetchWaveSlice(ctx, client, timeIdx)
	if err != nil {
		return nil, err
	}

	n := pacioosWaveLat * pacioosWaveLon
	if len(thgt) != n || len(tdir) != n {
		return nil, fmt.Errorf("wave slice shape mismatch: thgt=%d tdir=%d expected=%d",
			len(thgt), len(tdir), n)
	}

	// Convert (height, direction-from) → (u, v) vectors and flip the
	// lat axis to la1=top convention while we're at it. PacIOOS stores
	// data bottom-up (row 0 = -77.5°S), but the JSON header below
	// advertises la1=+77.5° (north-on-top, matching GFS wind) so the
	// frontend's flipY:true does the right thing. Without this row
	// reversal, every northern-hemisphere lookup actually samples the
	// southern hemisphere — the Roaring Forties were rendering as 40°N.
	//
	// ol-wind interprets particle motion as moving in the +u/+v
	// direction; we want waves to drift toward (dir + 180°), so
	// u = -h·sin(dir·π/180), v = -h·cos(dir·π/180).
	uData := make([]float64, n)
	vData := make([]float64, n)
	for srcRow := 0; srcRow < pacioosWaveLat; srcRow++ {
		dstRow := pacioosWaveLat - 1 - srcRow
		for col := 0; col < pacioosWaveLon; col++ {
			srcIdx := srcRow*pacioosWaveLon + col
			dstIdx := dstRow*pacioosWaveLon + col
			h := float64(thgt[srcIdx])
			d := float64(tdir[srcIdx])
			// PacIOOS uses ~9.999e20 to encode missing values over land
			// / outside the model domain. Zero them so the colour ramp
			// and particle seeding don't blow up.
			if math.IsNaN(h) || h < 0 || h > 1e6 || math.IsNaN(d) || d < 0 || d > 360 {
				continue
			}
			rad := d * math.Pi / 180
			uData[dstIdx] = -h * math.Sin(rad)
			vData[dstIdx] = -h * math.Cos(rad)
		}
	}

	// Headers advertise north-on-top so the frontend's flipY:true
	// indexes from la1 (top) downward; data above has been row-reversed
	// to match.
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
		Nx:                         pacioosWaveLon,
		Ny:                         pacioosWaveLat,
		Lo1:                        0,
		La1:                        77.5,
		Lo2:                        359.5,
		La2:                        -77.5,
		Dx:                         0.5,
		Dy:                         0.5,
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

// PacIOOS encodes the WW3 best-forecast time axis as "hours since
// 2017-01-01 00:00:00 UTC". See:
//
//	curl https://pae-paha.pacioos.hawaii.edu/thredds/dodsC/ww3_global/WaveWatch_III_Global_Wave_Model_best.ncd.das
var waveTimeBase = time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC)

// nearestWaveTimeIndex picks the index whose time value is closest to
// `target`, returning the index and the resolved time.
func nearestWaveTimeIndex(axis []float64, target time.Time) (int, time.Time) {
	if len(axis) == 0 {
		return 0, waveTimeBase
	}
	want := target.Sub(waveTimeBase).Hours()
	best := 0
	bestDelta := math.Abs(axis[0] - want)
	for i := 1; i < len(axis); i++ {
		d := math.Abs(axis[i] - want)
		if d < bestDelta {
			best = i
			bestDelta = d
		}
	}
	chosen := waveTimeBase.Add(time.Duration(axis[best]*float64(time.Hour)) /* hours */)
	return best, chosen
}

// fetchWaveTimeAxis pulls just the `time` 1-D variable via OPeNDAP DODS
// — small payload (~640 KB float64 array) we use to map the user's
// requested forecast time to an actual model time slice.
func fetchWaveTimeAxis(ctx context.Context, client *http.Client) ([]float64, error) {
	url := pacioosWaveDataset + ".dods?time"
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

// fetchWaveSlice pulls the Thgt + Tdir grids for a single time index
// at every lat/lon. ~1.8 MB total.
func fetchWaveSlice(ctx context.Context, client *http.Client, timeIdx int) ([]float32, []float32, error) {
	q := fmt.Sprintf(
		"Thgt[%d:%d][0:0][0:%d][0:%d],Tdir[%d:%d][0:0][0:%d][0:%d]",
		timeIdx, timeIdx, pacioosWaveLat-1, pacioosWaveLon-1,
		timeIdx, timeIdx, pacioosWaveLat-1, pacioosWaveLon-1,
	)
	url := pacioosWaveDataset + ".dods?" + urlEncodeBrackets(q)
	body, err := opendapGet(ctx, client, url)
	if err != nil {
		return nil, nil, err
	}
	dataOff, err := dodsDataOffset(body)
	if err != nil {
		return nil, nil, err
	}
	br := newDODSReader(body[dataOff:])
	// Each Grid is: ARRAY (Float32) then 4 MAPS (time Float64, z Float32,
	// lat Float32, lon Float32). We only care about the ARRAYs.
	thgt, err := br.readF32Array()
	if err != nil {
		return nil, nil, fmt.Errorf("thgt array: %w", err)
	}
	// Skip Thgt's maps.
	if _, err := br.readF64Array(); err != nil {
		return nil, nil, err
	}
	if _, err := br.readF32Array(); err != nil {
		return nil, nil, err
	}
	if _, err := br.readF32Array(); err != nil {
		return nil, nil, err
	}
	if _, err := br.readF32Array(); err != nil {
		return nil, nil, err
	}
	tdir, err := br.readF32Array()
	if err != nil {
		return nil, nil, fmt.Errorf("tdir array: %w", err)
	}
	return thgt, tdir, nil
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
