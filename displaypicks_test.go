package vc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	geo "github.com/kellydunn/golang-geo"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
	"go.viam.com/rdk/testutils/inject"
)

// resetDisplayPicks clears the package-level picks registry before and after
// a test that touches it, so tests don't leak picks into each other.
func resetDisplayPicks(t *testing.T) {
	t.Helper()
	clear := func() {
		displayPicksReg.mu.Lock()
		displayPicksReg.picks = DisplayPicks{}
		displayPicksReg.nav = nil
		displayPicksReg.gen = 0
		displayPicksReg.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestDisplayPicksPersistRoundTrip(t *testing.T) {
	resetDisplayPicks(t)
	logger := logging.NewTestLogger(t)
	path := filepath.Join(t.TempDir(), "picks.json")

	want := DisplayPicks{MovementSensor: "gps1", DepthSensor: "depth1", Cameras: []string{"cam1", "cam2"}}
	setDisplayPicks(want, nil, path, logger)

	// Simulate a restart: empty registry, load from disk.
	displayPicksReg.mu.Lock()
	displayPicksReg.picks = DisplayPicks{}
	displayPicksReg.gen = 0
	displayPicksReg.mu.Unlock()
	loadDisplayPicks(path, logger)

	got, _, gen := getDisplayPicks()
	if gen != 1 || got.MovementSensor != "gps1" || got.DepthSensor != "depth1" || len(got.Cameras) != 2 {
		t.Fatalf("bad loaded picks: gen=%d %+v", gen, got)
	}

	// A report that already landed must not be clobbered by a later load.
	setDisplayPicks(DisplayPicks{MovementSensor: "gps2"}, nil, path+".other", logger)
	loadDisplayPicks(path, logger)
	got, _, _ = getDisplayPicks()
	if got.MovementSensor != "gps2" {
		t.Fatalf("load clobbered reported picks: %+v", got)
	}
}

func TestNavSetDisplayResources(t *testing.T) {
	resetDisplayPicks(t)
	svc := &navService{
		name:      navigation.Named("nav"),
		logger:    logging.NewTestLogger(t),
		picksPath: filepath.Join(t.TempDir(), "picks.json"),
	}

	out, err := svc.DoCommand(context.Background(), map[string]interface{}{
		"set_display_resources": map[string]interface{}{
			"movement_sensor": "gps1",
			"depth_sensor":    "depth1",
			"route_sensor":    "",
			"cameras":         []interface{}{"cam1", "cam2"},
		},
	})
	if err != nil || out["ok"] != true {
		t.Fatalf("DoCommand: %v %v", out, err)
	}
	picks, nav, gen := getDisplayPicks()
	if gen != 1 || picks.MovementSensor != "gps1" || picks.DepthSensor != "depth1" ||
		picks.RouteSensor != "" || len(picks.Cameras) != 2 {
		t.Fatalf("bad picks: gen=%d %+v", gen, picks)
	}
	if nav != navigation.Service(svc) {
		t.Fatal("registry should hold the reporting nav service")
	}

	if _, err := svc.DoCommand(context.Background(), map[string]interface{}{
		"set_display_resources": map[string]interface{}{"cameras": []interface{}{7}},
	}); err == nil {
		t.Fatal("non-string camera should error")
	}
}

// TestDisplayAPIFallbackFromPicks: an unconfigured display API should serve
// /api/state, depth, route and info off the web app's reported picks,
// resolved through the (injected) machine dependencies.
func TestDisplayAPIFallbackFromPicks(t *testing.T) {
	resetDisplayPicks(t)
	logger := logging.NewTestLogger(t)

	ms := &inject.MovementSensor{
		PositionFunc: func(ctx context.Context, extra map[string]interface{}) (*geo.Point, float64, error) {
			return geo.NewPoint(40.7, -74.0), 0, nil
		},
		LinearVelocityFunc: func(ctx context.Context, extra map[string]interface{}) (r3.Vector, error) {
			return r3.Vector{Y: 5.0}, nil
		},
		CompassHeadingFunc: func(ctx context.Context, extra map[string]interface{}) (float64, error) {
			return 87.5, nil
		},
		ReadingsFunc: func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{}, nil
		},
	}
	depth := &inject.Sensor{
		ReadingsFunc: func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"Depth": 10.0}, nil
		},
	}
	nav := &inject.NavigationService{
		WaypointsFunc: func(ctx context.Context, extra map[string]interface{}) ([]navigation.Waypoint, error) {
			return []navigation.Waypoint{{ID: primitive.NilObjectID, Lat: 41.0, Long: -73.5}}, nil
		},
	}

	api := &DisplayAPI{
		logger: logger,
		fbDeps: func(ctx context.Context) (resource.Dependencies, error) {
			return resource.Dependencies{
				movementsensor.Named("gps1"): ms,
				sensor.Named("depth1"):       depth,
			}, nil
		},
	}
	t.Cleanup(api.Close)
	srv := displayAPIServer(t, api)

	// Nothing picked yet: state and route are 503, like unconfigured.
	getJSON(t, srv.URL+"/api/state", 503)
	getJSON(t, srv.URL+"/api/route", 503)

	setDisplayPicks(DisplayPicks{MovementSensor: "gps1", DepthSensor: "depth1"}, nav,
		filepath.Join(t.TempDir(), "picks.json"), logger)

	// Resolution happens off the request path; poll until it lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := getJSON(t, srv.URL+"/api/info", 200)
		if out["state"] == true {
			if out["depth"] != true || out["nav"] != true || out["track"] != true {
				t.Fatalf("bad info after picks: %v", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("picks never resolved: %v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	out := getJSON(t, srv.URL+"/api/state", 200)
	if out["lat"] != 40.7 || out["lng"] != -74.0 {
		t.Fatalf("bad fallback state: %v", out)
	}
	if d := out["depth_ft"].(float64); d < 32.8 || d > 32.9 {
		t.Fatalf("bad fallback depth: %v", d)
	}

	out = getJSON(t, srv.URL+"/api/route", 200)
	wps, ok := out["waypoints"].([]any)
	if !ok || len(wps) != 1 {
		t.Fatalf("bad fallback route: %v", out)
	}
}
