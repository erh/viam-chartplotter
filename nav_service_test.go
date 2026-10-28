package vc

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	geo "github.com/kellydunn/golang-geo"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/navigation"
	"go.viam.com/rdk/testutils/inject"
)

func pos(lat, lng float64) navigation.Position { return navPoint{Lat: lat, Lng: lng}.position() }

func inlineWaypoints(pts ...navPoint) []navigation.Waypoint { return positionWaypoints(pts) }

// startRoute makes pts the service's active route goal.
func startRoute(t *testing.T, svc *navService, pts ...navPoint) navigation.Status {
	t.Helper()
	st, err := svc.NavigateRoute(context.Background(), navigation.NavigateRouteReq{Waypoints: inlineWaypoints(pts...)})
	if err != nil {
		t.Fatalf("NavigateRoute: %v", err)
	}
	return st
}

// boat is a movement sensor parked at *at.
func boat(at *navPoint) *inject.MovementSensor {
	ms := inject.NewMovementSensor("gps")
	ms.PositionFunc = func(ctx context.Context, extra map[string]interface{}) (*geo.Point, float64, error) {
		return geo.NewPoint(at.Lat, at.Lng), 0, nil
	}
	ms.CompassHeadingFunc = func(ctx context.Context, extra map[string]interface{}) (float64, error) { return 90, nil }
	ms.LinearVelocityFunc = func(ctx context.Context, extra map[string]interface{}) (r3.Vector, error) {
		return r3.Vector{X: 3, Y: 4}, nil
	}
	return ms
}

func testNavService(t *testing.T) *navService {
	return &navService{logger: logging.NewTestLogger(t), store: tempStore(t)}
}

func TestNavigateRouteStatus(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	svc := testNavService(t)
	svc.ms = boat(&at)

	st, err := svc.NavigateRoute(ctx, navigation.NavigateRouteReq{
		Waypoints:  inlineWaypoints(navPoint{Lat: 41.1, Lng: -71}, navPoint{Lat: 41.2, Lng: -71}, navPoint{Lat: 41.3, Lng: -71}),
		StartIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Localized || st.Position.GeoPose.Location.Lat() != 41.0 || *st.Position.GeoPose.Heading != 90 {
		t.Fatalf("bad position: %+v", st.Position.GeoPose)
	}
	if st.Velocity.LinearMPerSec != 5 {
		t.Fatalf("velocity = %v, want 5", st.Velocity.LinearMPerSec)
	}
	g := st.Goal
	if g == nil || g.State != navigation.GoalStateActive || g.Mode != navigation.NavModeRoute {
		t.Fatalf("bad goal: %+v", g)
	}
	if g.Route == nil || g.Route.Index != 1 || len(g.Route.Waypoints) != 3 {
		t.Fatalf("bad route progress: %+v", g.Route)
	}
	if g.Target.GeoPose.Location.Lat() != 41.2 {
		t.Fatalf("target = %v, want the waypoint at the cursor", g.Target.GeoPose.Location)
	}
	// Boat → 41.2 → 41.3: 0.3° of latitude.
	if g.Progress == nil || len(g.Progress.Path) != 3 || g.Progress.DistanceRemainingM < 33000 || g.Progress.DistanceRemainingM > 34000 {
		t.Fatalf("bad progress: %+v", g.Progress)
	}
	if got := remainingGoalWaypoints(g); len(got) != 2 || got[0].Index != 1 || got[1].Lat != 41.3 {
		t.Fatalf("remainingGoalWaypoints = %+v", got)
	}
}

func TestNavigateRouteValidation(t *testing.T) {
	ctx := context.Background()
	svc := testNavService(t)
	cases := map[string]navigation.NavigateRouteReq{
		"empty":       {},
		"both":        {Name: "x", Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1})},
		"start index": {Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1}), StartIndex: 1},
		"range":       {Waypoints: inlineWaypoints(navPoint{Lat: 91, Lng: 1})},
		"no position": {Waypoints: []navigation.Waypoint{{}}},
		"unknown loc": {Waypoints: []navigation.Waypoint{{LocationName: "nope"}}},
		"nearest/fix": {Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1}), StartNearest: true},
	}
	for name, req := range cases {
		if _, err := svc.NavigateRoute(ctx, req); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNavigateRouteStartNearest(t *testing.T) {
	at := navPoint{Lat: 41.19, Lng: -71}
	svc := testNavService(t)
	svc.ms = boat(&at)
	st, err := svc.NavigateRoute(context.Background(), navigation.NavigateRouteReq{
		Waypoints:    inlineWaypoints(navPoint{Lat: 41.1, Lng: -71}, navPoint{Lat: 41.2, Lng: -71}, navPoint{Lat: 41.3, Lng: -71}),
		StartNearest: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal.Route.Index != 1 {
		t.Fatalf("index = %d, want 1", st.Goal.Route.Index)
	}
}

func TestNavigateRouteFromSavedRoute(t *testing.T) {
	loc := newFakeMetaStore(nil)
	svc := newTestNav(t, loc, nil)
	svc.store = tempStore(t)
	if err := routesSave(context.Background(), loc, mkRouteRec()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Test", "rte_a"} {
		st, err := svc.NavigateRoute(context.Background(), navigation.NavigateRouteReq{Name: key})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if st.Goal.Route.Name != "Test" || len(st.Goal.Route.Waypoints) != 2 {
			t.Fatalf("%s: bad route progress %+v", key, st.Goal.Route)
		}
	}
	if _, err := svc.NavigateRoute(context.Background(), navigation.NavigateRouteReq{Name: "missing"}); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestSuspendResumeCancel(t *testing.T) {
	ctx := context.Background()
	svc := testNavService(t)
	if _, err := svc.Suspend(ctx, "x", nil); err == nil {
		t.Fatal("suspend with no goal should fail")
	}
	startRoute(t, svc, navPoint{Lat: 1, Lng: 1})

	st, err := svc.Suspend(ctx, "anchored", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal.State != navigation.GoalStateSuspended || *st.Goal.SuspendReason != "anchored" || st.Goal.Progress != nil {
		t.Fatalf("bad suspended goal: %+v", st.Goal)
	}
	if got := remainingGoalWaypoints(st.Goal); len(got) != 1 {
		t.Fatalf("a suspended route keeps its waypoints, got %+v", got)
	}
	if _, err := svc.NavigateRoute(ctx, navigation.NavigateRouteReq{Waypoints: inlineWaypoints(navPoint{Lat: 2, Lng: 2})}); err == nil ||
		!strings.Contains(err.Error(), "suspended") {
		t.Fatalf("new goal over a suspended one: %v", err)
	}

	if st, err = svc.Resume(ctx, nil); err != nil || st.Goal.State != navigation.GoalStateActive {
		t.Fatalf("resume: %v %+v", err, st.Goal)
	}
	if st, err = svc.Cancel(ctx, nil); err != nil || st.Goal.State != navigation.GoalStateCanceled || st.Goal.FinishedAt == nil {
		t.Fatalf("cancel: %v %+v", err, st.Goal)
	}
	if got := remainingGoalWaypoints(st.Goal); len(got) != 0 {
		t.Fatalf("canceled goal has waypoints: %+v", got)
	}
	// Cancel with nothing open is a no-op, not an error.
	if _, err := svc.Cancel(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestArrivalAdvancesGoal(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	svc := testNavService(t)
	svc.ms = boat(&at)
	startRoute(t, svc, navPoint{Lat: 41.0, Lng: -71.0}, navPoint{Lat: 41.1, Lng: -71.0})

	svc.checkArrival(ctx) // sitting on waypoint 0
	g := svc.store.Goal()
	if g.Index != 1 || g.State != goalActive {
		t.Fatalf("after first arrival: %+v", g)
	}
	at = navPoint{Lat: 41.1, Lng: -71.0}
	svc.checkArrival(ctx)
	g = svc.store.Goal()
	if g.Index != 2 || g.State != goalSucceeded {
		t.Fatalf("after last arrival: %+v", g)
	}

	// A suspended goal doesn't advance.
	startRoute(t, svc, navPoint{Lat: 41.1, Lng: -71.0})
	if _, err := svc.Suspend(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	svc.checkArrival(ctx)
	if g := svc.store.Goal(); g.Index != 0 || g.State != goalSuspended {
		t.Fatalf("suspended goal advanced: %+v", g)
	}
}

func TestAdvanceGoalIgnoresReplacedGoal(t *testing.T) {
	svc := testNavService(t)
	startRoute(t, svc, navPoint{Lat: 1, Lng: 1}, navPoint{Lat: 2, Lng: 2})
	stale := svc.store.Goal()
	startRoute(t, svc, navPoint{Lat: 3, Lng: 3}, navPoint{Lat: 4, Lng: 4})
	applied, err := svc.advanceGoal(stale.StartedAt, stale.Index, 1)
	if err != nil || applied {
		t.Fatalf("stale advance applied=%v err=%v", applied, err)
	}
	if g := svc.store.Goal(); g.Index != 0 || g.Waypoints[0].Lat != 3 {
		t.Fatalf("goal clobbered: %+v", g)
	}
}

func TestSinglePointGoals(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	svc := testNavService(t)
	svc.ms = boat(&at)
	if _, err := svc.AddLocation(ctx, navigation.AddLocationReq{Name: "mooring", Position: &navigation.Position{
		GeoPose: &navigation.GeoPose{Location: geo.NewPoint(41.5, -71.5), Heading: new(float64)},
	}}); err != nil {
		t.Fatal(err)
	}

	st, err := svc.NavigateToLocation(ctx, "mooring", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal.Mode != navigation.NavModePlanned || *st.Goal.Location != "mooring" || st.Goal.Route != nil {
		t.Fatalf("bad location goal: %+v", st.Goal)
	}
	if got := remainingGoalWaypoints(st.Goal); len(got) != 1 || got[0].Lat != 41.5 {
		t.Fatalf("target-only goal waypoints = %+v", got)
	}

	st, err = svc.GoToPoint(ctx, navigation.GoToPointReq{Destination: pos(41.2, -71.2)})
	if err != nil || st.Goal.Mode != navigation.NavModeDirect {
		t.Fatalf("GoToPoint: %v %+v", err, st.Goal)
	}

	plan, err := svc.PlanToLocation(ctx, navigation.PlanToLocationReq{Name: "mooring"})
	if err != nil || len(plan.Path) != 2 || plan.LengthM <= 0 || *plan.Location != "mooring" {
		t.Fatalf("PlanToLocation: %v %+v", err, plan)
	}
	st, err = svc.ExecutePlan(ctx, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := remainingGoalWaypoints(st.Goal); len(got) != 1 || got[0].Lat != 41.5 {
		t.Fatalf("ExecutePlan should skip the start point: %+v", got)
	}
}

func TestLocations(t *testing.T) {
	ctx := context.Background()
	at := navPoint{Lat: 41.0, Lng: -71.0}
	svc := testNavService(t)

	if _, err := svc.AddLocation(ctx, navigation.AddLocationReq{Name: "here"}); err == nil {
		t.Fatal("AddLocation without a position or fix should fail")
	}
	svc.ms = boat(&at)
	loc, err := svc.AddLocation(ctx, navigation.AddLocationReq{Name: "here"})
	if err != nil || loc.Position.GeoPose.Location.Lat() != 41.0 || *loc.Position.GeoPose.Heading != 90 {
		t.Fatalf("AddLocation at fix: %v %+v", err, loc)
	}
	if _, err := svc.AddLocation(ctx, navigation.AddLocationReq{Name: "here"}); err == nil {
		t.Fatal("duplicate name should fail")
	}
	if _, err := svc.NavigateToLocation(ctx, "here", nil); err != nil {
		t.Fatal(err)
	}

	newName := "there"
	p := pos(42, -72)
	if _, err := svc.UpdateLocation(ctx, navigation.UpdateLocationReq{Name: "here", NewName: &newName, Position: &p}); err != nil {
		t.Fatal(err)
	}
	if g := svc.store.Goal(); g.Location != "there" {
		t.Fatalf("goal should follow the rename, got %q", g.Location)
	}
	got, err := svc.GetLocation(ctx, "there", nil)
	if err != nil || got.Position.GeoPose.Location.Lat() != 42 {
		t.Fatalf("GetLocation: %v %+v", err, got)
	}
	if locs, _ := svc.ListLocations(ctx, nil); len(locs) != 1 {
		t.Fatalf("ListLocations = %d", len(locs))
	}
	if err := svc.DeleteLocation(ctx, "there", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteLocation(ctx, "there", nil); err == nil {
		t.Fatal("deleting a missing location should fail")
	}
}

func TestRouteRPCs(t *testing.T) {
	ctx := context.Background()
	loc := newFakeMetaStore(nil)
	parent := newFakeMetaStore(nil)
	if err := routesSave(ctx, parent, routeRec{
		ID: "rte_p", Name: "Inherited", Source: "manual", CreatedAt: "x", UpdatedAt: "x",
		Waypoints: []routeWaypoint{{Lat: 1, Lng: 1}, {Lat: 2, Lng: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	svc := newTestNav(t, loc, nil)
	svc.store = tempStore(t)
	svc.parentStoresFn = func(context.Context) ([]metadataStore, error) { return []metadataStore{parent}, nil }

	r, err := svc.AddRoute(ctx, navigation.AddRouteReq{
		Name:      "Run",
		Waypoints: inlineWaypoints(navPoint{Lat: 41, Lng: -71}, navPoint{Lat: 42, Lng: -72}),
		Extra:     map[string]interface{}{"color": "#123456"},
	})
	if err != nil || r.Name != "Run" || len(r.Waypoints) != 2 {
		t.Fatalf("AddRoute: %v %+v", err, r)
	}
	recs, _ := routesList(ctx, loc)
	if len(recs) != 1 || !strings.HasPrefix(recs[0].ID, "rte_") || recs[0].Color != "#123456" || recs[0].Source != "manual" {
		t.Fatalf("stored record: %+v", recs)
	}
	if _, err := svc.AddRoute(ctx, navigation.AddRouteReq{Name: "Run", Waypoints: r.Waypoints}); err == nil {
		t.Fatal("duplicate route name should fail")
	}
	if _, err := svc.AddRoute(ctx, navigation.AddRouteReq{Name: "One", Waypoints: inlineWaypoints(navPoint{Lat: 1, Lng: 1})}); err == nil {
		t.Fatal("a one-waypoint route should fail validation")
	}

	all, err := svc.ListRoutes(ctx, nil)
	if err != nil || len(all) != 2 || all[0].Name != "Run" || all[1].Name != "Inherited" {
		t.Fatalf("ListRoutes: %v %+v", err, all)
	}
	if got, err := svc.GetRoute(ctx, "Inherited", nil); err != nil || len(got.Waypoints) != 2 {
		t.Fatalf("GetRoute inherited: %v", err)
	}

	newName := "Run 2"
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{Name: "Run", NewName: &newName}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRoute(ctx, navigation.UpdateRouteReq{Name: "Inherited", NewName: &newName}); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("updating an inherited route: %v", err)
	}
	if err := svc.DeleteRoute(ctx, "Run 2", nil); err != nil {
		t.Fatal(err)
	}
	if recs, _ := routesList(ctx, loc); len(recs) != 0 {
		t.Fatalf("route not deleted: %+v", recs)
	}
}

func TestPointFromPositionRejectsPose(t *testing.T) {
	if _, err := pointFromPosition(navigation.Position{}); err == nil {
		t.Fatal("empty position should fail")
	}
}
