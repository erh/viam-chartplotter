package vc

import (
	"math"
)

// This file factors the arrival decision out of nav.go's poller into a
// pure function (decideArrival) so it can be unit-tested against canned
// scenarios. The poller is the only runtime caller; tests in
// arrival_test.go drive the same function directly with fixtures from
// testdata/arrival/*.json.
//
// Field names use Capital + json tags throughout so the same structs
// serialise to the log line emitted by checkArrival AND deserialise from
// the test fixture files. That's how copy-paste-from-log-to-test works.

// ArrivalInput is the per-tick input to decideArrival: positions of the
// boat, the active waypoint, and (optionally) the next waypoint after
// that. All lat/lng values are decimal degrees.
type ArrivalInput struct {
	BoatLat              float64 `json:"boat_lat"`
	BoatLng              float64 `json:"boat_lng"`
	WaypointID           string  `json:"waypoint_id"`
	WaypointLat          float64 `json:"waypoint_lat"`
	WaypointLng          float64 `json:"waypoint_lng"`
	HasFollowing         bool    `json:"has_following"`
	FollowingWaypointID  string  `json:"following_waypoint_id,omitempty"`
	FollowingWaypointLat float64 `json:"following_waypoint_lat,omitempty"`
	FollowingWaypointLng float64 `json:"following_waypoint_lng,omitempty"`
}

// ArrivalState is the rolling per-active-pair memory carried tick to
// tick: the closest the boat has been to the active waypoint and the
// one following it on this approach, plus the furthest from the
// following one (which the far-overshoot rule needs). Reset whenever
// the active waypoint pair changes.
type ArrivalState struct {
	WaypointID                     string  `json:"waypoint_id,omitempty"`
	FollowingWaypointID            string  `json:"following_waypoint_id,omitempty"`
	MinDistanceToWaypoint          float64 `json:"min_distance_to_waypoint"`
	MinDistanceToFollowingWaypoint float64 `json:"min_distance_to_following_waypoint,omitempty"`
	MaxDistanceToFollowingWaypoint float64 `json:"max_distance_to_following_waypoint,omitempty"`
}

// ArrivalDecision is what decideArrival returns: whether to mark the
// active waypoint visited, the human-readable reason, the distances it
// computed (handy for diagnostics in logs), and the rolling state to
// carry into the next tick.
type ArrivalDecision struct {
	Arrived                     bool         `json:"arrived"`
	Reason                      string       `json:"reason,omitempty"`
	DistanceToWaypoint          float64      `json:"distance_to_waypoint"`
	DistanceToFollowingWaypoint float64      `json:"distance_to_following_waypoint,omitempty"`
	NewState                    ArrivalState `json:"new_state"`
}

// ArrivalLogPayload is the full envelope logged by checkArrival on every
// "interesting" tick: input + state-going-in + decision-coming-out.
// Designed so a user can copy a log line, paste it into a JSON fixture,
// add `"expected_arrived": true|false` (and optionally
// `"expected_reason": "..."`), and run the test to lock in that
// behaviour. See arrival_test.go for the fixture loader.
type ArrivalLogPayload struct {
	Input     ArrivalInput    `json:"input"`
	PrevState ArrivalState    `json:"prev_state"`
	Decision  ArrivalDecision `json:"decision"`
}

// decideArrival is the pure arrival-decision function. Given the boat's
// current position, the active waypoint, an optional following
// waypoint, and the rolling state from the previous tick, it returns
// whether the active waypoint should be marked visited, the reason
// string, and the new rolling state.
//
// The function does no I/O and no logging — call sites do those. Three
// rules can fire arrival:
//   - distanceToWaypoint < arrivalDistanceMeters (50 m): "we're there".
//   - With a following waypoint: bypass / far-overshoot rules detect
//     "rounded the corner without entering the arrival radius".
//   - Last waypoint (no following): fallback overshoot — we got close,
//     then moved well past.
func decideArrival(in ArrivalInput, prev ArrivalState) ArrivalDecision {
	distanceToWaypoint := haversineMeters(
		in.BoatLat, in.BoatLng,
		in.WaypointLat, in.WaypointLng,
	)
	distanceToFollowing := -1.0
	if in.HasFollowing {
		distanceToFollowing = haversineMeters(
			in.BoatLat, in.BoatLng,
			in.FollowingWaypointLat, in.FollowingWaypointLng,
		)
	}

	state := prev
	// Reset rolling memory whenever the active pair changes (first tick,
	// previous waypoint visited, list edited, etc.). Includes the case
	// where prev belonged to a different leg entirely (different
	// waypoint or different following).
	if state.WaypointID != in.WaypointID || state.FollowingWaypointID != in.FollowingWaypointID {
		state = ArrivalState{
			WaypointID:                     in.WaypointID,
			FollowingWaypointID:            in.FollowingWaypointID,
			MinDistanceToWaypoint:          distanceToWaypoint,
			MinDistanceToFollowingWaypoint: distanceToFollowing,
			MaxDistanceToFollowingWaypoint: distanceToFollowing,
		}
	}

	// Capture the pre-update minimum for the following waypoint so the
	// bypass rule can detect a NEW minimum on this tick.
	prevMinFollowing := state.MinDistanceToFollowingWaypoint

	if distanceToWaypoint < state.MinDistanceToWaypoint {
		state.MinDistanceToWaypoint = distanceToWaypoint
	}
	if distanceToFollowing >= 0 {
		if distanceToFollowing < state.MinDistanceToFollowingWaypoint {
			state.MinDistanceToFollowingWaypoint = distanceToFollowing
		}
		if distanceToFollowing > state.MaxDistanceToFollowingWaypoint {
			state.MaxDistanceToFollowingWaypoint = distanceToFollowing
		}
	}

	arrived := false
	reason := ""
	switch {
	case distanceToWaypoint < arrivalDistanceMeters:
		arrived = true
		reason = "inside arrival distance"
	case distanceToFollowing >= 0:
		switch {
		case distanceToWaypoint < nearWaypointMeters &&
			distanceToWaypoint > state.MinDistanceToWaypoint &&
			distanceToFollowing+followingClosingMarginMeters < prevMinFollowing:
			arrived = true
			reason = "passed (moved away from current + new min to following)"
		case distanceToWaypoint > state.MinDistanceToWaypoint*lastWaypointPassFactor &&
			distanceToFollowing+followingWaypointApproachMeters < state.MaxDistanceToFollowingWaypoint:
			arrived = true
			reason = "passed (far overshoot + closed on following)"
		}
	default:
		// Last waypoint: no following to compare to. Fall back to
		// overshoot detection.
		minD := state.MinDistanceToWaypoint
		if minD <= lastWaypointPassMinDistanceMeters &&
			distanceToWaypoint >= minD*lastWaypointPassFactor &&
			distanceToWaypoint > arrivalDistanceMeters {
			arrived = true
			reason = "overshot last waypoint"
		}
	}

	return ArrivalDecision{
		Arrived:                     arrived,
		Reason:                      reason,
		DistanceToWaypoint:          distanceToWaypoint,
		DistanceToFollowingWaypoint: distanceToFollowing,
		NewState:                    state,
	}
}

// haversineMeters returns the great-circle distance between two
// (lat, lng) points in metres. Same calculation the live arrival poller
// used to do via geo.GreatCircleDistance × 1000, inlined so the
// decision logic has no external dependencies and is easy to reason
// about in unit tests.
func haversineMeters(aLat, aLng, bLat, bLng float64) float64 {
	const R = 6371000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	φ1 := toRad(aLat)
	φ2 := toRad(bLat)
	Δφ := toRad(bLat - aLat)
	Δλ := toRad(bLng - aLng)
	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}
