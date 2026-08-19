package vc

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang/geo/r3"
	geo "github.com/kellydunn/golang-geo"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
	"go.viam.com/rdk/testutils/inject"
	rutils "go.viam.com/rdk/utils"
)

func displayAPIServer(t *testing.T, api *DisplayAPI) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, wantStatus int) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDisplayAPIState(t *testing.T) {
	ms := &inject.MovementSensor{
		PositionFunc: func(ctx context.Context, extra map[string]interface{}) (*geo.Point, float64, error) {
			return geo.NewPoint(40.7, -74.0), 0, nil
		},
		LinearVelocityFunc: func(ctx context.Context, extra map[string]interface{}) (r3.Vector, error) {
			return r3.Vector{Y: 5.0}, nil // m/s northward
		},
		CompassHeadingFunc: func(ctx context.Context, extra map[string]interface{}) (float64, error) {
			return 87.5, nil
		},
		ReadingsFunc: func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"Course Over Ground": 91.0}, nil
		},
	}
	depth := &inject.Sensor{
		ReadingsFunc: func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"Depth": 10.0}, nil // meters
		},
	}
	api := &DisplayAPI{logger: logging.NewTestLogger(t), ms: ms, depth: depth}
	srv := displayAPIServer(t, api)

	out := getJSON(t, srv.URL+"/api/state", http.StatusOK)
	if out["lat"] != 40.7 || out["lng"] != -74.0 {
		t.Fatalf("bad position: %v", out)
	}
	if sog := out["sog_kn"].(float64); sog < 9.7 || sog > 9.8 { // 5 m/s ≈ 9.72 kn
		t.Fatalf("bad sog: %v", sog)
	}
	if out["heading_deg"] != 87.5 || out["cog_deg"] != 91.0 {
		t.Fatalf("bad heading/cog: %v", out)
	}
	if d := out["depth_ft"].(float64); d < 32.8 || d > 32.9 { // 10 m ≈ 32.8 ft
		t.Fatalf("bad depth: %v", d)
	}
}

func TestDisplayAPIStateUnconfigured(t *testing.T) {
	api := &DisplayAPI{logger: logging.NewTestLogger(t)}
	srv := displayAPIServer(t, api)
	getJSON(t, srv.URL+"/api/state", http.StatusServiceUnavailable)
	getJSON(t, srv.URL+"/api/route", http.StatusServiceUnavailable)
	// info always works and reports nothing configured
	out := getJSON(t, srv.URL+"/api/info", http.StatusOK)
	if out["state"] != false || out["route"] != false {
		t.Fatalf("bad info: %v", out)
	}
}

func TestDisplayAPIRoute(t *testing.T) {
	routeSensor := &inject.Sensor{
		ReadingsFunc: func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{
				"Destination Latitude":      41.0,
				"Destination Longitude":     -73.5,
				"Distance to Waypoint":      1852.0,
				"Waypoint Closing Velocity": 3.0,
				"Unrelated":                 "ignored",
			}, nil
		},
	}
	nav := &inject.NavigationService{
		WaypointsFunc: func(ctx context.Context, extra map[string]interface{}) ([]navigation.Waypoint, error) {
			return []navigation.Waypoint{{ID: primitive.NilObjectID, Lat: 41.0, Long: -73.5}}, nil
		},
	}
	api := &DisplayAPI{logger: logging.NewTestLogger(t), route: routeSensor, nav: nav}
	srv := displayAPIServer(t, api)

	out := getJSON(t, srv.URL+"/api/route", http.StatusOK)
	if out["distance_to_waypoint_m"] != 1852.0 || out["closing_velocity_m_s"] != 3.0 {
		t.Fatalf("bad route: %v", out)
	}
	if out["destination_lat"] != 41.0 || out["destination_lng"] != -73.5 {
		t.Fatalf("bad destination: %v", out)
	}
	wps := out["waypoints"].([]any)
	if len(wps) != 1 {
		t.Fatalf("bad waypoints: %v", out["waypoints"])
	}
	wp := wps[0].(map[string]any)
	if wp["lat"] != 41.0 || wp["lng"] != -73.5 {
		t.Fatalf("bad waypoint: %v", wp)
	}
}

func TestDisplayAPICamera(t *testing.T) {
	// A camera that serves PNG-less raw JPEG bytes: the handler should
	// pass them through untouched.
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	jpg := buf.Bytes()
	ni, err := camera.NamedImageFromBytes(jpg, "cam1", rutils.MimeTypeJPEG, data.Annotations{})
	if err != nil {
		t.Fatal(err)
	}
	cam := &inject.Camera{
		ImagesFunc: func(ctx context.Context, filterSourceNames []string, extra map[string]interface{},
		) ([]camera.NamedImage, resource.ResponseMetadata, error) {
			return []camera.NamedImage{ni}, resource.ResponseMetadata{}, nil
		},
	}
	api := &DisplayAPI{
		logger:      logging.NewTestLogger(t),
		cameras:     map[string]camera.Camera{"cam1": cam},
		cameraNames: []string{"cam1"},
	}
	srv := displayAPIServer(t, api)

	// info lists the camera
	out := getJSON(t, srv.URL+"/api/info", http.StatusOK)
	if cams := out["cameras"].([]any); len(cams) != 1 || cams[0] != "cam1" {
		t.Fatalf("bad cameras: %v", out["cameras"])
	}

	// with and without .jpg suffix
	for _, path := range []string{"/api/camera/cam1.jpg", "/api/camera/cam1"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, len(jpg)+16)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != rutils.MimeTypeJPEG {
			t.Fatalf("GET %s content-type = %q", path, ct)
		}
		if !bytes.Equal(body[:n], jpg) {
			t.Fatalf("GET %s: bytes not passed through (got %d, want %d)", path, n, len(jpg))
		}
	}

	// unknown camera 404s and names the known ones
	out = getJSON(t, srv.URL+"/api/camera/nope.jpg", http.StatusNotFound)
	if out["error"] != "unknown camera" {
		t.Fatalf("bad 404 body: %v", out)
	}
}

func TestChartplotterConfigValidate(t *testing.T) {
	cfg := &ChartplotterConfig{
		MovementSensor: "gps",
		RouteSensor:    "route",
		Cameras:        []string{"bow", "stern"},
	}
	req, opt, err := cfg.Validate("")
	if err != nil {
		t.Fatal(err)
	}
	if len(req) != 0 {
		t.Fatalf("expected no required deps, got %v", req)
	}
	want := []string{"gps", "route", "bow", "stern"}
	if len(opt) != len(want) {
		t.Fatalf("optional deps = %v, want %v", opt, want)
	}
	for i := range want {
		if opt[i] != want[i] {
			t.Fatalf("optional deps = %v, want %v", opt, want)
		}
	}
}
