package vc

import (
	"context"
	"math"
	"sync"

	"github.com/golang/geo/r3"
	"github.com/pkg/errors"

	"go.viam.com/rdk/components/base"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
)

// N2KBaseModel is a base that steers a boat through an NMEA 2000 autopilot.
//
// A boat isn't driven with MoveStraight/Spin/SetVelocity: the nav service
// (or anything else) tells the base which heading to hold, and the autopilot
// does the steering. The base talks to the bus through a generic sender
// component (e.g. viam-labs:viamboat:sender) whose DoCommand accepts
// {"pgn", "priority", "dst", "fields"} with canboat field names.
//
// DoCommand:
//
//	{"set_heading": <degrees true>}
//	{"send_waypoints": {"waypoints": [{"lat", "lng"}, ...], "route_name"?, "database_id"?, "route_id"?, "dst"?}}
//	{"get_heading": true}
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
// the bus. Dst, when set, addresses heading commands to one device (the
// autopilot's source address) instead of broadcasting them.
type N2KBaseConfig struct {
	Sender string `json:"sender"`
	Dst    *int   `json:"dst,omitempty"`
}

func (cfg *N2KBaseConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Sender == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "sender")
	}
	if cfg.Dst != nil && (*cfg.Dst < 0 || *cfg.Dst > 255) {
		return nil, nil, errors.Errorf("%s: dst out of range: %d", path, *cfg.Dst)
	}
	return []string{cfg.Sender}, nil, nil
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
	dst := -1
	if cfg.Dst != nil {
		dst = *cfg.Dst
	}
	return &n2kBase{name: conf.ResourceName(), logger: logger, sender: sender, dst: dst}, nil
}

type n2kBase struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	sender resource.Resource
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

// SetHeading commands the autopilot to hold headingDeg (degrees true).
func (b *n2kBase) SetHeading(ctx context.Context, headingDeg float64) error {
	if math.IsNaN(headingDeg) || math.IsInf(headingDeg, 0) {
		return errors.Errorf("invalid heading %v", headingDeg)
	}
	h := normalizeDegrees(headingDeg)
	if _, err := b.sender.DoCommand(ctx, n2kHeadingCmd(h, b.dst)); err != nil {
		return errors.Wrap(err, "set_heading: sender failed")
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
	if raw, ok := cmd["set_heading"]; ok {
		h, ok := raw.(float64)
		if !ok {
			return nil, errors.Errorf("set_heading must be a number of degrees, got %T", raw)
		}
		if err := b.SetHeading(ctx, h); err != nil {
			return nil, err
		}
		return map[string]interface{}{"heading": normalizeDegrees(h)}, nil
	}
	if raw, ok := cmd["send_waypoints"]; ok {
		return b.sendWaypoints(ctx, raw)
	}
	if _, ok := cmd["get_heading"]; ok {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.heading == nil {
			return map[string]interface{}{"heading": nil}, nil
		}
		return map[string]interface{}{"heading": *b.heading}, nil
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

var errHeadingOnly = errors.New("the n2k base is steered by heading: use DoCommand {\"set_heading\": degrees}")

func (b *n2kBase) MoveStraight(ctx context.Context, distanceMm int, mmPerSec float64, extra map[string]interface{}) error {
	return errHeadingOnly
}

func (b *n2kBase) Spin(ctx context.Context, angleDeg, degsPerSec float64, extra map[string]interface{}) error {
	return errHeadingOnly
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
