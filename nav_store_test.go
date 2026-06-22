package vc

import (
	"context"
	"path/filepath"
	"testing"

	geo "github.com/kellydunn/golang-geo"
)

func tempStore(t *testing.T) *diskNavStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nav.json")
	st, err := newDiskNavStore(path)
	if err != nil {
		t.Fatalf("newDiskNavStore: %v", err)
	}
	return st
}

func TestReplaceWaypointsBasics(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)

	// Seed with a couple of waypoints, mark one visited, so we can prove
	// ReplaceWaypoints discards everything (including visited).
	if _, err := st.AddWaypoint(ctx, geo.NewPoint(1, 1)); err != nil {
		t.Fatal(err)
	}
	seed, err := st.AddWaypoint(ctx, geo.NewPoint(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WaypointVisited(ctx, seed.ID); err != nil {
		t.Fatal(err)
	}

	points := []*geo.Point{geo.NewPoint(41.1, -71.5), geo.NewPoint(41.2, -71.4), geo.NewPoint(41.3, -71.3)}
	count, err := st.ReplaceWaypoints(ctx, points)
	if err != nil {
		t.Fatalf("ReplaceWaypoints: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	wps, err := st.Waypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wps) != 3 {
		t.Fatalf("Waypoints len = %d, want 3", len(wps))
	}
	seen := map[string]bool{}
	for i, wp := range wps {
		if wp.Order != i {
			t.Errorf("waypoint %d Order = %d, want %d", i, wp.Order, i)
		}
		if wp.ID.IsZero() {
			t.Errorf("waypoint %d has zero ID", i)
		}
		if seen[wp.ID.Hex()] {
			t.Errorf("duplicate ID %s", wp.ID.Hex())
		}
		seen[wp.ID.Hex()] = true
		if wp.Visited {
			t.Errorf("waypoint %d should not be visited", i)
		}
	}
	if wps[0].Lat != 41.1 || wps[0].Long != -71.5 {
		t.Errorf("first waypoint = (%v,%v), want (41.1,-71.5)", wps[0].Lat, wps[0].Long)
	}
}

func TestReplaceWaypointsEmptyClears(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)
	if _, err := st.AddWaypoint(ctx, geo.NewPoint(1, 1)); err != nil {
		t.Fatal(err)
	}
	count, err := st.ReplaceWaypoints(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
	wps, err := st.Waypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wps) != 0 {
		t.Fatalf("Waypoints len = %d, want 0", len(wps))
	}
}

func TestReplaceWaypointsRejectsNil(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)
	if _, err := st.AddWaypoint(ctx, geo.NewPoint(5, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceWaypoints(ctx, []*geo.Point{geo.NewPoint(1, 1), nil}); err == nil {
		t.Fatal("expected error on nil point")
	}
	// The existing waypoint must survive a rejected replace.
	wps, err := st.Waypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wps) != 1 {
		t.Fatalf("Waypoints len = %d, want 1 (replace must not partially apply)", len(wps))
	}
}

func TestReplaceWaypointsPersistsAcrossReload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nav.json")
	st, err := newDiskNavStore(path)
	if err != nil {
		t.Fatal(err)
	}
	points := []*geo.Point{geo.NewPoint(10, 20), geo.NewPoint(11, 21)}
	if _, err := st.ReplaceWaypoints(ctx, points); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newDiskNavStore(path)
	if err != nil {
		t.Fatal(err)
	}
	wps, err := reloaded.Waypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wps) != 2 {
		t.Fatalf("reloaded Waypoints len = %d, want 2", len(wps))
	}
	if wps[0].Lat != 10 || wps[1].Lat != 11 {
		t.Errorf("reloaded order/values wrong: %+v", wps)
	}
}

func TestDoSetWaypointsCommand(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)
	svc := &navService{store: st}

	out, err := svc.doSetWaypoints(ctx, map[string]interface{}{
		"waypoints": []interface{}{
			map[string]interface{}{"lat": 41.1, "lng": -71.5},
			map[string]interface{}{"lat": 41.2, "lng": -71.4},
		},
	})
	if err != nil {
		t.Fatalf("doSetWaypoints: %v", err)
	}
	if out["count"] != 2 {
		t.Fatalf("count = %v, want 2", out["count"])
	}
	wps, err := st.Waypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wps) != 2 {
		t.Fatalf("Waypoints len = %d, want 2", len(wps))
	}
}

func TestDoSetWaypointsValidation(t *testing.T) {
	ctx := context.Background()
	st := tempStore(t)
	svc := &navService{store: st}

	cases := []struct {
		name string
		raw  interface{}
	}{
		{"not an object", []interface{}{}},
		{"waypoints not array", map[string]interface{}{"waypoints": 5}},
		{"element not object", map[string]interface{}{"waypoints": []interface{}{5}}},
		{"missing lng", map[string]interface{}{"waypoints": []interface{}{map[string]interface{}{"lat": 1.0}}}},
		{"lat out of range", map[string]interface{}{"waypoints": []interface{}{map[string]interface{}{"lat": 91.0, "lng": 0.0}}}},
		{"lng out of range", map[string]interface{}{"waypoints": []interface{}{map[string]interface{}{"lat": 0.0, "lng": 181.0}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.doSetWaypoints(ctx, tc.raw); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	// An empty/missing waypoints array is valid and clears the route.
	if _, err := svc.doSetWaypoints(ctx, map[string]interface{}{}); err != nil {
		t.Fatalf("empty payload should be valid: %v", err)
	}
}
