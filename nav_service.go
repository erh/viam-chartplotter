package vc

import (
	"context"
	"math"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"

	"go.viam.com/rdk/services/navigation"
)

// This file is navService's implementation of the navigation API. The model
// it maps onto: every goal is an ordered waypoint list with a cursor (see
// navGoal). A route goal (NavigateRoute) is how clients set "the active
// route"; they edit it by re-sending the whole list, with start_index at the
// current cursor so the reached prefix stays on record. Cancel clears it.
//
// The boat is steered by a person or an autopilot, not by this service, so
// "planning" is a straight line and the drive-style calls (NavigateToPoint,
// GoToLocation, ExecutePlan, ...) all just set a goal for the arrival poller
// to track. Zones and costmaps aren't supported.

// pollGoalInterval is how often a Wait-ing call re-checks its goal.
const pollGoalInterval = time.Second

var (
	errSuspendedGoal = errors.New("a suspended goal must be canceled before a new one starts")
	errNoFix         = errors.New("no position fix: the nav service needs a movement_sensor with a GPS fix")
	errZones         = errors.New("zones are not supported by the chartplotter nav service")
)

// --- Position conversion -------------------------------------------------------

// position renders a navPoint as a GPS-map Position. The API requires a
// heading (or orientation) on every GeoPose; a waypoint has none, so 0.
func (p navPoint) position() navigation.Position {
	heading := 0.0
	if p.Heading != nil {
		heading = *p.Heading
	}
	return navigation.Position{GeoPose: &navigation.GeoPose{
		Location: geo.NewPoint(p.Lat, p.Lng),
		Heading:  &heading,
	}}
}

// pointFromPosition validates and converts an incoming Position. This is a
// GPS-map service, so a local-frame Pose is rejected.
func pointFromPosition(p navigation.Position) (navPoint, error) {
	gp := p.GeoPose
	if gp == nil || gp.Location == nil {
		if p.Pose != nil {
			return navPoint{}, errors.New("this is a GPS-map navigation service; positions must be geo_pose, not pose")
		}
		return navPoint{}, errors.New("position must set geo_pose")
	}
	lat, lng := gp.Location.Lat(), gp.Location.Lng()
	if math.IsNaN(lat) || math.IsNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return navPoint{}, errors.Errorf("position out of range (lat=%v lng=%v)", lat, lng)
	}
	out := navPoint{Lat: lat, Lng: lng}
	if gp.Heading != nil {
		h := *gp.Heading
		out.Heading = &h
	}
	return out, nil
}

// resolveWaypoints turns API waypoints into points, looking saved-location
// references up in the store.
func (s *navService) resolveWaypoints(wps []navigation.Waypoint) ([]navPoint, error) {
	out := make([]navPoint, 0, len(wps))
	for i, wp := range wps {
		switch {
		case wp.Position != nil:
			p, err := pointFromPosition(*wp.Position)
			if err != nil {
				return nil, errors.Wrapf(err, "waypoint %d", i)
			}
			out = append(out, p)
		case wp.LocationName != "":
			loc, ok := s.store.LocationByName(wp.LocationName)
			if !ok {
				return nil, errors.Errorf("waypoint %d: no saved location %q", i, wp.LocationName)
			}
			out = append(out, loc.navPoint)
		default:
			return nil, errors.Errorf("waypoint %d must set a location name or a position", i)
		}
	}
	return out, nil
}

func positionWaypoints(pts []navPoint) []navigation.Waypoint {
	out := make([]navigation.Waypoint, len(pts))
	for i, p := range pts {
		pos := p.position()
		out[i] = navigation.Waypoint{Position: &pos}
	}
	return out
}

// --- Position fix ----------------------------------------------------------------

// fix reads the boat's position and heading. ok is false when there is no
// movement sensor, it errors, or it reports a null-island (0,0) placeholder.
func (s *navService) fix(ctx context.Context, extra map[string]interface{}) (navPoint, bool) {
	if s.ms == nil {
		return navPoint{}, false
	}
	pt, _, err := s.ms.Position(ctx, extra)
	if err != nil || pt == nil || (pt.Lat() == 0 && pt.Lng() == 0) {
		return navPoint{}, false
	}
	out := navPoint{Lat: pt.Lat(), Lng: pt.Lng()}
	// Heading may not be supported on every movement_sensor; omit it then.
	if h, err := s.ms.CompassHeading(ctx, extra); err == nil && !math.IsNaN(h) {
		out.Heading = &h
	}
	return out, true
}

// --- Locations -------------------------------------------------------------------

func toLocation(l savedLocation) navigation.Location {
	return navigation.Location{Name: l.Name, Position: l.navPoint.position()}
}

func (s *navService) AddLocation(ctx context.Context, req navigation.AddLocationReq) (navigation.Location, error) {
	if req.Name == "" {
		return navigation.Location{}, errors.New("location name is required")
	}
	var p navPoint
	if req.Position != nil {
		var err error
		if p, err = pointFromPosition(*req.Position); err != nil {
			return navigation.Location{}, err
		}
	} else {
		var ok bool
		if p, ok = s.fix(ctx, req.Extra); !ok {
			return navigation.Location{}, errNoFix
		}
	}
	loc := savedLocation{Name: req.Name, navPoint: p}
	err := s.store.mutate(true, func(st *navState) error {
		for _, l := range st.Locations {
			if l.Name == req.Name {
				return errors.Errorf("location %q already exists", req.Name)
			}
		}
		st.Locations = append(st.Locations, loc)
		return nil
	})
	if err != nil {
		return navigation.Location{}, err
	}
	return toLocation(loc), nil
}

func (s *navService) GetLocation(ctx context.Context, name string, extra map[string]interface{}) (navigation.Location, error) {
	loc, ok := s.store.LocationByName(name)
	if !ok {
		return navigation.Location{}, errors.Errorf("location %q not found", name)
	}
	return toLocation(loc), nil
}

func (s *navService) ListLocations(ctx context.Context, extra map[string]interface{}) ([]navigation.Location, error) {
	locs := s.store.Locations()
	out := make([]navigation.Location, len(locs))
	for i, l := range locs {
		out[i] = toLocation(l)
	}
	return out, nil
}

// UpdateLocation moves and/or renames a saved location. Routes store
// coordinates, not location references, so only a goal that targets the
// location by name needs following.
func (s *navService) UpdateLocation(ctx context.Context, req navigation.UpdateLocationReq) (navigation.Location, error) {
	var p *navPoint
	if req.Position != nil {
		np, err := pointFromPosition(*req.Position)
		if err != nil {
			return navigation.Location{}, err
		}
		p = &np
	}
	var out savedLocation
	err := s.store.mutate(true, func(st *navState) error {
		idx := -1
		for i, l := range st.Locations {
			if l.Name == req.Name {
				idx = i
			}
		}
		if idx < 0 {
			return errors.Errorf("location %q not found", req.Name)
		}
		if req.NewName != nil && *req.NewName != req.Name {
			if *req.NewName == "" {
				return errors.New("new location name must not be empty")
			}
			for _, l := range st.Locations {
				if l.Name == *req.NewName {
					return errors.Errorf("location %q already exists", *req.NewName)
				}
			}
			st.Locations[idx].Name = *req.NewName
			if st.Goal != nil && st.Goal.Location == req.Name {
				st.Goal.Location = *req.NewName
			}
		}
		if p != nil {
			st.Locations[idx].navPoint = *p
		}
		out = st.Locations[idx]
		return nil
	})
	if err != nil {
		return navigation.Location{}, err
	}
	return toLocation(out), nil
}

func (s *navService) DeleteLocation(ctx context.Context, name string, extra map[string]interface{}) error {
	return s.store.mutate(true, func(st *navState) error {
		out := st.Locations[:0]
		found := false
		for _, l := range st.Locations {
			if l.Name == name {
				found = true
				continue
			}
			out = append(out, l)
		}
		if !found {
			return errors.Errorf("location %q not found", name)
		}
		st.Locations = out
		return nil
	})
}

// --- Zones (unsupported) ------------------------------------------------------------

func (s *navService) AddZone(ctx context.Context, req navigation.AddZoneReq) (navigation.Zone, error) {
	return navigation.Zone{}, errZones
}

func (s *navService) GetZone(ctx context.Context, name string, extra map[string]interface{}) (navigation.Zone, error) {
	return navigation.Zone{}, errZones
}

func (s *navService) ListZones(ctx context.Context, zoneType string, extra map[string]interface{}) ([]navigation.Zone, error) {
	return []navigation.Zone{}, nil
}

func (s *navService) UpdateZone(ctx context.Context, req navigation.UpdateZoneReq) (navigation.Zone, error) {
	return navigation.Zone{}, errZones
}

func (s *navService) DeleteZone(ctx context.Context, name string, extra map[string]interface{}) error {
	return errZones
}

// --- Goals -------------------------------------------------------------------------

// startGoal replaces the latest goal with g (an active goal replaces another
// active one; a suspended one must be canceled first) and, with wait, blocks
// until it finishes.
func (s *navService) startGoal(ctx context.Context, g navGoal, wait bool, extra map[string]interface{}) (navigation.Status, error) {
	if len(g.Waypoints) == 0 {
		return navigation.Status{}, errors.New("goal has no waypoints")
	}
	if g.Index < 0 || g.Index >= len(g.Waypoints) {
		return navigation.Status{}, errors.Errorf("start index %d out of range for %d waypoints", g.Index, len(g.Waypoints))
	}
	g.State = goalActive
	g.StartedAt = time.Now()
	err := s.store.mutate(true, func(st *navState) error {
		if st.Goal != nil && st.Goal.State == goalSuspended {
			return errSuspendedGoal
		}
		st.Goal = &g
		return nil
	})
	if err != nil {
		return navigation.Status{}, err
	}
	if wait {
		if err := s.waitGoal(ctx, g.StartedAt); err != nil {
			return navigation.Status{}, err
		}
	}
	return s.GetNavigationStatus(ctx, extra)
}

// waitGoal blocks until the goal that started at startedAt is no longer
// open (finished, canceled, or replaced) or ctx ends.
func (s *navService) waitGoal(ctx context.Context, startedAt time.Time) error {
	t := time.NewTicker(pollGoalInterval)
	defer t.Stop()
	for {
		g := s.store.Goal()
		if g == nil || !g.StartedAt.Equal(startedAt) || !g.open() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *navService) NavigateToLocation(ctx context.Context, name string, extra map[string]interface{}) (navigation.Status, error) {
	loc, ok := s.store.LocationByName(name)
	if !ok {
		return navigation.Status{}, errors.Errorf("location %q not found", name)
	}
	return s.startGoal(ctx, navGoal{Mode: goalModePlanned, Location: name, Waypoints: []navPoint{loc.navPoint}}, false, extra)
}

func (s *navService) NavigateToPoint(ctx context.Context, destination navigation.Position, extra map[string]interface{}) (navigation.Status, error) {
	p, err := pointFromPosition(destination)
	if err != nil {
		return navigation.Status{}, err
	}
	return s.startGoal(ctx, navGoal{Mode: goalModePlanned, Waypoints: []navPoint{p}}, false, extra)
}

// NavigateRoute starts a route goal from a saved route (by name, or by the
// saved route's id) or an inline waypoint list.
func (s *navService) NavigateRoute(ctx context.Context, req navigation.NavigateRouteReq) (navigation.Status, error) {
	var (
		pts  []navPoint
		name string
		err  error
	)
	switch {
	case req.Name != "" && len(req.Waypoints) > 0:
		return navigation.Status{}, errors.New("provide either a route name or inline waypoints, not both")
	case req.Name != "":
		r, ferr := s.findRoute(ctx, req.Name)
		if ferr != nil {
			return navigation.Status{}, ferr
		}
		name = r.Name
		for _, w := range r.Waypoints {
			pts = append(pts, navPoint{Lat: w.Lat, Lng: w.Lng})
		}
	default:
		if pts, err = s.resolveWaypoints(req.Waypoints); err != nil {
			return navigation.Status{}, err
		}
	}
	if len(pts) == 0 {
		return navigation.Status{}, errors.New("route has no waypoints")
	}
	start := int(req.StartIndex)
	if req.StartNearest {
		fix, ok := s.fix(ctx, req.Extra)
		if !ok {
			return navigation.Status{}, errNoFix
		}
		start = nearestIndex(fix, pts)
	}
	g := navGoal{Mode: goalModeRoute, RouteName: name, Waypoints: pts, Index: start, Loop: req.Loop}
	return s.startGoal(ctx, g, req.Wait, req.Extra)
}

func nearestIndex(from navPoint, pts []navPoint) int {
	best, bestD := 0, math.Inf(1)
	for i, p := range pts {
		if d := haversineMeters(from.Lat, from.Lng, p.Lat, p.Lng); d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// straightPlan is the "plan" this service can make: a straight line from
// start (or the boat) to dest.
func (s *navService) straightPlan(ctx context.Context, start *navigation.Position, dest navPoint, extra map[string]interface{}) (navigation.Plan, error) {
	var from navPoint
	if start != nil {
		var err error
		if from, err = pointFromPosition(*start); err != nil {
			return navigation.Plan{}, errors.Wrap(err, "start")
		}
	} else {
		var ok bool
		if from, ok = s.fix(ctx, extra); !ok {
			return navigation.Plan{}, errNoFix
		}
	}
	return navigation.Plan{
		Path:    []navigation.Position{from.position(), dest.position()},
		LengthM: haversineMeters(from.Lat, from.Lng, dest.Lat, dest.Lng),
	}, nil
}

func (s *navService) PlanToPoint(ctx context.Context, req navigation.PlanToPointReq) (navigation.Plan, error) {
	dest, err := pointFromPosition(req.Destination)
	if err != nil {
		return navigation.Plan{}, err
	}
	return s.straightPlan(ctx, req.Start, dest, req.Extra)
}

func (s *navService) PlanToLocation(ctx context.Context, req navigation.PlanToLocationReq) (navigation.Plan, error) {
	loc, ok := s.store.LocationByName(req.Name)
	if !ok {
		return navigation.Plan{}, errors.Errorf("location %q not found", req.Name)
	}
	plan, err := s.straightPlan(ctx, req.Start, loc.navPoint, req.Extra)
	if err != nil {
		return navigation.Plan{}, err
	}
	name := req.Name
	plan.Location = &name
	return plan, nil
}

// ExecutePlan follows a plan's path. Its first point is where the plan
// started from, so it is skipped when there is more than one.
func (s *navService) ExecutePlan(ctx context.Context, plan navigation.Plan, extra map[string]interface{}) (navigation.Status, error) {
	if len(plan.Path) == 0 {
		return navigation.Status{}, errors.New("plan path is empty")
	}
	path := plan.Path
	if len(path) > 1 {
		path = path[1:]
	}
	pts := make([]navPoint, 0, len(path))
	for i, p := range path {
		pt, err := pointFromPosition(p)
		if err != nil {
			return navigation.Status{}, errors.Wrapf(err, "plan path %d", i)
		}
		pts = append(pts, pt)
	}
	g := navGoal{Mode: goalModePlanned, Waypoints: pts}
	if plan.Location != nil {
		g.Location = *plan.Location
	}
	return s.startGoal(ctx, g, false, extra)
}

func (s *navService) GoToLocation(ctx context.Context, req navigation.GoToLocationReq) (navigation.Status, error) {
	loc, ok := s.store.LocationByName(req.Name)
	if !ok {
		return navigation.Status{}, errors.Errorf("location %q not found", req.Name)
	}
	g := navGoal{Mode: goalModeDirect, Location: req.Name, Waypoints: []navPoint{loc.navPoint}}
	return s.startGoal(ctx, g, req.Wait != nil && *req.Wait, req.Extra)
}

func (s *navService) GoToPoint(ctx context.Context, req navigation.GoToPointReq) (navigation.Status, error) {
	p, err := pointFromPosition(req.Destination)
	if err != nil {
		return navigation.Status{}, err
	}
	g := navGoal{Mode: goalModeDirect, Waypoints: []navPoint{p}}
	return s.startGoal(ctx, g, req.Wait != nil && *req.Wait, req.Extra)
}

func (s *navService) Suspend(ctx context.Context, reason string, extra map[string]interface{}) (navigation.Status, error) {
	err := s.store.mutate(true, func(st *navState) error {
		if st.Goal == nil || st.Goal.State != goalActive {
			return errors.New("no active goal to suspend")
		}
		st.Goal.State = goalSuspended
		st.Goal.SuspendReason = reason
		return nil
	})
	if err != nil {
		return navigation.Status{}, err
	}
	return s.GetNavigationStatus(ctx, extra)
}

func (s *navService) Resume(ctx context.Context, extra map[string]interface{}) (navigation.Status, error) {
	err := s.store.mutate(true, func(st *navState) error {
		if st.Goal == nil || st.Goal.State != goalSuspended {
			return errors.New("no suspended goal to resume")
		}
		st.Goal.State = goalActive
		st.Goal.SuspendReason = ""
		return nil
	})
	if err != nil {
		return navigation.Status{}, err
	}
	return s.GetNavigationStatus(ctx, extra)
}

// Cancel discards the active or suspended goal. The goal stays on record as
// canceled (GetNavigationStatus reports the latest goal); with no open goal
// it is a no-op.
func (s *navService) Cancel(ctx context.Context, extra map[string]interface{}) (navigation.Status, error) {
	err := s.store.mutate(true, func(st *navState) error {
		if st.Goal.open() {
			st.Goal.finish(goalCanceled, time.Now())
		}
		return nil
	})
	if err != nil {
		return navigation.Status{}, err
	}
	return s.GetNavigationStatus(ctx, extra)
}

// --- Status -------------------------------------------------------------------------

func (s *navService) GetPosition(ctx context.Context, extra map[string]interface{}) (navigation.Position, error) {
	p, ok := s.fix(ctx, extra)
	if !ok {
		return navigation.Position{}, errNoFix
	}
	return p.position(), nil
}

var goalStates = map[string]navigation.GoalState{
	goalActive:    navigation.GoalStateActive,
	goalSuspended: navigation.GoalStateSuspended,
	goalSucceeded: navigation.GoalStateSucceeded,
	goalFailed:    navigation.GoalStateFailed,
	goalCanceled:  navigation.GoalStateCanceled,
}

var goalModes = map[string]navigation.NavMode{
	goalModeRoute:   navigation.NavModeRoute,
	goalModeDirect:  navigation.NavModeDirect,
	goalModePlanned: navigation.NavModePlanned,
}

// goalStatus renders a goal for the API. Progress (only while active) is the
// path from the boat, when it has a fix, through every remaining waypoint.
func goalStatus(g *navGoal, fix navPoint, localized bool) *navigation.GoalStatus {
	out := &navigation.GoalStatus{
		State:      goalStates[g.State],
		Mode:       goalModes[g.Mode],
		StartedAt:  g.StartedAt,
		FinishedAt: g.FinishedAt,
	}
	if len(g.Waypoints) > 0 {
		target := g.Waypoints[len(g.Waypoints)-1]
		if g.Index < len(g.Waypoints) {
			target = g.Waypoints[g.Index]
		}
		out.Target = target.position()
	}
	if g.Location != "" {
		loc := g.Location
		out.Location = &loc
	}
	if g.State == goalSuspended {
		reason := g.SuspendReason
		out.SuspendReason = &reason
	}
	if g.Mode == goalModeRoute {
		out.Route = &navigation.RouteProgress{
			Name:      g.RouteName,
			Waypoints: positionWaypoints(g.Waypoints),
			Index:     uint32(g.Index),
			Loop:      g.Loop,
			Lap:       uint32(g.Lap),
		}
	}
	if g.State == goalActive {
		path := g.remaining()
		if localized {
			path = append([]navPoint{fix}, path...)
		}
		length := 0.0
		positions := make([]navigation.Position, len(path))
		for i, p := range path {
			positions[i] = p.position()
			if i > 0 {
				length += haversineMeters(path[i-1].Lat, path[i-1].Lng, p.Lat, p.Lng)
			}
		}
		out.Progress = &navigation.Progress{DistanceRemainingM: length, Path: positions, LengthM: length}
	}
	return out
}

func (s *navService) GetNavigationStatus(ctx context.Context, extra map[string]interface{}) (navigation.Status, error) {
	fix, localized := s.fix(ctx, extra)
	out := navigation.Status{UpdatedAt: time.Now(), Localized: localized}
	if localized {
		out.Position = fix.position()
	} else {
		// The API requires a position; callers ignore it while not localized.
		out.Position = navPoint{}.position()
	}
	if localized && s.ms != nil {
		if v, err := s.ms.LinearVelocity(ctx, extra); err == nil {
			out.Velocity.LinearMPerSec = math.Hypot(v.X, v.Y)
		}
	}
	if g := s.store.Goal(); g != nil {
		out.Goal = goalStatus(g, fix, localized)
	}
	return out, nil
}

func (s *navService) GetProperties(ctx context.Context) (navigation.Properties, error) {
	return navigation.Properties{
		MapType:         navigation.GPSMap,
		RoutesSupported: true,
	}, nil
}

func (s *navService) GetCostmap(ctx context.Context, req navigation.CostmapReq) (navigation.Costmap, error) {
	return navigation.Costmap{Available: false}, nil
}
