package vc

import (
	"fmt"
	"testing"
)

// Route running due north along -72.0°, one waypoint every ~3 nm.
func rejoinRoute(n int) []RejoinWaypoint {
	wps := make([]RejoinWaypoint, n)
	for i := range wps {
		wps[i] = RejoinWaypoint{ID: fmt.Sprintf("w%d", i), Lat: 41.0 + float64(i)*0.05, Lng: -72.0}
	}
	return wps
}

// runTicks feeds successive boat positions through decideRejoin, carrying
// state like the poller does, and returns the final decision.
func runTicks(t *testing.T, wps []RejoinWaypoint, positions [][2]float64) RejoinDecision {
	t.Helper()
	var state RejoinState
	var d RejoinDecision
	for _, p := range positions {
		d = decideRejoin(p[0], p[1], wps, state)
		state = d.NewState
	}
	return d
}

func TestRejoinBetweenLaterWaypoints(t *testing.T) {
	// Between W2 and W3, advancing north a little each tick.
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.120, -72.0},
		{41.125, -72.0},
		{41.130, -72.0},
	})
	if d.SkipCount != 3 { // W0,W1,W2 behind; W3 next
		t.Fatalf("SkipCount = %d, want 3 (%+v)", d.SkipCount, d)
	}
}

func TestRejoinPicksFurthestLeg(t *testing.T) {
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.220, -72.0},
		{41.225, -72.0},
		{41.230, -72.0},
	})
	if d.SkipCount != 5 {
		t.Fatalf("SkipCount = %d, want 5", d.SkipCount)
	}
}

func TestRejoinNeverFiresOnFirstLeg(t *testing.T) {
	// Between W0 and W1: the normal arrival logic's territory.
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.020, -72.0},
		{41.025, -72.0},
		{41.030, -72.0},
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 on the first leg", d.SkipCount)
	}
}

func TestRejoinRejectsOffTrack(t *testing.T) {
	// ~0.9 nm west of the leg — not "on the route".
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.120, -72.02},
		{41.125, -72.02},
		{41.130, -72.02},
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 off-track", d.SkipCount)
	}
}

func TestRejoinRequiresProgress(t *testing.T) {
	// Drifting on the leg (same fix every tick): between two later
	// waypoints but NOT advancing — must not fire.
	pos := [2]float64{41.125, -72.0}
	d := runTicks(t, rejoinRoute(6), [][2]float64{pos, pos, pos, pos, pos})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 while drifting", d.SkipCount)
	}
}

func TestRejoinRequiresForwardProgress(t *testing.T) {
	// Moving BACKWARD along the leg (southbound on a northbound route).
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.130, -72.0},
		{41.125, -72.0},
		{41.120, -72.0},
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 moving backward", d.SkipCount)
	}
}

func TestRejoinConfirmationWindow(t *testing.T) {
	// Two matching ticks are not enough; the third confirms.
	wps := rejoinRoute(6)
	var state RejoinState
	positions := [][2]float64{{41.120, -72.0}, {41.125, -72.0}, {41.130, -72.0}}
	for i, p := range positions {
		d := decideRejoin(p[0], p[1], wps, state)
		state = d.NewState
		if i < 2 && d.SkipCount != 0 {
			t.Fatalf("tick %d fired early: %+v", i, d)
		}
		if i == 2 && d.SkipCount != 3 {
			t.Fatalf("tick %d: SkipCount = %d, want 3", i, d.SkipCount)
		}
	}
}

func TestRejoinLegChangeResetsCounter(t *testing.T) {
	// Two ticks on one leg, then a jump to another leg: counter restarts,
	// so the third tick (first on the new leg) must not fire.
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.120, -72.0}, // leg W2→W3
		{41.125, -72.0}, // leg W2→W3
		{41.175, -72.0}, // leg W3→W4 — new candidate
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 right after a leg change", d.SkipCount)
	}
}

func TestRejoinNearWaypointStaysAmbiguous(t *testing.T) {
	// Within the edge margin of W3: which-leg is ambiguous, arrival radius
	// territory.
	d := runTicks(t, rejoinRoute(6), [][2]float64{
		{41.1505, -72.0},
		{41.1509, -72.0},
		{41.1512, -72.0},
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 near a waypoint", d.SkipCount)
	}
}

// Regression, from the field (part 4bfe0e5c, Long Island Sound): a ~100 km
// offshore leg. The boat ~2 km past the leg's start is only t≈0.02 along
// it — the original FRACTIONAL edge margin (5%) treated everything within
// 5 km of the waypoint as ambiguous and never fired; fractional progress
// had the same disease (1% of 100 km per window). Real coordinates from
// the deployed route.
func TestRejoinLongOffshoreLeg(t *testing.T) {
	wps := []RejoinWaypoint{
		{ID: "w23", Lat: 40.9662872, Lng: -73.4915713},
		{ID: "w24", Lat: 40.997715, Lng: -73.3542559},
		{ID: "w25", Lat: 41.0342062, Lng: -73.1431587},
		{ID: "w26", Lat: 41.2470329, Lng: -71.986997},
		{ID: "w27", Lat: 41.3198892, Lng: -71.4867542},
	}
	// Boat just east of w25, advancing ENE along the w25→w26 leg
	// (~370 m per 5 s tick ≈ 40 kn is generous; any progress ≥ 20 m works).
	d := runTicks(t, wps, [][2]float64{
		{41.0421496, -73.1219855},
		{41.0428, -73.1180},
		{41.0435, -73.1140},
	})
	if d.SkipCount != 3 { // w23, w24, w25 behind; w26 is next
		t.Fatalf("SkipCount = %d, want 3 (%+v)", d.SkipCount, d)
	}
}

func TestRejoinShortRoute(t *testing.T) {
	d := runTicks(t, rejoinRoute(2), [][2]float64{
		{41.020, -72.0}, {41.025, -72.0}, {41.030, -72.0},
	})
	if d.SkipCount != 0 {
		t.Fatalf("SkipCount = %d, want 0 for a two-waypoint route", d.SkipCount)
	}
}
