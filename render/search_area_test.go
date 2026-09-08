package render

import (
	"testing"

	"go.viam.com/test"
)

func TestNearestSettlement(t *testing.T) {
	settlements := []settlement{
		{name: "Jamestown", lat: 41.497, lng: -71.368, weight: 1.0},   // village, ~3 km
		{name: "Newport", lat: 41.490, lng: -71.313, weight: 0.5},     // city, ~5 km
		{name: "Providence", lat: 41.824, lng: -71.413, weight: 0.5},  // city, ~35 km
		{name: "New London", lat: 41.355, lng: -72.100, weight: 0.75}, // town, ~60+ km
	}
	// A point in Narragansett Bay between Jamestown and Newport: the city's
	// weight outranks the marginally nearer village.
	idx := nearestSettlement(settlements, 41.50, -71.34)
	test.That(t, idx, test.ShouldEqual, 1)

	// Far offshore, nothing within the cap names the point.
	test.That(t, nearestSettlement(settlements, 39.0, -70.0), test.ShouldEqual, -1)

	// Right on top of the village, the weight bias cannot pull the answer
	// 5 km away: distance still dominates at village scale.
	test.That(t, nearestSettlement(settlements, 41.497, -71.369), test.ShouldEqual, 0)
}

func TestFormatArea(t *testing.T) {
	test.That(t, formatArea("Brenton Reef Light", "Newport", "RI"),
		test.ShouldEqual, "Newport, RI")
	// No state resolved: the city still places the hit.
	test.That(t, formatArea("Brenton Reef Light", "Newport", ""),
		test.ShouldEqual, "Newport")
	// The hit IS the settlement — naming it after itself says nothing, the
	// state alone does the placing.
	test.That(t, formatArea("Newport", "Newport", "RI"), test.ShouldEqual, "RI")
	test.That(t, formatArea("newport ", "Newport", "RI"), test.ShouldEqual, "RI")
}

func TestStateAbbrev(t *testing.T) {
	test.That(t, stateAbbrev("Rhode Island"), test.ShouldEqual, "RI")
	test.That(t, stateAbbrev("District Of Columbia"), test.ShouldEqual, "DC")
	// Unknown names pass through whole rather than get dropped or guessed.
	test.That(t, stateAbbrev("Nova Scotia"), test.ShouldEqual, "Nova Scotia")
}

func TestRegionArea(t *testing.T) {
	test.That(t, regionArea("us-rhode-island"), test.ShouldEqual, "RI")
	test.That(t, regionArea("us-new-york"), test.ShouldEqual, "NY")
	test.That(t, regionArea("us-district-of-columbia"), test.ShouldEqual, "DC")
	test.That(t, regionArea("us-us-virgin-islands"), test.ShouldEqual, "VI")
	// Provinces and countries show in full.
	test.That(t, regionArea("canada-nova-scotia"), test.ShouldEqual, "Nova Scotia")
	test.That(t, regionArea("bahamas"), test.ShouldEqual, "Bahamas")
	test.That(t, regionArea(""), test.ShouldEqual, "")
}

func TestWikipediaState(t *testing.T) {
	test.That(t, wikipediaState("en:Newport, Rhode Island"), test.ShouldEqual, "RI")
	test.That(t, wikipediaState("en:Newport (city), Vermont"), test.ShouldEqual, "VT")
	// No state suffix, a non-English tag, or a suffix we don't recognise all
	// defer to the ingest region instead of guessing.
	test.That(t, wikipediaState("en:Boston"), test.ShouldEqual, "")
	test.That(t, wikipediaState("de:Newport, Rhode Island"), test.ShouldEqual, "")
	test.That(t, wikipediaState("en:Springfield, Ontario County"), test.ShouldEqual, "")
	test.That(t, wikipediaState(""), test.ShouldEqual, "")
}
