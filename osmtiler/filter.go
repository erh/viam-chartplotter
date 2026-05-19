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

// GeomMinZoom returns the smallest zoom at which a feature's geometry
// should be drawn. Lifted (loosely) from openstreetmap-carto's
// `project.mml`/`roads.mss` thresholds — roads are filtered by
// `highway=*` sub-type, buildings appear at z=14, and most landuse
// only at z=8+. Without this, low-zoom tiles for any dense city look
// like uniform gray static.
func GeomMinZoom(class Class, tags osm.Tags) uint8 {
	switch class {
	case ClassRoad:
		switch tags.Find("highway") {
		case "motorway", "motorway_link":
			return 5
		case "trunk", "trunk_link":
			return 6
		case "primary", "primary_link":
			return 8
		case "secondary", "secondary_link":
			return 10
		case "tertiary", "tertiary_link":
			return 12
		case "unclassified", "residential", "living_street":
			return 13
		case "service":
			return 15
		case "pedestrian", "footway", "path", "cycleway", "bridleway", "steps", "track":
			return 14
		case "":
			return 13
		}
		return 13
	case ClassBuilding:
		return 14
	case ClassRailway:
		switch tags.Find("railway") {
		case "rail":
			return 8
		case "subway", "light_rail", "tram":
			return 12
		}
		return 13
	case ClassAeroway:
		switch tags.Find("aeroway") {
		case "runway":
			return 10
		case "taxiway":
			return 13
		}
		return 13
	case ClassAdmin:
		// admin_level=2 (country) at low zoom; finer admin lines at
		// higher zooms. Treat any administrative boundary as visible
		// from z=4 for now — we don't carry admin_level through.
		return 4
	case ClassLanduse:
		switch tags.Find("landuse") {
		case "forest", "park", "meadow", "grass", "farmland":
			return 8
		case "residential", "commercial", "industrial", "retail":
			return 10
		}
		return 10
	case ClassNatural:
		switch tags.Find("natural") {
		case "wood", "forest", "grassland", "meadow", "scrub", "heath", "wetland":
			return 7
		case "beach", "sand", "bare_rock":
			return 11
		case "peak", "saddle", "volcano":
			return 10
		}
		return 12
	case ClassLeisure:
		switch tags.Find("leisure") {
		case "park", "garden", "nature_reserve", "recreation_ground", "common":
			return 10
		}
		return 13
	case ClassPlace:
		// Place dots aren't really visible without their labels;
		// reuse the label threshold so we don't draw a dot at a
		// zoom where its name is hidden.
		return LabelMinZoom(class, tags)
	case ClassPOI:
		return 14
	}
	return 0
}

// LabelMinZoom returns the smallest zoom at which a feature should be
// labelled, or 0 if it should not. Thresholds are loosely modelled on
// osm-carto's place / POI rules — high-importance things (countries,
// cities) appear earlier than hamlets and POIs. Roads, buildings,
// landuse etc. return 0 here; their labels come in later v0.2 work
// (line labels along ways, area labels at centroids).
func LabelMinZoom(class Class, tags osm.Tags) uint8 {
	switch class {
	case ClassPlace:
		switch tags.Find("place") {
		case "country":
			return 3
		case "state", "province":
			return 5
		case "city":
			return 8
		case "town":
			return 11
		case "village":
			return 13
		case "hamlet":
			return 14
		case "island":
			return 9
		case "suburb", "neighbourhood":
			return 13
		case "locality", "islet":
			return 15
		}
	case ClassPOI:
		return 17
	case ClassRoad:
		// For now, all named roads at z>=16. Sub-type-aware
		// thresholds (motorway at z9, residential at z16) come with
		// the v0.3 carto-style port; without them, lowering this
		// makes residential-grid cities like Manhattan unreadable
		// from a wall of street names.
		return 16
	case ClassLeisure:
		// Carto's `leisure=*` lumps parks with gyms; we want parks
		// labelled early (Central Park at z=12), gyms only with the
		// rest of the POIs (z=17). Empty-tag fallback matches the
		// area path because relation-derived polygons don't carry
		// tags through and the vast majority of those are parks.
		switch tags.Find("leisure") {
		case "park", "garden", "nature_reserve", "common", "recreation_ground",
			"dog_park", "pitch", "":
			return 13
		case "playground":
			return 15
		}
		return 17
	case ClassNatural:
		switch tags.Find("natural") {
		case "wood", "forest", "scrub", "heath", "grassland", "meadow",
			"fell", "tundra", "bare_rock", "scree", "shingle", "sand",
			"beach", "glacier", "":
			return 13
		case "peak", "saddle", "volcano":
			return 12
		}
		return 15
	case ClassLanduse:
		switch tags.Find("landuse") {
		case "forest", "park", "":
			return 13
		}
		return 16
	}
	return 0
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
