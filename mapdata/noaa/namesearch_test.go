package noaa

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.viam.com/test"
)

func TestNameSearchFilterSingleWord(t *testing.T) {
	f := NameSearchFilter("boothbay")
	name, ok := f["name"].(bson.M)
	test.That(t, ok, test.ShouldBeTrue)
	// $exists keeps the query inside name_search's partial filter; without it
	// the planner won't consider that index at all.
	test.That(t, name["$exists"], test.ShouldEqual, true)
	test.That(t, name["$regex"], test.ShouldEqual, "boothbay")
	test.That(t, name["$options"], test.ShouldEqual, "i")
}

func TestNameSearchFilterMatchesWordsInAnyOrder(t *testing.T) {
	// The chart says "Boothbay"; a searcher types "booth bay marina". Requiring
	// the literal phrase finds nothing either way round, so each word has to
	// match independently.
	f := NameSearchFilter("booth bay marina")
	clauses, ok := f["$and"].([]bson.M)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, len(clauses), test.ShouldEqual, 4) // $exists + one per word

	got := map[string]bool{}
	for _, c := range clauses {
		if m, ok := c["name"].(bson.M); ok {
			if rx, ok := m["$regex"].(string); ok {
				got[rx] = true
				test.That(t, m["$options"], test.ShouldEqual, "i")
			}
		}
	}
	for _, w := range []string{"booth", "bay", "marina"} {
		test.That(t, got[w], test.ShouldBeTrue)
	}
}

func TestNameSearchFilterEscapesRegexMetacharacters(t *testing.T) {
	// A name with punctuation must be matched literally, not compiled as a
	// pattern — "point judith (n)" is a search, not a capture group.
	f := NameSearchFilter("judith (n)")
	clauses := f["$and"].([]bson.M)
	found := false
	for _, c := range clauses {
		if m, ok := c["name"].(bson.M); ok {
			if rx, ok := m["$regex"].(string); ok && rx != "" {
				test.That(t, rx, test.ShouldNotContainSubstring, "(n)")
				if rx == `\(n\)` {
					found = true
				}
			}
		}
	}
	test.That(t, found, test.ShouldBeTrue)
}

func TestNameSearchFilterCapsWordCount(t *testing.T) {
	// A pasted paragraph must not become a hundred-clause scan.
	f := NameSearchFilter("a b c d e f g h i j k l")
	clauses := f["$and"].([]bson.M)
	test.That(t, len(clauses), test.ShouldEqual, maxSearchWords+1)
}
