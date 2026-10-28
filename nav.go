package vc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/errors"

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
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
// MovementSensor is optional; when set, the service reports the live
// position/heading of that movement sensor and auto-advances route goals as
// the boat reaches each waypoint.
//
// DataPath, when set, is used as the JSON file that persists saved locations
// and the latest goal across restarts. When empty, state persists to
// "<user-cache-dir>/viam-chartplotter/nav/<resource_name>.json".
//
// N2KSender is optional; when set it names a generic component (e.g. a
// viam-labs:viamboat:sender) whose DoCommand sends raw NMEA 2000 PGNs, and it
// enables the send_waypoints_n2k DoCommand (see nav_n2k.go).
type NavConfig struct {
	MovementSensor string `json:"movement_sensor,omitempty"`
	DataPath       string `json:"data_path,omitempty"`
	N2KSender      string `json:"n2k_sender,omitempty"`
}

// Validate ensures all parts of the config are valid and returns the
// implicit dependencies required by the service.
func (cfg *NavConfig) Validate(path string) ([]string, []string, error) {
	var deps []string
	if cfg.MovementSensor != "" {
		deps = append(deps, cfg.MovementSensor)
	}
	if cfg.N2KSender != "" {
		deps = append(deps, cfg.N2KSender)
	}
	return deps, nil, nil
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

// followingClosingMarginMeters is how much the distance to the following
// waypoint must drop below its previous minimum for the near-bypass rule
// to count as "closing on the following waypoint". Without a margin, any
// sub-meter improvement (typical GPS jitter) trips the rule, which we've
// seen produce false-positive arrivals when the boat got near the active
// waypoint but turned away without rounding the corner.
const followingClosingMarginMeters = 50.0

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
	logger.Infof("nav state persisted at %s", dataPath)

	if cfg.MovementSensor == "" {
		logger.Warnf("nav auto-arrival disabled: no `movement_sensor` configured; route goals only change via explicit API calls")
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

	if cfg.N2KSender != "" {
		sender, err := generic.FromDependencies(deps, cfg.N2KSender)
		if err != nil {
			return nil, errors.Wrapf(err, "could not get n2k_sender %q", cfg.N2KSender)
		}
		svc.n2kSender = sender
		logger.Infof("nav n2k waypoint export enabled via sender %q", cfg.N2KSender)
	}

	return svc, nil
}

// navService is a GPS-map navigation service for a boat. It doesn't drive
// anything: a goal (a route, or a single point/location) is a list of
// waypoints the skipper is steering for, and the arrival poller advances the
// goal's cursor as the boat reaches each one. Saved locations and the latest
// goal persist in diskNavStore; saved routes live in the Viam location
// metadata (nav_routes.go). See nav_service.go for the navigation API.
type navService struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	store *diskNavStore
	ms    movementsensor.MovementSensor

	// Optional generic component that sends raw NMEA 2000 PGNs via its
	// DoCommand (config `n2k_sender`); nil disables send_waypoints_n2k.
	n2kSender resource.Resource

	// Cancellation for the background arrival poller, if started.
	arrivalCancel context.CancelFunc
	arrivalDone   chan struct{}

	// Rolling approach memory for the current waypoint and the one
	// following it. Read/written only from the arrival poller goroutine
	// (arrival_status reads it racily, for diagnostics only).
	arrivalState   ArrivalState
	lastArrivalLog time.Time
	// Rolling confirmation memory for mid-route rejoin detection
	// (rejoin.go). Same single-goroutine discipline as arrivalState.
	rejoinState RejoinState

	// Lazily-created Viam app client used to read/write saved routes in the
	// location metadata (see nav_routes.go). It authenticates with the
	// machine's own VIAM_API_KEY/VIAM_API_KEY_ID env vars and uses
	// VIAM_LOCATION_ID, so route storage works regardless of how the client
	// connected to the machine. routesMu serializes both the lazy init and the
	// metadata read-modify-write.
	routesMu   sync.Mutex
	viamClient *app.ViamClient
	appClient  *app.AppClient
	locationID string

	// Test seams: when set, used instead of dialing the real app client, so the
	// location/inheritance logic can be exercised against fake stores.
	// routesStoreFn is this machine's location; parentStoresFn the ancestors.
	routesStoreFn  func(ctx context.Context) (metadataStore, error)
	parentStoresFn func(ctx context.Context) ([]metadataStore, error)

	// Test seam: where set_display_resources persists picks; empty = the
	// real cache-dir location (displayPicksPath).
	picksPath string
}

// waypointKey identifies a waypoint to the arrival and rejoin memories. It is
// the coordinates rather than the index so the memory survives a client
// re-sending the route with an edit elsewhere in it (every edit replaces the
// whole goal), while a moved waypoint correctly starts fresh.
func waypointKey(p navPoint) string {
	return fmt.Sprintf("%.7f,%.7f", p.Lat, p.Lng)
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

// startArrivalPoller launches a background goroutine that periodically
// checks the boat's distance to the current goal waypoint and advances the
// goal once arrival is detected.
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
					"rejoin_leg", s.rejoinState.LegStartID,
					"rejoin_ticks", s.rejoinState.Ticks,
				)
			}
		}
	}()
}

// advanceGoal marks n waypoints reached on the goal that started at
// startedAt with its cursor at fromIndex. It is a no-op when the goal was
// replaced, suspended or moved on since the poller read it, so a client's
// edit landing mid-tick is never clobbered by stale progress.
func (s *navService) advanceGoal(startedAt time.Time, fromIndex, n int) (bool, error) {
	applied := false
	err := s.store.mutate(false, func(st *navState) error {
		g := st.Goal
		if g == nil || g.State != goalActive || !g.StartedAt.Equal(startedAt) || g.Index != fromIndex {
			return nil
		}
		g.advance(n, time.Now())
		applied = true
		return nil
	})
	return applied, err
}

// checkArrival is one tick of the arrival poller: pull the boat's
// position, look up the goal's current waypoint (and the one following it,
// if any), and mark the current waypoint reached when one of:
//   - distanceToWaypoint < arrivalDistanceMeters (we're there), or
//   - distanceToWaypoint < nearWaypointMeters AND we've moved past our
//     closest approach to the current waypoint AND we just hit a new
//     minimum approach to the following waypoint — i.e. we rounded the
//     corner without entering the arrival radius, or
//   - last-waypoint overshoot: with no following waypoint to compare to,
//     we got within lastWaypointPassMinDistanceMeters and have since
//     moved at least lastWaypointPassFactor× that minimum away.
//
// Only an active goal advances; a suspended one is left alone. Errors are
// surfaced at warn so an unhealthy sensor or store is visible in logs; the
// loop still keeps running so a transient blip doesn't disable auto-arrival.
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
	g := s.store.Goal()
	if g == nil || g.State != goalActive {
		state := "none"
		if g != nil {
			state = g.State
		}
		s.maybeArrivalInfo("arrival poller: no active goal",
			"state", state, "lat", pt.Lat(), "lng", pt.Lng())
		return
	}
	wps := g.remaining()
	if len(wps) == 0 {
		return
	}
	waypoint := wps[0]

	// Mid-route rejoin (rejoin.go): the boat is demonstrably between two
	// LATER waypoints and advancing along that leg, so everything behind
	// it is reached — the per-waypoint arrival rules below only ever watch
	// wps[0] and would steer for a mark behind the beam forever.
	rwps := make([]RejoinWaypoint, len(wps))
	for i, w := range wps {
		rwps[i] = RejoinWaypoint{ID: waypointKey(w), Lat: w.Lat, Lng: w.Lng}
	}
	rejoin := decideRejoin(pt.Lat(), pt.Lng(), rwps, s.rejoinState)
	// One line when a candidate leg STARTS being tracked, so "rejoin didn't
	// fire" is diagnosable from logs instead of silent (the Long Island
	// Sound field debug had nothing to go on).
	if rejoin.NewState.Ticks == 1 && rejoin.NewState.LegStartID != s.rejoinState.LegStartID {
		s.logger.Infow("route rejoin: tracking candidate leg",
			"leg_start_id", rejoin.NewState.LegStartID,
			"along_m", rejoin.NewState.FirstAlongM)
	}
	s.rejoinState = rejoin.NewState
	if rejoin.SkipCount > 0 {
		s.logger.Infof("route rejoin: marking %d leading waypoints reached (%s)",
			rejoin.SkipCount, rejoin.Reason)
		if _, err := s.advanceGoal(g.StartedAt, g.Index, rejoin.SkipCount); err != nil {
			s.logger.Warnw("route rejoin: advancing goal failed", "err", err)
		}
		// The active pair just changed wholesale — stale approach memory
		// must not colour the next arrival decision.
		s.arrivalState = ArrivalState{}
		return
	}

	// Build the per-tick input for decideArrival. The decision logic is
	// pure (see arrival.go); call sites do the I/O and logging.
	in := ArrivalInput{
		BoatLat:     pt.Lat(),
		BoatLng:     pt.Lng(),
		WaypointID:  waypointKey(waypoint),
		WaypointLat: waypoint.Lat,
		WaypointLng: waypoint.Lng,
	}
	if len(wps) >= 2 {
		following := wps[1]
		in.HasFollowing = true
		in.FollowingWaypointID = waypointKey(following)
		in.FollowingWaypointLat = following.Lat
		in.FollowingWaypointLng = following.Lng
	}

	prevState := s.arrivalState
	decision := decideArrival(in, prevState)
	s.arrivalState = decision.NewState

	// Emit a single JSON line so a user can copy it into a fixture
	// under testdata/arrival/<name>.json (add `expected_arrived: true|
	// false`) and lock the behaviour in. Throttled when within
	// proximity but not arriving so long offshore legs don't flood the
	// log; always logged on actual arrival.
	logTick := decision.Arrived
	if !logTick && decision.DistanceToWaypoint < arrivalLogProximityMeters {
		now := time.Now()
		if now.Sub(s.lastArrivalLog) >= arrivalLogInterval {
			s.lastArrivalLog = now
			logTick = true
		}
	}
	if logTick {
		payload := ArrivalLogPayload{
			Input:     in,
			PrevState: prevState,
			Decision:  decision,
		}
		if data, jerr := json.Marshal(payload); jerr == nil {
			s.logger.Infof("arrival decision: %s", string(data))
		}
	}

	if !decision.Arrived {
		return
	}
	applied, err := s.advanceGoal(g.StartedAt, g.Index, 1)
	if err != nil {
		s.logger.Warnw("arrival poller: advancing goal failed", "err", err, "index", g.Index)
		return
	}
	if applied {
		s.logger.Infof("arrived at waypoint %d of %d (%.0f m, %s)",
			g.Index+1, len(g.Waypoints), decision.DistanceToWaypoint, decision.Reason)
	}
}

func (s *navService) Name() resource.Name { return s.name }

// Status satisfies the resource.Resource interface.
func (s *navService) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// DoCommand exposes service-specific commands that don't have a first-class
// gRPC method on the navigation API:
//
//	{"routes_meta":     true}
//	{"arrival_status":  true}
//	{"send_waypoints_n2k": {"route_name"?: ..., "database_id"?: ..., "route_id"?: ..., "dst"?: ...}}
//	{"set_display_resources": {"movement_sensor"?: ..., "depth_sensor"?: ..., "route_sensor"?: ..., "cameras"?: [...]}}
//
// routes_meta returns the saved-route fields the API's Route message can't
// carry (id, notes, color, source, dates, stats, scope), in ListRoutes order
// (see nav_routes.go). Routes themselves are read and written through the
// Route RPCs, with notes/color/source in extra.
//
// send_waypoints_n2k pushes the active goal's remaining waypoints onto the
// NMEA 2000 bus as a Route and WP Service transfer (PGNs 130066 + 130067) via
// the configured n2k_sender component (see nav_n2k.go). All payload fields
// are optional; pass true for the defaults.
//
// set_display_resources records which machine resources the web app picked
// during its own discovery (see displaypicks.go); the LAN display API falls
// back to them for anything the chartplotter config doesn't name.
//
// arrival_status returns a snapshot of the auto-arrival poller so an operator
// can see, on demand, why a pass didn't fire (current target, boat position,
// distances, minDistance, etc.).
func (s *navService) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["routes_meta"]; ok {
		return s.doRoutesMeta(ctx)
	}
	if _, ok := cmd["arrival_status"]; ok {
		return s.doArrivalStatus(ctx)
	}
	if raw, ok := cmd["send_waypoints_n2k"]; ok {
		return s.doSendWaypointsN2K(ctx, raw)
	}
	if raw, ok := cmd["set_display_resources"]; ok {
		return s.doSetDisplayResources(raw)
	}
	return nil, resource.ErrDoUnimplemented
}

// doSetDisplayResources stores the web app's resource picks in the shared
// registry (displaypicks.go) for the display API to fall back on.
func (s *navService) doSetDisplayResources(raw interface{}) (map[string]interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("set_display_resources payload must be an object")
	}
	str := func(key string) string { v, _ := m[key].(string); return v }
	p := DisplayPicks{
		MovementSensor: str("movement_sensor"),
		DepthSensor:    str("depth_sensor"),
		RouteSensor:    str("route_sensor"),
	}
	if rawCams, ok := m["cameras"].([]interface{}); ok {
		for i, c := range rawCams {
			name, ok := c.(string)
			if !ok {
				return nil, errors.Errorf("set_display_resources.cameras[%d] must be a string", i)
			}
			if name != "" {
				p.Cameras = append(p.Cameras, name)
			}
		}
	}
	path := s.picksPath
	if path == "" {
		path = displayPicksPath()
	}
	setDisplayPicks(p, s, path, s.logger)
	return map[string]interface{}{"ok": true}, nil
}

// doArrivalStatus returns a snapshot of the arrival poller's current
// view: boat position, the goal's current waypoint and its distance, the
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

	g := s.store.Goal()
	if g == nil {
		out["note"] = "no goal"
		return out, nil
	}
	out["goal_state"] = g.State
	out["goal_index"] = g.Index
	wps := g.remaining()
	if !g.open() {
		wps = nil
	}
	out["wps_remaining"] = len(wps)
	if len(wps) == 0 {
		out["note"] = "no remaining waypoints"
		return out, nil
	}

	waypoint := wps[0]
	distanceToWaypoint := haversineMeters(pt.Lat(), pt.Lng(), waypoint.Lat, waypoint.Lng)
	out["waypoint_id"] = waypointKey(waypoint)
	out["waypoint_lat"] = waypoint.Lat
	out["waypoint_lng"] = waypoint.Lng
	out["distance_to_waypoint_m"] = distanceToWaypoint

	if s.arrivalState.WaypointID == waypointKey(waypoint) {
		out["min_distance_to_waypoint_m"] = s.arrivalState.MinDistanceToWaypoint
		out["away_from_min_m"] = distanceToWaypoint - s.arrivalState.MinDistanceToWaypoint
	}

	if len(wps) >= 2 {
		following := wps[1]
		out["following_waypoint_id"] = waypointKey(following)
		out["distance_to_following_waypoint_m"] = haversineMeters(pt.Lat(), pt.Lng(), following.Lat, following.Lng)
		if s.arrivalState.FollowingWaypointID == waypointKey(following) {
			out["min_distance_to_following_waypoint_m"] = s.arrivalState.MinDistanceToFollowingWaypoint
			out["max_distance_to_following_waypoint_m"] = s.arrivalState.MaxDistanceToFollowingWaypoint
		}
	}
	return out, nil
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
	s.routesMu.Lock()
	if s.viamClient != nil {
		if err := s.viamClient.Close(); err != nil {
			s.logger.Debugw("error closing viam app client", "error", err)
		}
		s.viamClient = nil
		s.appClient = nil
	}
	s.routesMu.Unlock()
	return s.store.Close()
}
