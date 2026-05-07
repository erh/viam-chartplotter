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
//
// ArrivalRadiusMeters controls auto-arrival: when MovementSensor is set, a
// background poller marks the next waypoint visited as soon as the boat is
// within this many meters of it. 0 disables auto-arrival; default is 200 m.
type NavConfig struct {
	MovementSensor      string  `json:"movement_sensor,omitempty"`
	DataPath            string  `json:"data_path,omitempty"`
	ArrivalRadiusMeters float64 `json:"arrival_radius_m,omitempty"`
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

// defaultArrivalRadiusMeters is the auto-arrival threshold used when the
// config doesn't specify one. ~0.1 nm.
const defaultArrivalRadiusMeters = 200.0

// arrivalCheckInterval is how often the background poller re-checks distance
// to the next waypoint. Cheap to compute, and 5 s is plenty for typical
// boat speeds (~30 m at 12 kn, well under the default radius).
const arrivalCheckInterval = 5 * time.Second

// passEpsilonMeters is how much the boat's distance from the target must
// grow past its recorded minimum before we treat it as "moving away."
// Sized to comfortably exceed typical GPS jitter at the 5 s tick rate.
const passEpsilonMeters = 30.0

// lastWaypointPassMinDistanceMeters and lastWaypointPassFactor configure
// the fallback arrival rule for the *final* waypoint, where there's no
// after-next to compare against. We mark the last waypoint visited if the
// boat got within lastWaypointPassMinDistanceMeters of it AND has since
// moved at least lastWaypointPassFactor× that minimum away — i.e. an
// overshoot we can be confident about even without a direction reference.
const lastWaypointPassMinDistanceMeters = 400.0
const lastWaypointPassFactor = 2.5

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

	arrivalRadius := cfg.ArrivalRadiusMeters
	if arrivalRadius == 0 {
		arrivalRadius = defaultArrivalRadiusMeters
	}

	svc := &navService{
		name:                conf.ResourceName(),
		logger:              logger,
		store:               store,
		arrivalRadiusMeters: arrivalRadius,
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
		if arrivalRadius <= 0 {
			logger.Warnf("nav auto-arrival disabled: `arrival_radius_m` is %.1f (must be > 0)", arrivalRadius)
		} else {
			svc.startArrivalPoller(arrivalRadius)
			logger.Infof("nav auto-arrival enabled: %.0f m radius, polling every %s",
				arrivalRadius, arrivalCheckInterval)
		}
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

	arrivalRadiusMeters float64

	// Cancellation for the background arrival poller, if started.
	arrivalCancel context.CancelFunc
	arrivalDone   chan struct{}

	// Rolling approach memory for the current next-waypoint. Read/written
	// only from the arrival poller goroutine, so no synchronisation needed.
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

// arrivalState tracks the closest the boat has been to targetID on the
// current approach. Used for the bypass rule: if we hit a local minimum
// in distance and started getting further away while simultaneously
// getting closer to the *next* waypoint, we've passed targetID even if
// we never crossed the absolute arrival radius.
type arrivalState struct {
	targetID    primitive.ObjectID
	minDistance float64 // km
}

// startArrivalPoller launches a background goroutine that periodically
// checks the boat's distance to the next unvisited waypoint and marks the
// waypoint visited (so it disappears from Waypoints()) once the boat is
// within radiusMeters.
func (s *navService) startArrivalPoller(radiusMeters float64) {
	ctx, cancel := context.WithCancel(context.Background())
	s.arrivalCancel = cancel
	s.arrivalDone = make(chan struct{})
	radiusKm := radiusMeters / 1000.0
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
		s.checkArrival(ctx, radiusKm)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkArrival(ctx, radiusKm)
			case <-heartbeat.C:
				s.logger.Infow("arrival poller: alive",
					"interval", arrivalCheckInterval.String(),
					"radius_m", radiusMeters,
				)
			}
		}
	}()
}

// checkArrival is one tick of the arrival poller: pull the boat's position,
// look up the next unvisited waypoint, and mark it visited if either:
//   - we're inside the arrival radius, or
//   - we hit a local minimum in distance to the next waypoint AND we're now
//     closer to the waypoint after it than to the next one — i.e. we
//     rounded a corner outside the radius but clearly passed the point.
//
// Errors are surfaced at warn so an unhealthy sensor or store is visible
// in logs; the loop still keeps running so a transient blip doesn't
// disable auto-arrival.
func (s *navService) checkArrival(ctx context.Context, radiusKm float64) {
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
	next := wps[0]
	distNext := pt.GreatCircleDistance(geo.NewPoint(next.Lat, next.Long))

	// Reset rolling approach memory whenever the active target changes
	// (first run, previous waypoint visited, list edited, etc.).
	if s.arrivalState.targetID != next.ID {
		s.arrivalState = arrivalState{targetID: next.ID, minDistance: distNext}
	} else if distNext < s.arrivalState.minDistance {
		s.arrivalState.minDistance = distNext
	}

	arrived := false
	reason := ""
	distAfterKm := -1.0 // -1 sentinel for "no after-next" in the diagnostic log
	switch {
	case distNext <= radiusKm:
		arrived = true
		reason = "inside arrival radius"
	case len(wps) >= 2:
		// Bypass rule: we've moved away from our closest approach to the
		// next waypoint AND we're now geometrically closer to the after-
		// next waypoint than to the next. We deliberately do NOT require
		// minDistance to have been particularly small — the user might
		// have rounded the corner well outside any reasonable "got close"
		// radius, and as long as we're now past it heading toward the
		// next one, that's the right call.
		afterNext := wps[1]
		distAfterKm = pt.GreatCircleDistance(geo.NewPoint(afterNext.Lat, afterNext.Long))
		passEpsKm := passEpsilonMeters / 1000.0
		if distNext > s.arrivalState.minDistance+passEpsKm &&
			distAfterKm < distNext {
			arrived = true
			reason = "passed (moved away + closer to next)"
		}
	default:
		// Single (last) waypoint: no after-next to compare to, so the
		// bypass rule above can't fire. Fall back to overshoot detection:
		// we got within a reasonable approach distance and have since
		// moved well past it. This handles "I drove by my final
		// destination" without requiring the boat to land in the radius.
		minKm := s.arrivalState.minDistance
		if minKm <= lastWaypointPassMinDistanceMeters/1000.0 &&
			distNext >= minKm*lastWaypointPassFactor &&
			distNext > radiusKm {
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
		if distNext*1000 < arrivalLogProximityMeters && now.Sub(s.lastArrivalLog) >= arrivalLogInterval {
			s.lastArrivalLog = now
			s.logger.Infow("arrival poller: not arrived",
				"target_id", next.ID.Hex(),
				"dist_m", distNext*1000,
				"min_dist_m", s.arrivalState.minDistance*1000,
				"radius_m", radiusKm*1000,
				"dist_after_m", distAfterKm*1000,
				"wps_remaining", len(wps),
			)
		}
		return
	}

	if err := s.store.WaypointVisited(ctx, next.ID); err != nil {
		s.logger.Warnw("arrival poller: WaypointVisited failed", "err", err, "id", next.ID.Hex())
		return
	}
	s.logger.Infof("arrived at waypoint %s (%.0f m, %s); marking visited",
		next.ID.Hex(), distNext*1000, reason)
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

// doArrivalStatus returns a snapshot of the arrival poller's current view:
// boat position, the next waypoint and its distance, the rolling
// minDistance, the after-next distance (if any), and whether auto-arrival
// is enabled. Cheap — no side effects on poller state.
func (s *navService) doArrivalStatus(ctx context.Context) (map[string]interface{}, error) {
	out := map[string]interface{}{
		"auto_arrival_enabled": s.ms != nil && s.arrivalCancel != nil,
		"radius_m":             s.arrivalRadiusMeters,
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

	next := wps[0]
	distNext := pt.GreatCircleDistance(geo.NewPoint(next.Lat, next.Long))
	out["next_id"] = next.ID.Hex()
	out["next_lat"] = next.Lat
	out["next_lng"] = next.Long
	out["next_dist_m"] = distNext * 1000

	// If state is for this target, expose it; otherwise just current dist.
	if s.arrivalState.targetID == next.ID {
		out["min_dist_m"] = s.arrivalState.minDistance * 1000
		out["away_from_min_m"] = (distNext - s.arrivalState.minDistance) * 1000
	}

	if len(wps) >= 2 {
		afterNext := wps[1]
		distAfter := pt.GreatCircleDistance(geo.NewPoint(afterNext.Lat, afterNext.Long))
		out["after_next_id"] = afterNext.ID.Hex()
		out["after_next_dist_m"] = distAfter * 1000
		out["closer_to_after_next"] = distAfter < distNext
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
