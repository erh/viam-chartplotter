package places

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.viam.com/test"
)

func TestIDCollapsesTheSameFeatureChartedTwice(t *testing.T) {
	// A major light is charted in the coastal, approach and harbour cells at
	// very slightly different positions. Those are not three places, and the
	// id has to say so or the search shows the same light three times.
	a := ID(SourceChart, "LIGHTS", "Brenton Reef Light", 41.42501, -71.39002)
	b := ID(SourceChart, "LIGHTS", "Brenton Reef Light", 41.42499, -71.38998)
	test.That(t, a, test.ShouldEqual, b)

	// Case differences in the name are the same place too.
	test.That(t, ID(SourceChart, "LIGHTS", "brenton reef light", 41.425, -71.39),
		test.ShouldEqual, a)
}

func TestIDSeparatesGenuinelyDifferentPlaces(t *testing.T) {
	base := ID(SourceChart, "LIGHTS", "Brenton Reef Light", 41.425, -71.39)
	// A different class at the same spot is a different thing (the light and
	// the sea area named after it).
	test.That(t, ID(SourceChart, "SEAARE", "Brenton Reef Light", 41.425, -71.39),
		test.ShouldNotEqual, base)
	// The same name a mile away is a different place.
	test.That(t, ID(SourceChart, "LIGHTS", "Brenton Reef Light", 41.45, -71.39),
		test.ShouldNotEqual, base)
	// And the chart's version is distinct from OSM's, since they carry
	// different detail and the UI labels them differently.
	test.That(t, ID(SourceOSM, "LIGHTS", "Brenton Reef Light", 41.425, -71.39),
		test.ShouldNotEqual, base)
}

func TestNamedFilterRequiresANonEmptyName(t *testing.T) {
	// $exists alone would match documents with name:"" — most of the
	// collection — and turn a scoped build into a full one.
	f := namedFilter(nil)
	name, ok := f["name"].(bson.M)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, name["$exists"], test.ShouldEqual, true)
	test.That(t, name["$gt"], test.ShouldEqual, "")
	// No bbox means no region predicate at all.
	test.That(t, f["bbox.0"], test.ShouldBeNil)
}

func TestNamedFilterBBoxIsAnIntersectionTest(t *testing.T) {
	// Overlap, not containment: a harbour whose bbox straddles the edge of the
	// region being built must still be included.
	bbox := [4]float64{-71.8, 40.9, -66.8, 44.6}
	f := namedFilter(&bbox)
	test.That(t, f["bbox.0"], test.ShouldNotBeNil) // feature min lon <= region max lon
	test.That(t, f["bbox.2"], test.ShouldNotBeNil) // feature max lon >= region min lon
	test.That(t, f["bbox.1"], test.ShouldNotBeNil)
	test.That(t, f["bbox.3"], test.ShouldNotBeNil)
}

func TestOSMKindFallsBackToIngestClass(t *testing.T) {
	kindOf := OSMKindFunc(func(tags map[string]string) string {
		if v, ok := tags["leisure"]; ok {
			return "leisure=" + v
		}
		return ""
	})
	test.That(t, kindOf(SourceDoc{Tags: map[string]string{"leisure": "marina"}}),
		test.ShouldEqual, "leisure=marina")
	// No interesting tag: keep whatever the ingest classified it as, rather
	// than losing the type entirely.
	test.That(t, kindOf(SourceDoc{Class: "highway"}), test.ShouldEqual, "highway")
}

func TestChartKindIsTheObjectClass(t *testing.T) {
	test.That(t, ChartKind(SourceDoc{ObjectClass: "LIGHTS"}), test.ShouldEqual, "LIGHTS")
}
