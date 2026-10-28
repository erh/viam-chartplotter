package vc

import (
	"context"
	"net"
	"testing"

	"go.viam.com/utils/rpc"

	viamgrpc "go.viam.com/rdk/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
)

// TestNavServiceOverGRPC drives the service through rdk's real navigation
// server and client, so every response is checked against the wire
// conversions (e.g. every GeoPose must carry a heading) the clients see.
func TestNavServiceOverGRPC(t *testing.T) {
	ctx := context.Background()
	logger := logging.NewTestLogger(t)
	at := navPoint{Lat: 41.0, Lng: -71.0}
	svc := testNavService(t)
	svc.ms = boat(&at)

	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := rpc.NewServer(logger, rpc.WithUnauthenticated())
	if err != nil {
		t.Fatal(err)
	}
	name := navigation.Named("nav")
	coll, err := resource.NewAPIResourceCollection(navigation.API, map[resource.Name]navigation.Service{name: svc})
	if err != nil {
		t.Fatal(err)
	}
	reg, ok, err := resource.LookupAPIRegistration[navigation.Service](navigation.API)
	if err != nil || !ok {
		t.Fatalf("navigation API registration: %v %v", ok, err)
	}
	if err := reg.RegisterRPCService(ctx, server, coll, logger); err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Stop()

	conn, err := viamgrpc.Dial(ctx, listener.Addr().String(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := navigation.NewClientFromConn(ctx, conn, "", name, logger)
	if err != nil {
		t.Fatal(err)
	}

	// No goal yet.
	st, err := client.GetNavigationStatus(ctx, nil)
	if err != nil || st.Goal != nil || !st.Localized {
		t.Fatalf("initial status: %v %+v", err, st)
	}

	st, err = client.NavigateRoute(ctx, navigation.NavigateRouteReq{
		Waypoints: inlineWaypoints(navPoint{Lat: 41.1, Lng: -71}, navPoint{Lat: 41.2, Lng: -71}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal == nil || st.Goal.Route == nil || len(st.Goal.Route.Waypoints) != 2 || st.Goal.Progress == nil {
		t.Fatalf("route status over the wire: %+v", st.Goal)
	}
	if _, err := client.Suspend(ctx, "pause", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resume(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if st, err = client.Cancel(ctx, nil); err != nil || st.Goal.State != navigation.GoalStateCanceled {
		t.Fatalf("cancel: %v %+v", err, st.Goal)
	}
	if _, err := client.GetPosition(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddLocation(ctx, navigation.AddLocationReq{Name: "here"}); err != nil {
		t.Fatal(err)
	}
	if locs, err := client.ListLocations(ctx, nil); err != nil || len(locs) != 1 {
		t.Fatalf("ListLocations: %v %+v", err, locs)
	}
	plan, err := client.PlanToLocation(ctx, navigation.PlanToLocationReq{Name: "here"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ExecutePlan(ctx, plan, nil); err != nil {
		t.Fatal(err)
	}
	if props, err := client.GetProperties(ctx); err != nil || props.MapType != navigation.GPSMap || !props.RoutesSupported {
		t.Fatalf("properties: %v %+v", err, props)
	}
	if cm, err := client.GetCostmap(ctx, navigation.CostmapReq{}); err != nil || cm.Available {
		t.Fatalf("costmap: %v %+v", err, cm)
	}
}
