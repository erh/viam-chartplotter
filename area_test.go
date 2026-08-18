package vc

import (
	"encoding/json"
	"math"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// geo parses a GeoJSON string into the decoded-object form AreaConfig.GeoJSON
// now uses (matching how Viam's attribute decoder delivers it).
func parseGeo(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test geojson %q: %v", s, err)
	}
	return m
}

func TestAreaConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     AreaConfig
		wantErr bool
	}{
		{"geojson only", AreaConfig{GeoJSON: parseGeo(t, `{"type":"Point","coordinates":[1,2]}`)}, false},
		{"circle only", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500}, false},
		{"both", AreaConfig{GeoJSON: parseGeo(t, `{"type":"Point","coordinates":[1,2]}`), Center: []float64{40, -70}, RadiusNM: 500}, false},
		{"neither", AreaConfig{}, true},
		{"circle missing radius", AreaConfig{Center: []float64{40, -70}}, true},
		{"bad center length", AreaConfig{Center: []float64{40}, RadiusNM: 500}, true},
		{"valid date range", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, StartDate: "06-01", EndDate: "09-01"}, false},
		{"open-ended start", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, StartDate: "06-01"}, false},
		{"open-ended end", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, EndDate: "09-01"}, false},
		{"wrap-around range allowed", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, StartDate: "12-01", EndDate: "02-01"}, false},
		{"bad date format", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, StartDate: "6/1"}, true},
		{"year included rejected", AreaConfig{Center: []float64{40, -70}, RadiusNM: 500, StartDate: "2026-06-01"}, true},
		{"sector bearings", AreaConfig{Center: []float64{40, -70}, RadiusNM: 150, BearingMin: ptr(135.0), BearingMax: ptr(225.0)}, false},
		{"wrap-around bearings", AreaConfig{Center: []float64{40, -70}, RadiusNM: 150, BearingMin: ptr(315.0), BearingMax: ptr(45.0)}, false},
		{"only one bearing set", AreaConfig{Center: []float64{40, -70}, RadiusNM: 150, BearingMin: ptr(135.0)}, true},
		{"bearing out of range", AreaConfig{Center: []float64{40, -70}, RadiusNM: 150, BearingMin: ptr(-1.0), BearingMax: ptr(45.0)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.cfg.Validate("area")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// features returns the feature list from a built FeatureCollection.
func features(t *testing.T, cfg *AreaConfig, color string) []interface{} {
	t.Helper()
	fc, err := buildAreaFeatureCollection(cfg, color)
	if err != nil {
		t.Fatalf("buildAreaFeatureCollection: %v", err)
	}
	if fc["type"] != "FeatureCollection" {
		t.Fatalf("type = %v, want FeatureCollection", fc["type"])
	}
	feats, ok := fc["features"].([]interface{})
	if !ok {
		t.Fatalf("features is not a slice: %T", fc["features"])
	}
	return feats
}

func featureColor(t *testing.T, feat interface{}) string {
	t.Helper()
	fm, ok := feat.(map[string]interface{})
	if !ok {
		t.Fatalf("feature not an object: %T", feat)
	}
	if fm["type"] != "Feature" {
		t.Fatalf("feature type = %v, want Feature", fm["type"])
	}
	props, ok := fm["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("feature has no properties object")
	}
	c, _ := props["color"].(string)
	return c
}

func TestBuildAreaFeatureCollectionGeoJSON(t *testing.T) {
	t.Run("bare geometry is wrapped and colored", func(t *testing.T) {
		cfg := &AreaConfig{GeoJSON: parseGeo(t, `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`)}
		feats := features(t, cfg, "#00ff00")
		if len(feats) != 1 {
			t.Fatalf("got %d features, want 1", len(feats))
		}
		if got := featureColor(t, feats[0]); got != "#00ff00" {
			t.Fatalf("color = %q, want #00ff00", got)
		}
	})

	t.Run("feature keeps its own color", func(t *testing.T) {
		cfg := &AreaConfig{GeoJSON: parseGeo(t,
			`{"type":"Feature","properties":{"color":"#123456"},"geometry":{"type":"Point","coordinates":[1,2]}}`)}
		feats := features(t, cfg, "#00ff00")
		if got := featureColor(t, feats[0]); got != "#123456" {
			t.Fatalf("color = %q, want #123456 (own color preserved)", got)
		}
	})

	t.Run("feature collection flattened", func(t *testing.T) {
		cfg := &AreaConfig{GeoJSON: parseGeo(t, `{
			"type":"FeatureCollection",
			"features":[
				{"type":"Feature","properties":{},"geometry":{"type":"Point","coordinates":[0,0]}},
				{"type":"Feature","properties":{"color":"#abcdef"},"geometry":{"type":"Point","coordinates":[1,1]}}
			]}`)}
		feats := features(t, cfg, "#00ff00")
		if len(feats) != 2 {
			t.Fatalf("got %d features, want 2", len(feats))
		}
		if got := featureColor(t, feats[0]); got != "#00ff00" {
			t.Fatalf("feature[0] color = %q, want default #00ff00", got)
		}
		if got := featureColor(t, feats[1]); got != "#abcdef" {
			t.Fatalf("feature[1] color = %q, want #abcdef", got)
		}
	})

	t.Run("missing type errors", func(t *testing.T) {
		cfg := &AreaConfig{GeoJSON: parseGeo(t, `{"coordinates":[1,2]}`)}
		if _, err := buildAreaFeatureCollection(cfg, "#fff"); err == nil {
			t.Fatal("expected error for geojson missing type")
		}
	})
}

// distFromCenterM returns the equirectangular distance (m) of a [lng,lat] point
// from (40, -70).
func distFromCenterM(t *testing.T, p interface{}) float64 {
	t.Helper()
	pt := p.([]interface{})
	lng := pt[0].(float64)
	lat := pt[1].(float64)
	dLatM := (lat - 40) * metersPerDegLat
	dLngM := (lng - -70) * metersPerDegLat * math.Cos(40*math.Pi/180)
	return math.Hypot(dLatM, dLngM)
}

func TestCircleFeature(t *testing.T) {
	cfg := &AreaConfig{Center: []float64{40, -70}, RadiusNM: 10}
	feats := features(t, cfg, "#ff0000")
	if len(feats) != 1 {
		t.Fatalf("got %d features, want 1", len(feats))
	}
	fm := feats[0].(map[string]interface{})
	geom := fm["geometry"].(map[string]interface{})
	if geom["type"] != "Polygon" {
		t.Fatalf("geometry type = %v, want Polygon", geom["type"])
	}
	rings := geom["coordinates"].([]interface{})
	ring := rings[0].([]interface{})
	if len(ring) != circleSteps+1 {
		t.Fatalf("ring has %d points, want %d", len(ring), circleSteps+1)
	}
	// Ring must be closed: first point equals last.
	first := ring[0].([]interface{})
	last := ring[len(ring)-1].([]interface{})
	if first[0] != last[0] || first[1] != last[1] {
		t.Fatalf("ring not closed: first=%v last=%v", first, last)
	}
	// Every vertex should be ~10 nm from the center (allow 2% slop for the
	// equirectangular approximation).
	wantM := 10 * nmToMeters
	for _, p := range ring {
		if d := distFromCenterM(t, p); math.Abs(d-wantM) > wantM*0.02 {
			t.Fatalf("vertex distance %.1f m, want ~%.0f m", d, wantM)
		}
	}
}

func TestSectorFeature(t *testing.T) {
	bmin, bmax := 135.0, 225.0
	cfg := &AreaConfig{Center: []float64{40, -70}, RadiusNM: 10, BearingMin: &bmin, BearingMax: &bmax}
	feats := features(t, cfg, "#00ff00")
	fm := feats[0].(map[string]interface{})
	geom := fm["geometry"].(map[string]interface{})
	ring := geom["coordinates"].([]interface{})[0].([]interface{})

	// Apex is the center, at both the start and end of the ring.
	first := ring[0].([]interface{})
	last := ring[len(ring)-1].([]interface{})
	if first[0] != -70.0 || first[1] != 40.0 {
		t.Fatalf("apex = %v, want [-70,40]", first)
	}
	if first[0] != last[0] || first[1] != last[1] {
		t.Fatalf("ring not closed: first=%v last=%v", first, last)
	}
	// The apex is at distance 0; the arc vertices sit on the radius. Confirm
	// the sector faces south: all arc vertices should be south of the center.
	arc := ring[1 : len(ring)-1]
	for _, p := range arc {
		if d := distFromCenterM(t, p); math.Abs(d-10*nmToMeters) > 10*nmToMeters*0.02 {
			t.Fatalf("arc vertex distance %.1f m, want ~%.0f m", d, 10*nmToMeters)
		}
		if p.([]interface{})[1].(float64) >= 40.0 {
			t.Fatalf("arc vertex %v not south of center", p)
		}
	}
	// The recorded bearings surface in properties for reference.
	props := fm["properties"].(map[string]interface{})
	if props["bearing_min"] != 135.0 || props["bearing_max"] != 225.0 {
		t.Fatalf("bearings in props = %v/%v, want 135/225", props["bearing_min"], props["bearing_max"])
	}
}
