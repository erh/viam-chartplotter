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
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
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
	// Penalty is added to the cost multiplier of every cell the area covers,
	// and applies only because the caller asked for this rule. A charted
	// restricted area costs nothing on a route that did not request it.
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
	// UnknownPenaltyRangeM is how far from land or a shoal that penalty
	// reaches. Uncharted water beyond it is open sea and costs nothing extra.
	UnknownPenaltyRangeM float64

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
	// An explicitly raised MaxCellM raises the ceiling with it. Otherwise the
	// topology pass, whose whole purpose is to accept a coarse grid, would be
	// held to the same limit as the pass it exists to make possible.
	ceiling := math.Max(maxCellCeilingM, opts.MaxCellM)
	want := longestLegMeters(points) / legCellRatio
	if want < opts.MaxCellM {
		want = opts.MaxCellM
	}
	return math.Min(want, ceiling)
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
		SafeDepthM:           safeDepthM,
		IdealDepthM:          2 * safeDepthM,
		HardClearanceM:       30,
		SoftClearanceM:       150,
		DepthPenalty:         1.5,
		ShorePenalty:         2.0,
		UnknownPenalty:       1.0,
		UnknownPenaltyRangeM: 2 * 1852,
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
	if o.UnknownPenaltyRangeM <= 0 {
		o.UnknownPenaltyRangeM = d.UnknownPenaltyRangeM
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

	// joins are the indices in Waypoints where two independently planned
	// sections were stitched together. Internal: they are scaffolding the
	// straightening pass needs, not something a client should see.
	joins []int `json:"-"`
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
	// Only two settings, and the threshold is high. A ceiling is a promise
	// that the detail being dropped is finer than the grid can express, and
	// band 4 broke that promise: a query returns whole features, so a
	// fine-scale LNDARE covering a shoreline is painted in full while the
	// fine-scale water beside it is left out, and fine land over coarse water
	// cannot be cleared. That mismatch walled Portland harbour in. Take
	// everything until the grid is genuinely too coarse to hold harbour
	// detail, then drop to coastal charting in one step.
	if cellM >= 150 {
		return 3
	}
	return 0
}

// routingFeatures fetches the chart the router will raster: the corridor at the
// usage band matching the grid, plus full detail around the ends.
func (r *ENCRenderer) routingFeatures(ctx context.Context, bbox [4]float64, opts AutoRouteOptions) ([]*mongoFeature, error) {
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

	// Charted channels are fetched at EVERY band, whatever ceiling the grid's
	// resolution implies. A canal is drawn in the harbour cells the ceiling
	// excludes — the Cape Cod Canal's fairway only exists at band 5 — so
	// without this the one feature that makes a narrow passage routable is the
	// one feature guaranteed to be missing. There are few of them, so the
	// extra query is cheap even over a whole corridor.
	//
	// Note what is NOT done here: an earlier version also fetched every class
	// at every band in a small box around each endpoint, to recover
	// berth-level detail where the boat manoeuvres. It walled harbours in. A
	// query returns whole features, so a fine-scale LNDARE covering a
	// shoreline is painted far outside the box that asked for it, while the
	// matching fine-scale water is not — and fine land over coarse water
	// cannot be cleared, because only a finer water feature may override land.
	// Measured on Portland: with those boxes a flood from the harbour reached
	// 339 cells and never got to sea; without them, 186,119 and out. Detail
	// has to arrive at a consistent scale or not at all.
	seen := make(map[string]struct{}, len(features))
	for _, f := range features {
		seen[f.id] = struct{}{}
	}
	channels, err := r.queryFeaturesClasses(ctx, bbox[0], bbox[1], bbox[2], bbox[3], noaa.ClassQuery{
		Classes:    autoRouteChannelClasses,
		UseLowGeom: useLowGeomForCell(cell),
		Projection: RoutingProjection(),
	})
	if err != nil {
		r.logger.Warnf("auto-route: channel query failed, narrow passages may not route: %v", err)
	}
	for _, f := range channels {
		if _, dup := seen[f.id]; dup {
			continue
		}
		seen[f.id] = struct{}{}
		features = append(features, f)
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
	classes = append(classes, autoRouteChannelClasses...)
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

// autoRouteChannelClasses are the charted navigable channels. A fairway is the
// chart saying "this is the way through" — the Cape Cod Canal is a FAIRWY
// named "Cape Cod Canal Channel" — and without them a passage narrower than a
// grid cell cannot be routed at all, however the surrounding water is charted.
var autoRouteChannelClasses = []string{"FAIRWY", "DRGARE"}

func isChannelClassForRouting(class string) bool {
	for _, c := range autoRouteChannelClasses {
		if c == class {
			return true
		}
	}
	return false
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
	// A leg too long to resolve is planned coarsely first, and its shape used
	// as anchors for the real pass. Subdividing the rhumb line instead would
	// be quicker and wrong: an evenly-spaced midpoint between New York and
	// Provincetown lands on Long Island, and snapping it to the nearest water
	// picks a side of the island at random.
	points, userPlaced, err := r.anchorLongLegs(points, opts, explicitPad)
	if err != nil {
		return nil, err
	}

	sections, err := sectionsForResolution(points, opts, explicitPad)
	if err != nil {
		return nil, err
	}

	parts, err := r.planSections(points, userPlaced, sections, opts, explicitPad)
	if err != nil {
		return nil, err
	}

	res := mergeRouteResults(parts, points)
	r.straightenSectionJoins(res, opts, explicitPad)
	r.refineTightWater(res, opts, explicitPad)
	r.repairLegsOverLand(res, opts, explicitPad)
	res.DistanceMeters = pathDistanceM(res.Waypoints)
	res.ElapsedMs = float64(time.Since(began).Microseconds()) / 1000.0
	return res, nil
}

// refineTightWater re-plans the stretches of a finished route that run through
// constrained water, each on its own grid sized for it.
//
// A route is only as accurate as the grid it was planned on. A canal 146 m
// wide planned on a 136 m grid is drawn to the nearest cell centre, which is
// the bank: a Cape Cod Canal transit had eight of its legs over land, and no
// amount of smoothing fixes that because the smoother is checking the same
// coarse grid that put it there. The fix is to plan that water at a resolution
// that can see it — 20 m rather than 136 — and the only stretches that need it
// are the ones the route already tells us about, because the smoother leaves
// marks close together exactly where the straight line between them would
// leave the channel.
func (r *ENCRenderer) refineTightWater(res *AutoRouteResult, opts AutoRouteOptions, explicitPad float64) {
	runs := tightWaterRuns(res.Waypoints)
	if len(runs) == 0 {
		return
	}
	refined := 0
	// Back to front: a splice changes the length of the list, so working from
	// the end leaves the earlier runs' indices valid. (Recomputing the runs
	// inside a range loop does not help — range still walks the stale slice,
	// which is an index out of range waiting to happen.)
	for ri := len(runs) - 1; ri >= 0; ri-- {
		run := runs[ri]
		if refined >= maxTightWaterRefinements {
			break
		}
		if run[0] < 0 || run[1] >= len(res.Waypoints) || run[1] <= run[0] {
			continue
		}
		from, to := res.Waypoints[run[0]], res.Waypoints[run[1]]
		// A run that spans most of the route cannot be re-planned any finer —
		// its bbox is the route's bbox.
		if haversineMeters(from.Lat, from.Lng, to.Lat, to.Lng) > maxTightRunFraction*res.DistanceMeters {
			continue
		}
		sub, err := r.planSection([]RoutePoint{from, to}, []bool{true, true}, opts, explicitPad)
		if err != nil || len(sub.Waypoints) < 2 {
			continue // the original stretch stands
		}
		// Only take the re-plan if it is actually finer; otherwise it is the
		// same grid and the same answer.
		if sub.CellSizeMeters >= res.CellSizeMeters {
			continue
		}
		// Finer is not automatically better. At the Cape Cod Canal's east
		// entrance this stretch runs beside the Sandwich breakwater — 84 m of
		// stone the re-plan's grid could see but did not respect. It came back
		// 386 m shorter with one 98 m leg straight across the jetty, and
		// repairLegsOverLand then had to undo that by splicing a detour round
		// the breakwater's tip, leaving the route 337 m LONGER and three
		// waypoints messier than before the refinement. Judge the replacement
		// the way that safety pass will, and keep what we had when it loses.
		if r.replacementCrossesMoreLand(res.Waypoints[run[0]:run[1]+1], sub.Waypoints) {
			continue
		}
		res.Waypoints = spliceWaypoints(res.Waypoints, run[0], run[1], sub.Waypoints)
		if sub.MinDepthMeters != nil && (res.MinDepthMeters == nil || *sub.MinDepthMeters < *res.MinDepthMeters) {
			d := *sub.MinDepthMeters
			res.MinDepthMeters = &d
		}
		refined++
	}
	if refined > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d stretch(es) through constrained water re-planned at finer resolution", refined))
	}
}

// maxJoinStraightenings bounds the section-join pass. Each straightening is a
// re-plan, and a route with more joins than this is long enough that the odd
// dogleg costs less than the time to look for them all.
const maxJoinStraightenings = 8

// straightenSectionJoins re-plans the route across the waypoints where its
// sections were joined.
//
// Sections are planned independently, and each must begin and end exactly on
// the point that joins it to the next. Those points are anchors: a coarse
// pass's guesses, placed on a grid too coarse to see what is actually there.
// At the Cape Cod Canal's east entrance one landed north-west of the Sandwich
// breakwater — 84 m of stone that a 408 m coarse cell cannot resolve — so the
// route approached down the wrong side of the jetty and had to come back round
// its tip to get in. The water either side is navigable and every leg is
// clear, so nothing downstream calls it an error. It is simply not the way in.
//
// Anchors are scaffolding, so once the route exists it is allowed to
// straighten across them: re-plan the short span from the waypoint before a
// join to the one after, on a grid sized for that span rather than for the
// section, and take the result when it is clear of land and no longer than
// what it replaces. Nothing is dropped that the operator placed — those points
// are the route, and they are never section joins.
func (r *ENCRenderer) straightenSectionJoins(res *AutoRouteResult, opts AutoRouteOptions, explicitPad float64) {
	straightened := 0
	// Back to front, so a splice cannot invalidate an earlier join's index.
	for ji := len(res.joins) - 1; ji >= 0; ji-- {
		if straightened >= maxJoinStraightenings {
			break
		}
		j := res.joins[ji]
		if j <= 0 || j+1 >= len(res.Waypoints) {
			continue
		}
		from, to := res.Waypoints[j-1], res.Waypoints[j+1]
		sub, err := r.planSection([]RoutePoint{from, to}, []bool{true, true}, opts, explicitPad)
		if err != nil || len(sub.Waypoints) < 2 {
			continue // the join stands
		}
		span := res.Waypoints[j-1 : j+2]
		if pathDistanceM(sub.Waypoints) > pathDistanceM(span) {
			continue // no shorter than going through the join
		}
		if len(r.legsOverLand(sub.Waypoints)) > len(r.legsOverLand(span)) {
			continue // and not at the cost of putting a leg on the beach
		}
		res.Waypoints = spliceWaypoints(res.Waypoints, j-1, j+1, sub.Waypoints)
		straightened++
	}
}

// replacementCrossesMoreLand reports whether a re-planned stretch puts more
// legs over land than the stretch it would replace. Both are measured against
// the finest charted grid — the one repairLegsOverLand uses — because that is
// the only view that settles a disagreement between two coarser ones.
//
// A replacement that crosses nothing is always fine, which is the common case
// and costs one check. The original is only examined when the replacement is
// already suspect: refining a canal transit that had eight legs on the bank
// into one that has two is exactly what this pass is for, so the test is
// "more", not "any".
func (r *ENCRenderer) replacementCrossesMoreLand(original, replacement []RoutePoint) bool {
	after := len(r.legsOverLand(replacement))
	if after == 0 {
		return false
	}
	return after > len(r.legsOverLand(original))
}

// repairLegsOverLand is the last word on safety: it checks the finished route
// against the finest charted grid available and re-plans any leg that crosses
// land.
//
// Everything upstream reasons about the grid the route was planned on, and a
// route is only ever as accurate as that grid. In a canal narrower than a cell
// the two disagree — a leg the planner believes is in the channel is drawn
// across the bank — and no amount of smoothing catches it, because the
// smoother is consulting the same grid that put it there. This pass consults a
// finer one, so what it reports is what a plotter would actually draw.
func (r *ENCRenderer) repairLegsOverLand(res *AutoRouteResult, opts AutoRouteOptions, explicitPad float64) {
	repaired, remaining := 0, 0
	for pass := 0; pass < maxLandRepairPasses; pass++ {
		bad := r.legsOverLand(res.Waypoints)
		remaining = len(bad)
		if len(bad) == 0 {
			return
		}
		fixed := false
		// Work from the end so splicing doesn't invalidate earlier indices.
		for i := len(bad) - 1; i >= 0 && repaired < maxLandRepairs; i-- {
			leg := bad[i]
			sub, err := r.planSection(
				[]RoutePoint{res.Waypoints[leg], res.Waypoints[leg+1]},
				[]bool{true, true}, opts, explicitPad)
			if err != nil || len(sub.Waypoints) < 2 {
				continue
			}
			res.Waypoints = spliceWaypoints(res.Waypoints, leg, leg+1, sub.Waypoints)
			repaired++
			fixed = true
		}
		if !fixed {
			break
		}
	}
	res.DistanceMeters = pathDistanceM(res.Waypoints)
	if repaired > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d leg(s) re-planned after checking against the detailed chart", repaired))
	}
	if remaining > 0 {
		// Never silent. A leg the router could not get off the ground is the
		// one thing an operator must be told about.
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d leg(s) still cross charted land at full detail — check them before steering this route", remaining))
	}
}

// legsOverLand returns the indices of legs whose straight line crosses charted
// land, checked against the finest tiles the ladder offers.
func (r *ENCRenderer) legsOverLand(pts []RoutePoint) []int {
	if r.navColl == nil || len(pts) < 2 {
		return nil
	}
	var bad []int
	for i := 0; i+1 < len(pts); i++ {
		if r.legCrossesLand(pts[i], pts[i+1]) {
			bad = append(bad, i)
		}
	}
	return bad
}

// legCrossesLand samples one leg against a fine local grid.
func (r *ENCRenderer) legCrossesLand(a, b RoutePoint) bool {
	pad := landCheckPadM / metresPerDegreeLat
	bbox := [4]float64{
		math.Min(a.Lng, b.Lng) - pad/clampCosLat(a.Lat), math.Min(a.Lat, b.Lat) - pad,
		math.Max(a.Lng, b.Lng) + pad/clampCosLat(a.Lat), math.Max(a.Lat, b.Lat) + pad,
	}
	ctx, cancel := context.WithTimeout(context.Background(), landCheckTimeout)
	defer cancel()
	tiles, err := r.NavTiles(ctx, navMaxZoom, bbox[0], bbox[1], bbox[2], bbox[3])
	if err != nil || len(tiles) == 0 {
		return false // cannot check; do not invent a problem
	}
	scale := float64(int(1) << navMaxZoom)
	n := noaa.NavTileSize
	steps := int(haversineMeters(a.Lat, a.Lng, b.Lat, b.Lng)/landCheckStepM) + 1
	for s := 0; s <= steps; s++ {
		f := float64(s) / float64(steps)
		lat := a.Lat + f*(b.Lat-a.Lat)
		lon := a.Lng + f*(b.Lng-a.Lng)
		fx, fy := lonLatToTileFrac(lon, lat, scale)
		tile := tiles[[3]int{navMaxZoom, int(math.Floor(fx)), int(math.Floor(fy))}]
		if tile == nil {
			continue
		}
		px := clampIndex(int((fx-math.Floor(fx))*float64(n)), n)
		py := clampIndex(int((fy-math.Floor(fy))*float64(n)), n)
		fl := tile.Flags[py*n+px]
		if fl&noaa.NavFlagLand != 0 && fl&noaa.NavFlagChannel == 0 {
			return true
		}
	}
	return false
}

const (
	// landCheckStepM is how finely a leg is sampled. Below the finest tile's
	// cell size, so nothing is stepped over.
	landCheckStepM = 10
	// landCheckPadM is the margin around a leg when fetching tiles to check it.
	landCheckPadM = 200
	// maxLandRepairs and maxLandRepairPasses bound the extra planning. A route
	// needing more than this has something wrong with it that re-planning
	// individual legs will not fix, and the operator is told instead.
	maxLandRepairs      = 12
	maxLandRepairPasses = 3
	landCheckTimeout    = 60 * time.Second
)

// tightWaterRuns finds maximal runs of waypoints joined by short legs, with
// one waypoint of context on each side. A short leg is the smoother saying the
// straight line between these marks would leave the water.
func tightWaterRuns(pts []RoutePoint) [][2]int {
	var runs [][2]int
	i := 1
	for i < len(pts) {
		if haversineMeters(pts[i-1].Lat, pts[i-1].Lng, pts[i].Lat, pts[i].Lng) >= tightWaterLegM {
			i++
			continue
		}
		start := i - 1
		for i < len(pts) && haversineMeters(pts[i-1].Lat, pts[i-1].Lng, pts[i].Lat, pts[i].Lng) < tightWaterLegM {
			i++
		}
		end := i - 1
		// Deliberately no context expansion. Reaching out a mark on each side
		// pulls in the long approach legs either end of a canal, and the
		// refinement bbox then covers the whole passage again — which is the
		// grid we are trying to get away from. The run's own ends are already
		// real waypoints, so the splice joins cleanly without them.
		if end > start {
			runs = append(runs, [2]int{start, end})
		}
	}
	return runs
}

// spliceWaypoints replaces pts[from..to] with the replacement, INCLUDING the
// replacement's own end positions.
//
// Keeping the original ends instead looks harmless — they are the same
// positions the re-plan was asked for — but a re-plan may have had to move one
// off land to find navigable water, and dropping it reconnects the route to
// the very waypoint that was over the bank. That is why re-planning a bad leg
// left it just as bad.
func spliceWaypoints(pts []RoutePoint, from, to int, replacement []RoutePoint) []RoutePoint {
	out := make([]RoutePoint, 0, len(pts)+len(replacement))
	out = append(out, pts[:from]...)
	out = append(out, replacement...)
	return append(out, pts[to+1:]...)
}

// tightWaterLegM is the leg length below which a stretch is treated as
// constrained enough to be worth re-planning finely.
const tightWaterLegM = 2 * 1852

// maxTightWaterRefinements bounds the extra planning one route can trigger.
const maxTightWaterRefinements = 6

// maxTightRunFraction is how much of a route a single tight run may span
// before re-planning it is pointless: past this its bbox is the route's bbox
// and the "finer" grid is the same grid.
const maxTightRunFraction = 0.5

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
func (r *ENCRenderer) planSections(points []RoutePoint, userPlaced []bool, sections [][2]int, opts AutoRouteOptions, explicitPad float64) ([]*AutoRouteResult, error) {
	parts := make([]*AutoRouteResult, len(sections))
	errs := make([]error, len(sections))

	var wg sync.WaitGroup
	sem := make(chan struct{}, sectionConcurrency)
	for i, rng := range sections {
		wg.Add(1)
		go func(i int, rng [2]int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			parts[i], errs[i] = r.planSection(points[rng[0]:rng[1]+1], userPlaced[rng[0]:rng[1]+1], opts, explicitPad)
		}(i, rng)
	}
	wg.Wait()

	// A section that failed gets one more try, leg by leg.
	//
	// A section's grid resolution is set by its LONGEST leg, so a section that
	// runs twenty miles across open water and then two hundred yards into a
	// harbour plans both on the open-water grid — and the harbour entrance is
	// narrower than a cell. Each leg on its own gets a grid sized for itself.
	// Verified: the leg into Provincetown fails inside its section and routes
	// in seven waypoints alone.
	out := make([]*AutoRouteResult, 0, len(parts))
	for i, err := range errs {
		if err == nil {
			out = append(out, parts[i])
			continue
		}
		rng := sections[i]
		if rng[1]-rng[0] < 2 {
			return nil, fmt.Errorf("section %d of %d: %w", i+1, len(sections), err)
		}
		secPts := points[rng[0] : rng[1]+1]
		secUser := userPlaced[rng[0] : rng[1]+1]
		split, serr := r.planLegByLeg(secPts, secUser, opts, explicitPad)
		if serr == nil {
			out = append(out, split...)
			continue
		}
		// Last resort: drop the section's anchors and plan straight through.
		//
		// Anchors are a coarse pass's guesses at a corridor, and a pair of them
		// can be unroutable at fine resolution even though the water between
		// their neighbours is fine. Holding the route to them then fails the
		// whole passage for the sake of scaffolding. Points the operator placed
		// are kept — those are the route.
		if bare, bareUser := keepUserPlaced(secPts, secUser); len(bare) >= 2 && len(bare) < len(secPts) {
			if direct, derr := r.planLegByLeg(bare, bareUser, opts, explicitPad); derr == nil {
				out = append(out, direct...)
				continue
			}
		}
		return nil, fmt.Errorf("section %d of %d: %w", i+1, len(sections), serr)
	}
	return out, nil
}

// keepUserPlaced strips the anchors from a run, keeping its ends and anything
// the operator placed.
func keepUserPlaced(points []RoutePoint, userPlaced []bool) ([]RoutePoint, []bool) {
	pts := make([]RoutePoint, 0, len(points))
	user := make([]bool, 0, len(points))
	for i := range points {
		if i == 0 || i == len(points)-1 || userPlaced[i] {
			pts = append(pts, points[i])
			user = append(user, userPlaced[i])
		}
	}
	return pts, user
}

// planLegByLeg plans each leg of a section on its own grid.
func (r *ENCRenderer) planLegByLeg(points []RoutePoint, userPlaced []bool, opts AutoRouteOptions, explicitPad float64) ([]*AutoRouteResult, error) {
	out := make([]*AutoRouteResult, 0, len(points)-1)
	i := 0
	for i+1 < len(points) {
		// A leg that will not plan is retried with its far end dropped, as
		// long as that end is an anchor rather than a waypoint the operator
		// placed. Anchors come from a coarse pass and can land somewhere that
		// is navigable at 600 m and a shoal at 30 m; they are scaffolding, so
		// the right answer is to remove one, not to fail the route. A point
		// the operator placed is never dropped — if that cannot be reached,
		// the route genuinely cannot be planned and they need to know.
		end := i + 1
		var res *AutoRouteResult
		var err error
		for end < len(points) {
			res, err = r.planSection(points[i:end+1], userPlaced[i:end+1], opts, explicitPad)
			if err == nil {
				break
			}
			if end+1 >= len(points) || userPlaced[end] {
				return nil, fmt.Errorf("leg %d of %d: %w", i+1, len(points)-1, err)
			}
			end++
		}
		if err != nil {
			return nil, fmt.Errorf("leg %d of %d: %w", i+1, len(points)-1, err)
		}
		out = append(out, res)
		i = end
	}
	return out, nil
}

// coarsePassCellM is the resolution the topology pass may fall back to. Far
// coarser than anything worth steering — a kilometre cell cannot see a
// channel — but it is deciding which side of Long Island to pass, not where
// to put the boat, and the fine pass that follows fixes the detail.
const coarsePassCellM = 2500

// coarsePassPenalty is what the soft costs are damped to during the topology
// pass. Not zero: a tiebreak between two equal-length corridors should still
// prefer the deeper, more open one.
const coarsePassPenalty = 0.05

// coarseAnchorMinSpacingM only drops anchors that are nearly coincident. The
// coarse pass's own spacing carries the information about where the water is
// tight, so it is kept.
const coarseAnchorMinSpacingM = 0.4 * 1852

// legResolutionRatio is how many grid cells must span a section's SHORTEST
// leg. A short leg means tight water — the smoother only puts marks close
// together where the straight line between them leaves the channel — so a
// section holding one may not be planned on a grid too coarse to see it. This
// is what stops a canal being planned on the open-water grid it shares a
// section with, and what kept eight legs of a canal transit off the banks.
const legResolutionRatio = 20

// anchorLongLegs replaces any leg too long to plan at a useful resolution with
// a coarse route through the same water, so the rest of the pipeline sees legs
// it can actually handle. Legs that already fit are passed through untouched.
// The returned mask marks which points the caller actually placed. Anchors
// are scaffolding — they exist so the planner can work at a useful resolution,
// and preserving them as waypoints litters a long route with marks nobody
// asked for and no reason to steer to.
func (r *ENCRenderer) anchorLongLegs(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) ([]RoutePoint, []bool, error) {
	out := []RoutePoint{points[0]}
	mine := []bool{true}
	for i := 0; i+1 < len(points); i++ {
		leg := points[i : i+2]
		if !sectionFits(leg, opts, explicitPad) {
			anchors, err := r.coarseAnchors(leg, opts, explicitPad)
			if err != nil {
				return nil, nil, fmt.Errorf("leg %d of %d: %w", i+1, len(points)-1, err)
			}
			for _, a := range anchors {
				out = append(out, a)
				mine = append(mine, false)
			}
		}
		out = append(out, points[i+1])
		mine = append(mine, true)
	}
	return out, mine, nil
}

// coarseAnchors plans one over-long leg at whatever resolution it takes, and
// returns intermediate points from that route.
func (r *ENCRenderer) coarseAnchors(leg []RoutePoint, opts AutoRouteOptions, explicitPad float64) ([]RoutePoint, error) {
	coarse := opts
	coarse.MaxCellM = coarsePassCellM
	coarse.KeepWaypoints = false
	// The topology pass answers one question — which way round — so it is
	// scored on distance, not on comfort. Its preferences are damped almost to
	// nothing: what BLOCKS a cell (land, an obstruction, water shoaler than the
	// boat's draft, the clearance buffer) is untouched, but the soft costs are
	// not allowed to decide the corridor. Measured: with them in force the
	// corridor comes out 307.8 nm, with them damped 293.5 nm.
	coarse.DepthPenalty = coarsePassPenalty
	coarse.ShorePenalty = coarsePassPenalty
	coarse.UnknownPenalty = coarsePassPenalty
	// The corridor pad is what makes a long leg's box enormous; the topology
	// pass does not need room to wander, only to get round the land.
	coarse.CorridorPadM = math.Min(sectionPadM(leg, explicitPad), 20*1852)
	if !sectionFits(leg, coarse, coarse.CorridorPadM) {
		cell := gridCellSize(pointsBBox(leg, coarse.CorridorPadM), coarse)
		return nil, fmt.Errorf("%.0f nm needs %.0f m grid cells, past even the coarse limit of %.0f m: split it with an intermediate waypoint",
			haversineMeters(leg[0].Lat, leg[0].Lng, leg[1].Lat, leg[1].Lng)/1852, cell, coarse.MaxCellM)
	}

	res, err := r.planSection(leg, []bool{true, true}, coarse, coarse.CorridorPadM)
	if err != nil {
		return nil, err
	}
	// Keep the coarse route's own spacing rather than thinning to a fixed
	// interval. Its marks are already density-adapted — close together where
	// the water is tight, far apart offshore — and that density is exactly the
	// signal the detail pass needs to know where it must plan finely. Thinning
	// to 15 nm threw that signal away and handed the canal to a grid sized for
	// open water.
	var out []RoutePoint
	last := leg[0]
	for _, w := range res.Waypoints[1 : len(res.Waypoints)-1] {
		if haversineMeters(last.Lat, last.Lng, w.Lat, w.Lng) < coarseAnchorMinSpacingM {
			continue
		}
		out = append(out, w)
		last = w
	}
	return out, nil
}

// navTilesBudget bounds building the tiles one section needs. Generous
// because it is paid once; `chartdiag prewarm` is how you avoid paying it in
// front of a user.
const navTilesBudget = 20 * time.Minute

// planSection plans one contiguous run of waypoints on its own grid.
//
// The grid comes from precomputed navigability tiles when they are available,
// and from the polygons when they are not. Reading tiles is the difference
// between fetching 58 MB of coastline to rasterise and fetching a few hundred
// kilobytes of grid that is already rasterised; the fallback exists so a
// deployment with no tiles built (or a gap in coverage) still routes.
func (r *ENCRenderer) planSection(points []RoutePoint, userPlaced []bool, opts AutoRouteOptions, explicitPad float64) (*AutoRouteResult, error) {
	bbox := pointsBBox(points, sectionPadM(points, explicitPad))
	ctx, cancel := context.WithTimeout(context.Background(), autoRouteQueryTimeout)
	defer cancel()

	if r.navColl != nil {
		wanted := gridCellSize(bbox, opts)
		midLat := (bbox[1] + bbox[3]) / 2
		z := navTileZoomFor(wanted, midLat)
		// Tile building gets its own, far longer budget than a chart query.
		// It is a one-time cost paid once per piece of water and read forever
		// after — a continental-scale route can be hundreds of tiles on its
		// first run — whereas autoRouteQueryTimeout bounds a single query.
		tileCtx, tileCancel := context.WithTimeout(context.Background(), navTilesBudget)
		tiles, err := r.NavTiles(tileCtx, z, bbox[0], bbox[1], bbox[2], bbox[3])
		tileCancel()
		if err != nil {
			return nil, err
		}
		o := opts
		o.normalize(longestLegMeters(points))
		g := newNavGrid(bbox[0], bbox[1], bbox[2], bbox[3], o.MaxCells, o.MinCellM, o.MaxGridDim)
		if sampleTilesIntoGrid(g, tiles, z) > 0 {
			return planRouteOnGrid(g, bbox, points, userPlaced, opts)
		}
	}

	features, err := r.routingFeatures(ctx, bbox, opts)
	if err != nil {
		return nil, err
	}
	var ways []osmtiler.Feature
	if r.osm != nil {
		if w, werr := osmtiler.NavigableWaterways(ctx, r.osm, bbox[0], bbox[1], bbox[2], bbox[3]); werr != nil {
			r.logger.Warnf("auto-route: waterway query failed, narrow passages may not route: %v", werr)
		} else {
			ways = w
		}
	}
	return planRouteViaWithWays(features, ways, bbox, points, userPlaced, opts)
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
func sectionsForResolution(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) ([][2]int, error) {
	var out [][2]int
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
		out = append(out, [2]int{start, end})
		start = end
	}
	return out, nil
}

func sectionFits(points []RoutePoint, opts AutoRouteOptions, explicitPad float64) bool {
	cell := gridCellSize(pointsBBox(points, sectionPadM(points, explicitPad)), opts)
	if cell > effectiveMaxCellM(points, opts) {
		return false
	}
	// And the grid has to resolve the tightest water in the section.
	if shortest := shortestLegMeters(points); shortest > 0 {
		if want := shortest / legResolutionRatio; want > opts.MinCellM && cell > want {
			return false
		}
	}
	return true
}

// shortestLegMeters is the length of the shortest leg in a run of waypoints.
func shortestLegMeters(points []RoutePoint) float64 {
	shortest := math.Inf(1)
	for i := 1; i < len(points); i++ {
		if d := haversineMeters(points[i-1].Lat, points[i-1].Lng, points[i].Lat, points[i].Lng); d < shortest {
			shortest = d
		}
	}
	if math.IsInf(shortest, 1) {
		return 0
	}
	return shortest
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
			// The waypoint the two sections share is where they were forced to
			// meet. straightenSectionJoins needs to know which ones those are.
			res.joins = append(res.joins, len(res.Waypoints)-1)
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
	// Drop marks that sit within the plan's own precision of the line between
	// their neighbours. Each section is smoothed against its own grid and then
	// concatenated, so nothing ever looks at the route as a whole — and a long
	// passage arrives carrying a mark every few miles that bends it by a
	// boat-length. The tolerance is one grid cell: removing such a point moves
	// the track less than the resolution the route was planned at, so it
	// cannot claim precision the plan never had.
	res.DistanceMeters = pathDistanceM(res.Waypoints)
	return res
}

// dropWithinTolerance removes waypoints whose cross-track offset from the line
// between their neighbours is under toleranceM. Purely geometric: it never
// moves the track further than the tolerance, so it cannot steer the route
// into water the planner didn't already accept.
func (g *navGrid) dropWithinTolerance(pts []RoutePoint, toleranceM float64) []RoutePoint {
	if len(pts) < 3 || toleranceM <= 0 {
		return pts
	}
	// Repeat until nothing more can go. One pass is not enough: a cluster of
	// marks protects itself, because the proportional test measures against
	// legs that are short only because the cluster is there. Dropping one
	// lengthens its neighbours' legs and lets the next go.
	for {
		next := g.dropWithinTolerancePass(pts, toleranceM)
		if len(next) == len(pts) {
			return next
		}
		pts = next
	}
}

func (g *navGrid) dropWithinTolerancePass(pts []RoutePoint, toleranceM float64) []RoutePoint {
	if len(pts) < 3 {
		return pts
	}
	out := make([]RoutePoint, 0, len(pts))
	out = append(out, pts[0])
	for i := 1; i < len(pts)-1; i++ {
		prev := out[len(out)-1]
		// The tolerance is relative as well as absolute. One grid cell is the
		// right scale in open water, and far too coarse in a canal: the Cape
		// Cod Canal is 146 m wide, so a mark bending the track by "only" a
		// 136 m cell is most of the channel. Tying it to the shorter adjacent
		// leg keeps the test proportionate — tens of metres where the marks
		// are a cable apart, a full cell where they are miles apart.
		legM := math.Min(
			haversineMeters(prev.Lat, prev.Lng, pts[i].Lat, pts[i].Lng),
			haversineMeters(pts[i].Lat, pts[i].Lng, pts[i+1].Lat, pts[i+1].Lng))
		limit := math.Min(toleranceM, legM*dropRelativeFraction)
		// Measure against the last KEPT point, so a run of small bends can't
		// accumulate into a large one.
		if crossTrackMeters(prev, pts[i], pts[i+1]) > limit || !g.legIsClear(prev, pts[i+1]) {
			out = append(out, pts[i])
		}
	}
	return append(out, pts[len(pts)-1])
}

// dropRelativeFraction is how far, as a fraction of the shorter adjacent leg,
// a waypoint may sit off the line before it is worth keeping.
const dropRelativeFraction = 0.1

// legIsClear reports whether the straight line between two positions stays in
// navigable water.
//
// This simplifier used to be purely geometric, on the reasoning that moving
// the track by less than a grid cell could not steer it anywhere the planner
// had not already accepted. That reasoning is wrong wherever the water is
// narrower than a cell: in the Cape Cod Canal, 146 m wide and planned on a
// 136 m grid, a "within tolerance" shift is the bank. It put eight legs of one
// canal transit over land.
func (g *navGrid) legIsClear(a, b RoutePoint) bool {
	ai, ok1 := g.cellAt(a.Lng, a.Lat)
	bi, ok2 := g.cellAt(b.Lng, b.Lat)
	if !ok1 || !ok2 {
		return false
	}
	_, _, ok := g.traverse(ai, bi, nil)
	return ok
}

// crossTrackMeters is the perpendicular distance from m to the line a-b, on a
// local flat-earth approximation — accurate well past the leg lengths here.
func crossTrackMeters(a, m, b RoutePoint) float64 {
	cos := clampCosLat(a.Lat)
	ax, ay := 0.0, 0.0
	mx := (m.Lng - a.Lng) * metresPerDegreeLat * cos
	my := (m.Lat - a.Lat) * metresPerDegreeLat
	bx := (b.Lng - a.Lng) * metresPerDegreeLat * cos
	by := (b.Lat - a.Lat) * metresPerDegreeLat
	dx, dy := bx-ax, by-ay
	den := math.Hypot(dx, dy)
	if den == 0 {
		return math.Hypot(mx, my)
	}
	return math.Abs(dy*(mx-ax)-dx*(my-ay)) / den
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
	return planRouteViaWithWays(features, nil, bbox, points, allUserPlaced(len(points)), opts)
}

// allUserPlaced treats every point as the caller's, which is what the tests
// and the two-point case want.
func allUserPlaced(n int) []bool {
	m := make([]bool, n)
	for i := range m {
		m[i] = true
	}
	return m
}

func planRouteViaWithWays(features []*mongoFeature, ways []osmtiler.Feature, bbox [4]float64, points []RoutePoint, userPlaced []bool, opts AutoRouteOptions) (*AutoRouteResult, error) {
	o := opts
	o.normalize(longestLegMeters(points))
	g := newNavGrid(bbox[0], bbox[1], bbox[2], bbox[3], o.MaxCells, o.MinCellM, o.MaxGridDim)
	rasterizeForRouting(g, features, o)
	stampWaterways(g, ways)
	res, err := planRouteOnGrid(g, bbox, points, userPlaced, opts)
	if err != nil {
		return nil, err
	}
	res.FeatureCount = len(features)
	return res, nil
}

// planRouteOnGrid searches an already-rasterised grid. Split out so a grid
// read from precomputed tiles and one built from polygons take exactly the
// same path from here on.
func planRouteOnGrid(g *navGrid, bbox [4]float64, points []RoutePoint, userPlaced []bool, opts AutoRouteOptions) (*AutoRouteResult, error) {
	directM := legTotalMeters(points)
	opts.normalize(longestLegMeters(points))

	g.finalize(gridCost{
		SafeDepthM:           opts.SafeDepthM,
		IdealDepthM:          opts.IdealDepthM,
		HardClearanceM:       opts.HardClearanceM,
		SoftClearanceM:       opts.SoftClearanceM,
		DepthPenalty:         opts.DepthPenalty,
		ShorePenalty:         opts.ShorePenalty,
		UnknownPenalty:       opts.UnknownPenalty,
		UnknownPenaltyRangeM: opts.UnknownPenaltyRangeM,
		UnsurveyedPenalty:    unsurveyedPenalty,
		RestrictedPenalty:    restrictedPenalty(opts.Avoid),
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

	res.Waypoints = g.pathToWaypoints(full, legBounds, points, userPlaced, opts, res)
	// Simplify here, against THIS section's cell size. Doing it after the
	// sections are merged judges every one of them by the coarsest — so a
	// canal planned on 136 m cells gets thinned as though it were the 348 m
	// open-water leg it shares a route with, and the channel loses the marks
	// that keep it in the water.
	res.Waypoints = g.dropWithinTolerance(res.Waypoints, g.cellSizeM())
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
				UnknownPenalty: opts.UnknownPenalty, UnknownPenaltyRangeM: opts.UnknownPenaltyRangeM,
				UnsurveyedPenalty: unsurveyedPenalty, RestrictedPenalty: restrictedPenalty(opts.Avoid),
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
func (g *navGrid) pathToWaypoints(path []int, legBounds []int, points []RoutePoint, userPlaced []bool, opts AutoRouteOptions, res *AutoRouteResult) []RoutePoint {
	// Only the caller's own waypoints anchor the smoothing. Coarse-pass
	// anchors are scaffolding for the planner, and holding the route to them
	// scatters a long passage with marks nobody placed and nothing to steer
	// to. Smoothing runs straight through them.
	legBounds = userLegBounds(legBounds, userPlaced)

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

	pulled = g.dropReversals(pulled)
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

// userLegBounds keeps only the boundaries that fall on a waypoint the caller
// placed. The first and last always survive: they are the route's ends.
func userLegBounds(legBounds []int, userPlaced []bool) []int {
	if len(userPlaced) != len(legBounds) {
		return legBounds // shapes disagree; keep every boundary rather than guess
	}
	out := make([]int, 0, len(legBounds))
	for i, b := range legBounds {
		if i == 0 || i == len(legBounds)-1 || userPlaced[i] {
			out = append(out, b)
		}
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
			markGeometry(g.navGrid, geom, hazardRadius, func(i int) { g.markLand(i, scale) })

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
				g.fillRings(splitRings(geom.Coordinates), func(i int) {
					g.setDepth(i, key, scale)
					g.markWater(i, scale)
				})
			}

		case isChannelClassForRouting(class):
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			key, ok := depareKeyDepth(f)
			rings := splitRings(geom.Coordinates)
			markChannel := func(i int) {
				if ok {
					g.setDepth(i, key, scale)
					g.mark(i, cellChannelDepth)
				}
				g.mark(i, cellChannel)
				g.markWater(i, scale)
				if class == "DRGARE" {
					g.mark(i, cellDredged)
				}
			}
			g.fillRings(rings, markChannel)
			g.stampEdges(rings, markChannel)

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
			markGeometry(g, geom, hazardRadius, func(i int) { g.markLand(i, scale) })

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
			g.fillRings(splitRings(geom.Coordinates), func(i int) {
				g.setDepth(i, key, scale)
				g.markWater(i, scale)
			})

		case isChannelClassForRouting(class):
			if geom.Type != s57.GeometryTypePolygon {
				continue
			}
			// A charted channel is the way through, so it is rasterised to
			// stay connected: filled AND walked along its outline, because a
			// channel narrower than a cell survives only as a broken chain of
			// dots under centre-sampling alone.
			key, ok := depareKeyDepth(f)
			rings := splitRings(geom.Coordinates)
			markChannel := func(i int) {
				if ok {
					g.setDepth(i, key, scale)
					g.mark(i, cellChannelDepth)
				}
				g.mark(i, cellChannel)
				g.markWater(i, scale)
				if class == "DRGARE" {
					g.mark(i, cellDredged)
				}
			}
			g.fillRings(rings, markChannel)
			g.stampEdges(rings, markChannel)

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

// stampWaterways marks the cells along navigable waterway centrelines as
// channel, guaranteeing a connected path through a passage the raster cannot
// resolve. Depth still governs: a channel cell charted shoaler than the boat's
// safe depth blocks like anything else (see navGrid.finalize).
func stampWaterways(g *navGrid, ways []osmtiler.Feature) int {
	marked := 0
	for _, w := range ways {
		for i := 0; i+1 < len(w.Coords); i++ {
			a, b := w.Coords[i], w.Coords[i+1]
			g.stampSegment(a.Lon, a.Lat, b.Lon, b.Lat, func(idx int) {
				g.mark(idx, cellChannel)
				marked++
			})
		}
	}
	return marked
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

// unsurveyedPenalty is the cost added on unsurveyed (UNSARE) water. It always
// applies: an absence of survey is a fact about the chart, not a preference.
const unsurveyedPenalty = 2.0

// restrictedPenalty is what a charted restricted area costs — and it is zero
// unless the caller asked to avoid them. Entry to most of these is regulated,
// not forbidden, and charging for them by default routes a boat the long way
// round its own harbour.
func restrictedPenalty(areas []AvoidArea) float64 {
	worst := 0.0
	for _, a := range areas {
		if a.Class == "RESARE" && a.Penalty > worst {
			worst = a.Penalty
		}
	}
	return worst
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
