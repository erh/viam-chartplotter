package vc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// navPoint is one GPS position as persisted: a waypoint, a saved location, or
// a goal target. Heading is optional; the wire always carries one (the
// navigation API requires heading or orientation on every GeoPose), so a
// missing heading goes out as 0.
type navPoint struct {
	Lat     float64  `json:"lat"`
	Lng     float64  `json:"lng"`
	Heading *float64 `json:"heading,omitempty"`
}

// savedLocation is a named navPoint (navigation.Location).
type savedLocation struct {
	Name string `json:"name"`
	navPoint
}

// Goal states as persisted. They map 1:1 onto navigation.GoalState.
const (
	goalActive    = "active"
	goalSuspended = "suspended"
	goalSucceeded = "succeeded"
	goalFailed    = "failed"
	goalCanceled  = "canceled"
)

// Goal modes as persisted. They map 1:1 onto navigation.NavMode.
const (
	goalModeRoute   = "route"
	goalModeDirect  = "direct"
	goalModePlanned = "planned"
)

// navGoal is the latest navigation goal. Every goal is stored as an ordered
// waypoint list with a cursor — a single-point goal (NavigateToPoint,
// GoToLocation, ...) is a one-waypoint list — so the arrival poller handles
// them all the same way. Waypoints[:Index] have been reached and stay
// recorded as a trip log; Waypoints[Index] is the current target.
type navGoal struct {
	Mode          string     `json:"mode"`
	RouteName     string     `json:"route_name,omitempty"`
	Location      string     `json:"location,omitempty"`
	Waypoints     []navPoint `json:"waypoints"`
	Index         int        `json:"index"`
	Loop          bool       `json:"loop,omitempty"`
	Lap           int        `json:"lap,omitempty"`
	State         string     `json:"state"`
	SuspendReason string     `json:"suspend_reason,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// open reports whether the goal is still being pursued (active or suspended).
func (g *navGoal) open() bool {
	return g != nil && (g.State == goalActive || g.State == goalSuspended)
}

// remaining is the not-yet-reached part of the goal's waypoints.
func (g *navGoal) remaining() []navPoint {
	if g == nil || g.Index >= len(g.Waypoints) {
		return nil
	}
	return g.Waypoints[g.Index:]
}

// advance marks n waypoints starting at the cursor reached. Past the last one
// a looping goal wraps to a new lap; anything else succeeds.
func (g *navGoal) advance(n int, now time.Time) {
	g.Index += n
	if g.Index < len(g.Waypoints) {
		return
	}
	if g.Loop && len(g.Waypoints) > 0 {
		g.Index = 0
		g.Lap++
		return
	}
	g.Index = len(g.Waypoints)
	g.State = goalSucceeded
	g.FinishedAt = &now
}

// finish moves an open goal to a terminal state.
func (g *navGoal) finish(state string, now time.Time) {
	g.State = state
	g.SuspendReason = ""
	g.FinishedAt = &now
}

// navState is everything the nav service persists.
type navState struct {
	Version   int             `json:"version"`
	Locations []savedLocation `json:"locations"`
	Goal      *navGoal        `json:"goal,omitempty"`
}

const navStateVersion = 2

// legacyWaypoint is the pre-v2 file format: a bare JSON array of waypoints,
// with reached ones flagged visited.
type legacyWaypoint struct {
	ID      string  `json:"id"`
	Lat     float64 `json:"lat"`
	Long    float64 `json:"long"`
	Order   int     `json:"order,omitempty"`
	Visited bool    `json:"visited,omitempty"`
}

// diskNavStore holds the nav service's saved locations and latest goal and
// mirrors them to a JSON file so they survive module restarts. Every mutation
// rewrites the file via a tmp+rename to avoid leaving a half-written file on
// crash.
type diskNavStore struct {
	mu    sync.Mutex
	path  string
	state navState
}

func newDiskNavStore(path string) (*diskNavStore, error) {
	s := &diskNavStore{path: path, state: navState{Version: navStateVersion}}
	if err := s.load(); err != nil {
		return nil, errors.Wrapf(err, "failed to load nav state from %q", path)
	}
	return s, nil
}

func (s *diskNavStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var raw []legacyWaypoint
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		s.state.Goal = goalFromLegacy(raw, time.Now())
		return nil
	}
	var st navState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	st.Version = navStateVersion
	if g := st.Goal; g != nil {
		if g.Index < 0 {
			g.Index = 0
		}
		if g.Index > len(g.Waypoints) {
			g.Index = len(g.Waypoints)
		}
	}
	s.state = st
	return nil
}

// goalFromLegacy converts a pre-v2 waypoint list into an active route goal:
// the leading visited waypoints become the reached prefix, and visited
// waypoints after the first unvisited one (which the old store also hid) are
// dropped. An all-visited or empty list is no goal.
func goalFromLegacy(raw []legacyWaypoint, now time.Time) *navGoal {
	g := &navGoal{Mode: goalModeRoute, State: goalActive, StartedAt: now}
	seenUnvisited := false
	for _, w := range raw {
		if w.Visited && seenUnvisited {
			continue
		}
		if w.Visited {
			g.Index++
		} else {
			seenUnvisited = true
		}
		g.Waypoints = append(g.Waypoints, navPoint{Lat: w.Lat, Lng: w.Long})
	}
	if !seenUnvisited {
		return nil
	}
	return g
}

// snapshot copies the current on-disk file to a timestamped sibling, e.g.
// nav.json -> nav.2026-05-07T15-04-23.123.json. Best-effort: any error (file
// missing, write failure) is swallowed so a backup problem never blocks a
// real mutation. Caller must hold s.mu so the file can't be mid-write.
//
// User-driven mutations snapshot; the arrival poller's progress updates
// deliberately do not — they fire once per passed waypoint and would
// otherwise produce dozens of backups during normal cruising.
func (s *diskNavStore) snapshot() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil || len(data) == 0 {
		return
	}
	// Millisecond resolution so two mutations in the same second don't
	// overwrite each other's backups. Filesystem-safe (no colons).
	ts := time.Now().Format("2006-01-02T15-04-05.000")
	ext := filepath.Ext(s.path)
	base := strings.TrimSuffix(s.path, ext)
	_ = os.WriteFile(fmt.Sprintf("%s.%s%s", base, ts, ext), data, 0o644)
}

// save flushes the state to disk. Caller must hold s.mu.
func (s *diskNavStore) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// cloneState deep-copies a navState (JSON round trip — the state is small).
func cloneState(st navState) navState {
	data, err := json.Marshal(st)
	if err != nil {
		panic(err) // plain data; cannot fail
	}
	var out navState
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// mutate applies fn to the state and persists it, rolling memory back if fn
// or the write fails so disk and memory stay in sync. userDriven snapshots
// the previous file first (see snapshot).
func (s *diskNavStore) mutate(userDriven bool, fn func(st *navState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := cloneState(s.state)
	if err := fn(&s.state); err != nil {
		s.state = prev
		return err
	}
	if userDriven {
		s.snapshot()
	}
	if err := s.save(); err != nil {
		s.state = prev
		return err
	}
	return nil
}

// Goal returns a copy of the latest goal, or nil if there has never been one.
func (s *diskNavStore) Goal() *navGoal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Goal == nil {
		return nil
	}
	g := cloneState(navState{Goal: s.state.Goal}).Goal
	return g
}

// Locations returns a copy of the saved locations.
func (s *diskNavStore) Locations() []savedLocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]savedLocation(nil), s.state.Locations...)
}

// LocationByName returns the saved location with the given name.
func (s *diskNavStore) LocationByName(name string) (savedLocation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.state.Locations {
		if l.Name == name {
			return l, true
		}
	}
	return savedLocation{}, false
}

func (s *diskNavStore) Close() error {
	return nil
}
