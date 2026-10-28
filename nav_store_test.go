package vc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestNavStorePersistsAcrossReload(t *testing.T) {
	st := tempStore(t)
	err := st.mutate(true, func(s *navState) error {
		s.Locations = append(s.Locations, savedLocation{Name: "home", navPoint: navPoint{Lat: 41, Lng: -71}})
		s.Goal = &navGoal{
			Mode:      goalModeRoute,
			RouteName: "Run",
			Waypoints: []navPoint{{Lat: 1, Lng: 2}, {Lat: 3, Lng: 4}},
			Index:     1,
			State:     goalActive,
			StartedAt: time.Now(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := newDiskNavStore(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if loc, ok := again.LocationByName("home"); !ok || loc.Lat != 41 {
		t.Fatalf("location not persisted: %+v %v", loc, ok)
	}
	g := again.Goal()
	if g == nil || g.RouteName != "Run" || g.Index != 1 || len(g.Waypoints) != 2 || g.State != goalActive {
		t.Fatalf("goal not persisted: %+v", g)
	}
}

func TestNavStoreMutateRollsBackOnError(t *testing.T) {
	st := tempStore(t)
	_ = st.mutate(true, func(s *navState) error {
		s.Locations = []savedLocation{{Name: "a"}}
		return nil
	})
	err := st.mutate(true, func(s *navState) error {
		s.Locations = append(s.Locations, savedLocation{Name: "b"})
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := len(st.Locations()); n != 1 {
		t.Fatalf("rollback failed: %d locations", n)
	}
}

func TestNavStoreGoalIsACopy(t *testing.T) {
	st := tempStore(t)
	_ = st.mutate(true, func(s *navState) error {
		s.Goal = &navGoal{Waypoints: []navPoint{{Lat: 1, Lng: 1}}, State: goalActive}
		return nil
	})
	g := st.Goal()
	g.Waypoints[0].Lat = 99
	g.Index = 5
	if again := st.Goal(); again.Waypoints[0].Lat != 1 || again.Index != 0 {
		t.Fatalf("Goal() leaked internal state: %+v", again)
	}
}

func TestNavStoreMigratesLegacyWaypointList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.json")
	legacy := `[
	  {"id": "aaaaaaaaaaaaaaaaaaaaaaaa", "lat": 1, "long": 1, "visited": true},
	  {"id": "bbbbbbbbbbbbbbbbbbbbbbbb", "lat": 2, "long": 2},
	  {"id": "cccccccccccccccccccccccc", "lat": 3, "long": 3, "visited": true},
	  {"id": "dddddddddddddddddddddddd", "lat": 4, "long": 4}
	]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := newDiskNavStore(path)
	if err != nil {
		t.Fatal(err)
	}
	g := st.Goal()
	if g == nil || g.State != goalActive || g.Mode != goalModeRoute {
		t.Fatalf("expected an active route goal, got %+v", g)
	}
	// The leading visited waypoint is the reached prefix; the visited one
	// after the cursor (hidden by the old store) is dropped.
	if g.Index != 1 || len(g.Waypoints) != 3 || g.Waypoints[1].Lat != 2 || g.Waypoints[2].Lat != 4 {
		t.Fatalf("bad migration: %+v", g)
	}
}

func TestNavStoreLegacyAllVisitedIsNoGoal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.json")
	_ = os.WriteFile(path, []byte(`[{"id":"x","lat":1,"long":1,"visited":true}]`), 0o644)
	st, err := newDiskNavStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if g := st.Goal(); g != nil {
		t.Fatalf("expected no goal, got %+v", g)
	}
}

func TestNavGoalAdvance(t *testing.T) {
	now := time.Now()
	g := &navGoal{Waypoints: []navPoint{{}, {}, {}}, State: goalActive}
	g.advance(1, now)
	if g.Index != 1 || g.State != goalActive {
		t.Fatalf("after 1: %+v", g)
	}
	g.advance(2, now)
	if g.Index != 3 || g.State != goalSucceeded || g.FinishedAt == nil {
		t.Fatalf("after end: %+v", g)
	}
	if len(g.remaining()) != 0 {
		t.Fatal("finished goal has remaining waypoints")
	}

	loop := &navGoal{Waypoints: []navPoint{{}, {}}, State: goalActive, Loop: true}
	loop.advance(2, now)
	if loop.Index != 0 || loop.Lap != 1 || loop.State != goalActive {
		t.Fatalf("loop should wrap: %+v", loop)
	}
}

func TestNavStoreSnapshotOnUserMutation(t *testing.T) {
	st := tempStore(t)
	_ = st.mutate(true, func(s *navState) error { s.Locations = []savedLocation{{Name: "a"}}; return nil })
	// The first write has nothing to back up; the second snapshots it.
	_ = st.mutate(true, func(s *navState) error { s.Locations = nil; return nil })
	_ = st.mutate(false, func(s *navState) error { s.Locations = []savedLocation{{Name: "c"}}; return nil })
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(st.path), "nav.*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 snapshot, got %v", matches)
	}
}
