package vc

import (
	"context"
	"math"
	"sync"

	"github.com/golang/geo/r3"
	"github.com/pkg/errors"

	"go.viam.com/rdk/components/base"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
)

// N2KBaseModel is a base that steers a boat through an NMEA 2000 autopilot.
//
// It is steered with the base API's Spin: Spin(angle) means "turn angle
// degrees from where the boat is pointing now" (positive = left, as the base
// API defines it), so the base reads the boat's current compass heading from
// its movement sensor and tells the autopilot to hold current - angle. The
// autopilot does the turning, at its own rate. MoveStraight, SetPower and
// SetVelocity have no autopilot equivalent and return an error.
//
// The base talks to the bus through a generic sender component (e.g.
// viam-labs:viamboat:sender) whose DoCommand accepts {"pgn", "priority",
// "dst", "fields"} with canboat field names.
//
// DoCommand (route transfer, which the base API has no method for):
//
//	{"send_waypoints": {"waypoints": [{"lat", "lng"}, ...], "route_name"?, "database_id"?, "route_id"?, "dst"?}}
var N2KBaseModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("n2k-base")

func init() {
	resource.RegisterComponent(
		base.API,
		N2KBaseModel,
		resource.Registration[base.Base, *N2KBaseConfig]{
			Constructor: newN2KBase,
		})
}

// N2KBaseConfig: Sender names the generic component that puts raw PGNs on
// the bus. MovementSensor supplies the compass heading Spin turns from. Dst,
// when set, addresses commands to one device (the autopilot's source
// address) instead of broadcasting them.
type N2KBaseConfig struct {
	Sender         string `json:"sender"`
	MovementSensor string `json:"movement_sensor"`
	Dst            *int   `json:"dst,omitempty"`
}

func (cfg *N2KBaseConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Sender == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "sender")
	}
	if cfg.MovementSensor == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "movement_sensor")
	}
	if cfg.Dst != nil && (*cfg.Dst < 0 || *cfg.Dst > 255) {
		return nil, nil, errors.Errorf("%s: dst out of range: %d", path, *cfg.Dst)
	}
	return []string{cfg.Sender, cfg.MovementSensor}, nil, nil
}

const (
	// pgnHeadingTrackControl is PGN 127237 Heading/Track Control, whose
	// "Heading-To-Steer (Course)" field carries the commanded heading.
	pgnHeadingTrackControl = 127237
	// headingPriority: steering commands are time-sensitive.
	headingPriority = 2
)

// n2kHeadingCmd builds the sender command that asks the autopilot to hold
// headingDeg (degrees true, normalised to [0, 360)). dst < 0 broadcasts.
func n2kHeadingCmd(headingDeg float64, dst int) map[string]interface{} {
	cmd := map[string]interface{}{
		"pgn":      pgnHeadingTrackControl,
		"priority": headingPriority,
		"fields": map[string]interface{}{
			"Steering Mode":             "Heading Control",
			"Heading Reference":         "True",
			"Heading-To-Steer (Course)": normalizeDegrees(headingDeg),
		},
	}
	if dst >= 0 {
		cmd["dst"] = dst
	}
	return cmd
}

func normalizeDegrees(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

func newN2KBase(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (base.Base, error) {
	cfg, err := resource.NativeConfig[*N2KBaseConfig](conf)
	if err != nil {
		return nil, err
	}
	sender, err := generic.FromDependencies(deps, cfg.Sender)
	if err != nil {
		return nil, errors.Wrapf(err, "could not get sender %q", cfg.Sender)
	}
	ms, err := movementsensor.FromDependencies(deps, cfg.MovementSensor)
	if err != nil {
		return nil, errors.Wrapf(err, "could not get movement_sensor %q", cfg.MovementSensor)
	}
	dst := -1
	if cfg.Dst != nil {
		dst = *cfg.Dst
	}
	return &n2kBase{name: conf.ResourceName(), logger: logger, sender: sender, ms: ms, dst: dst}, nil
}

type n2kBase struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	sender resource.Resource
	ms     movementsensor.MovementSensor
	dst    int

	mu sync.Mutex
	// heading is the last heading commanded; nil when not steering.
	heading *float64
}

func (b *n2kBase) Name() resource.Name { return b.name }

func (b *n2kBase) Close(ctx context.Context) error { return nil }

func (b *n2kBase) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// setHeading commands the autopilot to hold headingDeg.
func (b *n2kBase) setHeading(ctx context.Context, headingDeg float64) error {
	if math.IsNaN(headingDeg) || math.IsInf(headingDeg, 0) {
		return errors.Errorf("invalid heading %v", headingDeg)
	}
	h := normalizeDegrees(headingDeg)
	if _, err := b.sender.DoCommand(ctx, n2kHeadingCmd(h, b.dst)); err != nil {
		return errors.Wrap(err, "heading command: sender failed")
	}
	b.mu.Lock()
	b.heading = &h
	b.mu.Unlock()
	return nil
}

// sendWaypoints transfers a waypoint list as an NMEA 2000 Route and WP
// Service transfer (see n2k_route.go).
func (b *n2kBase) sendWaypoints(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	m, _ := raw.(map[string]interface{})
	wps, err := parseWaypointList(m["waypoints"])
	if err != nil {
		return nil, err
	}
	opts, err := parseN2KRouteOptions(raw)
	if err != nil {
		return nil, err
	}
	if opts.Dst < 0 {
		opts.Dst = b.dst
	}
	msgs, err := n2kRouteMessages(wps, opts)
	if err != nil {
		return nil, err
	}
	for i, msg := range msgs {
		if _, err := b.sender.DoCommand(ctx, msg); err != nil {
			return nil, errors.Wrapf(err, "send_waypoints: sender failed on message %d of %d (pgn %v)",
				i+1, len(msgs), msg["pgn"])
		}
	}
	b.logger.Infof("sent %d waypoints as n2k route %q (%d messages)", len(wps), opts.RouteName, len(msgs))
	return map[string]interface{}{
		"ok":         true,
		"waypoints":  len(wps),
		"messages":   len(msgs),
		"route_name": opts.RouteName,
	}, nil
}

// parseWaypointList reads [{"lat": .., "lng": ..}, ...].
func parseWaypointList(raw interface{}) ([]navPoint, error) {
	list, ok := raw.([]interface{})
	if !ok {
		if raw == nil {
			return nil, errors.New("send_waypoints.waypoints is required")
		}
		return nil, errors.Errorf("send_waypoints.waypoints must be a list, got %T", raw)
	}
	out := make([]navPoint, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.Errorf("send_waypoints.waypoints[%d] must be an object", i)
		}
		lat, latOK := m["lat"].(float64)
		lng, lngOK := m["lng"].(float64)
		if !latOK || !lngOK {
			return nil, errors.Errorf("send_waypoints.waypoints[%d] needs numeric lat and lng", i)
		}
		out = append(out, navPoint{Lat: lat, Lng: lng})
	}
	return out, nil
}

func (b *n2kBase) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if raw, ok := cmd["send_waypoints"]; ok {
		return b.sendWaypoints(ctx, raw)
	}
	return nil, resource.ErrDoUnimplemented
}

// Stop forgets the commanded heading. It deliberately sends nothing: putting
// an autopilot into standby mid-passage is a decision for the helm, so the
// autopilot keeps holding the last heading it was given.
func (b *n2kBase) Stop(ctx context.Context, extra map[string]interface{}) error {
	b.mu.Lock()
	b.heading = nil
	b.mu.Unlock()
	return nil
}

func (b *n2kBase) IsMoving(ctx context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.heading != nil, nil
}

var errHeadingOnly = errors.New("the n2k base steers an autopilot by heading: use Spin")

func (b *n2kBase) MoveStraight(ctx context.Context, distanceMm int, mmPerSec float64, extra map[string]interface{}) error {
	return errHeadingOnly
}

// Spin turns the boat angleDeg from its current heading (positive = left)
// by asking the autopilot to hold current - angleDeg. It returns once the
// command is sent: the autopilot carries out the turn and keeps holding the
// heading. degsPerSec is ignored (the autopilot picks its own turn rate),
// except that 0 means Stop, as the base API defines.
func (b *n2kBase) Spin(ctx context.Context, angleDeg, degsPerSec float64, extra map[string]interface{}) error {
	if degsPerSec == 0 {
		return b.Stop(ctx, extra)
	}
	if math.IsNaN(angleDeg) || math.IsInf(angleDeg, 0) {
		return errors.Errorf("invalid spin angle %v", angleDeg)
	}
	cur, err := b.ms.CompassHeading(ctx, extra)
	if err != nil {
		return errors.Wrap(err, "spin: reading compass heading")
	}
	if math.IsNaN(cur) {
		return errors.New("spin: movement sensor has no compass heading")
	}
	return b.setHeading(ctx, cur-angleDeg)
}

func (b *n2kBase) SetPower(ctx context.Context, linear, angular r3.Vector, extra map[string]interface{}) error {
	return errHeadingOnly
}

func (b *n2kBase) SetVelocity(ctx context.Context, linear, angular r3.Vector, extra map[string]interface{}) error {
	return errHeadingOnly
}

func (b *n2kBase) Properties(ctx context.Context, extra map[string]interface{}) (base.Properties, error) {
	return base.Properties{}, nil
}

func (b *n2kBase) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, nil
}
