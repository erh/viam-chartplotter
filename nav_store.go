package vc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"go.viam.com/rdk/services/navigation"
)

// diskNavStore is a navigation.NavStore that mirrors its waypoints to a JSON
// file on disk so they survive module restarts. Every mutation rewrites the
// file via a tmp+rename to avoid leaving a half-written file on crash.
type diskNavStore struct {
	mu        sync.Mutex
	path      string
	waypoints []*navigation.Waypoint
}

// persistedWaypoint is the on-disk schema. We keep our own struct rather than
// serialising navigation.Waypoint directly so the file stays human-friendly
// (string ID, named lat/long fields) and decoupled from any future bson
// changes upstream. Visited waypoints stay on disk so the trip log survives
// restarts; Waypoints() filters them out at read time.
type persistedWaypoint struct {
	ID      string  `json:"id"`
	Lat     float64 `json:"lat"`
	Long    float64 `json:"long"`
	Order   int     `json:"order,omitempty"`
	Visited bool    `json:"visited,omitempty"`
}

func newDiskNavStore(path string) (*diskNavStore, error) {
	s := &diskNavStore{path: path}
	if err := s.load(); err != nil {
		return nil, errors.Wrapf(err, "failed to load nav waypoints from %q", path)
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
	if len(data) == 0 {
		return nil
	}
	var raw []persistedWaypoint
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make([]*navigation.Waypoint, 0, len(raw))
	for _, p := range raw {
		id, err := primitive.ObjectIDFromHex(p.ID)
		if err != nil {
			// Skip an unparseable ID rather than refusing to load the whole
			// file; a stale waypoint is less disruptive than a refusal to
			// start.
			id = primitive.NewObjectID()
		}
		out = append(out, &navigation.Waypoint{
			ID:      id,
			Lat:     p.Lat,
			Long:    p.Long,
			Order:   p.Order,
			Visited: p.Visited,
		})
	}
	s.waypoints = out
	return nil
}

// snapshot copies the current on-disk waypoints file to a timestamped
// sibling, e.g. nav.json -> nav.2026-05-07T15-04-23.123.json. Best-effort:
// any error (file missing, write failure) is swallowed so a backup
// problem never blocks a real mutation. Caller must hold s.mu so the
// file content can't be mid-write under us.
//
// Called from user-driven mutators (Add/Remove/Insert/Move) before the
// in-memory list is changed. Auto-arrival's WaypointVisited deliberately
// does not snapshot — it fires once per passed waypoint and would
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
	backupPath := fmt.Sprintf("%s.%s%s", base, ts, ext)
	_ = os.WriteFile(backupPath, data, 0o644)
}

// save flushes the current waypoint slice to disk. Caller must hold s.mu.
func (s *diskNavStore) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	out := make([]persistedWaypoint, 0, len(s.waypoints))
	for _, wp := range s.waypoints {
		out = append(out, persistedWaypoint{
			ID:      wp.ID.Hex(),
			Lat:     wp.Lat,
			Long:    wp.Long,
			Order:   wp.Order,
			Visited: wp.Visited,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *diskNavStore) Waypoints(ctx context.Context) ([]navigation.Waypoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]navigation.Waypoint, 0, len(s.waypoints))
	for _, wp := range s.waypoints {
		if wp.Visited {
			continue
		}
		out = append(out, *wp)
	}
	return out, nil
}

func (s *diskNavStore) AddWaypoint(ctx context.Context, point *geo.Point) (navigation.Waypoint, error) {
	if err := ctx.Err(); err != nil {
		return navigation.Waypoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot()
	wp := &navigation.Waypoint{
		ID:   primitive.NewObjectID(),
		Lat:  point.Lat(),
		Long: point.Lng(),
	}
	s.waypoints = append(s.waypoints, wp)
	if err := s.save(); err != nil {
		// Roll back the in-memory append so disk and memory stay in sync.
		s.waypoints = s.waypoints[:len(s.waypoints)-1]
		return navigation.Waypoint{}, err
	}
	return *wp, nil
}

func (s *diskNavStore) RemoveWaypoint(ctx context.Context, id primitive.ObjectID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.waypoints
	next := make([]*navigation.Waypoint, 0, len(prev))
	for _, wp := range prev {
		if wp.ID == id {
			continue
		}
		next = append(next, wp)
	}
	if len(next) == len(prev) {
		return nil
	}
	s.snapshot()
	s.waypoints = next
	if err := s.save(); err != nil {
		s.waypoints = prev
		return err
	}
	return nil
}

// InsertWaypoint inserts a new waypoint immediately before the waypoint with
// the given ID, preserving the order of every other waypoint. If beforeID is
// the zero ObjectID the new waypoint is appended to the end (equivalent to
// AddWaypoint). It is not part of the upstream NavStore interface; the
// chartplotter UI uses it (via DoCommand) to "add waypoint here" between two
// existing legs.
func (s *diskNavStore) InsertWaypoint(
	ctx context.Context,
	point *geo.Point,
	beforeID primitive.ObjectID,
) (navigation.Waypoint, error) {
	if err := ctx.Err(); err != nil {
		return navigation.Waypoint{}, err
	}
	if point == nil {
		return navigation.Waypoint{}, errors.New("waypoint location is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wp := &navigation.Waypoint{
		ID:   primitive.NewObjectID(),
		Lat:  point.Lat(),
		Long: point.Lng(),
	}
	insertAt := len(s.waypoints)
	if !beforeID.IsZero() {
		found := false
		for i, existing := range s.waypoints {
			if existing.ID == beforeID {
				insertAt = i
				found = true
				break
			}
		}
		if !found {
			return navigation.Waypoint{}, errors.Errorf("no waypoint with id %s", beforeID.Hex())
		}
	}
	s.snapshot()
	prev := s.waypoints
	next := make([]*navigation.Waypoint, 0, len(prev)+1)
	next = append(next, prev[:insertAt]...)
	next = append(next, wp)
	next = append(next, prev[insertAt:]...)
	s.waypoints = next
	if err := s.save(); err != nil {
		s.waypoints = prev
		return navigation.Waypoint{}, err
	}
	return *wp, nil
}

// MoveWaypoint updates the lat/long of an existing waypoint in place,
// preserving its ID and order. It is not part of the upstream NavStore
// interface; the chartplotter UI uses it (via DoCommand) to support
// drag-to-edit on the map.
func (s *diskNavStore) MoveWaypoint(ctx context.Context, id primitive.ObjectID, point *geo.Point) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if point == nil {
		return errors.New("waypoint location is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, wp := range s.waypoints {
		if wp.ID != id {
			continue
		}
		s.snapshot()
		oldLat, oldLong := wp.Lat, wp.Long
		wp.Lat = point.Lat()
		wp.Long = point.Lng()
		if err := s.save(); err != nil {
			wp.Lat = oldLat
			wp.Long = oldLong
			return err
		}
		return nil
	}
	return errors.Errorf("no waypoint with id %s", id.Hex())
}

// ReplaceWaypoints atomically swaps the entire active waypoint list for the
// given ordered points. Every previous waypoint (including visited ones) is
// discarded and a fresh ObjectID + sequential Order is assigned to each new
// point, so loading a saved route can't collide with stale IDs. An empty
// points slice clears the route. It is not part of the upstream NavStore
// interface; the chartplotter UI uses it (via DoCommand) to load a saved route
// in one shot rather than N AddWaypoint round-trips. Returns the new count.
func (s *diskNavStore) ReplaceWaypoints(ctx context.Context, points []*geo.Point) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i, p := range points {
		if p == nil {
			return 0, errors.Errorf("waypoint %d is nil", i)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]*navigation.Waypoint, 0, len(points))
	for i, p := range points {
		next = append(next, &navigation.Waypoint{
			ID:    primitive.NewObjectID(),
			Lat:   p.Lat(),
			Long:  p.Lng(),
			Order: i,
		})
	}
	s.snapshot()
	prev := s.waypoints
	s.waypoints = next
	if err := s.save(); err != nil {
		s.waypoints = prev
		return 0, err
	}
	return len(next), nil
}

func (s *diskNavStore) NextWaypoint(ctx context.Context) (navigation.Waypoint, error) {
	if err := ctx.Err(); err != nil {
		return navigation.Waypoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, wp := range s.waypoints {
		if !wp.Visited {
			return *wp, nil
		}
	}
	return navigation.Waypoint{}, errors.New("no more waypoints")
}

func (s *diskNavStore) WaypointVisited(ctx context.Context, id primitive.ObjectID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, wp := range s.waypoints {
		if wp.ID == id && !wp.Visited {
			wp.Visited = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

func (s *diskNavStore) Close(ctx context.Context) error {
	return nil
}
