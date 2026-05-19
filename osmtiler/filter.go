// Package osmtiler implements the self-hosted OSM tile pipeline:
// feature classification (this file), PBF ingest, projection, and
// per-tile raster rendering. See OSM_TILES_PLAN.md for the overall
// design and v0.1/v0.2/v0.3 scopes.
package osmtiler

import "github.com/paulmach/osm"

// Class labels an OSM feature for the self-hosted tile renderer.
// The classifier is the choke point that implements the "no water"
// rule for our own tiles: anything water-related collapses to
// ClassSkip before it reaches geometry assembly, so the chart
// renderer's water layer shows through unobstructed.
type Class uint8

const (
	ClassSkip Class = iota
	ClassRoad
	ClassBuilding
	ClassLanduse
	ClassLeisure
	ClassNatural
	ClassPlace // place=country|city|town|... — point label
	ClassPOI   // amenity|shop|tourism|... — POI point
	ClassAdmin // boundary=administrative — admin line
	ClassRailway
	ClassAeroway

	ClassCount
)

var classNames = [ClassCount]string{
	"skip", "road", "building", "landuse", "leisure",
	"natural", "place", "poi", "admin", "railway", "aeroway",
}

func (c Class) String() string {
	if c < ClassCount {
		return classNames[c]
	}
	return "unknown"
}

// Classify returns the renderable class for an OSM tag set, or
// ClassSkip if the feature should not be drawn. Water-related tag
// combinations always return ClassSkip.
func Classify(tags osm.Tags) Class {
	if isWater(tags) {
		return ClassSkip
	}
	if v := tags.Find("highway"); v != "" {
		return ClassRoad
	}
	if v := tags.Find("building"); v != "" && v != "no" {
		return ClassBuilding
	}
	if v := tags.Find("railway"); v != "" {
		return ClassRailway
	}
	if v := tags.Find("aeroway"); v != "" {
		return ClassAeroway
	}
	if tags.Find("boundary") == "administrative" {
		return ClassAdmin
	}
	if isRenderedPlace(tags.Find("place")) {
		return ClassPlace
	}
	if isRenderedNatural(tags.Find("natural")) {
		return ClassNatural
	}
	if tags.Find("landuse") != "" {
		return ClassLanduse
	}
	if tags.Find("leisure") != "" {
		return ClassLeisure
	}
	if isPOI(tags) {
		return ClassPOI
	}
	return ClassSkip
}

func isWater(tags osm.Tags) bool {
	for _, t := range tags {
		switch t.Key {
		case "waterway":
			return true
		case "natural":
			switch t.Value {
			case "water", "coastline", "bay", "strait",
				"spring", "hot_spring", "geyser", "wetland":
				return true
			}
		case "place":
			switch t.Value {
			case "sea", "ocean":
				return true
			}
		case "landuse":
			switch t.Value {
			case "reservoir", "basin", "salt_pond":
				return true
			}
		case "leisure":
			switch t.Value {
			case "swimming_pool", "water_park", "marina", "swimming_area":
				return true
			}
		}
	}
	return false
}

func isRenderedPlace(v string) bool {
	switch v {
	case "country", "state", "province", "city", "town", "village",
		"hamlet", "island", "islet", "locality", "suburb", "neighbourhood":
		return true
	}
	return false
}

func isRenderedNatural(v string) bool {
	switch v {
	case "wood", "forest", "scrub", "heath", "grassland", "meadow",
		"fell", "tundra", "bare_rock", "scree", "shingle", "sand",
		"beach", "cliff", "rock", "peak", "saddle", "volcano",
		"tree", "tree_row", "glacier":
		return true
	}
	return false
}

func isPOI(tags osm.Tags) bool {
	for _, k := range []string{"amenity", "shop", "tourism", "historic", "office"} {
		if tags.Find(k) != "" {
			return true
		}
	}
	if v := tags.Find("man_made"); v != "" {
		// Water-adjacent man_made features straddle the chart's water
		// layer; drop them so we don't render structures hanging over
		// blue tiles the chart owns.
		switch v {
		case "pier", "breakwater", "groyne":
			return false
		}
		return true
	}
	return false
}
