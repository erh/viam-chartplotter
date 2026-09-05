package poi

import (
	"fmt"
	"strings"
	"unicode"
)

// Turning a feed's properties into a Point.
//
// Field names are the whole problem. These datasets are published by a dozen
// different agencies, re-exported through ArcGIS, and renamed between vintages
// — the same wreck name is "vesslterms" in AWOIS, "VESSLTERMS" in the OCS
// export and "vessel_name" in a state copy. So nothing here matches a field
// exactly: lookup folds case and punctuation and tries a list of candidates,
// the same tolerance the clients already apply to sensor keys. A feed that
// renames a column loses an attribute; it does not lose the dataset.

// Adapter turns one decoded GeoJSON feature into a Point. ok=false drops the
// feature (no usable position, or a row the dataset uses as a placeholder).
type Adapter func(Feature) (Point, bool)

// Dataset describes one ingestable feed.
type Dataset struct {
	// Key is the --dataset name, and the value stored in each document's
	// `cell` field, so a re-ingest or a Prune can address exactly this feed.
	Key string
	// Description is what the CLI prints in its dataset list.
	Description string
	// URL is the published endpoint, for the CLI's help text. It is not
	// fetched automatically: these are big public services, and which layer of
	// a MapServer to read is a decision to make deliberately.
	URL     string
	Adapter Adapter
}

// Datasets is the registry of feeds the ingester knows how to read. Adding a
// state reef program is one entry here — the adapters below are field mapping
// and nothing else.
var Datasets = map[string]Dataset{
	"ocs-wrecks": {
		Key:         "ocs-wrecks",
		Description: "NOAA Office of Coast Survey wrecks and obstructions (ENC + AWOIS), ~19k features",
		URL:         "https://wrecks.nauticalcharts.noaa.gov/arcgis/rest/services/public_wrecks/Wrecks_And_Obstructions/MapServer/<layer>",
		Adapter:     wreckAdapter,
	},
	"artificial-reefs": {
		Key:         "artificial-reefs",
		Description: "Artificial reefs, national compilation of the state reef programs (MarineCadastre)",
		URL:         "https://hub.marinecadastre.gov/datasets/artificial-reefs-1",
		Adapter:     reefAdapter,
	},
	"fl-reefs": {
		Key:         "fl-reefs",
		Description: "Florida FWC artificial reef deployments — richer and fresher than the national roll-up",
		URL:         "https://gis.myfwc.com/mapping/rest/services/Open_Data/Artificial_Reef_Locations_in_Florida/MapServer/<layer>",
		Adapter:     reefAdapter,
	},
	"platforms": {
		Key:         "platforms",
		Description: "Offshore oil and gas platforms (BOEM/BSEE) — structure, hazard and fishing mark",
		URL:         "https://www.data.boem.gov/Mapping/Files/platform_meta.html",
		Adapter:     platformAdapter,
	},
}

// DatasetKeys lists the registry in a stable order for help text.
func DatasetKeys() []string {
	keys := make([]string, 0, len(Datasets))
	for k := range Datasets {
		keys = append(keys, k)
	}
	// Small and fixed; insertion order isn't available from a map, so sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// wreckAdapter reads the OCS wrecks-and-obstructions schema (and the AWOIS
// schema it absorbed). The distinction that matters is wreck vs obstruction:
// a wreck is a thing people dive and fish, an obstruction is a snag, a crib or
// a wellhead, and mixing them makes both harder to find.
func wreckAdapter(f Feature) (Point, bool) {
	lng, lat, ok := f.Geometry.RepresentativePoint()
	if !ok {
		// Some exports carry the position only as decimal-degree columns.
		lat, ok = numProp(f.Properties, "latdec", "latitude", "lat", "y")
		if !ok {
			return Point{}, false
		}
		lng, ok = numProp(f.Properties, "londec", "longitude", "lon", "lng", "x")
		if !ok {
			return Point{}, false
		}
	}
	kind := strProp(f.Properties, "feature_type", "featuretype", "category", "type")
	class := ClassObstruction
	if kind == "" || containsFold(kind, "wreck") {
		// AWOIS's own default: a record with no feature type is a wreck
		// report. Charted obstructions always carry one.
		class = ClassWreck
	}
	name := strProp(f.Properties, "vesslterms", "vessel_name", "vesselname", "name", "objnam", "sitename")
	if name == "" {
		name = titleFor(class, kind)
	}
	p := Point{
		ExternalID: strProp(f.Properties, "recrd", "record", "objectid", "fid", "id"),
		Name:       name,
		Class:      class,
		Lat:        lat,
		Lng:        lng,
		Attributes: keepProps(f.Properties,
			"feature_type", "vesslterms", "yearsunk", "depth", "sounding_type",
			"history", "quasou", "gp_quality", "chart", "vessel_type", "cargo"),
	}
	p.Detail = joinDetail(
		strProp(f.Properties, "feature_type", "vessel_type"),
		yearDetail(f.Properties, "yearsunk", "year_sunk", "sunk"),
		depthDetail(f.Properties, "depth", "depth_ft", "sounding"),
	)
	return p, true
}

// reefAdapter reads an artificial-reef feed: the national MarineCadastre
// roll-up and the state programs it is compiled from share enough field
// vocabulary that one mapping covers them.
func reefAdapter(f Feature) (Point, bool) {
	lng, lat, ok := f.Geometry.RepresentativePoint()
	if !ok {
		lat, ok = numProp(f.Properties, "latitude", "lat", "latdec", "y")
		if !ok {
			return Point{}, false
		}
		lng, ok = numProp(f.Properties, "longitude", "lon", "lng", "londec", "x")
		if !ok {
			return Point{}, false
		}
	}
	name := strProp(f.Properties, "reefname", "reef_name", "sitename", "site_name",
		"name", "areaname", "permit_area", "location")
	if name == "" {
		name = "Artificial reef"
	}
	p := Point{
		ExternalID: strProp(f.Properties, "reefid", "reef_id", "siteid", "objectid", "fid", "id"),
		Name:       name,
		Class:      ClassReef,
		Lat:        lat,
		Lng:        lng,
		Attributes: keepProps(f.Properties,
			"reefname", "material", "materials", "state", "depth", "relief",
			"deploydate", "deployment_date", "permit", "county", "program"),
	}
	p.Detail = joinDetail(
		strProp(f.Properties, "material", "materials", "structure"),
		depthDetail(f.Properties, "depth", "depth_ft", "water_depth"),
		strProp(f.Properties, "state"),
	)
	return p, true
}

// platformAdapter reads the BOEM/BSEE offshore structure feeds.
func platformAdapter(f Feature) (Point, bool) {
	lng, lat, ok := f.Geometry.RepresentativePoint()
	if !ok {
		lat, ok = numProp(f.Properties, "latitude", "lat", "y")
		if !ok {
			return Point{}, false
		}
		lng, ok = numProp(f.Properties, "longitude", "lon", "lng", "x")
		if !ok {
			return Point{}, false
		}
	}
	name := strProp(f.Properties, "structure_name", "structurename", "complex_name", "name")
	if name == "" {
		// Block and area are how a Gulf platform is actually referred to:
		// "EI 322" reads to a fisherman the way a vessel name does.
		area := strProp(f.Properties, "area_code", "areacode", "area")
		block := strProp(f.Properties, "block_number", "blocknumber", "block")
		switch {
		case area != "" && block != "":
			name = strings.TrimSpace(area + " " + block)
		case block != "":
			name = "Block " + block
		default:
			name = "Platform"
		}
	}
	p := Point{
		ExternalID: strProp(f.Properties, "structure_number", "struc_number", "complex_id", "objectid", "fid", "id"),
		Name:       name,
		Class:      ClassPlatform,
		Lat:        lat,
		Lng:        lng,
		Attributes: keepProps(f.Properties,
			"structure_name", "area_code", "block_number", "water_depth",
			"install_date", "removal_date", "status_code", "operator"),
	}
	p.Detail = joinDetail(
		strProp(f.Properties, "status_code", "status"),
		depthDetail(f.Properties, "water_depth", "depth"),
	)
	return p, true
}

// titleFor names an unnamed feature after what it is. Most AWOIS records have
// no vessel name — an unnamed row still has to say something in a search
// result, and "Wreck" beats an empty line.
func titleFor(class, kind string) string {
	if kind != "" {
		return capitalise(kind)
	}
	switch class {
	case ClassWreck:
		return "Wreck"
	case ClassObstruction:
		return "Obstruction"
	case ClassReef:
		return "Artificial reef"
	case ClassPlatform:
		return "Platform"
	}
	return "Point of interest"
}

// SampleKeys returns the property keys present across the first n features,
// for the CLI's --inspect mode. Field names are the one thing about these
// feeds that cannot be looked up in a document reliably, so the ingester can
// show what actually arrived before anyone writes a mapping for it.
func SampleKeys(features []Feature, n int) []string {
	if n <= 0 || n > len(features) {
		n = len(features)
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range features[:n] {
		for k := range f.Properties {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Property lookup: fold case and punctuation, try candidates in order.
// ---------------------------------------------------------------------------

// foldKey lowercases a field name and drops everything that isn't a letter or
// digit, so "Water_Depth", "waterdepth" and "WATER DEPTH" are one key.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lookup finds the first candidate present in props, folded.
func lookup(props map[string]any, candidates ...string) (any, bool) {
	if len(props) == 0 {
		return nil, false
	}
	folded := make(map[string]any, len(props))
	for k, v := range props {
		folded[foldKey(k)] = v
	}
	for _, c := range candidates {
		if v, ok := folded[foldKey(c)]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// strProp reads the first present candidate as a trimmed string. Feeds use
// several spellings of "no value here" — empty, "N/A", "UNKNOWN", "NULL" —
// and every one of them would otherwise become a chart label.
func strProp(props map[string]any, candidates ...string) string {
	v, ok := lookup(props, candidates...)
	if !ok {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	switch strings.ToUpper(s) {
	case "", "N/A", "NA", "NULL", "NONE", "UNKNOWN", "<NULL>":
		return ""
	}
	return s
}

// numProp reads the first present candidate as a float, tolerating numbers
// that arrived as strings (a CSV-derived export makes everything a string).
func numProp(props map[string]any, candidates ...string) (float64, bool) {
	v, ok := lookup(props, candidates...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// keepProps copies the listed fields (folded) into a normalised attribute bag.
// Only these are kept: an ArcGIS export carries shape areas, internal ids and
// audit columns that would triple the document size and mean nothing on a
// chart.
func keepProps(props map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := lookup(props, k); ok {
			if s, isStr := v.(string); isStr {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				out[foldKey(k)] = s
				continue
			}
			out[foldKey(k)] = v
		}
	}
	return out
}

// joinDetail assembles the non-empty parts of a one-line description.
func joinDetail(parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}

// yearDetail renders a sinking year, ignoring the zero and placeholder values
// these feeds use for "unknown".
func yearDetail(props map[string]any, candidates ...string) string {
	if v, ok := numProp(props, candidates...); ok && v > 1000 && v < 2200 {
		return fmt.Sprintf("sank %d", int(v))
	}
	return ""
}

// depthDetail renders a depth. The feeds publish feet (AWOIS and the state
// reef programs are US customary throughout), which is also what the chart
// shows, so this is a passthrough with a unit rather than a conversion.
func depthDetail(props map[string]any, candidates ...string) string {
	if v, ok := numProp(props, candidates...); ok && v > 0 {
		return fmt.Sprintf("%g ft", v)
	}
	return ""
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(strings.ToLower(s))
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
