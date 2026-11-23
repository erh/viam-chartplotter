package vc

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/pkg/errors"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func TestBearingDegrees(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		aLat, aLng, bLat, bLng float64
		want                   float64
	}{
		{"north", 41, -71, 42, -71, 0},
		{"south", 41, -71, 40, -71, 180},
		{"east on equator", 0, 10, 0, 11, 90},
		{"west on equator", 0, 10, 0, 9, 270},
	} {
		if got := bearingDegrees(tc.aLat, tc.aLng, tc.bLat, tc.bLng); math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("%s: bearing = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Northeast at mid-latitude: between 0 and 90.
	if got := bearingDegrees(41, -71, 41.1, -70.9); got <= 0 || got >= 90 {
		t.Errorf("northeast bearing = %v", got)
	}
	if d := headingDelta(350, 10); d != 20 {
		t.Errorf("headingDelta(350, 10) = %v, want 20", d)
	}
}

// recordingBase is an inject.Base that records Spin angles and Stops.
func recordingBase() (*inject.Base, *[]float64, *int) {
	b := inject.NewBase("autopilot")
	var spins []float64
	stops := 0
	b.SpinFunc = func(ctx context.Context, angleDeg, degsPerSec float64, extra map[string]interface{}) error {
		if degsPerSec == 0 {
			return errors.New("nav must not Spin at 0 deg/s (that means stop)")
		}
		spins = append(spins, angleDeg)
		return nil
	}
	b.StopFunc = func(ctx context.Context, extra map[string]interface{}) error {
		stops++
		return nil
	}
	return b, &spins, &stops
}

func TestTurnDegrees(t *testing.T) {
	for _, tc := range []struct{ from, to, want float64 }{
		{90, 0, 90},    // east -> north: 90 left
		{0, 90, -90},   // north -> east: 90 right
		{10, 350, 20},  // short way across north, left
		{350, 10, -20}, // short way across north, right
		{0, 180, 180},  // dead astern: (-180, 180]
		{45, 45, 0},
	} {
		if got := turnDegrees(tc.from, tc.to); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("turnDegrees(%v, %v) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestNavSteersBaseTowardNextWaypoint(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	b, spins, stops := recordingBase()
	// boat() reports compass heading 090.
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: boat(&at), base: b}

	// No goal: nothing sent, nothing to stop.
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*spins) != 0 || *stops != 0 {
		t.Fatalf("idle tick: spins=%v stops=%d", *spins, *stops)
	}

	// Route due north while pointing east: turn 90 left.
	startRoute(t, svc, navPoint{Lat: 41.5, Lng: -71.0}, navPoint{Lat: 41.5, Lng: -70.5})
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*spins) != 1 || math.Abs((*spins)[0]-90) > 1e-6 {
		t.Fatalf("spins = %v, want [90]", *spins)
	}

	// Arrive at the first waypoint: the same tick steers for the second,
	// which is roughly east — already the heading, so a small turn.
	at = navPoint{Lat: 41.5, Lng: -71.0}
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if got := (*spins)[len(*spins)-1]; math.Abs(got) > 10 {
		t.Fatalf("after arrival spin = %v, want ~0", got)
	}

	// Cancel stops the base once; later ticks send nothing.
	if _, err := svc.Cancel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if *stops != 1 {
		t.Fatalf("stops after cancel = %d, want 1", *stops)
	}
	n := len(*spins)
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*spins) != n || *stops != 1 {
		t.Fatalf("tick after cancel: spins %d->%d, stops=%d", n, len(*spins), *stops)
	}
}

// End to end: nav steering a real n2k-base that shares nav's movement
// sensor tells the autopilot to hold the bearing to the waypoint itself.
func TestNavSteersN2KBaseToBearing(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	ms := boat(&at)
	sender := &fakeN2KSender{}
	nb := &n2kBase{logger: logging.NewTestLogger(t), sender: sender, ms: ms, dst: -1}
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: ms, base: nb}

	wp := navPoint{Lat: 41.3, Lng: -70.6}
	startRoute(t, svc, wp)
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.cmds) != 1 {
		t.Fatalf("sender got %d commands, want 1", len(sender.cmds))
	}
	got := sender.cmds[0]["fields"].(map[string]interface{})["Heading-To-Steer (Course)"].(float64)
	want := bearingDegrees(at.Lat, at.Lng, wp.Lat, wp.Lng)
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("autopilot told %v, want bearing %v", got, want)
	}
}

func TestNavSendWaypointsForwardsToBase(t *testing.T) {
	ctx := context.Background()
	b := inject.NewBase("autopilot")
	var got map[string]interface{}
	b.DoFunc = func(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		got = cmd
		return map[string]interface{}{"ok": true}, nil
	}
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), base: b}

	if _, err := svc.DoCommand(ctx, map[string]interface{}{"send_waypoints": true}); err == nil || !strings.Contains(err.Error(), "no waypoints") {
		t.Fatalf("expected no-waypoints error, got %v", err)
	}

	startRoute(t, svc, navPoint{Lat: 41.1631, Lng: -71.5784}, navPoint{Lat: 41.2042, Lng: -71.5511})
	if _, err := svc.DoCommand(ctx, map[string]interface{}{
		"send_waypoints_n2k": map[string]interface{}{"route_name": "Test Route"},
	}); err != nil {
		t.Fatal(err)
	}
	payload := got["send_waypoints"].(map[string]interface{})
	if payload["route_name"] != "Test Route" {
		t.Fatalf("route_name not forwarded: %v", payload)
	}
	wps := payload["waypoints"].([]interface{})
	if len(wps) != 2 || wps[0].(map[string]interface{})["lat"] != 41.1631 {
		t.Fatalf("waypoints = %v", wps)
	}

	// No base configured.
	svc = &navService{logger: logging.NewTestLogger(t), store: tempStore(t)}
	if _, err := svc.DoCommand(ctx, map[string]interface{}{"send_waypoints": true}); err == nil || !strings.Contains(err.Error(), "no base") {
		t.Fatalf("expected no-base error, got %v", err)
	}
}

func TestNavConfigBaseRequiresMovementSensor(t *testing.T) {
	if _, _, err := (&NavConfig{Base: "autopilot"}).Validate("nav"); err == nil {
		t.Fatal("expected error: base without movement_sensor")
	}
	deps, _, err := (&NavConfig{Base: "autopilot", MovementSensor: "gps"}).Validate("nav")
	if err != nil || len(deps) != 2 {
		t.Fatalf("deps=%v err=%v", deps, err)
	}
}
