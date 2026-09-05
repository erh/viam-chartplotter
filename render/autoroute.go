package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/beetlebugorg/s57/pkg/s57"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// ---------------------------------------------------------------------------
// Auto-routing over the ENC.
//
// Given two points, rasterise the charted water between them (DEPARE depths,
// land, obstructions) into a navGrid and A* across it. The boat's safe depth
// is the hard constraint — never route through water charted shoaler than the
// draft the operator gave us — and an optional *ideal* depth is the soft one:
// among the routes that are safe, prefer the one that stays in the deeper
// water, so the track hugs the channel instead of shaving the 7 ft edge of it.
//
// Soundings (SOUNDG) are deliberately not consulted: the Mongo feature store
// drops the Z coordinate (see coordPair in feature.go), so a sounding carries
// no depth here. DEPARE's DRVAL1 — the shoalest depth charted for an area —
// is the conservative reading we route on instead.
// ---------------------------------------------------------------------------

// RoutePoint is a waypoint in the auto-router's input and output. Field names
// match the frontend's { lat, lng } waypoint shape.
type RoutePoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// AvoidArea names a class of charted area the router should steer around but
// may still cross when there is no alternative. This is the extension point
// for "avoid no-wake zones", speed-restricted areas, and the like: add the
// S-57 class plus a predicate over its attributes and it costs more to cross
// without ever becoming impassable.
type AvoidArea struct {
	Name  string                  // reported back in the result
	Class string                  // S-57 object class, e.g. "RESARE"
	Match func(f encFeature) bool // nil matches every feature of the class
	// Penalty is added to the cost multiplier of every cell the area covers.
	// The grid prices all avoid-flagged cells at the heaviest penalty in play
	// (see avoidPenalty), so this is a weight, not a per-area setting.
	Penalty float64
}

// RestrictedAreaAvoid steers around charted restricted areas (RESARE).
func RestrictedAreaAvoid(penalty float64) AvoidArea {
	return AvoidArea{Name: "restricted", Class: "RESARE", Penalty: penalty}
}

// AutoRouteOptions configures one auto-route request. Zero values are filled
// in by DefaultAutoRouteOptions; the HTTP handler maps query parameters onto
// this struct.
type AutoRouteOptions struct {
	// SafeDepthM is the hard floor: water charted shoaler than this is
	// impassable. Normally the boat's draft plus the skipper's margin.
	SafeDepthM float64
	// IdealDepthM is the soft preference: water shoaler than this costs more,
	// in proportion to how far below it sits, reaching the full DepthPenalty
	// at SafeDepthM. Set equal to SafeDepthM to disable the preference.
	IdealDepthM float64

	// HardClearanceM is a no-go buffer held off land, shoals and obstructions.
	// SoftClearanceM is the wider band that merely costs more, which is what
	// centres the route in a channel rather than letting it graze the edge.
	HardClearanceM float64
	SoftClearanceM float64

	DepthPenalty   float64 // cost added at SafeDepthM, tapering to 0 at IdealDepthM
	ShorePenalty   float64 // cost added at the hard-clearance edge, tapering to 0
	UnknownPenalty float64 // cost added where no DEPARE charts a depth

	Avoid []AvoidArea

	// CorridorPadM widens the search box around the straight line between the
	// endpoints; it is how far off the rhumb line the router may wander to get
	// round a headland. Defaults to 40% of the direct distance, floored at 1 nm.
	CorridorPadM float64
	// MaxCellM is the coarsest grid cell the router will plan on. The grid
	// covers the whole search corridor with a bounded number of cells, so cell
	// size grows with the leg: past a point each cell is wider than the channel
	// it is meant to represent and the answer stops meaning anything. Bounding
	// resolution rather than distance puts the limit where the real constraint
	// is — a long leg through open water is fine, a long leg demanding harbour
	// detail is not — and lets the caller trade range for precision with
	// CorridorPadM.
	MaxCellM float64
	// MaxCells / MaxGridDim / MinCellM bound the raster. MaxCells is the
	// memory budget: each cell costs ~46 bytes across the grid and the A*
	// arrays over it, so 600k cells is ~28 MB per section in flight.
	MaxCells   int
	MaxGridDim int
	MinCellM   float64
	// MaxWaypoints caps the returned route.
	MaxWaypoints int

	// KeepWaypoints applies when optimising an existing route: every point the
	// operator placed stays in the result, and only the water between them is
	// re-planned. A waypoint is usually there for a reason the chart doesn't
	// record — a stop, a tide gate, a hazard someone saw — so dropping one is
	// not a decision to make on their behalf without being asked. False lets
	// the smoother straighten through them, removing the redundant ones.
	KeepWaypoints bool
}

// maxCorridorPadM caps how wide a search corridor gets. The pad exists to let
// a route deviate around something in the way, and 15 nm of lateral room is
// more deviation than any obstacle demands. Left uncapped at 40% of the leg, a
// 70 nm passage asks for a 28 nm margin on each side — quadrupling the area to
// raster for no routing benefit, and forcing the cells coarse to cover it.
const maxCorridorPadM = 15 * 1852

// corridorPadFor is how far off the rhumb line the router may wander.
func corridorPadFor(legM float64) float64 {
	return math.Min(math.Max(0.4*legM, 1852), maxCorridorPadM)
}

// legCellRatio relates a leg's length to the grid it deserves: cells about
// 1/300th of the leg. A 1 nm harbour hop wants metres of precision; a 70 nm
// offshore passage does not, and demanding it only makes the leg unplannable.
const legCellRatio = 300

// maxCellCeilingM is the coarsest grid the router will ever use, however long
// the leg. Past this the cells are wider than the features that would make a
// route wrong, and a plan drawn on them would be a guess wearing the clothes
// of an answer.
const maxCellCeilingM = 400

// effectiveMaxCellM is the resolution limit for a run of waypoints: the
// configured floor for short legs, relaxed in proportion to the longest leg,
// and never past the ceiling. Anything coarser than opts.MaxCellM is reported
// as a warning on the result — it is a real loss of fidelity, just a smaller
// one than refusing to plan at all.
func effectiveMaxCellM(points []RoutePoint, opts AutoRouteOptions) float64 {
	want := longestLegMeters(points) / legCellRatio
	if want < opts.MaxCellM {
		want = opts.MaxCellM
	}
	if want > maxCellCeilingM {
		want = maxCellCeilingM
	}
	return want
}

// DefaultAutoRouteOptions returns sane defaults for a boat of the given safe
// depth (metres). The ideal depth defaults to twice the safe depth, matching
// the chart's own "safe water" (DEPDW) band, so an auto-route stays in the
// water the chart paints white.
func DefaultAutoRouteOptions(safeDepthM float64) AutoRouteOptions {
	if safeDepthM <= 0 {
		safeDepthM = 6.0 / feetPerMetre
	}
	return AutoRouteOptions{
		SafeDepthM:     safeDepthM,
		IdealDepthM:    2 * safeDepthM,
		HardClearanceM: 30,
		SoftClearanceM: 150,
		DepthPenalty:   1.5,
		ShorePenalty:   2.0,
		UnknownPenalty: 1.0,
		// 120 m is about the narrowest a buoyed channel gets; below that
		// resolution the router would start planning through channel edges it
		// cannot see. With the default corridor pad this allows legs out to
		// roughly 28 nm — widen MaxCellM (or narrow CorridorPadM) for a longer
		// open-water passage where that precision isn't needed.
		MaxCellM:     120,
		MaxCells:     400000,
		MaxGridDim:   1400,
		MinCellM:     15,
		MaxWaypoints: 80,
	}
}

// normalize fills in anything the caller left at zero and enforces the
// invariants the grid relies on.
func (o *AutoRouteOptions) normalize(directM float64) {
	d := DefaultAutoRouteOptions(o.SafeDepthM)
	if o.SafeDepthM <= 0 {
		o.SafeDepthM = d.SafeDepthM
	}
	if o.IdealDepthM < o.SafeDepthM {
		o.IdealDepthM = o.SafeDepthM
	}
	if o.HardClearanceM < 0 {
		o.HardClearanceM = 0
	}
	if o.SoftClearanceM < o.HardClearanceM {
		o.SoftClearanceM = o.HardClearanceM
	}
	if o.DepthPenalty <= 0 {
		o.DepthPenalty = d.DepthPenalty
	}
	if o.ShorePenalty <= 0 {
		o.ShorePenalty = d.ShorePenalty
	}
	if o.UnknownPenalty <= 0 {
		o.UnknownPenalty = d.UnknownPenalty
	}
	if o.MaxCellM <= 0 {
		o.MaxCellM = d.MaxCellM
	}
	if o.MaxCells <= 0 {
		o.MaxCells = d.MaxCells
	}
	if o.MaxGridDim <= 0 {
		o.MaxGridDim = d.MaxGridDim
	}
	if o.MinCellM <= 0 {
		o.MinCellM = d.MinCellM
	}
	if o.MaxWaypoints <= 1 {
		o.MaxWaypoints = d.MaxWaypoints
	}
	if o.CorridorPadM <= 0 {
		o.CorridorPadM = corridorPadFor(directM)
	}
}

// AutoRouteResult is what the router found, plus enough about how it found it
// that the UI can tell the operator what to double-check.
type AutoRouteResult struct {
	Waypoints []RoutePoint `json:"waypoints"`

	DistanceMeters float64 `json:"distance_meters"`
	DirectMeters   float64 `json:"direct_meters"`

	// MinDepthMeters is the shoalest charted depth anywhere on the route, null
	// when no DEPARE charted any of it. CrossedUnknown reports whether any of
	// the route runs through water no DEPARE charts. Both are warnings to
	// surface, not guarantees.
	MinDepthMeters *float64 `json:"min_depth_meters"`
	CrossedUnknown bool     `json:"crossed_unknown"`

	SafeDepthMeters  float64 `json:"safe_depth_meters"`
	IdealDepthMeters float64 `json:"ideal_depth_meters"`

	SnappedStart bool `json:"snapped_start"`
	SnappedEnd   bool `json:"snapped_end"`

	CellSizeMeters float64 `json:"cell_size_meters"`
	// Sections is how many independently planned runs the route was split
	// into — 1 for anything that fit one grid. More than 1 means the route was
	// too long to plan whole at a useful resolution, and CellSizeMeters is the
	// coarsest any section used.
	Sections     int        `json:"sections"`
	GridWidth    int        `json:"grid_width"`
	GridHeight   int        `json:"grid_height"`
	BBox         [4]float64 `json:"bbox"` // [minLon, minLat, maxLon, maxLat]
	FeatureCount int        `json:"feature_count"`
	ElapsedMs    float64    `json:"elapsed_ms"`

	Warnings []string `json:"warnings,omitempty"`
}

// autoRouteQueryTimeout bounds the chart query. Generous compared with a tile
// query — a routing corridor is far bigger than a tile — but bounded, so a
// too-ambitious request fails fast instead of hanging the HTTP handler.
const autoRouteQueryTimeout = 30 * time.Second

// gridCellSize is how coarse the routing grid would be for this corridor.
func gridCellSize(bbox [4]float64, opts AutoRouteOptions) float64 {
	return cellSizeFor(bbox[0], bbox[1], bbox[2], bbox[3], opts.MaxCells, opts.MinCellM, opts.MaxGridDim)
}

// RoutingProjection is the set of fields the rasteriser actually reads. An ENC
// feature carries a lot the router never looks at — free-text remarks, source
// dates, national-language names, colours, light characteristics — and over a
// remote link that unread payload is most of the wall time. Everything here is
// something rasterizeForRouting or the paint order depends on.
func RoutingProjection() bson.M {
	return bson.M{
		"cell":        1, // paint order tiebreaker
		"scale":       1, // finest-cell-wins depth rule
		"objectClass": 1,
		"bbox":        1,
		"geometry":    1,
		// The only attributes routing reads: depth range, depth over an
		// obstruction, and the restricted-area category an avoid rule matches.
		"attributes.DRVAL1": 1,
		"attributes.DRVAL2": 1,
		"attributes.VALSOU": 1,
		"attributes.CATREA": 1,
		"attributes.RESTRN": 1,
	}
}

// routingUsageBand matches the chart's detail to the grid's. Harbour and
// berthing cells (bands 5-6) are the great majority of features in coastal
// water — a 35 nm corridor is 31k documents and 59 MB at every band — and
// their detail is finer than a coarse cell can express, so fetching it costs
// everything and changes nothing.
//
// This ceiling was tried once before and reverted because it made the router
// refuse every route: with only coarse cells left, their undifferentiated
// depth areas read as 0 m and blocked all open water. That was a bug in how
// their DRVAL1 was interpreted, not in the ceiling — see depareKeyDepth, which
// now leaves a too-wide range uncharted rather than pessimistically shoal.
// With that fixed the ceiling is safe, and it is what makes a long leg
// affordable at all.
//
// The ends are handled separately (see routingFeatures): full detail is
// fetched around each endpoint, where the boat manoeuvres and where a pier or
// a berth-scale rock is exactly what matters.
func routingUsageBand(cellM float64) int {
	switch {
	case cellM >= 150:
		return 3 // coastal and coarser: a long offshore leg
	case cellM >= 60:
		return 4 // approach and coarser: a normal coastal leg
	default:
		return 0 // fine grid, short leg, small box — take everything
	}
}

// endpointDetailRadiusM is how far around each end of a section to fetch full
// harbour detail. Big enough to cover getting off a dock and out of a marina,
// small enough that the extra query is trivial.
const endpointDetailRadiusM = 2000

// routingFeatures fetches the chart the router will raster: the corridor at the
// usage band matching the grid, plus full detail around the ends.
func (r *ENCRenderer) routingFeatures(ctx context.Context, bbox [4]float64, ends []RoutePoint, opts AutoRouteOptions) ([]*mongoFeature, error) {
	classes := autoRouteClasses(opts)
	cell := gridCellSize(bbox, opts)
	band := routingUsageBand(cell)

	features, err := r.queryFeaturesClasses(ctx, bbox[0], bbox[1], bbox[2], bbox[3], noaa.ClassQuery{
		Classes:      classes,
		MaxUsageBand: band,
		// Safe to thin: geomLow's tolerance is finer than the grid cell (see
		// useLowGeomForCell), and the projection only drops fields the
		// rasteriser never reads.
		UseLowGeom: useLowGeomForCell(cell),
		Projection: RoutingProjection(),
	})
	if err != nil {
		return nil, wrapChartQueryError(err, "auto-route",
			"run `chartdiag route` to see whether an index is missing or the corridor is simply too big")
	}
	if band == 0 {
		return features, nil // already full detail everywhere
	}

	seen := make(map[string]struct{}, len(features))
	for _, f := range features {
		seen[f.id] = struct{}{}
	}
	detail := noaa.ClassQuery{Classes: classes, Projection: RoutingProjection()}
	for _, p := range ends {
		b := pointBox(p, endpointDetailRadiusM)
		extra, err := r.queryFeaturesClasses(ctx, b[0], b[1], b[2], b[3], detail)
		if err != nil {
			// The corridor is the safety-critical part and we have it. Losing
			// the endpoint detail costs precision where the boat manoeuvres,
			// which is worth a log line, not a failed route.
			r.logger.Warnf("auto-route: endpoint detail query failed, continuing on corridor data: %v", err)
			continue
		}
		for _, f := range extra {
			if _, dup := seen[f.id]; dup {
				continue
			}
			seen[f.id] = struct{}{}
			features = append(features, f)
		}
	}
	return features, nil
}

// pointBox is a lat/lon box of the given radius around a point.
func pointBox(p RoutePoint, radiusM float64) [4]float64 {
	dLat := radiusM / metresPerDegreeLat
	dLon := radiusM / (metresPerDegreeLat * clampCosLat(p.Lat))
	return [4]float64{p.Lng - dLon, p.Lat - dLat, p.Lng + dLon, p.Lat + dLat}
}

// AutoRoutePlan describes what a request would do before it does it: the
// corridor, the grid, and the chart query behind it. The diagnostic command
// uses it to explain() the real query rather than a hand-copied guess at it.
type AutoRoutePlan struct {
	BBox       [4]float64
	CellM      float64
	GridW      int
	GridH      int
	Classes    []string
	UseLowGeom bool
}

// PlanAutoRoute works out the corridor, grid and query for a request without
// touching the database.
func PlanAutoRoute(start, end RoutePoint, opts AutoRouteOptions) AutoRoutePlan {
	opts.normalize(haversineMeters(start.Lat, start.Lng, end.Lat, end.Lng))
	bbox := routeBBox(start, end, opts.CorridorPadM)
	cell := gridCellSize(bbox, opts)
	g := newNavGrid(bbox[0], bbox[1], bbox[2], bbox[3], opts.MaxCells, opts.MinCellM, opts.MaxGridDim)
	return AutoRoutePlan{
		BBox:       bbox,
		CellM:      cell,
		GridW:      g.nx,
		GridH:      g.ny,
		Classes:    autoRouteClasses(opts),
		UseLowGeom: useLowGeomForCell(cell),
	}
}

// useLowGeomForCell decides whether to fetch the pre-simplified geometry tier
// rather than full resolution. geomLow is simplified with a ~38 m tolerance
// (noaa.LowGeomMaxZoom), so once the routing grid's cells are coarser than
// that, the detail being thrown away is smaller than a cell and the router
// cannot act on it anyway — while the full-resolution coastlines and depth
// areas it replaces are the bulk of what crosses the wire on a corridor-sized
// query. Fine grids (a short harbour leg) keep full geometry, where the box is
// small enough for it to be cheap.
func useLowGeomForCell(cellM float64) bool {
	return cellM >= lowGeomToleranceMeters
}

// lowGeomToleranceMeters is noaa's geomLow simplification tolerance expressed
// in metres: one pixel of longitude at LowGeomMaxZoom.
const lowGeomToleranceMeters = 360.0 / float64(256*(1<<noaa.LowGeomMaxZoom)) * metresPerDegreeLat

// autoRouteClasses is every S-57 object class the rasteriser reads, and so
// exactly what the chart query needs to fetch. Anything not on this list —
// soundings, navaids, contours, labels — is dead weight over a corridor-sized
// bbox.
func autoRouteClasses(opts AutoRouteOptions) []string {
	classes := []string{"DEPARE", "DRGARE", "UNSARE"}
	for c := range autoRouteLandClasses {
		classes = append(classes, c)
	}
	for c := range autoRouteObstructionClasses {
		classes = append(classes, c)
	}
	for c := range autoRoutePointHazardClasses {
		classes = append(classes, c)
	}
	for _, a := range opts.Avoid {
		classes = append(classes, a.Class)
	}
	sort.Strings(classes) // stable order keeps the Mongo query plan cacheable
	return slices.Compact(classes)
}

// ErrNoRoute is returned when no safe path exists between the two points
// within the search corridor.
var ErrNoRoute = errors.New("no safe route found")

// errNoCharts is returned by anything that needs the ENC feature store when
// this instance has no Mongo attached.
var errNoCharts = errors.New("this needs the NOAA chart collection (mongo_uri is not configured)")

// ErrChartQueryTimeout marks a chart query that ran past its deadline, so the
// HTTP layer can answer 504 rather than a generic failure.
var ErrChartQueryTimeout = errors.New("chart query timed out")

// wrapChartQueryError turns the driver's raw timeout ("incomplete read of full
// message: context deadline exceeded: read tcp …") into something that says
// which operation gave up and what usually causes it. A read deadline here is
// nearly always a query falling back to a collection scan for want of the
// right index, which the raw message gives no hint of.
func wrapChartQueryError(err error, what, hint string) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
		return fmt.Errorf("%s: %w after %s — %s", what, ErrChartQueryTimeout, autoRouteQueryTimeout, hint)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// autoRouteLandClasses are the S-57 area classes the router treats as solid.
// Beyond dry land this includes the shoreline constructions a hull cannot
// pass through — piers, pontoons, floating docks, dykes, causeways.
var autoRouteLandClasses = map[string]bool{
	"LNDARE": true, "BUAARE": true, "BUISGL": true,
	"SLCONS": true, "PONTON": true, "FLODOC": true,
	"HULKES": true, "DYKCON": true, "CAUSWY": true, "DAMCON": true,
}

// autoRouteObstructionClasses carry a VALSOU (depth over the obstruction):
// deep enough for the boat and they are ignored, otherwise they block.
var autoRouteObstructionClasses = map[string]bool{
	"OBSTRN": true, "WRECKS": true, "UWTROC": true,
}

// autoRoutePointHazardClasses are fixed structures with no useful depth
// attribute — always blocking, at whatever the clearance radius is.
var autoRoutePointHazardClasses = map[string]bool{
	"PILPNT": true, "MORFAC": true, "MOORNG": true,
	"OFSPLF": true, "PYLONS": true,
}

// AutoRouteVia plans a route through a list of points in order — two points is
// an ordinary route, more is an existing waypoint list being re-planned.
//
// Everything happens on one grid over one chart query, not a query per leg:
// a ten-waypoint route would otherwise be nine overlapping corridor fetches of
// the same water.
func (r *ENCRenderer) AutoRouteVia(points []RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	began := time.Now()
	if r.noaaColl == nil {
		return nil, fmt.Errorf("auto-route: %w", errNoCharts)
	}
	if len(points) < 2 {
		return nil, errors.New("need at least two waypoints")
	}
	for _, p := range points {
		if !validLatLng(p) {
			return nil, errors.New("every waypoint must be a valid lat/lng")
		}
	}
	// Capture the caller's pad BEFORE normalize fills one in. Each section
	// sizes its own corridor from its own longest leg; a route-wide pad would
	// give a 20 nm leg the 20 nm margin its longest sibling needed, blowing up
	// a bbox that had no reason to be large.
	explicitPad := opts.CorridorPadM
	opts.normalize(longestLegMeters(points))

	// A long route doesn't need one grid: it needs several. Planning it whole
	// would demand a corridor spanning the entire passage, whose cells are
	// wider than the channels they represent. Split it into sections that each
	// resolve properly, sharing a chart query wherever consecutive legs are
	// close enough to fit one.
	sections, err := sectionsForResolution(points, opts, explicitPad)
	if err != nil {
		return nil, err
	}

	parts, err := r.planSections(sections, opts, explicitPad)
	if err != nil {
		return nil, err
	}

	res := mergeRouteResults(parts, points)
	res.ElapsedMs = float64(time.Since(began).Microseconds()) / 1000.0
	return res, nil
}

// sectionConcurrency bounds how many sections are planned at once. Each holds
// its own grid and its own slice of the chart, so this is a memory and
// database-load ceiling, not a throughput knob.
// Three, not four: the grid budget below doubled, and each section holds its
// own grid plus the A* arrays over it — about 28 MB at MaxCells. Three at once
// is ~85 MB, which a boat computer can carry.
const sectionConcurrency = 3

// planSections plans every section, several at a time. They are independent —
// separate queries, separate grids — and planning them in series is the
// difference between a route arriving in seven seconds and in forty.
func (r *ENCRenderer) planSections(sections [][]RoutePoint, opts AutoRouteOptions, explicitPad float64) ([]*AutoRouteResult, error) {
	parts := make([]*AutoRouteResult, len(sections))
	errs := make([]error, len(sections))

	var wg sync.WaitGroup
	sem := make(chan struct{}, sectionConcurrency)
	for i, sec := range sections {
		wg.Add(1)
		go func(i int, sec []RoutePoint) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			parts[i], errs[i] = r.planSection(sec, opts, explicitPad)
		}(i, sec)
	}
	wg.Wait()

	// Report the earliest failing section, so "leg 3 of 7" means the same
	// thing however the work happened to be scheduled.
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("section %d of %d: %w", i+1, len(sections), err)
		}
	}
	return parts, nil
}

// planSection plans one contiguous run of waypoints on its own grid.
//
// The grid comes from precomputed navigability tiles when they are available,
// and from the polygons when they are not. Reading tiles is the difference
// between fetching 58 MB of coastline to rasterise and fetching a few hundred
// kilobytes of grid that is already rasterised; the fallback exists so a
// deployment with no tiles built (or a gap in coverage) still routes.
func (r *ENCRenderer) planSection(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) (*AutoRouteResult, error) {
	bbox := pointsBBox(points, sectionPadM(points, explicitPad))
	ctx, cancel := context.WithTimeout(context.Background(), autoRouteQueryTimeout)
	defer cancel()

	if r.navColl != nil {
		wanted := gridCellSize(bbox, opts)
		midLat := (bbox[1] + bbox[3]) / 2
		z := navTileZoomFor(wanted, midLat)
		tiles, err := r.NavTiles(ctx, z, bbox[0], bbox[1], bbox[2], bbox[3])
		if err != nil {
			return nil, err
		}
		o := opts
		o.normalize(longestLegMeters(points))
		g := newNavGrid(bbox[0], bbox[1], bbox[2], bbox[3], o.MaxCells, o.MinCellM, o.MaxGridDim)
		if sampleTilesIntoGrid(g, tiles, z) > 0 {
			return planRouteOnGrid(g, bbox, points, opts)
		}
	}

	features, err := r.routingFeatures(ctx, bbox, []RoutePoint{points[0], points[len(points)-1]}, opts)
	if err != nil {
		return nil, err
	}
	return planRouteVia(features, bbox, points, opts)
}

// sectionPadM is the corridor width for one run of waypoints: a fraction of
// ITS OWN longest leg, which is the leg needing the most room to get around
// something. explicitPad, when the caller set one, overrides it.
func sectionPadM(points []RoutePoint, explicitPad float64) float64 {
	if explicitPad > 0 {
		return explicitPad
	}
	return corridorPadFor(longestLegMeters(points))
}

// sectionsForResolution greedily groups consecutive waypoints into the fewest
// runs that each raster at or below MaxCellM. Sections overlap by one point —
// the waypoint they share — so the route stays continuous.
//
// A single leg that cannot meet the limit on its own is unplannable, and the
// error names it: nothing can be split further, and the operator needs to know
// which leg to shorten rather than being told the whole route is too long.
func sectionsForResolution(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) ([][]RoutePoint, error) {
	var out [][]RoutePoint
	start := 0
	for start < len(points)-1 {
		end := start + 1
		if !sectionFits(points[start:end+1], opts, explicitPad) {
			legNM := haversineMeters(points[start].Lat, points[start].Lng, points[end].Lat, points[end].Lng) / 1852
			leg := points[start : end+1]
			cell := gridCellSize(pointsBBox(leg, sectionPadM(leg, explicitPad)), opts)
			return nil, fmt.Errorf(
				"leg %d of %d is %.1f nm, which needs %.0f m grid cells (ceiling %.0f m): split it with an intermediate waypoint, or raise max_cell if you only need open-water routing",
				start+1, len(points)-1, legNM, cell, effectiveMaxCellM(leg, opts))
		}
		// Extend while the accumulated run still resolves finely enough.
		for end+1 < len(points) && sectionFits(points[start:end+2], opts, explicitPad) {
			end++
		}
		out = append(out, points[start:end+1])
		start = end
	}
	return out, nil
}

func sectionFits(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) bool {
	cell := gridCellSize(pointsBBox(points, sectionPadM(points, explicitPad)), opts)
	return cell <= effectiveMaxCellM(points, opts)
}

// mergeRouteResults stitches the sections back into one route. Each section
// after the first starts on the waypoint the previous one ended at, so that
// duplicate is dropped.
func mergeRouteResults(parts []*AutoRouteResult, original []RoutePoint) *AutoRouteResult {
	res := &AutoRouteResult{
		DirectMeters: legTotalMeters(original),
		Sections:     len(parts),
	}
	if len(parts) == 0 {
		return res
	}
	res.SafeDepthMeters = parts[0].SafeDepthMeters
	res.IdealDepthMeters = parts[0].IdealDepthMeters
	res.SnappedStart = parts[0].SnappedStart
	res.SnappedEnd = parts[len(parts)-1].SnappedEnd

	seenWarning := map[string]bool{}
	bbox := parts[0].BBox
	for i, p := range parts {
		if i == 0 {
			res.Waypoints = append(res.Waypoints, p.Waypoints...)
		} else if len(p.Waypoints) > 1 {
			res.Waypoints = append(res.Waypoints, p.Waypoints[1:]...)
		}
		res.FeatureCount += p.FeatureCount
		// The coarsest cell any section used — the honest figure, since the
		// route is only as well resolved as its worst-resolved part.
		if p.CellSizeMeters > res.CellSizeMeters {
			res.CellSizeMeters = p.CellSizeMeters
			res.GridWidth, res.GridHeight = p.GridWidth, p.GridHeight
		}
		if p.CrossedUnknown {
			res.CrossedUnknown = true
		}
		if p.MinDepthMeters != nil && (res.MinDepthMeters == nil || *p.MinDepthMeters < *res.MinDepthMeters) {
			d := *p.MinDepthMeters
			res.MinDepthMeters = &d
		}
		bbox = [4]float64{
			math.Min(bbox[0], p.BBox[0]), math.Min(bbox[1], p.BBox[1]),
			math.Max(bbox[2], p.BBox[2]), math.Max(bbox[3], p.BBox[3]),
		}
		for _, w := range p.Warnings {
			if !seenWarning[w] {
				seenWarning[w] = true
				res.Warnings = append(res.Warnings, w)
			}
		}
	}
	res.BBox = bbox
	res.DistanceMeters = pathDistanceM(res.Waypoints)
	return res
}

// AutoRoute plans a route from start to end over the charted ENC data.
func (r *ENCRenderer) AutoRoute(start, end RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	return r.AutoRouteVia([]RoutePoint{start, end}, opts)
}

// planRoute is AutoRoute with the chart query already done: rasterise the
// features, search, and package the result. Split out so the router can be
// exercised against hand-built features with no Mongo behind it.
func planRoute(features []*mongoFeature, bbox [4]float64, start, end RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	return planRouteVia(features, bbox, []RoutePoint{start, end}, opts)
}

// planRouteVia is AutoRouteVia with the chart query already done: rasterise
// once, search each leg on the shared grid, and stitch the legs together.
// Split out so the router can be exercised against hand-built features with no
// Mongo behind it.
func planRouteVia(features []*mongoFeature, bbox [4]float64, points []RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	o := opts
	o.normalize(longestLegMeters(points))
	g := newNavGrid(bbox[0], bbox[1], bbox[2], bbox[3], o.MaxCells, o.MinCellM, o.MaxGridDim)
	rasterizeForRouting(g, features, o)
	res, err := planRouteOnGrid(g, bbox, points, opts)
	if err != nil {
		return nil, err
	}
	res.FeatureCount = len(features)
	return res, nil
}

// planRouteOnGrid searches an already-rasterised grid. Split out so a grid
// read from precomputed tiles and one built from polygons take exactly the
// same path from here on.
func planRouteOnGrid(g *navGrid, bbox [4]float64, points []RoutePoint, opts AutoRouteOptions) (*AutoRouteResult, error) {
	directM := legTotalMeters(points)
	opts.normalize(longestLegMeters(points))

	g.finalize(gridCost{
		SafeDepthM:     opts.SafeDepthM,
		IdealDepthM:    opts.IdealDepthM,
		HardClearanceM: opts.HardClearanceM,
		SoftClearanceM: opts.SoftClearanceM,
		DepthPenalty:   opts.DepthPenalty,
		ShorePenalty:   opts.ShorePenalty,
		UnknownPenalty: opts.UnknownPenalty,
		AvoidPenalty:   avoidPenalty(opts.Avoid),
	})

	res := &AutoRouteResult{
		SafeDepthMeters:  opts.SafeDepthM,
		IdealDepthMeters: opts.IdealDepthM,
		DirectMeters:     directM,
		CellSizeMeters:   g.cellSizeM(),
		GridWidth:        g.nx,
		GridHeight:       g.ny,
		BBox:             bbox,
	}

	// One A* per leg, on the one grid. The legs are stitched into a single
	// cell path so the smoother sees the route as a whole.
	var full []int
	legBounds := []int{0} // index into full where each leg ends
	for i := 0; i+1 < len(points); i++ {
		leg, err := g.route(points[i], points[i+1], opts, res, i == 0, i+2 == len(points))
		if err != nil {
			return nil, fmt.Errorf("leg %d of %d (%.4f,%.4f to %.4f,%.4f): %w",
				i+1, len(points)-1, points[i].Lat, points[i].Lng, points[i+1].Lat, points[i+1].Lng, err)
		}
		if i > 0 && len(leg) > 0 {
			leg = leg[1:] // the join cell is already the previous leg's end
		}
		full = append(full, leg...)
		legBounds = append(legBounds, len(full))
	}

	// Coarser than the configured floor means this leg was long enough to need
	// a relaxed grid. That is a real loss of fidelity — just a smaller one than
	// refusing to plan — so say so rather than let it pass silently.
	if g.cellSizeM() > opts.MaxCellM {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"planned at %.0f m resolution (coarser than the usual %.0f m) because of the leg length — fine offshore, but check it where it closes the coast",
			g.cellSizeM(), opts.MaxCellM))
	}

	res.Waypoints = g.pathToWaypoints(full, legBounds, points, opts, res)
	res.DistanceMeters = pathDistanceM(res.Waypoints)
	minDepth, unknown := g.pathDepthStats(full)
	if !math.IsNaN(minDepth) {
		res.MinDepthMeters = &minDepth
	}
	res.CrossedUnknown = unknown
	return res, nil
}

// route snaps the endpoints onto navigable water and searches. If the search
// fails with a shore-clearance buffer in force, it retries once without it —
// a route that squeezes past a pierhead with a warning beats no route at all,
// and the depth constraint is untouched either way.
// isFirst/isLast say whether this leg carries the route's own start or
// destination, so a snap is only reported for those — an intermediate waypoint
// nudged onto water is expected, not news.
func (g *navGrid) route(start, end RoutePoint, opts AutoRouteOptions, res *AutoRouteResult, isFirst, isLast bool) ([]int, error) {
	// Snap far enough to get a boat off a dock or out of a marina berth, but
	// not so far that we silently route from somewhere else entirely.
	snapRadius := math.Max(400, 8*g.cellSizeM())

	for attempt := 0; attempt < 2; attempt++ {
		s, sOK := g.cellAt(start.Lng, start.Lat)
		e, eOK := g.cellAt(end.Lng, end.Lat)
		if !sOK || !eOK {
			return nil, errors.New("start or end fell outside the search area")
		}
		sSnap, ok1 := g.nearestPassable(s, snapRadius)
		eSnap, ok2 := g.nearestPassable(e, snapRadius)
		if ok1 && ok2 {
			if path := g.findPath(sSnap, eSnap); path != nil {
				if isFirst && sSnap != s {
					res.SnappedStart = true
					res.Warnings = append(res.Warnings, "start moved to the nearest navigable water")
				}
				if isLast && eSnap != e {
					res.SnappedEnd = true
					res.Warnings = append(res.Warnings, "destination moved to the nearest navigable water")
				}
				return path, nil
			}
		} else if attempt == 1 || opts.HardClearanceM <= 0 {
			which := "start"
			if ok1 {
				which = "destination"
			}
			return nil, fmt.Errorf("%w: the %s is not on water charted deeper than %.1f ft",
				ErrNoRoute, which, opts.SafeDepthM*feetPerMetre)
		}

		if attempt == 0 && opts.HardClearanceM > 0 {
			relaxed := gridCost{
				SafeDepthM: opts.SafeDepthM, IdealDepthM: opts.IdealDepthM,
				HardClearanceM: 0, SoftClearanceM: opts.SoftClearanceM,
				DepthPenalty: opts.DepthPenalty, ShorePenalty: opts.ShorePenalty,
				UnknownPenalty: opts.UnknownPenalty, AvoidPenalty: avoidPenalty(opts.Avoid),
			}
			g.finalize(relaxed)
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("no route with a %.0f m clearance off land and shoals — this one has none, check it closely", opts.HardClearanceM))
			continue
		}
		break
	}
	return nil, fmt.Errorf("%w between these points at a %.1f ft safe depth", ErrNoRoute, opts.SafeDepthM*feetPerMetre)
}

// pathToWaypoints turns the raw cell path into the waypoint list: pull it
// taut, thin it to the cap, then convert cell centres to lon/lat. The exact
// requested endpoints replace the first/last cell centre unless they had to be
// snapped, in which case the snapped water is what the caller gets.
func (g *navGrid) pathToWaypoints(path []int, legBounds []int, points []RoutePoint, opts AutoRouteOptions, res *AutoRouteResult) []RoutePoint {
	var pulled []int
	if opts.KeepWaypoints && len(legBounds) > 2 {
		// Smooth inside each leg only, so every point the operator placed
		// survives as a corner of the result. Straightening across a leg
		// boundary would quietly delete it.
		for i := 0; i+1 < len(legBounds); i++ {
			lo, hi := legBounds[i], legBounds[i+1]
			if i > 0 {
				lo-- // include the shared join cell so the leg is continuous
			}
			if lo >= hi || hi > len(path) {
				continue
			}
			leg := g.pullTaut(path[lo:hi])
			if i > 0 && len(leg) > 0 {
				leg = leg[1:]
			}
			pulled = append(pulled, leg...)
		}
	} else {
		pulled = g.pullTaut(path)
	}

	pulled, thinned := g.thinToLimit(pulled, opts.MaxWaypoints)
	if thinned {
		res.Warnings = append(res.Warnings, "route thinned to fit the waypoint limit")
	}
	out := make([]RoutePoint, 0, len(pulled))
	for _, c := range pulled {
		lon, lat := g.centreOf(c)
		out = append(out, RoutePoint{Lat: lat, Lng: lon})
	}
	// The exact requested endpoints replace the first/last cell centre unless
	// they had to be snapped, in which case the snapped water is what the
	// caller gets.
	if len(out) > 0 && !res.SnappedStart {
		out[0] = points[0]
	}
	if len(out) > 1 && !res.SnappedEnd {
		out[len(out)-1] = points[len(points)-1]
	}
	return out
}

// pathDepthStats reports the shoalest charted depth along the path and whether
// any of it crosses water nothing charted.
func (g *navGrid) pathDepthStats(path []int) (minDepth float64, unknown bool) {
	minDepth = math.NaN()
	for _, c := range path {
		if g.flags[c]&cellDredged != 0 {
			continue
		}
		d := g.depth[c]
		if math.IsNaN(d) {
			unknown = true
			continue
		}
		if math.IsNaN(minDepth) || d < minDepth {
			minDepth = d
		}
	}
	return minDepth, unknown
}

// rasterizeForNavTile paints the boat-independent facts into a tile grid:
// charted depth, land, obstructions with no charted depth over them,
// unsurveyed water, restricted areas.
//
// The split from rasterizeForRouting matters. Anything that depends on the
// boat — whether a shoal is too shallow, whether a wreck with 8 m over it is a
// hazard, how wide a berth to give the shore — is a judgement the router makes
// when it reads the tile, never something baked into it. That is what lets one
// set of tiles serve every draft.
func rasterizeForNavTile(g *tileGrid, features []*mongoFeature) {
	sortFeaturesForPaint(features)
	hazardRadius := 0.75 * g.cellSizeM()

	for _, f := range features {
		class := f.ObjectClass()
		geom := f.Geometry()
		scale := int32(f.scale)
		if scale <= 0 {
			scale = math.MaxInt32
		}

		switch {
		case autoRouteLandClasses[class]:
			markGeometry(g.navGrid, geom, hazardRadius, func(i int) { g.mark(i, cellLand) })

		case autoRouteObstructionClasses[class]:
			// A charted depth over an obstruction is a depth, not a wall: fold
			// it into the cell so the reader's own draft decides. Only an
			// obstruction with no sounding is unconditionally impassable.
			if d, ok := obstructionDepth(f); ok {
				markGeometry(g.navGrid, geom, hazardRadius, func(i int) { g.setDepth(i, d, scale) })
				continue
			}
			markGeometry(g.navGrid, geom, hazardRadius, func(i int) { g.mark(i, cellObstruction) })

		case autoRoutePointHazardClasses[class]:
			markGeometry(g.navGrid, geom, hazardRadius, func(i int) { g.mark(i, cellObstruction) })

		case class == "DEPARE":
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			if key, ok := depareKeyDepth(f); ok {
				g.fillRings(splitRings(geom.Coordinates), func(i int) { g.setDepth(i, key, scale) })
			}

		case class == "DRGARE":
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			key, ok := depareKeyDepth(f)
			g.fillRings(splitRings(geom.Coordinates), func(i int) {
				if ok {
					g.setDepth(i, key, scale)
				}
				g.mark(i, cellDredged)
			})

		case class == "UNSARE":
			if geom.Type == s57.GeometryTypePolygon {
				g.fillRings(splitRings(geom.Coordinates), func(i int) { g.mark(i, cellUnsurveyed) })
			}

		case class == "RESARE":
			if geom.Type == s57.GeometryTypePolygon {
				g.fillRings(splitRings(geom.Coordinates), func(i int) { g.mark(i, cellRestricted) })
			}
		}
	}
}

// obstructionDepth is the charted depth over a wreck/rock/obstruction (VALSOU,
// metres), when it has one.
func obstructionDepth(f encFeature) (float64, bool) {
	v, ok := f.Attribute("VALSOU")
	if !ok {
		return 0, false
	}
	d := numAttr(v)
	if math.IsNaN(d) {
		return 0, false
	}
	return d, true
}

// rasterizeForRouting paints the ENC features into the grid. Features are
// painted coarsest-cell-first, exactly as the renderer paints tiles, so where
// a harbour cell and a coastal cell chart the same water the finer cell's
// depth is the one that survives (see navGrid.setDepth).
func rasterizeForRouting(g *navGrid, features []*mongoFeature, opts AutoRouteOptions) {
	sortFeaturesForPaint(features)
	hazardRadius := math.Max(opts.HardClearanceM, 0.75*g.cellSizeM())

	for _, f := range features {
		class := f.ObjectClass()
		geom := f.Geometry()
		scale := int32(f.scale)
		if scale <= 0 {
			scale = math.MaxInt32
		}

		switch {
		case autoRouteLandClasses[class]:
			markGeometry(g, geom, hazardRadius, func(i int) { g.mark(i, cellLand) })

		case autoRouteObstructionClasses[class]:
			if obstructionIsClear(f, opts.SafeDepthM) {
				continue
			}
			markGeometry(g, geom, hazardRadius, func(i int) { g.mark(i, cellObstruction) })

		case autoRoutePointHazardClasses[class]:
			markGeometry(g, geom, hazardRadius, func(i int) { g.mark(i, cellObstruction) })

		case class == "DEPARE":
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			key, ok := depareKeyDepth(f)
			if !ok {
				continue
			}
			g.fillRings(splitRings(geom.Coordinates), func(i int) { g.setDepth(i, key, scale) })

		case class == "DRGARE":
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			// A dredged area is a maintained channel: the chart guarantees it
			// even where the surrounding DEPARE is shoal. Record its depth
			// when it carries one, and flag it either way so the depth
			// penalties leave it alone.
			key, ok := depareKeyDepth(f)
			g.fillRings(splitRings(geom.Coordinates), func(i int) {
				if ok {
					g.setDepth(i, key, scale)
				}
				g.mark(i, cellDredged)
			})

		case class == "UNSARE":
			// Unsurveyed: no depth to record, and worth steering around.
			if geom.Type == s57.GeometryTypePolygon {
				g.fillRings(splitRings(geom.Coordinates), func(i int) { g.mark(i, cellUnsurveyed) })
			}
		}

		for _, a := range opts.Avoid {
			if a.Class != class || (a.Match != nil && !a.Match(f)) {
				continue
			}
			if geom.Type == s57.GeometryTypePolygon {
				g.fillRings(splitRings(geom.Coordinates), func(i int) { g.mark(i, cellRestricted) })
			}
		}
	}
}

// markGeometry applies fn to every cell a feature covers, whatever its
// geometry: polygons are filled and their outlines walked (so a pier thinner
// than a cell still blocks), lines have their vertices walked, and points are
// stamped with the hazard radius.
func markGeometry(g *navGrid, geom s57.Geometry, radiusM float64, fn func(i int)) {
	switch geom.Type {
	case s57.GeometryTypePolygon:
		rings := splitRings(geom.Coordinates)
		g.fillRings(rings, fn)
		g.stampVertices(rings, fn)
	case s57.GeometryTypeLineString:
		g.stampVertices([][][]float64{geom.Coordinates}, fn)
	case s57.GeometryTypePoint:
		for _, c := range geom.Coordinates {
			if len(c) < 2 {
				continue
			}
			g.stampDisc(c[0], c[1], radiusM, fn)
		}
	}
}

// wideDepthRangeM is the DRVAL1..DRVAL2 span above which a depth area stops
// being a statement about the depth anywhere in particular. Coarse cells chart
// huge undifferentiated areas — central Long Island Sound is a single
// 0-18.2 m DEPARE — and reading their DRVAL1 as "this is 0 m deep" marks all
// of it unnavigable. Ten metres comfortably admits a real charted shoal
// (0-2 m, 2-5 m) while excluding the undifferentiated ones.
const wideDepthRangeM = 10.0

// depareKeyDepth is the depth a DEPARE/DRGARE polygon contributes: DRVAL1, the
// shoalest depth charted inside it. That is the conservative reading and the
// same one the renderer shades detail zooms with. DRVAL2 stands in when DRVAL1
// is absent.
//
// It reports nothing for an area whose range is too wide to mean anything at a
// point (see wideDepthRangeM), leaving the cell uncharted rather than
// pessimistically shoal. Uncharted is passable-but-penalised, which is the
// honest reading of "somewhere between 0 and 18 m" — and where a finer cell
// charts the same water, its narrower range wins anyway.
func depareKeyDepth(f encFeature) (float64, bool) {
	min, max := depthRange(f)
	if !math.IsNaN(min) && !math.IsNaN(max) && max-min > wideDepthRangeM {
		return 0, false
	}
	if !math.IsNaN(min) {
		return min, true
	}
	if !math.IsNaN(max) {
		return max, true
	}
	return 0, false
}

// obstructionIsClear reports whether a wreck/obstruction/rock carries a
// charted depth over it (VALSOU, metres) that the boat clears. Anything
// without a VALSOU is treated as a hazard.
func obstructionIsClear(f encFeature, safeDepthM float64) bool {
	d, ok := obstructionDepth(f)
	return ok && d >= safeDepthM
}

// unsurveyedPenalty is the cost added on unsurveyed (UNSARE) water, which is
// always marked cellAvoid whether or not the caller asked to avoid anything.
const unsurveyedPenalty = 2.0

// avoidPenalty is the single weight the grid prices every cellAvoid cell at.
// One weight, not one per rule: the grid carries a flag, not a rule id, so a
// configured area with a heavier penalty raises the cost of all of them. That
// is deliberate — the flag exists to say "there is a reason to stay out of
// here", and the heaviest reason in play is the honest one to charge.
func avoidPenalty(areas []AvoidArea) float64 {
	best := unsurveyedPenalty
	for _, a := range areas {
		if a.Penalty > best {
			best = a.Penalty
		}
	}
	return best
}

// routeBBox is the search corridor: the endpoints' bounding box grown by padM
// on every side, so the router can go round a headland that sits outside the
// straight line between them.
func routeBBox(start, end RoutePoint, padM float64) [4]float64 {
	minLat := math.Min(start.Lat, end.Lat)
	maxLat := math.Max(start.Lat, end.Lat)
	minLon := math.Min(start.Lng, end.Lng)
	maxLon := math.Max(start.Lng, end.Lng)
	cosLat := math.Cos((minLat + maxLat) / 2 * math.Pi / 180)
	if cosLat < 0.05 {
		cosLat = 0.05
	}
	padLat := padM / metresPerDegreeLat
	padLon := padM / (metresPerDegreeLat * cosLat)
	return [4]float64{minLon - padLon, minLat - padLat, maxLon + padLon, maxLat + padLat}
}

// legTotalMeters is the rhumb-line length of the whole waypoint list — the
// "before" figure an optimisation is measured against.
func legTotalMeters(points []RoutePoint) float64 {
	return pathDistanceM(points)
}

// longestLegMeters sizes the corridor. The pad is a fraction of a leg, and on
// a multi-waypoint route the longest leg is the one that needs the most room
// to get around something.
func longestLegMeters(points []RoutePoint) float64 {
	longest := 0.0
	for i := 1; i < len(points); i++ {
		if d := haversineMeters(points[i-1].Lat, points[i-1].Lng, points[i].Lat, points[i].Lng); d > longest {
			longest = d
		}
	}
	return longest
}

// pointsBBox is the corridor for a whole waypoint list: their bounding box
// grown by padM on every side.
func pointsBBox(points []RoutePoint, padM float64) [4]float64 {
	minLat, maxLat := math.Inf(1), math.Inf(-1)
	minLon, maxLon := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		minLat, maxLat = math.Min(minLat, p.Lat), math.Max(maxLat, p.Lat)
		minLon, maxLon = math.Min(minLon, p.Lng), math.Max(maxLon, p.Lng)
	}
	padLat := padM / metresPerDegreeLat
	padLon := padM / (metresPerDegreeLat * clampCosLat((minLat+maxLat)/2))
	return [4]float64{minLon - padLon, minLat - padLat, maxLon + padLon, maxLat + padLat}
}

func pathDistanceM(pts []RoutePoint) float64 {
	total := 0.0
	for i := 1; i < len(pts); i++ {
		total += haversineMeters(pts[i-1].Lat, pts[i-1].Lng, pts[i].Lat, pts[i].Lng)
	}
	return total
}

func validLatLng(p RoutePoint) bool {
	return !math.IsNaN(p.Lat) && !math.IsNaN(p.Lng) &&
		p.Lat >= -90 && p.Lat <= 90 && p.Lng >= -180 && p.Lng <= 180
}
