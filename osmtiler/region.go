package osmtiler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.viam.com/rdk/logging"
)

// Region is one row in the static download catalog: a named geographic
// extract with a lon/lat bbox and a stable upstream URL.
//
// We default to Geofabrik state-level extracts because they're the
// canonical "standard OSM" distribution (download pages from
// openstreetmap.org point here) and the per-state granularity matches
// US chartplotter use: a boat in NY waters needs the NY PBF, not
// the whole-US one.
type Region struct {
	Key                            string // stable filesystem-safe key, used as cache filename
	Name                           string // human-readable
	MinLon, MinLat, MaxLon, MaxLat float64
	URL                            string // direct .osm.pbf download
}

// KnownRegions covers the US coastal states most relevant to the
// chartplotter. Bounding boxes are deliberately generous so a tile
// that lies a few miles offshore still maps to the right state. When
// two regions overlap (state borders), the first match in this slice
// wins — order from smaller / more-coastal toward larger so harbor
// charts hit the most specific extract first.
var KnownRegions = []Region{
	// East Coast, north to south.
	{Key: "us-maine", Name: "Maine",
		MinLon: -71.10, MaxLon: -66.93, MinLat: 42.97, MaxLat: 47.46,
		URL: "https://download.geofabrik.de/north-america/us/maine-latest.osm.pbf"},
	{Key: "us-new-hampshire", Name: "New Hampshire",
		MinLon: -72.56, MaxLon: -70.61, MinLat: 42.70, MaxLat: 45.31,
		URL: "https://download.geofabrik.de/north-america/us/new-hampshire-latest.osm.pbf"},
	{Key: "us-massachusetts", Name: "Massachusetts",
		MinLon: -73.51, MaxLon: -69.93, MinLat: 41.24, MaxLat: 42.89,
		URL: "https://download.geofabrik.de/north-america/us/massachusetts-latest.osm.pbf"},
	{Key: "us-rhode-island", Name: "Rhode Island",
		MinLon: -71.91, MaxLon: -71.12, MinLat: 41.15, MaxLat: 42.02,
		URL: "https://download.geofabrik.de/north-america/us/rhode-island-latest.osm.pbf"},
	{Key: "us-connecticut", Name: "Connecticut",
		MinLon: -73.73, MaxLon: -71.79, MinLat: 40.99, MaxLat: 42.05,
		URL: "https://download.geofabrik.de/north-america/us/connecticut-latest.osm.pbf"},
	{Key: "us-new-york", Name: "New York",
		MinLon: -79.76, MaxLon: -71.85, MinLat: 40.49, MaxLat: 45.02,
		URL: "https://download.geofabrik.de/north-america/us/new-york-latest.osm.pbf"},
	{Key: "us-new-jersey", Name: "New Jersey",
		MinLon: -75.56, MaxLon: -73.89, MinLat: 38.93, MaxLat: 41.36,
		URL: "https://download.geofabrik.de/north-america/us/new-jersey-latest.osm.pbf"},
	{Key: "us-delaware", Name: "Delaware",
		MinLon: -75.79, MaxLon: -74.98, MinLat: 38.45, MaxLat: 39.84,
		URL: "https://download.geofabrik.de/north-america/us/delaware-latest.osm.pbf"},
	{Key: "us-maryland", Name: "Maryland",
		MinLon: -79.49, MaxLon: -74.99, MinLat: 37.89, MaxLat: 39.72,
		URL: "https://download.geofabrik.de/north-america/us/maryland-latest.osm.pbf"},
	{Key: "us-virginia", Name: "Virginia",
		MinLon: -83.68, MaxLon: -75.16, MinLat: 36.54, MaxLat: 39.47,
		URL: "https://download.geofabrik.de/north-america/us/virginia-latest.osm.pbf"},
	{Key: "us-north-carolina", Name: "North Carolina",
		MinLon: -84.32, MaxLon: -75.40, MinLat: 33.75, MaxLat: 36.59,
		URL: "https://download.geofabrik.de/north-america/us/north-carolina-latest.osm.pbf"},
	{Key: "us-south-carolina", Name: "South Carolina",
		MinLon: -83.36, MaxLon: -78.50, MinLat: 32.03, MaxLat: 35.22,
		URL: "https://download.geofabrik.de/north-america/us/south-carolina-latest.osm.pbf"},
	{Key: "us-georgia", Name: "Georgia",
		MinLon: -85.61, MaxLon: -80.75, MinLat: 30.31, MaxLat: 35.00,
		URL: "https://download.geofabrik.de/north-america/us/georgia-latest.osm.pbf"},
	{Key: "us-florida", Name: "Florida",
		MinLon: -87.64, MaxLon: -79.97, MinLat: 24.39, MaxLat: 31.00,
		URL: "https://download.geofabrik.de/north-america/us/florida-latest.osm.pbf"},
	// Gulf.
	{Key: "us-alabama", Name: "Alabama",
		MinLon: -88.47, MaxLon: -84.89, MinLat: 30.18, MaxLat: 35.01,
		URL: "https://download.geofabrik.de/north-america/us/alabama-latest.osm.pbf"},
	{Key: "us-mississippi", Name: "Mississippi",
		MinLon: -91.66, MaxLon: -88.10, MinLat: 30.17, MaxLat: 35.00,
		URL: "https://download.geofabrik.de/north-america/us/mississippi-latest.osm.pbf"},
	{Key: "us-louisiana", Name: "Louisiana",
		MinLon: -94.04, MaxLon: -88.81, MinLat: 28.86, MaxLat: 33.02,
		URL: "https://download.geofabrik.de/north-america/us/louisiana-latest.osm.pbf"},
	{Key: "us-texas", Name: "Texas",
		MinLon: -106.65, MaxLon: -93.51, MinLat: 25.84, MaxLat: 36.50,
		URL: "https://download.geofabrik.de/north-america/us/texas-latest.osm.pbf"},
	// West Coast.
	{Key: "us-california", Name: "California",
		MinLon: -124.48, MaxLon: -114.13, MinLat: 32.53, MaxLat: 42.01,
		URL: "https://download.geofabrik.de/north-america/us/california-latest.osm.pbf"},
	{Key: "us-oregon", Name: "Oregon",
		MinLon: -124.57, MaxLon: -116.46, MinLat: 41.99, MaxLat: 46.30,
		URL: "https://download.geofabrik.de/north-america/us/oregon-latest.osm.pbf"},
	{Key: "us-washington", Name: "Washington",
		MinLon: -124.85, MaxLon: -116.92, MinLat: 45.54, MaxLat: 49.00,
		URL: "https://download.geofabrik.de/north-america/us/washington-latest.osm.pbf"},
	{Key: "us-alaska", Name: "Alaska",
		MinLon: -179.15, MaxLon: -129.97, MinLat: 51.21, MaxLat: 71.41,
		URL: "https://download.geofabrik.de/north-america/us/alaska-latest.osm.pbf"},
	{Key: "us-hawaii", Name: "Hawaii",
		MinLon: -178.45, MaxLon: -154.74, MinLat: 18.86, MaxLat: 28.52,
		URL: "https://download.geofabrik.de/north-america/us/hawaii-latest.osm.pbf"},
}

// RegionFor returns the first region whose bbox contains (lon, lat),
// or nil if none does.
func RegionFor(lon, lat float64) *Region {
	for i := range KnownRegions {
		r := &KnownRegions[i]
		if lon >= r.MinLon && lon <= r.MaxLon && lat >= r.MinLat && lat <= r.MaxLat {
			return r
		}
	}
	return nil
}

// RegionForBBox returns the best region for a tile with the supplied
// bbox. Selection order:
//
//  1. Region whose bbox contains the tile's centre (the "right"
//     state — for a tile in CT/MA border waters whose centre is in
//     CT, prefer CT even if MA is earlier in the table).
//  2. Region whose bbox overlaps the tile bbox (offshore coastal
//     tiles whose centre falls between every state's bbox).
//  3. nil — no known region covers this tile.
//
// Use this in the tile render path instead of point-based RegionFor.
func RegionForBBox(minLon, minLat, maxLon, maxLat float64) *Region {
	cLon := (minLon + maxLon) / 2
	cLat := (minLat + maxLat) / 2
	for i := range KnownRegions {
		r := &KnownRegions[i]
		if cLon >= r.MinLon && cLon <= r.MaxLon && cLat >= r.MinLat && cLat <= r.MaxLat {
			return r
		}
	}
	for i := range KnownRegions {
		r := &KnownRegions[i]
		if minLon <= r.MaxLon && maxLon >= r.MinLon &&
			minLat <= r.MaxLat && maxLat >= r.MinLat {
			return r
		}
	}
	return nil
}

// RegionsForBBox returns every region whose bbox overlaps the
// supplied tile bbox. At low zooms a single tile commonly straddles
// 3–4 states (south New England at z=8 covers CT/RI/MA/NY all at
// once), and we want to draw from every one of them so the tile
// doesn't show only its centre state's features.
func RegionsForBBox(minLon, minLat, maxLon, maxLat float64) []*Region {
	var out []*Region
	for i := range KnownRegions {
		r := &KnownRegions[i]
		if minLon <= r.MaxLon && maxLon >= r.MinLon &&
			minLat <= r.MaxLat && maxLat >= r.MinLat {
			out = append(out, r)
		}
	}
	return out
}

// RegionManager owns the on-disk cache of region PBFs and the
// in-memory FeatureSets parsed from them. Lazy: a region is neither
// downloaded nor parsed until a tile request asks for it. Single-flight:
// concurrent requests for the same region wait on one shared download.
//
// Memory model: every region that's been requested at least once is
// kept resident — no eviction yet. Operators should expect a few GB
// of RAM per region (Geofabrik state extract → parsed FeatureSet is
// ~4× the PBF size). With the keep-all policy, set OSM_REGIONS in the
// module config or stick to one chart area at a time.
type RegionManager struct {
	cacheDir string
	logger   logging.Logger
	client   *http.Client
	ua       string

	mu       sync.Mutex
	loaded   map[string]*FeatureSet
	inflight map[string]*sync.WaitGroup
	failed   map[string]time.Time // most-recent failure timestamp per key
	epoch    atomic.Int64         // bumps whenever loaded[] or failed[] changes
}

// NewRegionManager creates the manager rooted at cacheDir. The
// directory is created if missing. ua is the User-Agent header sent
// on Geofabrik downloads — pass something meaningful (chartplotter
// version + contact) per their (generous) usage policy.
func NewRegionManager(cacheDir, ua string, logger logging.Logger) (*RegionManager, error) {
	if cacheDir == "" {
		return nil, errors.New("region manager: cacheDir required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("region manager: mkdir %q: %w", cacheDir, err)
	}
	if ua == "" {
		ua = "viam-chartplotter (+https://github.com/erh/viam-chartplotter)"
	}
	return &RegionManager{
		cacheDir: cacheDir,
		logger:   logger,
		client: &http.Client{
			// PBF downloads can be 1–3 GB. Use a long overall timeout
			// instead of relying on default keepalive.
			Timeout: 30 * time.Minute,
		},
		ua:       ua,
		loaded:   map[string]*FeatureSet{},
		inflight: map[string]*sync.WaitGroup{},
		failed:   map[string]time.Time{},
	}, nil
}

// FeatureSetFor returns the FeatureSet covering (lon, lat) if it's
// already loaded. If the region is known but not loaded, it kicks off
// a background download+parse and returns nil. Subsequent calls for
// the same region will return the FeatureSet once it's ready.
//
// The boolean second return reports whether a region COVERS the
// point — useful for the renderer to distinguish "this point is
// outside any region we know about" (return blank tile permanently)
// from "we're still loading" (return blank for now, try again later).
func (m *RegionManager) FeatureSetFor(lon, lat float64) (*FeatureSet, bool) {
	return m.featureSetFor(RegionFor(lon, lat))
}

// FeatureSetForBBox is the overlap-based variant — a tile whose bbox
// touches any known region wins, even if its centre falls between
// region bboxes (e.g. a tile just offshore of Long Island whose
// centre lat is 0.01° south of New York's MinLat but whose northern
// edge clearly straddles the south shore). This is what the tile
// renderer should call.
func (m *RegionManager) FeatureSetForBBox(minLon, minLat, maxLon, maxLat float64) (*FeatureSet, bool) {
	return m.featureSetFor(RegionForBBox(minLon, minLat, maxLon, maxLat))
}

// FeatureSetsForBBox returns every loaded FeatureSet for the regions
// the tile bbox touches, and triggers background download/parse for
// any that aren't loaded yet. The second return reports whether at
// least one region was known to cover the bbox (lets the renderer
// distinguish "we have nothing to draw because the area is off the
// catalog" from "still loading, try again later").
//
// At low zooms a tile bbox commonly spans 3+ states. Calling this
// instead of FeatureSetForBBox lets the renderer composite features
// from every overlapping state's PBF so the tile doesn't show only
// one state's content.
func (m *RegionManager) FeatureSetsForBBox(minLon, minLat, maxLon, maxLat float64) ([]*FeatureSet, bool) {
	regions := RegionsForBBox(minLon, minLat, maxLon, maxLat)
	if len(regions) == 0 {
		return nil, false
	}
	out := make([]*FeatureSet, 0, len(regions))
	for _, region := range regions {
		fs, _ := m.featureSetFor(region) // also kicks off background load
		if fs != nil {
			out = append(out, fs)
		}
	}
	return out, true
}

func (m *RegionManager) featureSetFor(region *Region) (*FeatureSet, bool) {
	if region == nil {
		return nil, false
	}
	m.mu.Lock()
	fs, ok := m.loaded[region.Key]
	if ok {
		m.mu.Unlock()
		return fs, true
	}
	if _, inflight := m.inflight[region.Key]; inflight {
		m.mu.Unlock()
		return nil, true
	}
	// Don't retry within 30s of a failure — gives Geofabrik a break
	// if they 5xx us, and avoids spamming the logs from the tile burst
	// after pan.
	if t, bad := m.failed[region.Key]; bad && time.Since(t) < 30*time.Second {
		m.mu.Unlock()
		return nil, true
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	m.inflight[region.Key] = wg
	m.mu.Unlock()

	go m.ensureBackground(*region, wg)
	return nil, true
}

func (m *RegionManager) ensureBackground(region Region, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		m.mu.Lock()
		delete(m.inflight, region.Key)
		m.mu.Unlock()
	}()

	fs, err := m.ensure(context.Background(), region)
	m.mu.Lock()
	if err != nil {
		m.failed[region.Key] = time.Now()
		if m.logger != nil {
			m.logger.Warnf("osm region %s: %v", region.Key, err)
		}
	} else {
		delete(m.failed, region.Key)
		m.loaded[region.Key] = fs
		if m.logger != nil {
			m.logger.Infof("osm region %s: loaded (%d features)", region.Key, len(fs.Features))
		}
	}
	m.mu.Unlock()
	// Bump after the lock so observers reading Status see a coherent
	// snapshot — the new region is either in loaded[] or failed[],
	// not in a half-installed state.
	m.epoch.Add(1)
}

// Status is a snapshot the HTTP layer hands to the frontend so it can
// invalidate stale blank tiles. Epoch increments whenever a region
// transitions to loaded or failed; the frontend polls this endpoint
// and bumps the OSM tile URL's cache-bust token when epoch changes,
// which forces tiles cached during the "still loading" window to be
// re-fetched.
type Status struct {
	Epoch  int64    `json:"epoch"`
	Loaded []string `json:"loaded"`
	Failed []string `json:"failed,omitempty"`
}

// Snapshot returns the current loaded/failed region keys and the
// running epoch.
func (m *RegionManager) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded := make([]string, 0, len(m.loaded))
	for k := range m.loaded {
		loaded = append(loaded, k)
	}
	sort.Strings(loaded)
	var failed []string
	if len(m.failed) > 0 {
		failed = make([]string, 0, len(m.failed))
		for k := range m.failed {
			failed = append(failed, k)
		}
		sort.Strings(failed)
	}
	return Status{
		Epoch:  m.epoch.Load(),
		Loaded: loaded,
		Failed: failed,
	}
}

// ensure makes sure the region's PBF is on disk (downloading if not)
// and returns a parsed FeatureSet. Safe to call concurrently — single-
// flight is handled by the caller via the inflight map.
func (m *RegionManager) ensure(ctx context.Context, region Region) (*FeatureSet, error) {
	path := m.cachePath(region.Key)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := m.download(ctx, region, path); err != nil {
			return nil, fmt.Errorf("download: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat cache: %w", err)
	}

	if m.logger != nil {
		m.logger.Infof("osm region %s: parsing %s", region.Key, path)
	}
	start := time.Now()
	fs, err := LoadPBF(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if m.logger != nil {
		m.logger.Infof("osm region %s: parsed in %s", region.Key, time.Since(start).Round(time.Millisecond))
	}
	return fs, nil
}

// download streams the region PBF to a .part file and atomically
// renames it on success, so an interrupted download never leaves a
// half-written file that looks valid to ensure() on next startup.
func (m *RegionManager) download(ctx context.Context, region Region, finalPath string) error {
	partPath := finalPath + ".part"
	// Best-effort cleanup of any leftover from a previous crash.
	_ = os.Remove(partPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, region.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", m.ua)

	if m.logger != nil {
		m.logger.Infof("osm region %s: downloading %s", region.Key, region.URL)
	}
	start := time.Now()
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	out, err := os.Create(partPath)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("read body: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("close part: %w", closeErr)
	}

	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("rename: %w", err)
	}
	if m.logger != nil {
		m.logger.Infof("osm region %s: downloaded %d bytes in %s",
			region.Key, n, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func (m *RegionManager) cachePath(key string) string {
	return filepath.Join(m.cacheDir, key+".osm.pbf")
}
