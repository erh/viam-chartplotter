package vc

// The display API is a small, unauthenticated JSON/JPEG HTTP API served
// alongside the chart tiles for thin display clients on the boat LAN
// (see TVOS_PLAN.md). It reads the machine's own resources — movement
// sensor, depth/route sensors, nav service, cameras — via module
// dependencies, so a client needs nothing but the server address:
//
//	GET /api/info               what's configured (capability probe)
//	GET /api/state              position / heading / SOG / depth
//	GET /api/route              nav-system route + nav-service waypoints
//	GET /api/track              recent own-boat track (in-memory, 24h)
//	GET /api/camera/{name}.jpg  latest frame from the named camera
//
// Every endpoint is a cheap read; clients poll (state ~1s, route ~5s,
// cameras ~2s, track ~30s). All responses are uncacheable.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erh/vmodutils"

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/rimage"
	"go.viam.com/rdk/robot"
	"go.viam.com/rdk/services/navigation"
	rutils "go.viam.com/rdk/utils"
)

const metersPerSecToKnots = 1.94384
const metersToFeet = 3.28084

// Track recorder: sample the movement sensor on this cadence and keep
// this much history in memory. 24h at 10s is 8640 points — trivial to
// hold and cheap to serialize. The track restarts with the module; a
// display client just shows what's been recorded since.
const trackSampleInterval = 10 * time.Second
const trackKeep = 24 * time.Hour

type trackPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
	Ts  int64   `json:"ts"` // millis
}

// DisplayAPI holds the resolved resources behind the /api endpoints.
// Every field is optional; endpoints whose resources aren't configured
// answer 503 with an explanatory error, so a partially-configured
// machine still serves whatever it can.
type DisplayAPI struct {
	logger logging.Logger

	ms      movementsensor.MovementSensor
	depth   sensor.Sensor
	route   sensor.Sensor
	nav     navigation.Service
	cameras map[string]camera.Camera

	cameraNames []string // sorted, for /api/info

	// Movement sensor's configured name; the cloud track seed queries
	// captured data by its component name.
	msName string

	trackMu     sync.Mutex
	track       []trackPoint
	trackCancel context.CancelFunc
	trackDone   chan struct{}

	// Fallback resources resolved from the web app's reported picks
	// (displaypicks.go) for anything the config doesn't name; config
	// always wins. The picked names aren't declared dependencies, so
	// they're resolved by dialing this machine with its own env
	// credentials — done in a background goroutine (never on a poll's
	// request path) and throttled on failure.
	fbMu          sync.Mutex
	fbGen         int // picks generation the fields below were resolved from
	fbWarnedGen   int // picks generation already warned about; repeats log at debug
	fbResolving   bool
	fbNextDial    time.Time
	fbRobot       robot.Robot
	fbMS          movementsensor.MovementSensor
	fbMSName      string
	fbDepth       sensor.Sensor
	fbRoute       sensor.Sensor
	fbNav         navigation.Service
	fbCameras     map[string]camera.Camera
	fbCameraNames []string

	// Test seam: when set, used instead of dialing the machine.
	fbDeps func(ctx context.Context) (resource.Dependencies, error)
}

// NewDisplayAPI resolves the display-API resource names from deps. The
// names come through Validate as optional dependencies, so a name whose
// resource isn't (yet) available just logs a warning and leaves that
// endpoint 503 — the chartplotter rebuilds when the resource appears.
func NewDisplayAPI(deps resource.Dependencies, cfg *ChartplotterConfig, logger logging.Logger) *DisplayAPI {
	a := &DisplayAPI{logger: logger, cameras: map[string]camera.Camera{}}

	if cfg.MovementSensor != "" {
		ms, err := movementsensor.FromProvider(deps, cfg.MovementSensor)
		if err != nil {
			logger.Warnf("display api: movement_sensor %q unavailable: %v", cfg.MovementSensor, err)
		} else {
			a.ms = ms
			a.msName = cfg.MovementSensor
		}
	}
	if cfg.DepthSensor != "" {
		s, err := sensor.FromProvider(deps, cfg.DepthSensor)
		if err != nil {
			logger.Warnf("display api: depth_sensor %q unavailable: %v", cfg.DepthSensor, err)
		} else {
			a.depth = s
		}
	}
	if cfg.RouteSensor != "" {
		s, err := sensor.FromProvider(deps, cfg.RouteSensor)
		if err != nil {
			logger.Warnf("display api: route_sensor %q unavailable: %v", cfg.RouteSensor, err)
		} else {
			a.route = s
		}
	}
	if cfg.NavService != "" {
		n, err := navigation.FromProvider(deps, cfg.NavService)
		if err != nil {
			logger.Warnf("display api: nav_service %q unavailable: %v", cfg.NavService, err)
		} else {
			a.nav = n
		}
	}
	for _, name := range cfg.Cameras {
		c, err := camera.FromProvider(deps, name)
		if err != nil {
			logger.Warnf("display api: camera %q unavailable: %v", name, err)
			continue
		}
		a.cameras[name] = c
		a.cameraNames = append(a.cameraNames, name)
	}
	sort.Strings(a.cameraNames)
	if a.ms != nil {
		a.startTrackRecorder()
	}
	loadDisplayPicks(displayPicksPath(), logger)
	return a
}

// Close stops the background track recorder, if running, and any machine
// client dialed to resolve web-picked fallback resources.
func (a *DisplayAPI) Close() {
	if a.trackCancel != nil {
		a.trackCancel()
		<-a.trackDone
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	if a.fbRobot != nil {
		_ = a.fbRobot.Close(context.Background())
		a.fbRobot = nil
	}
}

// fbDialThrottle spaces out machine-dial attempts when resolving picked
// resources fails (e.g. no cloud credentials in a local run).
const fbDialThrottle = time.Minute

// refreshFallback kicks off re-resolution of the fallback resources when
// the web app's picks have changed. Called at the top of every /api
// handler; a no-op (one mutex hop) when the generation is unchanged.
func (a *DisplayAPI) refreshFallback() {
	picks, nav, gen := getDisplayPicks()
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	if gen == a.fbGen {
		return
	}
	a.fbNav = nav
	needDial := (a.ms == nil && picks.MovementSensor != "") ||
		(a.depth == nil && picks.DepthSensor != "") ||
		(a.route == nil && picks.RouteSensor != "") ||
		(len(a.cameras) == 0 && len(picks.Cameras) > 0)
	if !needDial {
		a.fbGen = gen
		return
	}
	if a.fbResolving || time.Now().Before(a.fbNextDial) {
		return // keep the old resolution; retry after the throttle
	}
	a.fbResolving = true
	go a.resolvePicks(picks, gen)
}

// resolvePicks resolves the picked names to resource handles and installs
// them as the fallbacks. Runs off the request path. The generation only
// advances once every picked resource has resolved; until then a later
// request retries (throttled) — a remote's resources can appear well after
// the first dial, and the first boat deployment hit exactly that.
func (a *DisplayAPI) resolvePicks(picks DisplayPicks, gen int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deps, err := a.pickDeps(ctx)

	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	a.fbResolving = false
	a.fbNextDial = time.Now().Add(fbDialThrottle)
	if err != nil {
		a.logger.Warnf("display api: can't resolve web-picked resources (%v); retrying in %s", err, fbDialThrottle)
		return
	}
	if a.ms == nil && picks.MovementSensor != "" {
		if ms, err := movementsensor.FromProvider(deps, picks.MovementSensor); err != nil {
			a.logger.Debugf("display api: picked movement_sensor %q unavailable: %v", picks.MovementSensor, err)
		} else {
			a.fbMS = ms
			a.fbMSName = picks.MovementSensor
		}
	}
	if a.depth == nil && picks.DepthSensor != "" {
		if s, err := sensor.FromProvider(deps, picks.DepthSensor); err != nil {
			a.logger.Debugf("display api: picked depth_sensor %q unavailable: %v", picks.DepthSensor, err)
		} else {
			a.fbDepth = s
		}
	}
	if a.route == nil && picks.RouteSensor != "" {
		if s, err := sensor.FromProvider(deps, picks.RouteSensor); err != nil {
			a.logger.Debugf("display api: picked route_sensor %q unavailable: %v", picks.RouteSensor, err)
		} else {
			a.fbRoute = s
		}
	}
	if len(a.cameras) == 0 {
		cams := map[string]camera.Camera{}
		names := make([]string, 0, len(picks.Cameras))
		for _, name := range picks.Cameras {
			c, err := camera.FromProvider(deps, name)
			if err != nil {
				a.logger.Debugf("display api: picked camera %q unavailable: %v", name, err)
				continue
			}
			cams[name] = c
			names = append(names, name)
		}
		sort.Strings(names)
		a.fbCameras, a.fbCameraNames = cams, names
	}
	// A movement sensor arriving via picks enables the track recorder,
	// which config-only construction couldn't start. msName feeds the
	// cloud seed query; set before the goroutine starts so it's visible.
	if a.fbMS != nil && a.trackCancel == nil {
		a.msName = a.fbMSName
		a.startTrackRecorder()
	}
	if missing := a.unresolvedPicks(picks); len(missing) > 0 {
		msg := "display api: web-picked resources unresolved: %s — retrying every %s"
		if gen != a.fbWarnedGen {
			a.fbWarnedGen = gen
			a.logger.Warnf(msg, strings.Join(missing, ", "), fbDialThrottle)
		} else {
			a.logger.Debugf(msg, strings.Join(missing, ", "), fbDialThrottle)
		}
		return
	}
	a.fbGen = gen
	a.logger.Infof("display api: using web-picked fallbacks (movement_sensor=%q, depth=%q, route=%q, %d cameras)",
		picks.MovementSensor, picks.DepthSensor, picks.RouteSensor, len(a.fbCameraNames))
}

// unresolvedPicks lists picked-but-unresolved resources. Caller holds fbMu.
func (a *DisplayAPI) unresolvedPicks(picks DisplayPicks) []string {
	var missing []string
	if a.ms == nil && picks.MovementSensor != "" && a.fbMS == nil {
		missing = append(missing, "movement_sensor "+picks.MovementSensor)
	}
	if a.depth == nil && picks.DepthSensor != "" && a.fbDepth == nil {
		missing = append(missing, "depth_sensor "+picks.DepthSensor)
	}
	if a.route == nil && picks.RouteSensor != "" && a.fbRoute == nil {
		missing = append(missing, "route_sensor "+picks.RouteSensor)
	}
	if len(a.cameras) == 0 {
		for _, name := range picks.Cameras {
			if _, ok := a.fbCameras[name]; !ok {
				missing = append(missing, "camera "+name)
			}
		}
	}
	return missing
}

// pickDeps returns the machine's resources as a Dependencies map, so picked
// names resolve through the same FromProvider helpers as configured ones.
// The machine client is dialed once and kept for later refreshes.
func (a *DisplayAPI) pickDeps(ctx context.Context) (resource.Dependencies, error) {
	if a.fbDeps != nil {
		return a.fbDeps(ctx)
	}
	a.fbMu.Lock()
	machine := a.fbRobot
	a.fbMu.Unlock()
	if machine == nil {
		m, err := vmodutils.ConnectToMachineFromEnv(ctx, a.logger)
		if err != nil {
			return nil, err
		}
		a.fbMu.Lock()
		a.fbRobot = m
		a.fbMu.Unlock()
		machine = m
	}
	return vmodutils.MachineToDependencies(machine)
}

// currentMS returns the effective movement sensor: configured, else picked.
func (a *DisplayAPI) currentMS() movementsensor.MovementSensor {
	if a.ms != nil {
		return a.ms
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	return a.fbMS
}

func (a *DisplayAPI) currentDepth() sensor.Sensor {
	if a.depth != nil {
		return a.depth
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	return a.fbDepth
}

func (a *DisplayAPI) currentRoute() sensor.Sensor {
	if a.route != nil {
		return a.route
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	return a.fbRoute
}

func (a *DisplayAPI) currentNav() navigation.Service {
	if a.nav != nil {
		return a.nav
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	return a.fbNav
}

// currentCamera looks the name up in the configured cameras when any are
// configured, else in the picked fallbacks — per-field config-wins, same
// as the other resources.
func (a *DisplayAPI) currentCamera(name string) (camera.Camera, bool) {
	if len(a.cameras) > 0 {
		c, ok := a.cameras[name]
		return c, ok
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	c, ok := a.fbCameras[name]
	return c, ok
}

func (a *DisplayAPI) currentCameraNames() []string {
	if len(a.cameraNames) > 0 {
		return append([]string{}, a.cameraNames...)
	}
	a.fbMu.Lock()
	defer a.fbMu.Unlock()
	return append([]string{}, a.fbCameraNames...)
}

func (a *DisplayAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/info", a.handleInfo)
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/route", a.handleRoute)
	mux.HandleFunc("GET /api/track", a.handleTrack)
	mux.HandleFunc("GET /api/camera/{name}", a.handleCamera)
}

// startTrackRecorder samples the movement sensor every
// trackSampleInterval and appends to the in-memory track, pruning
// entries older than trackKeep. It first seeds the track with the last
// trackKeep of captured position history from the Viam cloud (when the
// machine has cloud credentials), so the line doesn't start empty on
// every module restart.
func (a *DisplayAPI) startTrackRecorder() {
	ctx, cancel := context.WithCancel(context.Background())
	a.trackCancel = cancel
	a.trackDone = make(chan struct{})
	go func() {
		defer close(a.trackDone)
		seedCtx, seedCancel := context.WithTimeout(ctx, time.Minute)
		a.seedTrackFromCloud(seedCtx)
		seedCancel()
		ticker := time.NewTicker(trackSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			ms := a.currentMS()
			if ms == nil {
				continue
			}
			callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
			pos, _, err := ms.Position(callCtx, nil)
			callCancel()
			if err != nil || pos == nil {
				continue
			}
			lat, lng := pos.Lat(), pos.Lng()
			if lat == 0 && lng == 0 { // no fix yet
				continue
			}
			a.recordTrackPoint(trackPoint{Lat: lat, Lng: lng, Ts: time.Now().UnixMilli()})
		}
	}()
}

// trackSeedScope is one fully-resolved candidate to query for captured
// position history: which ids to filter on, which org's data store to
// ask, and which client (credentials) to ask with.
type trackSeedScope struct {
	locationID string
	robotID    string
	orgID      string
	dc         *app.DataClient
	desc       string
}

// seedTrackFromCloud pulls the last trackKeep of captured Position
// readings for the movement sensor from the Viam data API — callable
// from inside the module via the machine's own credentials env vars —
// and prepends them to the in-memory track. Best-effort: any failure
// logs and leaves the recorder running live-only.
//
// The data isn't necessarily captured under this machine's own ids —
// or even in this machine's org: the movement sensor is often a
// resource of a remote machine with its own robot_id, location, and
// (for shared locations) a different primary org whose data store this
// machine's key can't read. Candidates are tried in order: this
// machine, then each cloud-addressed remote in this machine's config —
// resolved to its robot/location/org via the app API, queried with the
// remote's own credentials from the remotes[] auth block when present
// (the same trick the web app's dataClientForComponent uses) — then,
// if exactly one robot in this org has matching data, that robot.
func (a *DisplayAPI) seedTrackFromCloud(ctx context.Context) {
	orgID := os.Getenv(rutils.PrimaryOrgIDEnvVar)
	robotID := os.Getenv(rutils.MachineIDEnvVar)
	locationID := os.Getenv(rutils.LocationIDEnvVar)
	if orgID == "" || robotID == "" {
		a.logger.Infof("track seed skipped: no cloud identity in env (%s/%s)",
			rutils.PrimaryOrgIDEnvVar, rutils.MachineIDEnvVar)
		return
	}
	vc, err := app.CreateViamClientFromEnvVars(ctx, nil, a.logger)
	if err != nil {
		a.logger.Warnf("track seed skipped: viam client: %v", err)
		return
	}
	defer func() { _ = vc.Close() }()
	dc := vc.DataClient()
	start := time.Now().Add(-trackKeep)

	scopes := []trackSeedScope{{locationID, robotID, orgID, dc, "this machine"}}
	remoteScopes, closers := a.remoteSeedScopes(ctx, vc, orgID)
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	scopes = append(scopes, remoteScopes...)
	for _, scope := range scopes {
		if a.trySeedScope(ctx, scope, start) {
			return
		}
	}
	if rid := a.discoverSeedRobot(ctx, dc, orgID, start); rid != "" {
		if a.trySeedScope(ctx, trackSeedScope{"", rid, orgID, dc, "discovered robot " + rid}, start) {
			return
		}
	}
	a.logger.Infof("track seed: no captured positions found for %q in the last %s", a.msName, trackKeep)
}

// trySeedScope queries one scope (hot storage first, standard as the
// fallback) and seeds the track when it yields points.
func (a *DisplayAPI) trySeedScope(ctx context.Context, scope trackSeedScope, start time.Time) bool {
	query := trackSeedQuery(scope.locationID, scope.robotID, a.msName, start)
	rows, err := scope.dc.TabularDataByMQL(ctx, scope.orgID, query,
		&app.TabularDataByMQLOptions{TabularDataSourceType: app.TabularDataSourceTypeHotStorage})
	if err != nil || len(rows) == 0 {
		if err != nil {
			a.logger.Debugf("track seed (%s): hot storage: %v", scope.desc, err)
		}
		rows, err = scope.dc.TabularDataByMQL(ctx, scope.orgID, query, nil)
		if err != nil {
			a.logger.Debugf("track seed (%s): %v", scope.desc, err)
			return false
		}
	}
	points := trackPointsFromRows(rows)
	if len(points) == 0 {
		return false
	}
	a.trackMu.Lock()
	a.track = append(points, a.track...)
	a.trackMu.Unlock()
	a.logger.Infof("track seeded from %s with %d historical points (%s → %s)",
		scope.desc, len(points),
		time.UnixMilli(points[0].Ts).Format(time.RFC3339),
		time.UnixMilli(points[len(points)-1].Ts).Format(time.RFC3339))
	return true
}

// remoteSeedScopes resolves this machine's cloud-addressed remotes to
// query scopes. A remote's address embeds its location id
// ("<part-fqdn>.<location-id>.viam.cloud"); the robot id comes from
// matching the address against part FQDNs in that location; the org is
// the location's primary org; and when the remotes[] entry carries its
// own api key we query with a client built from it — a shared-location
// remote's data lives in an org this machine's key may not read.
// Returned closers shut down any per-remote clients.
func (a *DisplayAPI) remoteSeedScopes(
	ctx context.Context, vc *app.ViamClient, moduleOrgID string,
) ([]trackSeedScope, []func()) {
	partID := os.Getenv(rutils.MachinePartIDEnvVar)
	if partID == "" {
		return nil, nil
	}
	part, _, err := vc.AppClient().GetRobotPart(ctx, partID)
	if err != nil || part == nil {
		a.logger.Debugf("track seed: get own part config: %v", err)
		return nil, nil
	}
	remotes, _ := part.RobotConfig["remotes"].([]any)
	var scopes []trackSeedScope
	var closers []func()
	for _, r := range remotes {
		rm, _ := r.(map[string]any)
		addr, _ := rm["address"].(string)
		name, _ := rm["name"].(string)
		locID := locationIDFromRemoteAddress(addr)
		if locID == "" {
			continue
		}
		client := vc
		if keyID, key := remoteCredentials(rm); key != "" && keyID != "" {
			rvc, err := app.CreateViamClientWithAPIKey(ctx, app.Options{}, key, keyID, a.logger)
			if err != nil {
				a.logger.Debugf("track seed: remote %q client: %v", name, err)
			} else {
				client = rvc
				closers = append(closers, func() { _ = rvc.Close() })
			}
		}
		orgID := moduleOrgID
		if loc, err := client.AppClient().GetLocation(ctx, locID); err == nil {
			if p := locationPrimaryOrgID(loc); p != "" {
				orgID = p
			}
		} else {
			a.logger.Debugf("track seed: get location %s: %v", locID, err)
		}
		robots, err := client.AppClient().ListRobots(ctx, locID)
		if err != nil {
			a.logger.Debugf("track seed: list robots in %s: %v", locID, err)
			continue
		}
		for _, robot := range robots {
			parts, err := client.AppClient().GetRobotParts(ctx, robot.ID)
			if err != nil {
				continue
			}
			for _, p := range parts {
				if p.FQDN == addr {
					scopes = append(scopes, trackSeedScope{locID, robot.ID, orgID, client.DataClient(), "remote " + name})
				}
			}
		}
	}
	return scopes, closers
}

// remoteCredentials extracts the api key id (entity) and key payload
// from a remotes[] config entry. The credentials may be a single
// object or an array — the same shapes the web app accepts.
func remoteCredentials(rm map[string]any) (apiKeyID, apiKey string) {
	auth, _ := rm["auth"].(map[string]any)
	if auth == nil {
		return "", ""
	}
	raw := auth["credentials"]
	if arr, ok := raw.([]any); ok && len(arr) > 0 {
		raw = arr[0]
	}
	cred, _ := raw.(map[string]any)
	if cred == nil {
		return "", ""
	}
	payload, _ := cred["payload"].(string)
	if payload == "" {
		return "", ""
	}
	entity, _ := auth["entity"].(string)
	if entity == "" {
		entity, _ = cred["authEntity"].(string)
	}
	if entity == "" {
		entity, _ = cred["entity"].(string)
	}
	return entity, payload
}

func locationPrimaryOrgID(loc *app.Location) string {
	if loc == nil {
		return ""
	}
	for _, o := range loc.Organizations {
		if o != nil && o.Primary {
			return o.OrganizationID
		}
	}
	return ""
}

// locationIDFromRemoteAddress extracts the location id from a Viam
// cloud remote address like "boat-main.abc123xyz.viam.cloud"; returns
// "" for anything else (e.g. LAN addresses).
func locationIDFromRemoteAddress(addr string) string {
	if !strings.HasSuffix(addr, ".viam.cloud") {
		return ""
	}
	segs := strings.Split(addr, ".")
	if len(segs) != 4 {
		return ""
	}
	return segs[1]
}

// discoverSeedRobot asks the data store which robots captured Position
// readings for this component name recently. Only an unambiguous
// answer (exactly one robot) is used — with several, guessing could
// seed another boat's track.
func (a *DisplayAPI) discoverSeedRobot(ctx context.Context, dc *app.DataClient, orgID string, start time.Time) string {
	leaf := a.msName
	if i := strings.LastIndex(leaf, ":"); i >= 0 {
		leaf = leaf[i+1:]
	}
	query := []map[string]any{
		{"$match": map[string]any{
			"component_name": leaf,
			"method_name":    "Position",
			"time_received":  map[string]any{"$gte": start},
		}},
		{"$group": map[string]any{"_id": "$robot_id"}},
		{"$limit": 5},
	}
	rows, err := dc.TabularDataByMQL(ctx, orgID, query,
		&app.TabularDataByMQLOptions{TabularDataSourceType: app.TabularDataSourceTypeHotStorage})
	if err != nil || len(rows) == 0 {
		rows, err = dc.TabularDataByMQL(ctx, orgID, query, nil)
		if err != nil {
			a.logger.Debugf("track seed: robot discovery: %v", err)
			return ""
		}
	}
	if len(rows) != 1 {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			if s, ok := r["_id"].(string); ok {
				ids = append(ids, s)
			}
		}
		a.logger.Infof("track seed: %d robots have %q Position data (%v) — can't pick one automatically",
			len(rows), leaf, ids)
		return ""
	}
	rid, _ := rows[0]["_id"].(string)
	return rid
}

// trackSeedQuery builds the MQL pipeline for the seed: per-minute
// buckets of the movement sensor's captured Position readings, oldest
// first — the same bucketing the web app's position-history query uses.
func trackSeedQuery(locationID, robotID, msName string, start time.Time) []map[string]any {
	leaf := msName
	if i := strings.LastIndex(leaf, ":"); i >= 0 {
		leaf = leaf[i+1:]
	}
	match := map[string]any{
		"component_name": leaf,
		"method_name":    "Position",
		"time_received":  map[string]any{"$gte": start},
	}
	if robotID != "" {
		match["robot_id"] = robotID
	}
	if locationID != "" {
		match["location_id"] = locationID
	}
	str := func(expr any) any { return map[string]any{"$toString": expr} }
	bucket := map[string]any{"$concat": []any{
		str(map[string]any{"$year": "$time_received"}), "-",
		str(map[string]any{"$month": "$time_received"}), "-",
		str(map[string]any{"$dayOfMonth": "$time_received"}), " ",
		str(map[string]any{"$hour": "$time_received"}), ":",
		str(map[string]any{"$minute": "$time_received"}),
	}}
	return []map[string]any{
		{"$match": match},
		{"$sort": map[string]any{"time_received": -1}},
		{"$group": map[string]any{
			"_id": bucket,
			"ts":  map[string]any{"$min": "$time_received"},
			"pos": map[string]any{"$first": "$data"},
		}},
		{"$sort": map[string]any{"ts": 1}},
	}
}

// trackPointsFromRows converts seed-query rows to track points,
// dropping anything malformed, non-finite, or at null island.
func trackPointsFromRows(rows []map[string]any) []trackPoint {
	points := make([]trackPoint, 0, len(rows))
	for _, row := range rows {
		ts, ok := row["ts"].(time.Time)
		if !ok {
			continue
		}
		pos, ok := row["pos"].(map[string]any)
		if !ok {
			continue
		}
		coord, ok := pos["coordinate"].(map[string]any)
		if !ok {
			continue
		}
		lat, latOK := toFloat(coord["latitude"])
		lng, lngOK := toFloat(coord["longitude"])
		if !latOK || !lngOK || !isFiniteCoord(lat, lng) || (lat == 0 && lng == 0) {
			continue
		}
		points = append(points, trackPoint{Lat: lat, Lng: lng, Ts: ts.UnixMilli()})
	}
	return points
}

func isFiniteCoord(lat, lng float64) bool {
	return !math.IsNaN(lat) && !math.IsInf(lat, 0) && !math.IsNaN(lng) && !math.IsInf(lng, 0)
}

func (a *DisplayAPI) recordTrackPoint(p trackPoint) {
	a.trackMu.Lock()
	defer a.trackMu.Unlock()
	a.track = append(a.track, p)
	dropBefore := p.Ts - trackKeep.Milliseconds()
	drop := 0
	for drop < len(a.track) && a.track[drop].Ts < dropBefore {
		drop++
	}
	if drop > 0 {
		a.track = a.track[drop:]
	}
}

// handleTrack returns the recorded own-boat track, oldest first.
func (a *DisplayAPI) handleTrack(w http.ResponseWriter, r *http.Request) {
	a.refreshFallback()
	if a.currentMS() == nil {
		writeAPIJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no movement_sensor configured"})
		return
	}
	a.trackMu.Lock()
	points := make([]trackPoint, len(a.track))
	copy(points, a.track)
	a.trackMu.Unlock()
	writeAPIJSON(w, http.StatusOK, map[string]any{"points": points})
}

func writeAPIJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *DisplayAPI) handleInfo(w http.ResponseWriter, r *http.Request) {
	a.refreshFallback()
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"service": "viam-chartplotter",
		"state":   a.currentMS() != nil,
		"depth":   a.currentDepth() != nil,
		"route":   a.currentRoute() != nil,
		"nav":     a.currentNav() != nil,
		"track":   a.currentMS() != nil,
		"cameras": a.currentCameraNames(),
	})
}

// handleState reports the boat's live position/heading/speed (and depth
// when a depth sensor is configured). Position is the one required
// reading; everything else is best-effort so a movement sensor that
// doesn't support e.g. LinearVelocity still yields a usable response.
func (a *DisplayAPI) handleState(w http.ResponseWriter, r *http.Request) {
	a.refreshFallback()
	ms := a.currentMS()
	if ms == nil {
		writeAPIJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no movement_sensor configured"})
		return
	}
	ctx := r.Context()

	pos, _, err := ms.Position(ctx, nil)
	if err != nil || pos == nil {
		writeAPIJSON(w, http.StatusBadGateway, map[string]any{"error": "position: " + errString(err)})
		return
	}
	out := map[string]any{
		"lat": pos.Lat(),
		"lng": pos.Lng(),
		"ts":  time.Now().UnixMilli(),
	}
	if vel, err := ms.LinearVelocity(ctx, nil); err == nil {
		out["sog_kn"] = vel.Y * metersPerSecToKnots
	}
	if hdg, err := ms.CompassHeading(ctx, nil); err == nil {
		out["heading_deg"] = hdg
	}
	if readings, err := ms.Readings(ctx, nil); err == nil {
		// Same COG key hunt as the web app — different NMEA sources
		// name the field differently.
		for _, k := range []string{"Course Over Ground", "course_over_ground", "CourseOverGround", "cog", "COG"} {
			if v, ok := toFloat(readings[k]); ok {
				out["cog_deg"] = v
				break
			}
		}
	}
	if depth := a.currentDepth(); depth != nil {
		if readings, err := depth.Readings(ctx, nil); err == nil {
			if m, ok := toFloat(readings["Depth"]); ok {
				out["depth_ft"] = m * metersToFeet
			}
		}
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// handleRoute merges the boat nav system's active-route readings (the
// route sensor, e.g. a viamboat N2K sensor) with the chartplotter nav
// service's waypoint list. Either half is optional.
func (a *DisplayAPI) handleRoute(w http.ResponseWriter, r *http.Request) {
	a.refreshFallback()
	route, nav := a.currentRoute(), a.currentNav()
	if route == nil && nav == nil {
		writeAPIJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no route_sensor or nav_service configured"})
		return
	}
	ctx := r.Context()
	out := map[string]any{"ts": time.Now().UnixMilli()}

	if route != nil {
		if readings, err := route.Readings(ctx, nil); err != nil {
			a.logger.Debugf("display api: route sensor readings: %v", err)
		} else {
			// Key names match what the web app reads off the same sensor
			// (App.svelte); distances are meters, velocity m/s.
			for key, field := range map[string]string{
				"Destination Latitude":      "destination_lat",
				"Destination Longitude":     "destination_lng",
				"Distance to Waypoint":      "distance_to_waypoint_m",
				"Waypoint Closing Velocity": "closing_velocity_m_s",
			} {
				if v, ok := toFloat(readings[key]); ok {
					out[field] = v
				}
			}
		}
	}

	if nav != nil {
		wps, err := nav.Waypoints(ctx, nil)
		if err != nil {
			a.logger.Debugf("display api: nav waypoints: %v", err)
		} else {
			list := make([]map[string]any, 0, len(wps))
			for _, wp := range wps {
				list = append(list, map[string]any{
					"id":  wp.ID.Hex(),
					"lat": wp.Lat,
					"lng": wp.Long,
				})
			}
			out["waypoints"] = list
		}
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// handleCamera serves the latest still frame from the named camera as a
// JPEG — the same frame-poll model the web app uses, no video stack.
// The client polls; an optional ".jpg" suffix on the name is accepted
// so URLs read naturally.
func (a *DisplayAPI) handleCamera(w http.ResponseWriter, r *http.Request) {
	a.refreshFallback()
	name := strings.TrimSuffix(r.PathValue("name"), ".jpg")
	cam, ok := a.currentCamera(name)
	if !ok {
		writeAPIJSON(w, http.StatusNotFound, map[string]any{"error": "unknown camera", "cameras": a.currentCameraNames()})
		return
	}
	ctx := r.Context()
	imgs, _, err := cam.Images(ctx, nil, nil)
	if err != nil || len(imgs) == 0 {
		writeAPIJSON(w, http.StatusBadGateway, map[string]any{"error": "camera: " + errString(err)})
		return
	}
	b, err := jpegBytes(ctx, imgs[0])
	if err != nil {
		writeAPIJSON(w, http.StatusBadGateway, map[string]any{"error": "encode: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", rutils.MimeTypeJPEG)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// jpegBytes returns the image as JPEG, passing the camera's bytes
// through untouched when they already are JPEG and re-encoding only
// otherwise (e.g. a camera that serves PNG or raw).
func jpegBytes(ctx context.Context, ni camera.NamedImage) ([]byte, error) {
	if ni.MimeType() == rutils.MimeTypeJPEG {
		return ni.Bytes(ctx)
	}
	img, err := ni.Image(ctx)
	if err != nil {
		return nil, err
	}
	return rimage.EncodeImage(ctx, img, rutils.MimeTypeJPEG)
}

// toFloat coerces the numeric types a sensor Readings map can carry.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func errString(err error) string {
	if err == nil {
		return "no data"
	}
	return err.Error()
}
