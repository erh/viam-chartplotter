package vc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// arrivalFixture is one decision-tick test case. The shape is identical
// to what checkArrival logs (input + prev_state + decision), with two
// extra fields the user adds when locking in behaviour:
//
//	expected_arrived  — must match the decision the live code makes;
//	expected_reason   — optional; when set, must match decision.Reason.
//
// Workflow:
//  1. Run the boat (or replay a track in the simulator).
//  2. Watch the module log for `arrival decision: {...}` lines.
//  3. Copy the JSON object into testdata/arrival/<descriptive-name>.json
//     and add `"expected_arrived": true|false` (and optionally
//     `"expected_reason": "..."`).
//  4. `go test ./...` — the fixture is now part of the suite.
//
// The `decision` field from the original log line is allowed (and
// ignored) so a verbatim paste-then-add-expected_arrived is a valid
// fixture file.
type arrivalFixture struct {
	Comment         string           `json:"comment,omitempty"`
	Input           ArrivalInput     `json:"input"`
	PrevState       ArrivalState     `json:"prev_state"`
	ExpectedArrived bool             `json:"expected_arrived"`
	ExpectedReason  string           `json:"expected_reason,omitempty"`
	Decision        *ArrivalDecision `json:"decision,omitempty"`
}

// TestArrivalFixtures loads every JSON fixture under testdata/arrival/
// and runs decideArrival against it, asserting the live behaviour
// matches expected_arrived (and expected_reason, if set).
func TestArrivalFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/arrival/*.json")
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no fixtures under testdata/arrival/ yet — drop a JSON file in there to add a case")
	}
	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var fx arrivalFixture
			if err := json.Unmarshal(data, &fx); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := decideArrival(fx.Input, fx.PrevState)
			if got.Arrived != fx.ExpectedArrived {
				t.Errorf("arrived = %v (reason=%q), want %v\n  fixture comment: %s",
					got.Arrived, got.Reason, fx.ExpectedArrived, fx.Comment)
			}
			if fx.ExpectedReason != "" && got.Reason != fx.ExpectedReason {
				t.Errorf("reason = %q, want %q", got.Reason, fx.ExpectedReason)
			}
		})
	}
}

// TestArrivalSanity exercises the obvious branches without requiring
// a fixture file. Functions as a smoke test so a build that breaks
// decideArrival is caught even before any fixtures exist.
func TestArrivalSanity(t *testing.T) {
	t.Run("inside arrival distance fires", func(t *testing.T) {
		// Boat 30 m from waypoint — should arrive on this tick alone.
		in := ArrivalInput{
			BoatLat:     40.7000000,
			BoatLng:     -74.0000000,
			WaypointID:  "wp1",
			WaypointLat: 40.7002700, // ≈ 30 m north
			WaypointLng: -74.0000000,
		}
		d := decideArrival(in, ArrivalState{})
		if !d.Arrived {
			t.Fatalf("expected arrival, got %+v", d)
		}
		if d.Reason != "inside arrival distance" {
			t.Errorf("reason = %q", d.Reason)
		}
	})
	t.Run("far away does not fire", func(t *testing.T) {
		in := ArrivalInput{
			BoatLat:     40.0,
			BoatLng:     -74.0,
			WaypointID:  "wp1",
			WaypointLat: 41.0,
			WaypointLng: -74.0,
		}
		d := decideArrival(in, ArrivalState{})
		if d.Arrived {
			t.Fatalf("expected no arrival, got %+v", d)
		}
	})
}
