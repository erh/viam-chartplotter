package osmtiler

import (
	"testing"

	"github.com/paulmach/osm"
)

func tags(kv ...string) osm.Tags {
	if len(kv)%2 != 0 {
		panic("tags: odd number of args")
	}
	t := make(osm.Tags, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		t = append(t, osm.Tag{Key: kv[i], Value: kv[i+1]})
	}
	return t
}

func TestClassify_KeptFeatures(t *testing.T) {
	cases := []struct {
		name string
		tags osm.Tags
		want Class
	}{
		{"motorway", tags("highway", "motorway"), ClassRoad},
		{"residential", tags("highway", "residential", "name", "Main St"), ClassRoad},
		{"building yes", tags("building", "yes"), ClassBuilding},
		{"church", tags("building", "church"), ClassBuilding},
		{"railway", tags("railway", "rail"), ClassRailway},
		{"runway", tags("aeroway", "runway"), ClassAeroway},
		{"admin boundary", tags("boundary", "administrative", "admin_level", "2"), ClassAdmin},
		{"city", tags("place", "city", "name", "Boston"), ClassPlace},
		{"island", tags("place", "island", "name", "Mount Desert"), ClassPlace},
		{"forest", tags("natural", "wood"), ClassNatural},
		{"peak", tags("natural", "peak", "name", "Cadillac"), ClassNatural},
		{"park", tags("landuse", "forest"), ClassLanduse},
		{"residential area", tags("landuse", "residential"), ClassLanduse},
		{"playground", tags("leisure", "playground"), ClassLeisure},
		{"restaurant", tags("amenity", "restaurant", "name", "Foo"), ClassPOI},
		{"shop", tags("shop", "bakery"), ClassPOI},
		{"lighthouse", tags("man_made", "lighthouse"), ClassPOI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.tags); got != tc.want {
				t.Fatalf("Classify(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestClassify_WaterAlwaysSkipped(t *testing.T) {
	cases := []struct {
		name string
		tags osm.Tags
	}{
		{"natural water", tags("natural", "water", "name", "Lake")},
		{"coastline", tags("natural", "coastline")},
		{"bay", tags("natural", "bay", "name", "Penobscot Bay")},
		{"wetland", tags("natural", "wetland")},
		{"river", tags("waterway", "river", "name", "Mississippi")},
		{"stream", tags("waterway", "stream")},
		{"canal", tags("waterway", "canal")},
		{"sea", tags("place", "sea", "name", "Mediterranean")},
		{"ocean", tags("place", "ocean", "name", "Atlantic")},
		{"reservoir", tags("landuse", "reservoir")},
		{"salt_pond", tags("landuse", "salt_pond")},
		{"swimming pool", tags("leisure", "swimming_pool")},
		{"water park", tags("leisure", "water_park")},
		{"marina", tags("leisure", "marina", "name", "Boat Basin")},
		{"swimming area", tags("leisure", "swimming_area")},
		// Mixed cases: water tag plus a "keep" tag still drops, because
		// the water classifier runs first.
		{"highway over river", tags("highway", "service", "waterway", "stream")},
		{"named lake with place tag", tags("natural", "water", "place", "city")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.tags); got != ClassSkip {
				t.Fatalf("Classify(%v) = %v, want ClassSkip", tc.tags, got)
			}
		})
	}
}

func TestClassify_WaterAdjacentManMade(t *testing.T) {
	for _, v := range []string{"pier", "breakwater", "groyne"} {
		t.Run(v, func(t *testing.T) {
			if got := Classify(tags("man_made", v)); got != ClassSkip {
				t.Fatalf("man_made=%s classified as %v, want ClassSkip", v, got)
			}
		})
	}
}

func TestClassify_Skip(t *testing.T) {
	cases := []struct {
		name string
		tags osm.Tags
	}{
		{"no tags", tags()},
		{"created_by only", tags("created_by", "JOSM")},
		{"building=no", tags("building", "no")},
		{"place=unknown", tags("place", "farm")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.tags); got != ClassSkip {
				t.Fatalf("Classify(%v) = %v, want ClassSkip", tc.tags, got)
			}
		})
	}
}
