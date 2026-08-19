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

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/rimage"
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
	return a
}

// Close stops the background track recorder, if running.
func (a *DisplayAPI) Close() {
	if a.trackCancel != nil {
		a.trackCancel()
		<-a.trackDone
	}
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
			callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
			pos, _, err := a.ms.Position(callCtx, nil)
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

// seedTrackFromCloud pulls the last trackKeep of captured Position
// readings for the movement sensor from the Viam data API — callable
// from inside the module via the machine's own credentials env vars —
// and prepends them to the in-memory track. Best-effort: any failure
// logs and leaves the recorder running live-only. Hot storage is tried
// first (fast); standard storage is the fallback.
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

	query := trackSeedQuery(locationID, robotID, a.msName, time.Now().Add(-trackKeep))
	rows, err := vc.DataClient().TabularDataByMQL(ctx, orgID, query,
		&app.TabularDataByMQLOptions{TabularDataSourceType: app.TabularDataSourceTypeHotStorage})
	if err != nil || len(rows) == 0 {
		if err != nil {
			a.logger.Debugf("track seed: hot storage query: %v", err)
		}
		rows, err = vc.DataClient().TabularDataByMQL(ctx, orgID, query, nil)
		if err != nil {
			a.logger.Warnf("track seed failed: %v", err)
			return
		}
	}

	points := trackPointsFromRows(rows)
	if len(points) == 0 {
		a.logger.Infof("track seed: no captured positions in the last %s", trackKeep)
		return
	}
	a.trackMu.Lock()
	a.track = append(points, a.track...)
	a.trackMu.Unlock()
	a.logger.Infof("track seeded with %d historical points (%s → %s)",
		len(points),
		time.UnixMilli(points[0].Ts).Format(time.RFC3339),
		time.UnixMilli(points[len(points)-1].Ts).Format(time.RFC3339))
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
		"robot_id":       robotID,
		"component_name": leaf,
		"method_name":    "Position",
		"time_received":  map[string]any{"$gte": start},
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
	if a.ms == nil {
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
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"service": "viam-chartplotter",
		"state":   a.ms != nil,
		"depth":   a.depth != nil,
		"route":   a.route != nil,
		"nav":     a.nav != nil,
		"track":   a.ms != nil,
		"cameras": append([]string{}, a.cameraNames...),
	})
}

// handleState reports the boat's live position/heading/speed (and depth
// when a depth sensor is configured). Position is the one required
// reading; everything else is best-effort so a movement sensor that
// doesn't support e.g. LinearVelocity still yields a usable response.
func (a *DisplayAPI) handleState(w http.ResponseWriter, r *http.Request) {
	if a.ms == nil {
		writeAPIJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no movement_sensor configured"})
		return
	}
	ctx := r.Context()

	pos, _, err := a.ms.Position(ctx, nil)
	if err != nil || pos == nil {
		writeAPIJSON(w, http.StatusBadGateway, map[string]any{"error": "position: " + errString(err)})
		return
	}
	out := map[string]any{
		"lat": pos.Lat(),
		"lng": pos.Lng(),
		"ts":  time.Now().UnixMilli(),
	}
	if vel, err := a.ms.LinearVelocity(ctx, nil); err == nil {
		out["sog_kn"] = vel.Y * metersPerSecToKnots
	}
	if hdg, err := a.ms.CompassHeading(ctx, nil); err == nil {
		out["heading_deg"] = hdg
	}
	if readings, err := a.ms.Readings(ctx, nil); err == nil {
		// Same COG key hunt as the web app — different NMEA sources
		// name the field differently.
		for _, k := range []string{"Course Over Ground", "course_over_ground", "CourseOverGround", "cog", "COG"} {
			if v, ok := toFloat(readings[k]); ok {
				out["cog_deg"] = v
				break
			}
		}
	}
	if a.depth != nil {
		if readings, err := a.depth.Readings(ctx, nil); err == nil {
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
	if a.route == nil && a.nav == nil {
		writeAPIJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no route_sensor or nav_service configured"})
		return
	}
	ctx := r.Context()
	out := map[string]any{"ts": time.Now().UnixMilli()}

	if a.route != nil {
		if readings, err := a.route.Readings(ctx, nil); err != nil {
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

	if a.nav != nil {
		wps, err := a.nav.Waypoints(ctx, nil)
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
	name := strings.TrimSuffix(r.PathValue("name"), ".jpg")
	cam, ok := a.cameras[name]
	if !ok {
		writeAPIJSON(w, http.StatusNotFound, map[string]any{"error": "unknown camera", "cameras": a.cameraNames})
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
