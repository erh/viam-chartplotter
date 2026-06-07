package render

import (
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/beetlebugorg/s57/pkg/s57"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
)

// TestFeatureFromDocRoundTrip parses a real ENC cell, marshals each feature doc
// to BSON and back (simulating the Mongo read path, where GeoJSON geometry
// arrives as bson.D/bson.A rather than map[string]any), then decodes it with
// featureFromDoc and asserts geometry-type and coordinate fidelity. This guards
// the trickiest part of the Mongo render path: the GeoJSON->s57.Geometry decode.
func TestFeatureFromDocRoundTrip(t *testing.T) {
	const path = "/Users/erh/Library/Caches/viam-chartplotter/noaa-enc/cells/US5MA1MK/US5MA1MK.000"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("cell not cached locally (%v); skipping", err)
	}
	res, err := noaa.ParseCell("", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Docs) == 0 {
		t.Fatal("no docs parsed")
	}

	kinds := map[s57.GeometryType]int{}
	var checked int
	for _, d := range res.Docs {
		// Round-trip through BSON so d.Geometry comes back as bson.D/bson.A,
		// exactly as the driver would hand it to us from a Find.
		raw, err := bson.Marshal(d)
		if err != nil {
			t.Fatalf("marshal %s: %v", d.ID, err)
		}
		var rt noaa.FeatureDoc
		if err := bson.Unmarshal(raw, &rt); err != nil {
			t.Fatalf("unmarshal %s: %v", d.ID, err)
		}
		mf, ok := featureFromDoc(rt)
		if !ok {
			t.Errorf("featureFromDoc returned !ok for %s (kind=%s)", d.ID, d.Kind)
			continue
		}
		g := mf.Geometry()
		if len(g.Coordinates) == 0 {
			t.Errorf("%s: empty coordinates after decode", d.ID)
			continue
		}
		// Kind <-> geometry-type consistency.
		switch d.Kind {
		case "point":
			if g.Type != s57.GeometryTypePoint {
				t.Errorf("%s: kind=point but geom type=%v", d.ID, g.Type)
			}
		case "line":
			if g.Type != s57.GeometryTypeLineString {
				t.Errorf("%s: kind=line but geom type=%v", d.ID, g.Type)
			}
		case "polygon":
			if g.Type != s57.GeometryTypePolygon {
				t.Errorf("%s: kind=polygon but geom type=%v", d.ID, g.Type)
			}
		}
		// Every coordinate is a [lon,lat] pair.
		for _, c := range g.Coordinates {
			if len(c) < 2 {
				t.Errorf("%s: coordinate with <2 components: %v", d.ID, c)
				break
			}
		}
		// ObjectClass survives.
		if mf.ObjectClass() != d.ObjectClass {
			t.Errorf("%s: class mismatch %q != %q", d.ID, mf.ObjectClass(), d.ObjectClass)
		}
		kinds[g.Type]++
		checked++
	}
	t.Logf("decoded %d/%d docs; geom types: %v", checked, len(res.Docs), kinds)
	if checked == 0 {
		t.Fatal("decoded no features")
	}
}
