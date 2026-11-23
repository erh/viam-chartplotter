package vc

import (
	"context"
	"math"

	"github.com/pkg/errors"
)

// Steering: when the nav service has a `base`, it steers by setting the
// base's heading. Every arrival-poller tick with an active goal, it computes
// the initial great-circle bearing from the boat to the goal's next waypoint
// and sends {"set_heading": <degrees true>} to the base's DoCommand (see
// n2k_base.go for the NMEA 2000 autopilot implementation). Re-sending every
// tick doubles as the keepalive autopilots expect from a navigation source.
// When the goal stops being active (reached, canceled, suspended) the base
// is stopped once.

// steerLogDeltaDeg: a heading change smaller than this isn't logged.
const steerLogDeltaDeg = 5.0

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

// setBaseHeading sends one heading command to the configured base.
func (s *navService) setBaseHeading(ctx context.Context, headingDeg float64) error {
	if s.base == nil {
		return errors.New("nav service has no base configured")
	}
	_, err := s.base.DoCommand(ctx, map[string]interface{}{"set_heading": normalizeDegrees(headingDeg)})
	return err
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

	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	if err := s.setBaseHeading(ctx, h); err != nil {
		s.logger.Warnw("steering: set_heading failed", "err", err, "heading", h)
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
