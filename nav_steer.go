package vc

import (
	"context"
	"math"

	"github.com/pkg/errors"
)

// Steering: when the nav service has a `base`, it steers it with the base
// API's Spin. Every arrival-poller tick with an active goal, it computes the
// initial great-circle bearing from the boat to the goal's next waypoint and
// the turn from the boat's current compass heading to that bearing, and calls
// Spin(turn) — positive is a turn to the left, as the base API defines it.
// On the n2k-base (n2k_base.go) a Spin becomes "hold current heading - angle",
// i.e. the bearing itself. Re-sending every tick doubles as the keepalive
// autopilots expect from a navigation source. When the goal stops being
// active (reached, canceled, suspended) the base is stopped once.

// steerLogDeltaDeg: a heading change smaller than this isn't logged.
const steerLogDeltaDeg = 5.0

// steerSpinDegsPerSec is the rate passed to Spin. The base API treats 0 as
// "stop", so it must be nonzero; an autopilot picks its own turn rate, and the
// n2k-base ignores it.
const steerSpinDegsPerSec = 5.0

// bearingDegrees is the initial great-circle bearing from a to b, degrees
// true in [0, 360).
func bearingDegrees(aLat, aLng, bLat, bLng float64) float64 {
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	φ1, φ2 := toRad(aLat), toRad(bLat)
	Δλ := toRad(bLng - aLng)
	y := math.Sin(Δλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(Δλ)
	return normalizeDegrees(math.Atan2(y, x) * 180 / math.Pi)
}

func headingDelta(a, b float64) float64 {
	d := math.Abs(normalizeDegrees(a) - normalizeDegrees(b))
	if d > 180 {
		d = 360 - d
	}
	return d
}

// turnDegrees is the Spin angle that turns a boat heading `from` onto `to`
// the short way: in (-180, 180], positive to the left (counter-clockwise), as
// the base API defines Spin.
func turnDegrees(from, to float64) float64 {
	d := normalizeDegrees(from - to)
	if d > 180 {
		d -= 360
	}
	return d
}

// stopSteering stops the base if this service was steering it.
func (s *navService) stopSteering(ctx context.Context) {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	if s.base == nil || s.steerHeading == nil {
		return
	}
	s.steerHeading = nil
	if err := s.base.Stop(ctx, nil); err != nil {
		s.logger.Warnw("steering: base Stop failed", "err", err)
		return
	}
	s.logger.Info("steering: stopped (no active goal)")
}

// steer is one steering tick: point the base at the active goal's next
// waypoint, or stop it when there's no active goal.
func (s *navService) steer(ctx context.Context, boatLat, boatLng float64) {
	if s.base == nil {
		return
	}
	g := s.store.Goal()
	if g == nil || g.State != goalActive || len(g.remaining()) == 0 {
		s.stopSteering(ctx)
		return
	}
	wp := g.remaining()[0]
	h := bearingDegrees(boatLat, boatLng, wp.Lat, wp.Lng)

	cur, err := s.ms.CompassHeading(ctx, nil)
	if err != nil || math.IsNaN(cur) {
		s.maybeArrivalInfo("steering: movement sensor has no compass heading; not steering", "err", err)
		return
	}
	turn := turnDegrees(cur, h)

	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	if err := s.base.Spin(ctx, turn, steerSpinDegsPerSec, nil); err != nil {
		s.logger.Warnw("steering: base Spin failed", "err", err, "bearing", h, "turn", turn)
		return
	}
	if s.steerHeading == nil || headingDelta(*s.steerHeading, h) >= steerLogDeltaDeg {
		s.logger.Infof("steering: heading %.0f° to waypoint %d of %d", h, g.Index+1, len(g.Waypoints))
	}
	s.steerHeading = &h
}

// doSendWaypoints hands the active goal's remaining waypoints to the base,
// with the caller's options (route name, ids, dst) passed through.
func (s *navService) doSendWaypoints(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	if s.base == nil {
		return nil, errors.New("send_waypoints: nav service has no base configured")
	}
	var wps []navPoint
	if g := s.store.Goal(); g.open() {
		wps = g.remaining()
	}
	if len(wps) == 0 {
		return nil, errors.New("send_waypoints: no waypoints to send (no open goal)")
	}
	payload := map[string]interface{}{}
	if m, ok := raw.(map[string]interface{}); ok {
		for k, v := range m {
			payload[k] = v
		}
	}
	list := make([]interface{}, len(wps))
	for i, p := range wps {
		list[i] = map[string]interface{}{"lat": p.Lat, "lng": p.Lng}
	}
	payload["waypoints"] = list
	return s.base.DoCommand(ctx, map[string]interface{}{"send_waypoints": payload})
}
