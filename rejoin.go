package vc

import "math"

// This file factors the route-REJOIN decision out of the arrival poller,
// mirroring arrival.go's shape (pure function, poller does the I/O).
//
// The arrival rules in arrival.go only ever watch the NEXT waypoint, so a
// boat that joins a route mid-way — loaded a saved route while already
// along it, or diverged and cut back in — keeps every leading waypoint
// forever: the plotter shows it steering for a mark miles behind the beam.
// decideRejoin detects the situation positionally: the boat sits BETWEEN
// two LATER waypoints, close to that leg, and its along-leg progress is
// advancing tick over tick. When that verdict holds for
// rejoinConfirmTicks consecutive poller ticks (~15 s at the 5 s cadence),
// everything behind the boat is marked visited in one go.
//
// Deliberately conservative:
//   - leg 0 (W0→W1) never fires this — advancing past W0 is the normal
//     arrival logic's job, and duplicating it buys nothing;
//   - progress is measured from repeated fixes, not COG, so it needs no
//     extra sensor support and one GPS blip can't fake "moving along the
//     leg";
//   - near a leg's endpoints the which-leg answer is ambiguous, so a
//     margin excludes them — the arrival radius owns those regions.

const (
	// Max perpendicular distance from the leg for "on the route": 0.5 nm.
	rejoinMaxCrossTrackMeters = 926.0
	// Fixed exclusion around each leg endpoint, where which-leg is
	// ambiguous and the arrival rules operate. FIXED distance, not a
	// fraction of the leg: a 5% margin was a 5 km dead zone on a 100 km
	// offshore leg — the real bug found on Long Island Sound, where the
	// boat 2 km past a waypoint (t=0.019 of a 100 km leg) never fired.
	// Matches the arrival logic's "near" zone.
	rejoinEndpointExclusionMeters = nearWaypointMeters
	// Same-leg verdicts required before acting (~15 s at 5 s ticks).
	rejoinConfirmTicks = 3
	// Minimum along-leg advance across the confirmation window, metres —
	// proves the boat is moving TOWARD the leg's end, not drifting.
	rejoinMinAlongProgressMeters = 20.0
)

// RejoinWaypoint is the minimal per-waypoint input: id (to survive list
// edits between ticks) and position in decimal degrees.
type RejoinWaypoint struct {
	ID  string  `json:"id"`
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// RejoinState is the rolling confirmation memory, carried tick to tick and
// reset whenever the candidate leg changes (or no leg matches).
type RejoinState struct {
	// ID of the leg's STARTING waypoint (W[j]); identifies the leg across
	// ticks even if indices shift under an edit.
	LegStartID string `json:"leg_start_id,omitempty"`
	// Metres along the leg on the first and most recent matching tick.
	// Metres, not fractions — fractional progress never confirmed on a
	// long offshore leg (1% of 100 km can't be covered in one window).
	FirstAlongM float64 `json:"first_along_m,omitempty"`
	LastAlongM  float64 `json:"last_along_m,omitempty"`
	Ticks       int     `json:"ticks,omitempty"`
}

// RejoinDecision reports how many leading waypoints to mark visited
// (0 = none), why, and the rolling state for the next tick.
type RejoinDecision struct {
	SkipCount int          `json:"skip_count"`
	Reason    string       `json:"reason,omitempty"`
	NewState  RejoinState  `json:"new_state"`
}

// decideRejoin is the pure decision: boat position, the unvisited
// waypoints in order, and the previous tick's state. A SkipCount of k
// means waypoints[0..k-1] are behind the boat (mark visited); the boat is
// on the leg from waypoints[k-1] to waypoints[k].
func decideRejoin(boatLat, boatLng float64, wps []RejoinWaypoint, prev RejoinState) RejoinDecision {
	if len(wps) < 3 {
		return RejoinDecision{NewState: RejoinState{}}
	}

	// Furthest leg (j >= 1) the boat is strictly between and close to —
	// with a self-crossing route the furthest match is the honest position.
	best := -1
	bestAlongM := 0.0
	for j := 1; j < len(wps)-1; j++ {
		t, xt := legProjection(
			boatLat, boatLng,
			wps[j].Lat, wps[j].Lng,
			wps[j+1].Lat, wps[j+1].Lng,
		)
		if t <= 0 || t >= 1 {
			continue
		}
		if xt > rejoinMaxCrossTrackMeters {
			continue
		}
		// Fixed-distance endpoint exclusion (see the constant's comment).
		if haversineMeters(boatLat, boatLng, wps[j].Lat, wps[j].Lng) < rejoinEndpointExclusionMeters {
			continue
		}
		if haversineMeters(boatLat, boatLng, wps[j+1].Lat, wps[j+1].Lng) < rejoinEndpointExclusionMeters {
			continue
		}
		best = j
		legLen := haversineMeters(wps[j].Lat, wps[j].Lng, wps[j+1].Lat, wps[j+1].Lng)
		bestAlongM = t * legLen
	}
	if best < 0 {
		return RejoinDecision{NewState: RejoinState{}}
	}

	state := prev
	if state.LegStartID != wps[best].ID {
		state = RejoinState{LegStartID: wps[best].ID, FirstAlongM: bestAlongM, LastAlongM: bestAlongM, Ticks: 1}
	} else {
		state.LastAlongM = bestAlongM
		state.Ticks++
	}

	if state.Ticks >= rejoinConfirmTicks && state.LastAlongM >= state.FirstAlongM+rejoinMinAlongProgressMeters {
		return RejoinDecision{
			SkipCount: best + 1, // W[0..best] are behind; W[best+1] is next
			Reason:    "rejoined route mid-way (confirmed between later waypoints, advancing)",
			NewState:  RejoinState{}, // fresh eyes after the list shrinks
		}
	}
	return RejoinDecision{NewState: state}
}

// legProjection projects the boat onto the leg a→b in a local
// equirectangular frame (real metres at leg scale — the same approach the
// clients use for track simplification): returns the along-leg fraction t
// (0 at a, 1 at b, unclamped) and the perpendicular distance in metres.
func legProjection(pLat, pLng, aLat, aLng, bLat, bLng float64) (t, perpMeters float64) {
	const R = 6371008.8
	toRad := math.Pi / 180
	cosLat := math.Cos(aLat * toRad)
	bx := (bLng - aLng) * toRad * cosLat * R
	by := (bLat - aLat) * toRad * R
	px := (pLng - aLng) * toRad * cosLat * R
	py := (pLat - aLat) * toRad * R
	segSq := bx*bx + by*by
	if segSq == 0 {
		return 0, math.Hypot(px, py)
	}
	t = (px*bx + py*by) / segSq
	cx, cy := t*bx, t*by
	return t, math.Hypot(px-cx, py-cy)
}
