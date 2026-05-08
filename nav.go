package vc

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
	"go.viam.com/rdk/spatialmath"
)

var NavModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("nav")

func init() {
	resource.RegisterService(
		navigation.API,
		NavModel,
		resource.Registration[navigation.Service, *NavConfig]{
			Constructor: newNav,
		})
}

// NavConfig is the config for the chartplotter navigation service.
// MovementSensor is optional; when set, Location() reports the live
// position/heading of that movement sensor.
//
// DataPath, when set, is used as the JSON file that mirrors the waypoint
// list across restarts. When empty, waypoints persist to
// "<user-cache-dir>/viam-chartplotter/nav/<resource_name>.json".
type NavConfig struct {
	MovementSensor string `json:"movement_sensor,omitempty"`
	DataPath       string `json:"data_path,omitempty"`
}

// Validate ensures all parts of the config are valid and returns the
// implicit dependencies required by the service.
func (cfg *NavConfig) Validate(path string) ([]string, error) {
	var deps []string
	if cfg.MovementSensor != "" {
		deps = append(deps, cfg.MovementSensor)
	}
	return deps, nil
}

// arrivalCheckInterval is how often the background poller re-checks
// distance to the next waypoint.
const arrivalCheckInterval = 5 * time.Second

// arrivalDistanceMeters: when distanceToWaypoint drops below this, we've
// arrived at the waypoint regardless of any other signal.
const arrivalDistanceMeters = 50.0

// nearWaypointMeters: the bypass rule (passed-without-arriving) only
// fires when distanceToWaypoint is below this. Outside this we don't
// trust the bypass signals — we're not close enough to the waypoint for
// "moving away from it" to mean anything.
const nearWaypointMeters = 500.0

// lastWaypointPassMinDistanceMeters and lastWaypointPassFactor configure
// the fallback arrival rule for the *final* waypoint, where there's no
// following waypoint to compare against. We mark the last waypoint visited
// if the boat got within lastWaypointPassMinDistanceMeters of it AND has
// since moved at least lastWaypointPassFactor× that minimum away — i.e.
// an overshoot we can be confident about even without a direction
// reference. lastWaypointPassFactor is also used as the "definitely
// overshot" multiplier in the far-overshoot bypass rule below.
const lastWaypointPassMinDistanceMeters = 400.0
const lastWaypointPassFactor = 2.5

// followingWaypointApproachMeters is how much closer the boat must be to
// the following waypoint than the furthest it's been from it on this
// approach for the far-overshoot bypass rule to fire. Combined with a
// lastWaypointPassFactor× overshoot of the current waypoint, this is a
// strong "we passed it" signal even when we never came inside
// nearWaypointMeters.
const followingWaypointApproachMeters = 500.0

// arrivalLogInterval throttles the periodic "where are we relative to the
// next waypoint" log line. Logs once per interval at info level when the
// boat is within arrivalLogProximityMeters of the next waypoint, so an
// operator can confirm the poller is alive and see why a pass-through
// didn't fire. ~30 km covers typical coastal-cruising approach distances
// (well over 10 nm) so a missed pass leaves a trail in the log.
const arrivalLogInterval = 30 * time.Second
const arrivalLogProximityMeters = 30000.0

func newNav(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (navigation.Service, error) {
	cfg, err := resource.NativeConfig[*NavConfig](conf)
	if err != nil {
		return nil, err
	}

	dataPath := cfg.DataPath
	if dataPath == "" {
		dataPath = filepath.Join(resolveCacheRoot(""), "nav", conf.ResourceName().Name+".json")
	}
	store, err := newDiskNavStore(dataPath)
	if err != nil {
		return nil, err
	}

	svc := &navService{
		name:   conf.ResourceName(),
		logger: logger,
		store:  store,
	}
	svc.mode.Store(uint32(navigation.ModeManual))
	logger.Infof("nav waypoints persisted at %s", dataPath)

	if cfg.MovementSensor == "" {
		logger.Warnf("nav auto-arrival disabled: no `movement_sensor` configured; waypoints will only be removed by explicit RemoveWaypoint calls")
	} else {
		ms, err := movementsensor.FromDependencies(deps, cfg.MovementSensor)
		if err != nil {
			return nil, errors.Wrapf(err, "could not get movement_sensor %q", cfg.MovementSensor)
		}
		svc.ms = ms
		svc.startArrivalPoller()
		logger.Infof("nav auto-arrival enabled: arrival<%.0fm, near<%.0fm, polling every %s",
			arrivalDistanceMeters, nearWaypointMeters, arrivalCheckInterval)
	}

	return svc, nil
}

type navService struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu    sync.Mutex
	store navigation.NavStore
	ms    movementsensor.MovementSensor

	// mode is held atomically so reads don't need to take the mutex.
	mode atomic.Uint32

	// Cancellation for the background arrival poller, if started.
	arrivalCancel context.CancelFunc
	arrivalDone   chan struct{}

	// Rolling approach memory for the current waypoint and the one
	// following it. Read/written only from the arrival poller goroutine,
	// so no synchronisation needed.
	arrivalState   arrivalState
	lastArrivalLog time.Time
}

// maybeArrivalInfo throttles a poller-status info log to one per
// arrivalLogInterval. Used for the silent early-return paths in
// checkArrival so we can tell *why* a tick did nothing without flooding
// the log every 5 s.
func (s *navService) maybeArrivalInfo(msg string, kvs ...interface{}) {
	now := time.Now()
	if now.Sub(s.lastArrivalLog) < arrivalLogInterval {
		return
	}
	s.lastArrivalLog = now
	s.logger.Infow(msg, kvs...)
}

// arrivalState tracks the closest the boat has been to the current
// waypoint and the one following it on this approach. The "passed without
// arriving" rule fires when we've moved away from our closest approach to
// the current waypoint while simultaneously hitting a new minimum on the
// following waypoint — i.e. we rounded the corner without entering the
// arrival radius.
type arrivalState struct {
	waypointID                     primitive.ObjectID
	followingWaypointID            primitive.ObjectID // zero if no following waypoint
	minDistanceToWaypoint          float64            // meters
	minDistanceToFollowingWaypoint float64            // meters; meaningless when followingWaypointID is zero
	maxDistanceToFollowingWaypoint float64            // meters; meaningless when followingWaypointID is zero
}

// startArrivalPoller launches a background goroutine that periodically
// checks the boat's distance to the next unvisited waypoint and marks it
// visited (so it disappears from Waypoints()) once arrival is detected.
func (s *navService) startArrivalPoller() {
	ctx, cancel := context.WithCancel(context.Background())
	s.arrivalCancel = cancel
	s.arrivalDone = make(chan struct{})
	go func() {
		defer close(s.arrivalDone)
		ticker := time.NewTicker(arrivalCheckInterval)
		defer ticker.Stop()
		// Heartbeat so an operator can confirm the poller is alive even when
		// far from any waypoint (where the per-tick proximity log stays
		// quiet). Fires on the first tick and every minute thereafter.
		heartbeat := time.NewTicker(time.Minute)
		defer heartbeat.Stop()
		s.logger.Info("arrival poller: starting first tick")
		s.checkArrival(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkArrival(ctx)
			case <-heartbeat.C:
				s.logger.Infow("arrival poller: alive",
					"interval", arrivalCheckInterval.String(),
					"arrival_m", arrivalDistanceMeters,
					"near_m", nearWaypointMeters,
				)
			}
		}
	}()
}

// checkArrival is one tick of the arrival poller: pull the boat's
// position, look up the current waypoint (and the one following it, if
// any), and mark the current waypoint visited when one of:
//   - distanceToWaypoint < arrivalDistanceMeters (we're there), or
//   - distanceToWaypoint < nearWaypointMeters AND we've moved past our
//     closest approach to the current waypoint AND we just hit a new
//     minimum approach to the following waypoint — i.e. we rounded the
//     corner without entering the arrival radius, or
//   - last-waypoint overshoot: with no following waypoint to compare to,
//     we got within lastWaypointPassMinDistanceMeters and have since
//     moved at least lastWaypointPassFactor× that minimum away.
//
// Errors are surfaced at warn so an unhealthy sensor or store is visible
// in logs; the loop still keeps running so a transient blip doesn't
// disable auto-arrival.
func (s *navService) checkArrival(ctx context.Context) {
	if s.ms == nil {
		return
	}
	pt, _, err := s.ms.Position(ctx, nil)
	if err != nil {
		s.logger.Warnw("arrival poller: movement sensor Position failed", "err", err)
		return
	}
	if pt == nil {
		s.maybeArrivalInfo("arrival poller: movement sensor returned nil position")
		return
	}
	// Bail on null-island fixes; some movement sensors emit (0,0) before they
	// have a lock and that would falsely "arrive" at any waypoint near it.
	if pt.Lat() == 0 && pt.Lng() == 0 {
		s.maybeArrivalInfo("arrival poller: skipping null-island fix",
			"lat", pt.Lat(), "lng", pt.Lng())
		return
	}
	wps, err := s.store.Waypoints(ctx)
	if err != nil {
		s.logger.Warnw("arrival poller: Waypoints failed", "err", err)
		return
	}
	if len(wps) == 0 {
		s.maybeArrivalInfo("arrival poller: no unvisited waypoints",
			"lat", pt.Lat(), "lng", pt.Lng())
		return
	}
	waypoint := wps[0]
	distanceToWaypoint := pt.GreatCircleDistance(geo.NewPoint(waypoint.Lat, waypoint.Long)) * 1000

	var followingID primitive.ObjectID
	distanceToFollowingWaypoint := -1.0
	if len(wps) >= 2 {
		following := wps[1]
		followingID = following.ID
		distanceToFollowingWaypoint = pt.GreatCircleDistance(geo.NewPoint(following.Lat, following.Long)) * 1000
	}

	// Reset rolling approach memory whenever the current waypoint or the
	// one following it changes (first run, previous waypoint visited, list
	// edited, etc.).
	if s.arrivalState.waypointID != waypoint.ID || s.arrivalState.followingWaypointID != followingID {
		s.arrivalState = arrivalState{
			waypointID:                     waypoint.ID,
			followingWaypointID:            followingID,
			minDistanceToWaypoint:          distanceToWaypoint,
			minDistanceToFollowingWaypoint: distanceToFollowingWaypoint,
			maxDistanceToFollowingWaypoint: distanceToFollowingWaypoint,
		}
	}

	// Capture pre-update minima so the bypass rule can detect a new minimum
	// for the following waypoint on this tick.
	prevMinDistanceToFollowingWaypoint := s.arrivalState.minDistanceToFollowingWaypoint

	if distanceToWaypoint < s.arrivalState.minDistanceToWaypoint {
		s.arrivalState.minDistanceToWaypoint = distanceToWaypoint
	}
	if distanceToFollowingWaypoint >= 0 {
		if distanceToFollowingWaypoint < s.arrivalState.minDistanceToFollowingWaypoint {
			s.arrivalState.minDistanceToFollowingWaypoint = distanceToFollowingWaypoint
		}
		if distanceToFollowingWaypoint > s.arrivalState.maxDistanceToFollowingWaypoint {
			s.arrivalState.maxDistanceToFollowingWaypoint = distanceToFollowingWaypoint
		}
	}

	arrived := false
	reason := ""
	switch {
	case distanceToWaypoint < arrivalDistanceMeters:
		arrived = true
		reason = "inside arrival distance"
	case distanceToFollowingWaypoint >= 0:
		// Bypass rule: we got close to the current waypoint, have started
		// moving away from our closest approach to it, and just hit a new
		// minimum on the following waypoint — we rounded the corner.
		switch {
		case distanceToWaypoint < nearWaypointMeters &&
			distanceToWaypoint > s.arrivalState.minDistanceToWaypoint &&
			distanceToFollowingWaypoint < prevMinDistanceToFollowingWaypoint:
			arrived = true
			reason = "passed (moved away from current + new min to following)"
		case distanceToWaypoint > s.arrivalState.minDistanceToWaypoint*lastWaypointPassFactor &&
			distanceToFollowingWaypoint+followingWaypointApproachMeters < s.arrivalState.maxDistanceToFollowingWaypoint:
			// Far-overshoot variant: we never came inside nearWaypointMeters
			// but we're now well past the current waypoint (≥2.5× our
			// closest approach) AND we've closed at least 500 m on the
			// following waypoint vs the furthest we'd been from it. Catches
			// passes that round the corner outside the proximity gate.
			arrived = true
			reason = "passed (far overshoot + closed on following)"
		}
	default:
		// Last waypoint: no following to compare to. Fall back to
		// overshoot detection: we got within a reasonable approach
		// distance and have since moved well past it.
		min := s.arrivalState.minDistanceToWaypoint
		if min <= lastWaypointPassMinDistanceMeters &&
			distanceToWaypoint >= min*lastWaypointPassFactor &&
			distanceToWaypoint > arrivalDistanceMeters {
			arrived = true
			reason = "overshot last waypoint"
		}
	}

	if !arrived {
		// Periodic diagnostic so a missed pass leaves a trail. Only log
		// when the boat is within arrivalLogProximityMeters and at most
		// once per arrivalLogInterval; otherwise this would flood logs
		// during long offshore legs.
		now := time.Now()
		if distanceToWaypoint < arrivalLogProximityMeters && now.Sub(s.lastArrivalLog) >= arrivalLogInterval {
			s.lastArrivalLog = now
			s.logger.Infow("arrival poller: not arrived",
				"waypoint_id", waypoint.ID.Hex(),
				"distance_to_waypoint_m", distanceToWaypoint,
				"min_distance_to_waypoint_m", s.arrivalState.minDistanceToWaypoint,
				"distance_to_following_waypoint_m", distanceToFollowingWaypoint,
				"min_distance_to_following_waypoint_m", s.arrivalState.minDistanceToFollowingWaypoint,
				"max_distance_to_following_waypoint_m", s.arrivalState.maxDistanceToFollowingWaypoint,
				"wps_remaining", len(wps),
			)
		}
		return
	}

	if err := s.store.WaypointVisited(ctx, waypoint.ID); err != nil {
		s.logger.Warnw("arrival poller: WaypointVisited failed", "err", err, "id", waypoint.ID.Hex())
		return
	}
	s.logger.Infof("arrived at waypoint %s (%.0f m, %s); marking visited",
		waypoint.ID.Hex(), distanceToWaypoint, reason)
}

func (s *navService) Name() resource.Name { return s.name }

func (s *navService) Mode(ctx context.Context, extra map[string]interface{}) (navigation.Mode, error) {
	return navigation.Mode(s.mode.Load()), nil
}

func (s *navService) SetMode(ctx context.Context, mode navigation.Mode, extra map[string]interface{}) error {
	switch mode {
	case navigation.ModeManual, navigation.ModeWaypoint, navigation.ModeExplore:
	default:
		return errors.Errorf("unknown navigation mode %v", mode)
	}
	s.mode.Store(uint32(mode))
	return nil
}

func (s *navService) Location(ctx context.Context, extra map[string]interface{}) (*spatialmath.GeoPose, error) {
	if s.ms == nil {
		return spatialmath.NewGeoPose(geo.NewPoint(0, 0), 0), nil
	}
	pt, _, err := s.ms.Position(ctx, extra)
	if err != nil {
		return nil, err
	}
	heading, err := s.ms.CompassHeading(ctx, extra)
	if err != nil {
		// Heading may not be supported on every movement_sensor; fall back to 0.
		heading = 0
	}
	return spatialmath.NewGeoPose(pt, heading), nil
}

func (s *navService) Waypoints(ctx context.Context, extra map[string]interface{}) ([]navigation.Waypoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Waypoints(ctx)
}

func (s *navService) AddWaypoint(ctx context.Context, point *geo.Point, extra map[string]interface{}) error {
	if point == nil {
		return errors.New("waypoint location is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.store.AddWaypoint(ctx, point)
	return err
}

func (s *navService) RemoveWaypoint(ctx context.Context, id primitive.ObjectID, extra map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.RemoveWaypoint(ctx, id)
}

func (s *navService) Obstacles(ctx context.Context, extra map[string]interface{}) ([]*spatialmath.GeoGeometry, error) {
	return nil, nil
}

func (s *navService) Paths(ctx context.Context, extra map[string]interface{}) ([]*navigation.Path, error) {
	return nil, nil
}

func (s *navService) Properties(ctx context.Context) (navigation.Properties, error) {
	return navigation.Properties{MapType: navigation.GPSMap}, nil
}

// DoCommand exposes service-specific commands that don't have a first-class
// gRPC method on the navigation API:
//
//	{"move_waypoint":   {"id": "<hex>", "lat": <float>, "lng": <float>}}
//	{"insert_waypoint": {"before_id": "<hex>", "lat": <float>, "lng": <float>}}
//	{"arrival_status":  true}
//
// move_waypoint updates an existing waypoint in place (preserving its ID and
// order). insert_waypoint inserts a new waypoint immediately before the
// waypoint with the given before_id; an empty/missing before_id is equivalent
// to AddWayPoint (append). arrival_status returns a snapshot of the
// auto-arrival poller so an operator can see, on demand, why a pass didn't
// fire (current target, boat position, distances, minDistance, etc.).
func (s *navService) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if raw, ok := cmd["move_waypoint"]; ok {
		return s.doMoveWaypoint(ctx, raw)
	}
	if raw, ok := cmd["insert_waypoint"]; ok {
		return s.doInsertWaypoint(ctx, raw)
	}
	if _, ok := cmd["arrival_status"]; ok {
		return s.doArrivalStatus(ctx)
	}
	return nil, resource.ErrDoUnimplemented
}

// doArrivalStatus returns a snapshot of the arrival poller's current
// view: boat position, the current waypoint and its distance, the
// rolling minimum, the following-waypoint distance and minimum (if any),
// and whether auto-arrival is enabled. Cheap — no side effects on poller
// state.
func (s *navService) doArrivalStatus(ctx context.Context) (map[string]interface{}, error) {
	out := map[string]interface{}{
		"auto_arrival_enabled": s.ms != nil && s.arrivalCancel != nil,
		"arrival_distance_m":   arrivalDistanceMeters,
		"near_waypoint_m":      nearWaypointMeters,
		"check_interval":       arrivalCheckInterval.String(),
	}
	if s.ms == nil {
		out["note"] = "no movement_sensor configured"
		return out, nil
	}
	pt, _, err := s.ms.Position(ctx, nil)
	if err != nil {
		out["position_error"] = err.Error()
		return out, nil
	}
	if pt == nil {
		out["note"] = "movement_sensor returned nil position"
		return out, nil
	}
	out["lat"] = pt.Lat()
	out["lng"] = pt.Lng()

	wps, err := s.store.Waypoints(ctx)
	if err != nil {
		out["waypoints_error"] = err.Error()
		return out, nil
	}
	out["wps_remaining"] = len(wps)
	if len(wps) == 0 {
		out["note"] = "no unvisited waypoints"
		return out, nil
	}

	waypoint := wps[0]
	distanceToWaypoint := pt.GreatCircleDistance(geo.NewPoint(waypoint.Lat, waypoint.Long)) * 1000
	out["waypoint_id"] = waypoint.ID.Hex()
	out["waypoint_lat"] = waypoint.Lat
	out["waypoint_lng"] = waypoint.Long
	out["distance_to_waypoint_m"] = distanceToWaypoint

	if s.arrivalState.waypointID == waypoint.ID {
		out["min_distance_to_waypoint_m"] = s.arrivalState.minDistanceToWaypoint
		out["away_from_min_m"] = distanceToWaypoint - s.arrivalState.minDistanceToWaypoint
	}

	if len(wps) >= 2 {
		following := wps[1]
		distanceToFollowingWaypoint := pt.GreatCircleDistance(geo.NewPoint(following.Lat, following.Long)) * 1000
		out["following_waypoint_id"] = following.ID.Hex()
		out["distance_to_following_waypoint_m"] = distanceToFollowingWaypoint
		if s.arrivalState.followingWaypointID == following.ID {
			out["min_distance_to_following_waypoint_m"] = s.arrivalState.minDistanceToFollowingWaypoint
			out["max_distance_to_following_waypoint_m"] = s.arrivalState.maxDistanceToFollowingWaypoint
		}
	}
	return out, nil
}

func (s *navService) doMoveWaypoint(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("move_waypoint payload must be an object")
	}
	idStr, _ := m["id"].(string)
	if idStr == "" {
		return nil, errors.New("move_waypoint.id is required")
	}
	id, err := primitive.ObjectIDFromHex(idStr)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid move_waypoint.id %q", idStr)
	}
	lat, latOK := m["lat"].(float64)
	lng, lngOK := m["lng"].(float64)
	if !latOK || !lngOK {
		return nil, errors.New("move_waypoint.lat and move_waypoint.lng are required numbers")
	}
	mover, ok := s.store.(interface {
		MoveWaypoint(ctx context.Context, id primitive.ObjectID, point *geo.Point) error
	})
	if !ok {
		return nil, errors.New("waypoint store does not support move")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := mover.MoveWaypoint(ctx, id, geo.NewPoint(lat, lng)); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true}, nil
}

func (s *navService) doInsertWaypoint(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("insert_waypoint payload must be an object")
	}
	lat, latOK := m["lat"].(float64)
	lng, lngOK := m["lng"].(float64)
	if !latOK || !lngOK {
		return nil, errors.New("insert_waypoint.lat and insert_waypoint.lng are required numbers")
	}
	var beforeID primitive.ObjectID
	if idStr, _ := m["before_id"].(string); idStr != "" {
		parsed, err := primitive.ObjectIDFromHex(idStr)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid insert_waypoint.before_id %q", idStr)
		}
		beforeID = parsed
	}
	inserter, ok := s.store.(interface {
		InsertWaypoint(ctx context.Context, point *geo.Point, beforeID primitive.ObjectID) (navigation.Waypoint, error)
	})
	if !ok {
		return nil, errors.New("waypoint store does not support insert")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wp, err := inserter.InsertWaypoint(ctx, geo.NewPoint(lat, lng), beforeID)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "id": wp.ID.Hex()}, nil
}

func (s *navService) Close(ctx context.Context) error {
	if s.arrivalCancel != nil {
		s.arrivalCancel()
		// Wait for the poller to exit so we don't race the store Close().
		select {
		case <-s.arrivalDone:
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Close(ctx)
}
