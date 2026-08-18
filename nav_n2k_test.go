package vc

import (
	"context"
	"strings"
	"testing"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
)

func mkWaypoints(n int) []navigation.Waypoint {
	wps := make([]navigation.Waypoint, n)
	for i := range wps {
		wps[i] = navigation.Waypoint{Lat: 41.0 + float64(i)*0.01, Long: -71.5 - float64(i)*0.01}
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

func TestDoSendWaypointsN2K(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)
	if _, err := st.ReplaceWaypoints(ctx, []*geo.Point{
		geo.NewPoint(41.1631, -71.5784),
		geo.NewPoint(41.2042, -71.5511),
	}); err != nil {
		t.Fatal(err)
	}

	sender := &fakeN2KSender{}
	svc := &navService{logger: logging.NewTestLogger(t), store: st, n2kSender: sender}

	out, err := svc.DoCommand(ctx, map[string]interface{}{
		"send_waypoints_n2k": map[string]interface{}{"route_name": "Test Route"},
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
}

func TestDoSendWaypointsN2KErrors(t *testing.T) {
	ctx := context.Background()

	// No sender configured.
	svc := &navService{logger: logging.NewTestLogger(t), store: tempStore(t)}
	if _, err := svc.doSendWaypointsN2K(ctx, true); err == nil || !strings.Contains(err.Error(), "n2k_sender") {
		t.Fatalf("expected no-sender error, got %v", err)
	}

	// Empty waypoint list.
	svc = &navService{logger: logging.NewTestLogger(t), store: tempStore(t), n2kSender: &fakeN2KSender{}}
	if _, err := svc.doSendWaypointsN2K(ctx, true); err == nil || !strings.Contains(err.Error(), "no waypoints") {
		t.Fatalf("expected empty-list error, got %v", err)
	}

	// Sender failure surfaces with message context.
	st := tempStore(t)
	if _, err := st.ReplaceWaypoints(ctx, []*geo.Point{geo.NewPoint(1, 1)}); err != nil {
		t.Fatal(err)
	}
	svc = &navService{
		logger:    logging.NewTestLogger(t),
		store:     st,
		n2kSender: &fakeN2KSender{err: errors.New("bus offline")},
	}
	if _, err := svc.doSendWaypointsN2K(ctx, true); err == nil || !strings.Contains(err.Error(), "bus offline") {
		t.Fatalf("expected sender error, got %v", err)
	}
}
