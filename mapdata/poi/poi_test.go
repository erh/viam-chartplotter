package poi

import (
	"strings"
	"testing"
	"time"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

func feature(geomJSON string, props map[string]any) Feature {
	f := Feature{Properties: props}
	if geomJSON != "" {
		f.Geometry = &Geometry{Type: "Point", Coordinates: []byte(geomJSON)}
	}
	return f
}

func TestRepresentativePoint(t *testing.T) {
	cases := []struct {
		name    string
		coords  string
		wantLng float64
		wantLat float64
		wantOK  bool
	}{
		{"point", `[-71.5,41.2]`, -71.5, 41.2, true},
		{"point with elevation", `[-71.5,41.2,0]`, -71.5, 41.2, true},
		{"linestring", `[[-72,41],[-70,43]]`, -71, 42, true},
		{"polygon", `[[[-72,41],[-70,41],[-70,43],[-72,43],[-72,41]]]`, -71, 42, true},
		{"multipolygon", `[[[[-72,41],[-70,41],[-70,43],[-72,43],[-72,41]]]]`, -71, 42, true},
		{"empty", `[]`, 0, 0, false},
		{"junk", `"nope"`, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Geometry{Coordinates: []byte(tc.coords)}
			lng, lat, ok := g.RepresentativePoint()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (lng != tc.wantLng || lat != tc.wantLat) {
				t.Fatalf("got %v,%v want %v,%v", lng, lat, tc.wantLng, tc.wantLat)
			}
		})
	}
}

func TestWreckAdapterNamesAndClass(t *testing.T) {
	cases := []struct {
		name      string
		props     map[string]any
		wantName  string
		wantClass string
	}{
		{
			name:      "awois spelling",
			props:     map[string]any{"vesslterms": "ANDREA DORIA", "feature_type": "Wreck"},
			wantName:  "ANDREA DORIA",
			wantClass: ClassWreck,
		},
		{
			name:      "state export spelling",
			props:     map[string]any{"Vessel_Name": "USS Monitor", "FeatureType": "wreck"},
			wantName:  "USS Monitor",
			wantClass: ClassWreck,
		},
		{
			name:      "obstruction",
			props:     map[string]any{"feature_type": "Obstruction"},
			wantName:  "Obstruction",
			wantClass: ClassObstruction,
		},
		{
			name:      "no feature type is a wreck report",
			props:     map[string]any{},
			wantName:  "Wreck",
			wantClass: ClassWreck,
		},
		{
			name:      "placeholder name is not a name",
			props:     map[string]any{"vesslterms": "UNKNOWN", "feature_type": "Wreck"},
			wantName:  "Wreck",
			wantClass: ClassWreck,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := wreckAdapter(feature(`[-71.5,41.2]`, tc.props))
			if !ok {
				t.Fatal("adapter dropped the feature")
			}
			if p.Name != tc.wantName {
				t.Errorf("name = %q, want %q", p.Name, tc.wantName)
			}
			if p.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", p.Class, tc.wantClass)
			}
		})
	}
}

func TestWreckAdapterFallsBackToCoordinateColumns(t *testing.T) {
	// An export with no geometry, positions in decimal-degree columns — the
	// shape AWOIS CSV conversions arrive in.
	p, ok := wreckAdapter(feature("", map[string]any{
		"LATDEC": 40.4926, "LONDEC": -69.8511, "VESSLTERMS": "ANDREA DORIA",
	}))
	if !ok {
		t.Fatal("adapter dropped the feature")
	}
	if p.Lat != 40.4926 || p.Lng != -69.8511 {
		t.Fatalf("got %v,%v", p.Lat, p.Lng)
	}
}

func TestWreckAdapterDetail(t *testing.T) {
	p, _ := wreckAdapter(feature(`[-69.85,40.49]`, map[string]any{
		"vesslterms": "ANDREA DORIA", "feature_type": "Wreck",
		"yearsunk": 1956, "depth": 240,
	}))
	if !strings.Contains(p.Detail, "sank 1956") || !strings.Contains(p.Detail, "240 ft") {
		t.Fatalf("detail = %q", p.Detail)
	}
}

func TestWreckAdapterIgnoresUnknownYearAndDepth(t *testing.T) {
	p, _ := wreckAdapter(feature(`[-69.85,40.49]`, map[string]any{
		"feature_type": "Wreck", "yearsunk": 0, "depth": 0,
	}))
	if p.Detail != "Wreck" {
		t.Fatalf("detail = %q, want just the feature type", p.Detail)
	}
}

func TestReefAdapter(t *testing.T) {
	p, ok := reefAdapter(feature(`[-80.1,25.8]`, map[string]any{
		"REEFNAME": "Miami Reef Site 4", "Material": "Concrete culverts",
		"depth": 62, "state": "FL", "reefid": "FL-0042",
	}))
	if !ok {
		t.Fatal("adapter dropped the feature")
	}
	if p.Class != ClassReef || p.Name != "Miami Reef Site 4" {
		t.Fatalf("got class=%q name=%q", p.Class, p.Name)
	}
	if p.ExternalID != "FL-0042" {
		t.Errorf("external id = %q", p.ExternalID)
	}
	for _, want := range []string{"Concrete culverts", "62 ft", "FL"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("detail %q missing %q", p.Detail, want)
		}
	}
	if p.Attributes["material"] != "Concrete culverts" {
		t.Errorf("attributes = %v", p.Attributes)
	}
}

func TestReefAdapterUnnamed(t *testing.T) {
	p, ok := reefAdapter(feature(`[-80.1,25.8]`, map[string]any{}))
	if !ok || p.Name != "Artificial reef" {
		t.Fatalf("ok=%v name=%q", ok, p.Name)
	}
}

func TestPlatformAdapterNamesByBlock(t *testing.T) {
	p, ok := platformAdapter(feature(`[-91.2,28.5]`, map[string]any{
		"AREA_CODE": "EI", "BLOCK_NUMBER": "322", "WATER_DEPTH": 210,
	}))
	if !ok {
		t.Fatal("adapter dropped the feature")
	}
	if p.Name != "EI 322" {
		t.Errorf("name = %q", p.Name)
	}
	if !strings.Contains(p.Detail, "210 ft") {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestDocRejectsUnusablePositions(t *testing.T) {
	now := time.Now()
	for _, p := range []Point{
		{Class: ClassWreck, Lat: 0, Lng: 0},    // null island: a missing position
		{Class: ClassWreck, Lat: 91, Lng: -70}, // out of range
		{Class: "", Lat: 41, Lng: -71},         // no class
	} {
		if _, ok := Doc("ocs-wrecks", p, now); ok {
			t.Errorf("Doc accepted %+v", p)
		}
	}
}

func TestDocShape(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	doc, ok := Doc("ocs-wrecks", Point{
		ExternalID: "12345", Name: "ANDREA DORIA", Class: ClassWreck,
		Lat: 40.4926, Lng: -69.8511, Detail: "sank 1956",
		Attributes: map[string]any{"depth": 240},
	}, now)
	if !ok {
		t.Fatal("Doc rejected a good point")
	}
	if doc.Cell != "ocs-wrecks" || doc.ObjectClass != ClassWreck || doc.Kind != "point" {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.Name != "ANDREA DORIA" || doc.Attributes["OBJNAM"] != "ANDREA DORIA" {
		t.Errorf("name not carried into OBJNAM: %+v", doc.Attributes)
	}
	if doc.Attributes[AttrDataset] != "ocs-wrecks" || doc.Attributes[AttrIngest] != now {
		t.Errorf("provenance attributes = %+v", doc.Attributes)
	}
	if doc.BBox != [4]float64{-69.8511, 40.4926, -69.8511, 40.4926} {
		t.Errorf("bbox = %v", doc.BBox)
	}
	geom, _ := doc.Geometry.(map[string]any)
	if geom["type"] != "Point" {
		t.Errorf("geometry = %v", doc.Geometry)
	}
	if doc.MinZoom <= 0 {
		t.Errorf("minZoom = %d, want the class threshold", doc.MinZoom)
	}
}

func TestIDIsStableAcrossPositionCorrections(t *testing.T) {
	a := ID("ocs-wrecks", Point{ExternalID: "12345", Lat: 40.49, Lng: -69.85})
	b := ID("ocs-wrecks", Point{ExternalID: "12345", Lat: 40.50, Lng: -69.86})
	if a != b {
		t.Fatal("a re-surveyed wreck should keep its id")
	}
	if ID("fl-reefs", Point{ExternalID: "12345"}) == a {
		t.Fatal("ids must not collide across datasets")
	}
}

func TestIDWithoutExternalIDUsesNameAndPosition(t *testing.T) {
	p := Point{Class: ClassReef, Name: "Reef 4", Lat: 25.8, Lng: -80.1}
	if ID("fl-reefs", p) != ID("fl-reefs", p) {
		t.Fatal("id is not deterministic")
	}
	moved := p
	moved.Lat = 26.9
	if ID("fl-reefs", moved) == ID("fl-reefs", p) {
		t.Fatal("different positions should be different rows without an upstream id")
	}
}

func TestDecodeFeaturesSurfacesArcGISErrors(t *testing.T) {
	// ArcGIS answers a bad query with HTTP 200 and an error object. Decoded
	// naively that is an empty dataset, which would silently ingest nothing.
	_, err := DecodeFeatures(strings.NewReader(
		`{"error":{"code":400,"message":"Unable to complete operation"}}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Unable to complete operation") {
		t.Fatalf("err = %v", err)
	}
}

func TestSampleKeys(t *testing.T) {
	got := SampleKeys([]Feature{
		{Properties: map[string]any{"b": 1, "a": 2}},
		{Properties: map[string]any{"c": 3}},
	}, 2)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestPOIClassesHaveMinZooms(t *testing.T) {
	// noaa spells the POI class strings out (it cannot import this package),
	// so this guards the two lists against drifting apart: an unknown class
	// silently falls through to noaa's default, and a POI that draws at the
	// wrong zoom is hard to notice and harder to explain.
	want := map[string]int{
		ClassPlatform:    11,
		ClassReef:        12,
		ClassWreck:       13,
		ClassObstruction: 14,
	}
	for class, z := range want {
		if got := noaa.MinZoomForObjectClass(class); got != z {
			t.Errorf("%s minZoom = %d, want %d", class, got, z)
		}
	}
}
