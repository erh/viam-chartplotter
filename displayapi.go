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
//	GET /api/camera/{name}.jpg  latest frame from the named camera
//
// Every endpoint is a cheap read; clients poll (state ~1s, route ~5s,
// cameras ~2s). All responses are uncacheable.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

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
	return a
}

func (a *DisplayAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/info", a.handleInfo)
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/route", a.handleRoute)
	mux.HandleFunc("GET /api/camera/{name}", a.handleCamera)
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
