package vc

import (
	"context"
	"math"
	"strings"
	"testing"

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

// recordingBase is an inject.Base that records set_heading commands and Stops.
func recordingBase() (*inject.Base, *[]float64, *int) {
	b := inject.NewBase("autopilot")
	var headings []float64
	stops := 0
	b.DoFunc = func(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		if h, ok := cmd["set_heading"].(float64); ok {
			headings = append(headings, h)
		}
		return map[string]interface{}{}, nil
	}
	b.StopFunc = func(ctx context.Context, extra map[string]interface{}) error {
		stops++
		return nil
	}
	return b, &headings, &stops
}

func TestNavSteersBaseTowardNextWaypoint(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	b, headings, stops := recordingBase()
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: boat(&at), base: b}

	// No goal: nothing sent, nothing to stop.
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*headings) != 0 || *stops != 0 {
		t.Fatalf("idle tick: headings=%v stops=%d", *headings, *stops)
	}

	// Route due north: steer 0°.
	startRoute(t, svc, navPoint{Lat: 41.5, Lng: -71.0}, navPoint{Lat: 41.5, Lng: -70.5})
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*headings) != 1 || math.Abs((*headings)[0]) > 1e-6 {
		t.Fatalf("headings = %v, want [0]", *headings)
	}

	// Arrive at the first waypoint: the same tick steers for the second
	// (east-ish).
	at = navPoint{Lat: 41.5, Lng: -71.0}
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if got := (*headings)[len(*headings)-1]; got < 80 || got > 100 {
		t.Fatalf("after arrival heading = %v, want ~90", got)
	}

	// Cancel stops the base once.
	if _, err := svc.Cancel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if *stops != 1 {
		t.Fatalf("stops after cancel = %d, want 1", *stops)
	}
	n := len(*headings)
	if err := svc.checkArrival(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*headings) != n || *stops != 1 {
		t.Fatalf("tick after cancel: headings %d->%d, stops=%d", n, len(*headings), *stops)
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
