package vc

import (
	"context"
	"strings"
	"testing"

	"github.com/pkg/errors"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
)

func mkWaypoints(n int) []navPoint {
	wps := make([]navPoint, n)
	for i := range wps {
		wps[i] = navPoint{Lat: 41.0 + float64(i)*0.01, Lng: -71.5 - float64(i)*0.01}
	}
	return wps
}

func TestN2KRouteMessagesBasic(t *testing.T) {
	opts, err := parseN2KRouteOptions(map[string]interface{}{"route_name": "Block Island Run"})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := n2kRouteMessages(mkWaypoints(2), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 130066 + one 130067, got %d messages", len(msgs))
	}

	attrs := msgs[0]
	if attrs["pgn"] != pgnRouteWPListAttributes || attrs["priority"] != routeWPPriority {
		t.Fatalf("bad attributes envelope: %v", attrs)
	}
	if _, hasDst := attrs["dst"]; hasDst {
		t.Fatalf("dst should be omitted by default: %v", attrs)
	}
	attrFields := attrs["fields"].(map[string]interface{})
	if attrFields["Route/WP-List Name"] != "Block Island Run" {
		t.Fatalf("route name = %v", attrFields["Route/WP-List Name"])
	}
	if attrFields["Number of WPs in the Route/WP-List"] != 2 {
		t.Fatalf("wp count = %v", attrFields["Number of WPs in the Route/WP-List"])
	}
	if attrFields["Database ID"] != defaultN2KDatabase || attrFields["Route ID"] != defaultN2KRoute {
		t.Fatalf("bad db/route ids: %v", attrFields)
	}

	wpMsg := msgs[1]
	if wpMsg["pgn"] != pgnRouteWPNamePosition {
		t.Fatalf("second message pgn = %v", wpMsg["pgn"])
	}
	wpFields := wpMsg["fields"].(map[string]interface{})
	if wpFields["Start RPS#"] != 0 || wpFields["nItems"] != 2 {
		t.Fatalf("bad chunk header: %v", wpFields)
	}
	list := wpFields["list"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("list len = %d", len(list))
	}
	first := list[0].(map[string]interface{})
	if first["WP ID"] != 0 || first["WP Name"] != "WP001" {
		t.Fatalf("bad first waypoint: %v", first)
	}
	if first["WP Latitude"] != 41.0 || first["WP Longitude"] != -71.5 {
		t.Fatalf("bad first position: %v", first)
	}
}

func TestN2KRouteMessagesChunking(t *testing.T) {
	const total = 40
	opts, _ := parseN2KRouteOptions(nil)
	msgs, err := n2kRouteMessages(mkWaypoints(total), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("expected %d waypoints to need multiple 130067 chunks, got %d messages", total, len(msgs))
	}

	seen := 0
	for _, msg := range msgs[1:] {
		fields := msg["fields"].(map[string]interface{})
		if fields["Start RPS#"] != seen {
			t.Fatalf("chunk starts at %v, want %d", fields["Start RPS#"], seen)
		}
		if fields["Number of WPs in the Route/WP-List"] != total {
			t.Fatalf("chunk total = %v", fields["Number of WPs in the Route/WP-List"])
		}
		list := fields["list"].([]interface{})
		if len(list) != fields["nItems"] {
			t.Fatalf("nItems %v != list len %d", fields["nItems"], len(list))
		}

		// Every chunk must fit one fast-packet: header + per-wp size.
		size := wpListHeaderBytes
		for i, item := range list {
			wp := item.(map[string]interface{})
			if wp["WP ID"] != seen+i {
				t.Fatalf("wp id %v, want %d", wp["WP ID"], seen+i)
			}
			size += wpFixedBytes + len(wp["WP Name"].(string))
		}
		if size > fastPacketMaxBytes {
			t.Fatalf("chunk marshals to %d bytes > fast-packet max %d", size, fastPacketMaxBytes)
		}
		seen += len(list)
	}
	if seen != total {
		t.Fatalf("chunks cover %d waypoints, want %d", seen, total)
	}
}

func TestN2KRouteMessagesLimits(t *testing.T) {
	opts, _ := parseN2KRouteOptions(nil)
	if _, err := n2kRouteMessages(nil, opts); err == nil {
		t.Fatal("expected error for empty waypoint list")
	}
	if _, err := n2kRouteMessages(mkWaypoints(maxN2KWaypoints+1), opts); err == nil {
		t.Fatal("expected error for oversized waypoint list")
	}
}

func TestParseN2KRouteOptions(t *testing.T) {
	// gRPC delivers numbers as float64; dst must flow through to the envelope.
	opts, err := parseN2KRouteOptions(map[string]interface{}{
		"database_id": 3.0, "route_id": 7.0, "dst": 42.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.DatabaseID != 3 || opts.RouteID != 7 || opts.Dst != 42 {
		t.Fatalf("parsed opts = %+v", opts)
	}
	cmd := n2kSenderCmd(pgnRouteWPListAttributes, opts, map[string]interface{}{})
	if cmd["dst"] != 42 {
		t.Fatalf("dst not forwarded: %v", cmd)
	}

	if _, err := parseN2KRouteOptions(map[string]interface{}{"dst": "everyone"}); err == nil {
		t.Fatal("expected type error for string dst")
	}
	if _, err := parseN2KRouteOptions(map[string]interface{}{"route_id": 999.0}); err == nil {
		t.Fatal("expected range error for route_id")
	}

	// Non-object payloads (e.g. `true`) mean "all defaults".
	opts, err = parseN2KRouteOptions(true)
	if err != nil {
		t.Fatal(err)
	}
	if opts.RouteName != defaultN2KRouteName || opts.Dst != -1 {
		t.Fatalf("default opts = %+v", opts)
	}
}

// fakeN2KSender is a stand-in for the viamboat sender component: it records
// every DoCommand payload.
type fakeN2KSender struct {
	resource.AlwaysRebuild
	cmds []map[string]interface{}
	err  error
}

func (f *fakeN2KSender) Name() resource.Name { return resource.Name{Name: "fake-sender"} }
func (f *fakeN2KSender) Close(context.Context) error {
	return nil
}
func (f *fakeN2KSender) Status(context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (f *fakeN2KSender) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.cmds = append(f.cmds, cmd)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]interface{}{"sent": true}, nil
}

func wpPayload(pts ...navPoint) []interface{} {
	out := make([]interface{}, len(pts))
	for i, p := range pts {
		out[i] = map[string]interface{}{"lat": p.Lat, "lng": p.Lng}
	}
	return out
}

func TestN2KBaseSendWaypoints(t *testing.T) {
	ctx := context.Background()
	sender := &fakeN2KSender{}
	b := &n2kBase{logger: logging.NewTestLogger(t), sender: sender, dst: 9}

	out, err := b.DoCommand(ctx, map[string]interface{}{
		"send_waypoints": map[string]interface{}{
			"route_name": "Test Route",
			"waypoints":  wpPayload(navPoint{Lat: 41.1631, Lng: -71.5784}, navPoint{Lat: 41.2042, Lng: -71.5511}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["waypoints"] != 2 || out["messages"] != 2 {
		t.Fatalf("unexpected response: %v", out)
	}
	if len(sender.cmds) != 2 {
		t.Fatalf("sender got %d commands, want 2", len(sender.cmds))
	}
	if sender.cmds[0]["pgn"] != pgnRouteWPListAttributes || sender.cmds[1]["pgn"] != pgnRouteWPNamePosition {
		t.Fatalf("wrong pgn order: %v, %v", sender.cmds[0]["pgn"], sender.cmds[1]["pgn"])
	}
	// The base's configured dst applies when the payload doesn't set one.
	if sender.cmds[0]["dst"] != 9 {
		t.Fatalf("dst = %v, want base default 9", sender.cmds[0]["dst"])
	}
}

func TestN2KBaseSendWaypointsErrors(t *testing.T) {
	ctx := context.Background()
	b := &n2kBase{logger: logging.NewTestLogger(t), sender: &fakeN2KSender{}, dst: -1}

	if _, err := b.sendWaypoints(ctx, map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "waypoints is required") {
		t.Fatalf("expected missing-waypoints error, got %v", err)
	}
	if _, err := b.sendWaypoints(ctx, map[string]interface{}{"waypoints": []interface{}{}}); err == nil || !strings.Contains(err.Error(), "no waypoints") {
		t.Fatalf("expected empty-list error, got %v", err)
	}
	if _, err := b.sendWaypoints(ctx, map[string]interface{}{"waypoints": []interface{}{map[string]interface{}{"lat": 1.0}}}); err == nil {
		t.Fatal("expected error for waypoint without lng")
	}

	// Sender failure surfaces with message context.
	b = &n2kBase{logger: logging.NewTestLogger(t), sender: &fakeN2KSender{err: errors.New("bus offline")}, dst: -1}
	_, err := b.sendWaypoints(ctx, map[string]interface{}{"waypoints": wpPayload(navPoint{Lat: 1, Lng: 1})})
	if err == nil || !strings.Contains(err.Error(), "bus offline") {
		t.Fatalf("expected sender error, got %v", err)
	}
}

// compass is a movement sensor that only reports *heading.
func compass(heading *float64) *inject.MovementSensor {
	ms := inject.NewMovementSensor("compass")
	ms.CompassHeadingFunc = func(ctx context.Context, extra map[string]interface{}) (float64, error) {
		return *heading, nil
	}
	return ms
}

func TestN2KBaseSpin(t *testing.T) {
	ctx := context.Background()
	sender := &fakeN2KSender{}
	cur := 10.0
	b := &n2kBase{logger: logging.NewTestLogger(t), sender: sender, ms: compass(&cur), dst: -1}

	if moving, _ := b.IsMoving(ctx); moving {
		t.Fatal("fresh base should not be moving")
	}
	// Spin +30 (left) from 010 -> hold 340.
	if err := b.Spin(ctx, 30, 5, nil); err != nil {
		t.Fatal(err)
	}
	if len(sender.cmds) != 1 {
		t.Fatalf("sender got %d commands, want 1", len(sender.cmds))
	}
	cmd := sender.cmds[0]
	if cmd["pgn"] != pgnHeadingTrackControl || cmd["priority"] != headingPriority {
		t.Fatalf("bad envelope: %v", cmd)
	}
	if _, hasDst := cmd["dst"]; hasDst {
		t.Fatalf("dst should be omitted when broadcasting: %v", cmd)
	}
	fields := cmd["fields"].(map[string]interface{})
	if fields["Heading-To-Steer (Course)"] != 340.0 || fields["Heading Reference"] != "True" {
		t.Fatalf("bad fields: %v", fields)
	}
	if moving, _ := b.IsMoving(ctx); !moving {
		t.Fatal("base should be moving after Spin")
	}

	// Spin -90 (right) from 300 -> hold 030.
	cur = 300
	if err := b.Spin(ctx, -90, 5, nil); err != nil {
		t.Fatal(err)
	}
	if got := sender.cmds[1]["fields"].(map[string]interface{})["Heading-To-Steer (Course)"]; got != 30.0 {
		t.Fatalf("heading = %v, want 30", got)
	}

	// Spin at 0 deg/s is Stop; Stop forgets the heading and sends nothing.
	if err := b.Spin(ctx, 45, 0, nil); err != nil {
		t.Fatal(err)
	}
	if moving, _ := b.IsMoving(ctx); moving || len(sender.cmds) != 2 {
		t.Fatalf("after Spin(rate 0): moving=%v, sender cmds=%d", moving, len(sender.cmds))
	}

	// No compass heading -> error, nothing sent.
	b.ms.(*inject.MovementSensor).CompassHeadingFunc = func(ctx context.Context, extra map[string]interface{}) (float64, error) {
		return 0, errors.New("no heading")
	}
	if err := b.Spin(ctx, 10, 5, nil); err == nil || len(sender.cmds) != 2 {
		t.Fatalf("expected error and no command, got err=%v cmds=%d", err, len(sender.cmds))
	}
	if err := b.MoveStraight(ctx, 1000, 100, nil); err == nil {
		t.Fatal("MoveStraight should be unsupported")
	}
}

func TestN2KBaseConfigValidate(t *testing.T) {
	if _, _, err := (&N2KBaseConfig{MovementSensor: "gps"}).Validate("b"); err == nil {
		t.Fatal("expected sender-required error")
	}
	if _, _, err := (&N2KBaseConfig{Sender: "n2k-sender"}).Validate("b"); err == nil {
		t.Fatal("expected movement_sensor-required error")
	}
	deps, _, err := (&N2KBaseConfig{Sender: "n2k-sender", MovementSensor: "gps"}).Validate("b")
	if err != nil || len(deps) != 2 {
		t.Fatalf("deps=%v err=%v", deps, err)
	}
	bad := 300
	if _, _, err := (&N2KBaseConfig{Sender: "s", MovementSensor: "gps", Dst: &bad}).Validate("b"); err == nil {
		t.Fatal("expected dst range error")
	}
}
