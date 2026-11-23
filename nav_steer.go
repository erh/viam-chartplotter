package vc

import (
	"context"
	"math"
	"time"

	"github.com/golang/geo/r3"
	"github.com/pkg/errors"
)

// Steering: when the nav service has a `base`, a steering loop runs every
// steerInterval. While a goal is active it reads the boat's position and
// compass heading, takes the initial great-circle bearing to the goal's next
// waypoint and the turn from the current heading to that bearing (positive =
// left, as the base API defines it), and commands the base in one of two
// modes:
//
//   - Turn only (no speed_mps): Spin(turn). For a base that holds a heading
//     while something else sets the speed — on the n2k-base (n2k_base.go) a
//     Spin becomes "autopilot, hold current heading - turn", i.e. the bearing.
//     Re-sending every tick doubles as the autopilot's keepalive.
//   - Drive (speed_mps set): SetVelocity(forward, turn rate). For a base that
//     moves the boat itself (thrusters). The turn rate is proportional to the
//     heading error, capped; forward speed falls off as the error grows and is
//     zero when the waypoint is more than 90° off the bow (turn first).
//
// When the goal stops being active (last waypoint reached, canceled,
// suspended) the base is stopped once. In drive mode the base is also stopped
// when position or heading can't be read, so it never runs blind on a stale
// command; and when the service closes.
//
// Waypoint arrival is still decided by the arrival poller (nav.go); the
// steering loop just reads the goal it leaves behind.

const (
	// steerInterval is how often the steering loop commands the base.
	steerInterval = time.Second
	// steerLogDeltaDeg: a bearing change smaller than this isn't logged.
	steerLogDeltaDeg = 5.0
	// steerLogInterval throttles repeated steering warnings.
	steerLogInterval = 30 * time.Second

	// steerSpinDegsPerSec is the rate passed to Spin. The base API treats 0
	// as "stop", so it must be nonzero; an autopilot picks its own turn rate,
	// and the n2k-base ignores it.
	steerSpinDegsPerSec = 5.0

	// steerTurnGain is the drive-mode turn rate per degree of heading error
	// (deg/s per deg): 10° off -> 10°/s.
	steerTurnGain = 1.0
	// steerMaxTurnDegsPerSec caps the drive-mode turn rate.
	steerMaxTurnDegsPerSec = 20.0
)

// velocityCommand is the drive-mode SetVelocity for a heading error of
// turnDeg (positive = waypoint is to the left) at cruising speed speedMPS:
// forward speed in mm/s (base API linear Y) and turn rate in deg/s (base API
// angular Z, positive = left).
func velocityCommand(turnDeg, speedMPS float64) (linearMmPerSec, angularDegsPerSec float64) {
	angularDegsPerSec = math.Max(-steerMaxTurnDegsPerSec, math.Min(steerMaxTurnDegsPerSec, steerTurnGain*turnDeg))
	if math.Abs(turnDeg) < 90 {
		linearMmPerSec = speedMPS * 1000 * math.Cos(turnDeg*math.Pi/180)
	}
	return linearMmPerSec, angularDegsPerSec
}

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

// startSteering launches the steering loop.
func (s *navService) startSteering() {
	ctx, cancel := context.WithCancel(context.Background())
	s.steerCancel = cancel
	s.steerDone = make(chan struct{})
	go func() {
		defer close(s.steerDone)
		ticker := time.NewTicker(steerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.steer(ctx)
			}
		}
	}()
}

// driving reports whether the base is driven with SetVelocity.
func (s *navService) driving() bool { return s.speedMPS > 0 }

// maybeSteerWarn throttles a steering warning to one per steerLogInterval.
// Call with steerMu held.
func (s *navService) maybeSteerWarn(msg string, kvs ...interface{}) {
	now := time.Now()
	if now.Sub(s.lastSteerLog) < steerLogInterval {
		return
	}
	s.lastSteerLog = now
	s.logger.Warnw(msg, kvs...)
}

// stopSteering stops the base if this service was steering it.
func (s *navService) stopSteering(ctx context.Context, reason string) {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	s.stopSteeringLocked(ctx, reason)
}

func (s *navService) stopSteeringLocked(ctx context.Context, reason string) {
	if s.base == nil || s.steerHeading == nil {
		return
	}
	s.steerHeading = nil
	if err := s.base.Stop(ctx, nil); err != nil {
		s.logger.Warnw("steering: base Stop failed", "err", err, "reason", reason)
		return
	}
	s.logger.Infof("steering: stopped (%s)", reason)
}

// steer is one steering tick: point the base at the active goal's next
// waypoint (and, in drive mode, move toward it), or stop it when there's no
// active goal.
func (s *navService) steer(ctx context.Context) {
	if s.base == nil || s.ms == nil {
		return
	}
	g := s.store.Goal()
	if g == nil || g.State != goalActive || len(g.remaining()) == 0 {
		s.stopSteering(ctx, "no active goal")
		return
	}
	wp := g.remaining()[0]

	s.steerMu.Lock()
	defer s.steerMu.Unlock()

	// lost handles a missing fix: a driven base is stopped rather than left
	// running on its last command; an autopilot keeps its last heading.
	lost := func(msg string, err error) {
		s.maybeSteerWarn("steering: "+msg, "err", err)
		if s.driving() {
			s.stopSteeringLocked(ctx, msg)
		}
	}
	pt, _, err := s.ms.Position(ctx, nil)
	if err != nil || pt == nil || (pt.Lat() == 0 && pt.Lng() == 0) {
		lost("no position fix", err)
		return
	}
	cur, err := s.ms.CompassHeading(ctx, nil)
	if err != nil || math.IsNaN(cur) {
		lost("movement sensor has no compass heading", err)
		return
	}
	h := bearingDegrees(pt.Lat(), pt.Lng(), wp.Lat, wp.Lng)
	turn := turnDegrees(cur, h)

	if s.driving() {
		lin, ang := velocityCommand(turn, s.speedMPS)
		err = s.base.SetVelocity(ctx, r3.Vector{Y: lin}, r3.Vector{Z: ang}, nil)
	} else {
		err = s.base.Spin(ctx, turn, steerSpinDegsPerSec, nil)
	}
	if err != nil {
		s.maybeSteerWarn("steering: commanding base failed", "err", err, "bearing", h, "turn", turn)
		if s.driving() {
			s.stopSteeringLocked(ctx, "commanding base failed")
		}
		return
	}
	if s.steerHeading == nil || headingDelta(*s.steerHeading, h) >= steerLogDeltaDeg {
		s.logger.Infof("steering: bearing %.0f° to waypoint %d of %d (heading %.0f°)", h, g.Index+1, len(g.Waypoints), cur)
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
