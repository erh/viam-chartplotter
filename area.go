package vc

import (
	"context"
	"math"
	"time"

	"github.com/pkg/errors"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// AreaModel is a generic component that describes a geographic region the
// chartplotter draws on the map: either an explicit GeoJSON geometry or a
// center point + radius, plus a display color. The chartplotter enumerates all
// area components on the machine (probing each generic component with the
// {"get_area": true} DoCommand) and renders them as a toggleable overlay.
var AreaModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("area")

// defaultAreaColor is used when a config omits `color`. A translucent fill of
// this color is drawn by the frontend; here we just carry the stroke color.
const defaultAreaColor = "#ff3b30"

// circleSteps is the number of vertices used to approximate a center+radius
// circle as a GeoJSON polygon.
const circleSteps = 64

// metersPerDegLat is the (approximate, spherical) meters covered by one degree
// of latitude. Good enough for drawing an area outline on a chart.
const metersPerDegLat = 111320.0

// nmToMeters converts nautical miles (the `radius_nm` unit) to meters.
const nmToMeters = 1852.0

func init() {
	resource.RegisterComponent(
		generic.API,
		AreaModel,
		resource.Registration[resource.Resource, *AreaConfig]{
			Constructor: newArea,
		})
}

// AreaConfig defines a region. Supply either `geojson` (a GeoJSON Geometry,
// Feature, or FeatureCollection) or `center` ([lat, lng]) + `radius_nm`, or
// both. `color` is the display color (CSS color string); it defaults to
// defaultAreaColor and is applied to any feature that doesn't already carry its
// own `color` property.
//
// For a center+radius region, `bearing_min` / `bearing_max` optionally restrict
// it to a compass sector (a pie slice) instead of a full circle: degrees
// clockwise from true north, drawn from bearing_min around to bearing_max. Set
// both or neither; bearing_min > bearing_max wraps through north (e.g. 315→45
// is the northern sector). This makes wedge regions (e.g. "150 nm south") easy
// without hand-writing GeoJSON.
//
// StartDate/EndDate optionally limit *when* the chartplotter draws the area, as
// inclusive month-day values (MM-DD, no year and no time) so the window recurs
// every year. Either may be set on its own for an open-ended range, and a range
// may wrap across the year end (e.g. start "12-01", end "02-01"). The frontend
// hides the area on days outside the window (compared against the local date).
//
// GeoJSON is a plain JSON object (Geometry, Feature, or FeatureCollection).
// It's typed as map[string]interface{} rather than json.RawMessage so Viam's
// attribute decoder (mapstructure) can populate it from the config's nested
// object — a []byte-based type fails to convert.
type AreaConfig struct {
	GeoJSON    map[string]interface{} `json:"geojson,omitempty"`
	Center     []float64              `json:"center,omitempty"`
	RadiusNM   float64                `json:"radius_nm,omitempty"`
	BearingMin *float64               `json:"bearing_min,omitempty"`
	BearingMax *float64               `json:"bearing_max,omitempty"`
	Color      string                 `json:"color,omitempty"`
	StartDate  string                 `json:"start_date,omitempty"`
	EndDate    string                 `json:"end_date,omitempty"`
}

// areaDateLayout is the recurring month-day format (no year, no time) used by
// StartDate/EndDate.
const areaDateLayout = "01-02"

// Validate ensures the config describes at least one region, that any center is
// a well-formed [lat, lng] pair, and that any start/end dates parse as MM-DD.
// A start after end is allowed — it means the window wraps across the year end.
// Area components have no dependencies.
func (cfg *AreaConfig) Validate(path string) ([]string, []string, error) {
	if len(cfg.Center) != 0 && len(cfg.Center) != 2 {
		return nil, nil, errors.New("area `center` must be [lat, lng]")
	}
	hasGeo := len(cfg.GeoJSON) > 0
	hasCircle := len(cfg.Center) == 2 && cfg.RadiusNM > 0
	if !hasGeo && !hasCircle {
		return nil, nil, errors.New("area requires either `geojson` or `center` ([lat,lng]) + `radius_nm`")
	}
	if err := validateBearings(cfg.BearingMin, cfg.BearingMax); err != nil {
		return nil, nil, err
	}
	if err := validateAreaDate("start_date", cfg.StartDate); err != nil {
		return nil, nil, err
	}
	if err := validateAreaDate("end_date", cfg.EndDate); err != nil {
		return nil, nil, err
	}
	return nil, nil, nil
}

// validateBearings checks the optional compass sector. Both must be set or
// neither, and each must be in [0, 360] degrees. (min > max is allowed — it
// just wraps the sector through north.)
func validateBearings(min, max *float64) error {
	if (min == nil) != (max == nil) {
		return errors.New("area `bearing_min` and `bearing_max` must be set together")
	}
	for _, b := range []*float64{min, max} {
		if b != nil && (*b < 0 || *b > 360) {
			return errors.Errorf("area bearings must be 0..360 degrees, got %v", *b)
		}
	}
	return nil
}

// validateAreaDate checks an optional MM-DD month-day value; empty is allowed.
// field names the config attribute for error messages.
func validateAreaDate(field, val string) error {
	if val == "" {
		return nil
	}
	if _, err := time.Parse(areaDateLayout, val); err != nil {
		return errors.Wrapf(err, "area `%s` must be an MM-DD month-day (no year)", field)
	}
	return nil
}

func newArea(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*AreaConfig](conf)
	if err != nil {
		return nil, err
	}
	color := cfg.Color
	if color == "" {
		color = defaultAreaColor
	}
	fc, err := buildAreaFeatureCollection(cfg, color)
	if err != nil {
		return nil, err
	}
	logger.Infof("area %q ready: %d feature(s), color=%s, start=%q, end=%q",
		conf.ResourceName().Name, len(fc["features"].([]interface{})), color, cfg.StartDate, cfg.EndDate)
	return &areaResource{
		name:      conf.ResourceName(),
		color:     color,
		geojson:   fc,
		startDate: cfg.StartDate,
		endDate:   cfg.EndDate,
	}, nil
}

// buildAreaFeatureCollection normalizes the config into a single GeoJSON
// FeatureCollection. Every feature carries a `color` property so the frontend
// can render each region independently.
func buildAreaFeatureCollection(cfg *AreaConfig, color string) (map[string]interface{}, error) {
	var features []interface{}
	if len(cfg.GeoJSON) > 0 {
		f, err := featuresFromGeoJSON(cfg.GeoJSON, color)
		if err != nil {
			return nil, err
		}
		features = append(features, f...)
	}
	if len(cfg.Center) == 2 && cfg.RadiusNM > 0 {
		features = append(features, sectorFeature(
			cfg.Center[0], cfg.Center[1], cfg.RadiusNM*nmToMeters, cfg.BearingMin, cfg.BearingMax, color))
	}
	if features == nil {
		features = []interface{}{}
	}
	return map[string]interface{}{
		"type":     "FeatureCollection",
		"features": features,
	}, nil
}

// featuresFromGeoJSON accepts a GeoJSON Geometry, Feature, or FeatureCollection
// (as a decoded object) and returns a flat list of Features, each with a
// `color` property (the configured default is applied only where a feature
// doesn't set its own).
func featuresFromGeoJSON(obj map[string]interface{}, color string) ([]interface{}, error) {
	t, _ := obj["type"].(string)
	switch t {
	case "":
		return nil, errors.New("geojson missing `type`")
	case "FeatureCollection":
		feats, _ := obj["features"].([]interface{})
		out := make([]interface{}, 0, len(feats))
		for _, f := range feats {
			fm, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			setFeatureColor(fm, color)
			out = append(out, fm)
		}
		return out, nil
	case "Feature":
		setFeatureColor(obj, color)
		return []interface{}{obj}, nil
	default:
		// Treat anything else as a bare geometry and wrap it in a Feature.
		feat := map[string]interface{}{
			"type":       "Feature",
			"geometry":   obj,
			"properties": map[string]interface{}{"color": color},
		}
		return []interface{}{feat}, nil
	}
}

// setFeatureColor sets `properties.color` on a Feature when it isn't already
// present, creating the properties object if needed.
func setFeatureColor(feat map[string]interface{}, color string) {
	props, ok := feat["properties"].(map[string]interface{})
	if !ok || props == nil {
		props = map[string]interface{}{}
		feat["properties"] = props
	}
	if _, exists := props["color"]; !exists {
		props["color"] = color
	}
}

// sectorFeature builds a GeoJSON Polygon Feature for a center (lat, lng) +
// radius (meters) region. With no bearings it's a full circle; with bearingMin
// / bearingMax (compass degrees, clockwise from north) it's a pie slice from
// bearingMin around to bearingMax — wrapping through north when min > max. Uses
// an equirectangular approximation, accurate enough for chart display.
func sectorFeature(centerLat, centerLng, radiusM float64, bearingMin, bearingMax *float64, color string) map[string]interface{} {
	cosLat := math.Cos(centerLat * math.Pi / 180)
	if math.Abs(cosLat) < 1e-6 {
		cosLat = 1e-6
	}
	// arcPoint returns the [lng, lat] on the radius circle at a compass bearing.
	arcPoint := func(bearingDeg float64) []interface{} {
		b := bearingDeg * math.Pi / 180
		dLat := (radiusM / metersPerDegLat) * math.Cos(b)          // north component
		dLng := (radiusM / (metersPerDegLat * cosLat)) * math.Sin(b) // east component
		return []interface{}{centerLng + dLng, centerLat + dLat}
	}

	props := map[string]interface{}{
		"color":     color,
		"radius_nm": radiusM / nmToMeters,
	}

	var ring []interface{}
	if bearingMin == nil || bearingMax == nil {
		// Full circle: closed ring of vertices around the center.
		ring = make([]interface{}, 0, circleSteps+1)
		for i := 0; i <= circleSteps; i++ {
			ring = append(ring, arcPoint(360*float64(i)/circleSteps))
		}
	} else {
		// Pie slice: apex at the center, arc from min to max (wrapping through
		// north when min > max), back to the apex.
		start := *bearingMin
		end := *bearingMax
		if end <= start {
			end += 360
		}
		props["bearing_min"] = *bearingMin
		props["bearing_max"] = *bearingMax
		// One arc vertex roughly every 360/circleSteps degrees, at least 2.
		steps := int(math.Ceil((end - start) / (360.0 / circleSteps)))
		if steps < 2 {
			steps = 2
		}
		ring = make([]interface{}, 0, steps+3)
		apex := []interface{}{centerLng, centerLat}
		ring = append(ring, apex)
		for i := 0; i <= steps; i++ {
			ring = append(ring, arcPoint(start+(end-start)*float64(i)/float64(steps)))
		}
		ring = append(ring, apex) // close back to the apex
	}

	return map[string]interface{}{
		"type": "Feature",
		"geometry": map[string]interface{}{
			"type":        "Polygon",
			"coordinates": []interface{}{ring},
		},
		"properties": props,
	}
}

type areaResource struct {
	resource.AlwaysRebuild

	name      resource.Name
	color     string
	geojson   map[string]interface{}
	startDate string
	endDate   string
}

func (r *areaResource) Name() resource.Name { return r.name }

// Status satisfies resource.Resource; areas are static so there's nothing
// meaningful to report.
func (r *areaResource) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (r *areaResource) Close(ctx context.Context) error { return nil }

// DoCommand returns the area definition. The chartplotter probes every generic
// component with {"get_area": true}; components that aren't areas either error
// or return no `geojson`, and are skipped. Any command returns the definition,
// so the exact key doesn't matter.
func (r *areaResource) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{
		"name":       r.name.Name,
		"color":      r.color,
		"geojson":    r.geojson,
		"start_date": r.startDate,
		"end_date":   r.endDate,
	}, nil
}
