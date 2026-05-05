package vc

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"

	geo "github.com/kellydunn/golang-geo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"
	"go.viam.com/rdk/spatialmath"
)

var NavModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("nav")

func init() {
	resource.RegisterService(
		navigation.API,
		NavModel,
		resource.Registration[navigation.Service, *NavConfig]{
			Constructor: newNav,
		})
}

// NavConfig is the config for the chartplotter navigation service.
// MovementSensor is optional; when set, Location() reports the live
// position/heading of that movement sensor.
//
// DataPath, when set, is used as the JSON file that mirrors the waypoint
// list across restarts. When empty, waypoints persist to
// "<user-cache-dir>/viam-chartplotter/nav/<resource_name>.json".
type NavConfig struct {
	MovementSensor string `json:"movement_sensor,omitempty"`
	DataPath       string `json:"data_path,omitempty"`
}

// Validate ensures all parts of the config are valid and returns the
// implicit dependencies required by the service.
func (cfg *NavConfig) Validate(path string) ([]string, error) {
	var deps []string
	if cfg.MovementSensor != "" {
		deps = append(deps, cfg.MovementSensor)
	}
	return deps, nil
}

func newNav(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (navigation.Service, error) {
	cfg, err := resource.NativeConfig[*NavConfig](conf)
	if err != nil {
		return nil, err
	}

	dataPath := cfg.DataPath
	if dataPath == "" {
		dataPath = filepath.Join(resolveCacheRoot(""), "nav", conf.ResourceName().Name+".json")
	}
	store, err := newDiskNavStore(dataPath)
	if err != nil {
		return nil, err
	}

	svc := &navService{
		name:   conf.ResourceName(),
		logger: logger,
		store:  store,
	}
	svc.mode.Store(uint32(navigation.ModeManual))
	logger.Infof("nav waypoints persisted at %s", dataPath)

	if cfg.MovementSensor != "" {
		ms, err := movementsensor.FromDependencies(deps, cfg.MovementSensor)
		if err != nil {
			return nil, errors.Wrapf(err, "could not get movement_sensor %q", cfg.MovementSensor)
		}
		svc.ms = ms
	}

	return svc, nil
}

type navService struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu    sync.Mutex
	store navigation.NavStore
	ms    movementsensor.MovementSensor

	// mode is held atomically so reads don't need to take the mutex.
	mode atomic.Uint32
}

func (s *navService) Name() resource.Name { return s.name }

func (s *navService) Mode(ctx context.Context, extra map[string]interface{}) (navigation.Mode, error) {
	return navigation.Mode(s.mode.Load()), nil
}

func (s *navService) SetMode(ctx context.Context, mode navigation.Mode, extra map[string]interface{}) error {
	switch mode {
	case navigation.ModeManual, navigation.ModeWaypoint, navigation.ModeExplore:
	default:
		return errors.Errorf("unknown navigation mode %v", mode)
	}
	s.mode.Store(uint32(mode))
	return nil
}

func (s *navService) Location(ctx context.Context, extra map[string]interface{}) (*spatialmath.GeoPose, error) {
	if s.ms == nil {
		return spatialmath.NewGeoPose(geo.NewPoint(0, 0), 0), nil
	}
	pt, _, err := s.ms.Position(ctx, extra)
	if err != nil {
		return nil, err
	}
	heading, err := s.ms.CompassHeading(ctx, extra)
	if err != nil {
		// Heading may not be supported on every movement_sensor; fall back to 0.
		heading = 0
	}
	return spatialmath.NewGeoPose(pt, heading), nil
}

func (s *navService) Waypoints(ctx context.Context, extra map[string]interface{}) ([]navigation.Waypoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Waypoints(ctx)
}

func (s *navService) AddWaypoint(ctx context.Context, point *geo.Point, extra map[string]interface{}) error {
	if point == nil {
		return errors.New("waypoint location is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.store.AddWaypoint(ctx, point)
	return err
}

func (s *navService) RemoveWaypoint(ctx context.Context, id primitive.ObjectID, extra map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.RemoveWaypoint(ctx, id)
}

func (s *navService) Obstacles(ctx context.Context, extra map[string]interface{}) ([]*spatialmath.GeoGeometry, error) {
	return nil, nil
}

func (s *navService) Paths(ctx context.Context, extra map[string]interface{}) ([]*navigation.Path, error) {
	return nil, nil
}

func (s *navService) Properties(ctx context.Context) (navigation.Properties, error) {
	return navigation.Properties{MapType: navigation.GPSMap}, nil
}

// DoCommand exposes service-specific commands that don't have a first-class
// gRPC method on the navigation API. Currently:
//
//	{"move_waypoint": {"id": "<hex>", "lat": <float>, "lng": <float>}}
//
// updates an existing waypoint in place (preserving its ID and order).
func (s *navService) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	raw, ok := cmd["move_waypoint"]
	if !ok {
		return nil, resource.ErrDoUnimplemented
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("move_waypoint payload must be an object")
	}
	idStr, _ := m["id"].(string)
	if idStr == "" {
		return nil, errors.New("move_waypoint.id is required")
	}
	id, err := primitive.ObjectIDFromHex(idStr)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid move_waypoint.id %q", idStr)
	}
	lat, latOK := m["lat"].(float64)
	lng, lngOK := m["lng"].(float64)
	if !latOK || !lngOK {
		return nil, errors.New("move_waypoint.lat and move_waypoint.lng are required numbers")
	}
	mover, ok := s.store.(interface {
		MoveWaypoint(ctx context.Context, id primitive.ObjectID, point *geo.Point) error
	})
	if !ok {
		return nil, errors.New("waypoint store does not support move")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := mover.MoveWaypoint(ctx, id, geo.NewPoint(lat, lng)); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true}, nil
}

func (s *navService) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Close(ctx)
}
