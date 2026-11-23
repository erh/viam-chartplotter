package vc

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	geo "github.com/kellydunn/golang-geo"
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

// tick runs one arrival check and then one steering tick, as the two loops
// would.
func tick(t *testing.T, svc *navService) {
	t.Helper()
	if err := svc.checkArrival(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.steer(context.Background())
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
	tick(t, svc)
	if len(*spins) != 0 || *stops != 0 {
		t.Fatalf("idle tick: spins=%v stops=%d", *spins, *stops)
	}

	// Route due north while pointing east: turn 90 left.
	startRoute(t, svc, navPoint{Lat: 41.5, Lng: -71.0}, navPoint{Lat: 41.5, Lng: -70.5})
	tick(t, svc)
	if len(*spins) != 1 || math.Abs((*spins)[0]-90) > 1e-6 {
		t.Fatalf("spins = %v, want [90]", *spins)
	}

	// Arrive at the first waypoint: the same tick steers for the second,
	// which is roughly east — already the heading, so a small turn.
	at = navPoint{Lat: 41.5, Lng: -71.0}
	tick(t, svc)
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
	tick(t, svc)
	if len(*spins) != n || *stops != 1 {
		t.Fatalf("tick after cancel: spins %d->%d, stops=%d", n, len(*spins), *stops)
	}
}

// End to end: nav steering a real n2k-base that shares nav's movement
// sensor tells the autopilot to hold the bearing to the waypoint itself.
func TestNavSteersN2KBaseToBearing(t *testing.T) {
	at := navPoint{Lat: 41.0, Lng: -71.0}
	ms := boat(&at)
	sender := &fakeN2KSender{}
	nb := &n2kBase{logger: logging.NewTestLogger(t), sender: sender, ms: ms, dst: -1}
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: ms, base: nb}

	wp := navPoint{Lat: 41.3, Lng: -70.6}
	startRoute(t, svc, wp)
	tick(t, svc)
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

func TestVelocityCommand(t *testing.T) {
	// On course: full speed, no turn.
	lin, ang := velocityCommand(0, 2)
	if math.Abs(lin-2000) > 1e-9 || ang != 0 {
		t.Fatalf("on course: lin=%v ang=%v", lin, ang)
	}
	// Small error: proportional turn the right way, slightly slower.
	lin, ang = velocityCommand(10, 2)
	if ang != 10*steerTurnGain || lin >= 2000 || lin < 1900 {
		t.Fatalf("10° left: lin=%v ang=%v", lin, ang)
	}
	lin, ang = velocityCommand(-10, 2)
	if ang != -10*steerTurnGain || lin < 1900 {
		t.Fatalf("10° right: lin=%v ang=%v", lin, ang)
	}
	// Big error: turn rate capped.
	if _, ang = velocityCommand(60, 2); ang != steerMaxTurnDegsPerSec {
		t.Fatalf("60° left: ang=%v, want cap %v", ang, steerMaxTurnDegsPerSec)
	}
	if _, ang = velocityCommand(-60, 2); ang != -steerMaxTurnDegsPerSec {
		t.Fatalf("60° right: ang=%v", ang)
	}
	// Waypoint behind the beam: turn without moving forward.
	if lin, ang = velocityCommand(120, 2); lin != 0 || ang != steerMaxTurnDegsPerSec {
		t.Fatalf("120° left: lin=%v ang=%v", lin, ang)
	}
	if lin, _ = velocityCommand(180, 2); lin != 0 {
		t.Fatalf("dead astern: lin=%v", lin)
	}
}

type velocityCall struct{ lin, ang r3.Vector }

// drivenBase is an inject.Base that records SetVelocity calls and Stops, and
// fails the test on Spin (drive mode must not use it).
func drivenBase(t *testing.T) (*inject.Base, *[]velocityCall, *int) {
	b := inject.NewBase("boat")
	var calls []velocityCall
	stops := 0
	b.SetVelocityFunc = func(ctx context.Context, lin, ang r3.Vector, extra map[string]interface{}) error {
		calls = append(calls, velocityCall{lin, ang})
		return nil
	}
	b.SpinFunc = func(ctx context.Context, angleDeg, degsPerSec float64, extra map[string]interface{}) error {
		t.Error("drive mode called Spin")
		return nil
	}
	b.StopFunc = func(ctx context.Context, extra map[string]interface{}) error {
		stops++
		return nil
	}
	return b, &calls, &stops
}

func TestNavDrivesBaseWithSpeed(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	b, calls, stops := drivenBase(t)
	// boat() reports compass heading 090.
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: boat(&at), base: b, speedMPS: 1.5}

	// Waypoint due east, dead ahead: full speed, no turn.
	startRoute(t, svc, navPoint{Lat: 41.0, Lng: -70.5}, navPoint{Lat: 41.5, Lng: -70.5})
	tick(t, svc)
	if len(*calls) != 1 {
		t.Fatalf("SetVelocity calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	if math.Abs(c.lin.Y-1500) > 1 || math.Abs(c.ang.Z) > 0.5 || c.lin.X != 0 {
		t.Fatalf("dead ahead: %+v", c)
	}

	// Arrive: next waypoint is due north, 90° to the left -> turn left at the
	// cap without driving forward.
	at = navPoint{Lat: 41.0, Lng: -70.5}
	tick(t, svc)
	c = (*calls)[len(*calls)-1]
	if c.lin.Y != 0 || c.ang.Z != steerMaxTurnDegsPerSec {
		t.Fatalf("waypoint abeam to port: %+v", c)
	}

	// Losing the position fix stops the driven base.
	svc.ms.(*inject.MovementSensor).PositionFunc = func(ctx context.Context, extra map[string]interface{}) (*geo.Point, float64, error) {
		return nil, 0, errors.New("no fix")
	}
	n := len(*calls)
	svc.steer(ctx)
	if *stops != 1 || len(*calls) != n {
		t.Fatalf("after fix loss: stops=%d calls %d->%d", *stops, n, len(*calls))
	}
	// Fix back: driving resumes.
	svc.ms = boat(&at)
	svc.steer(ctx)
	if len(*calls) != n+1 {
		t.Fatalf("after fix regained: calls %d->%d", n, len(*calls))
	}

	// Reaching the last waypoint finishes the route and stops the base.
	at = navPoint{Lat: 41.5, Lng: -70.5}
	tick(t, svc)
	if *stops != 2 {
		t.Fatalf("stops after finishing route = %d, want 2", *stops)
	}
}

func TestNavCloseStopsDrivenBase(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	b, calls, stops := drivenBase(t)
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t), ms: boat(&at), base: b, speedMPS: 1}
	startRoute(t, svc, navPoint{Lat: 41.0, Lng: -70.5})
	svc.steer(ctx)
	if len(*calls) != 1 {
		t.Fatalf("SetVelocity calls = %d", len(*calls))
	}
	if err := svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if *stops != 1 {
		t.Fatalf("stops after Close = %d, want 1", *stops)
	}
}

func TestNavConfigSpeed(t *testing.T) {
	if _, _, err := (&NavConfig{MovementSensor: "gps", SpeedMPS: 2}).Validate("nav"); err == nil {
		t.Fatal("expected error: speed_mps without base")
	}
	if _, _, err := (&NavConfig{MovementSensor: "gps", Base: "boat", SpeedMPS: -1}).Validate("nav"); err == nil {
		t.Fatal("expected error: negative speed_mps")
	}
	if _, _, err := (&NavConfig{MovementSensor: "gps", Base: "boat", SpeedMPS: 2}).Validate("nav"); err != nil {
		t.Fatal(err)
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
