package render

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/erh/viam-chartplotter/mapdata/noaa"
	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
	"github.com/erh/viam-chartplotter/mapdata/places"
)

// ---------------------------------------------------------------------------
// Chart search: find anything named in the ENC store — a light, a wreck, a
// canyon, a channel, an anchorage — and get back somewhere to look on the map.
//
// Search is over the stored `name` field (OBJNAM), which is the only free text
// the ENC carries for a feature. Everything else about a feature is codes, so
// this is name search, not full-text search over the chart.
// ---------------------------------------------------------------------------

// SearchResult is one chart feature matching a search.
type SearchResult struct {
	Name  string `json:"name"`
	Class string `json:"class"` // S-57 acronym, e.g. "LIGHTS"
	// Label is Class rendered for a human ("Light", "Wreck"), falling back to
	// the raw acronym for classes we have no wording for.
	Label string `json:"label"`
	Cell  string `json:"cell"`
	// Source is where the name came from: "chart" (the ENC) or "osm". The two
	// answer different questions — the chart names navigation features, OSM
	// names places and businesses — and a searcher should be able to tell
	// which they are looking at.
	Source string `json:"source"`

	// Lat/Lng is the feature's centre, and BBox its full extent — a channel or
	// canyon is a big thing and the caller usually wants to frame it, not
	// centre on a point.
	Lat  float64    `json:"lat"`
	Lng  float64    `json:"lng"`
	BBox [4]float64 `json:"bbox"` // [minLon, minLat, maxLon, maxLat]

	// DistanceMeters from the origin the caller supplied, or -1 when none was.
	DistanceMeters float64 `json:"distance_meters"`
}

const searchQueryTimeout = 10 * time.Second

// searchOverFetch is how many index hits to pull per displayed result. The
// index scan returns matches in name order, which for a query like "light"
// means a few hundred candidates scattered nationwide; over-fetching gives the
// distance sort something to actually choose from, so a search near Newport
// surfaces the Newport ones instead of whichever sorted first alphabetically.
const searchOverFetch = 8

// Search finds named ENC features matching q. When origin is non-nil, results
// come back nearest-first from that point; otherwise alphabetically. class,
// when non-empty, narrows to a single S-57 object class.
// It also returns the terms that actually produced the hits, which may be
// fewer than were typed (see searchFallbacks) — the caller should say so
// rather than present them as an answer to the original question.
func (r *ENCRenderer) Search(q string, class string, limit int, origin *RoutePoint) ([]SearchResult, string, error) {
	if r.noaaColl == nil && r.osm == nil && r.placesColl == nil {
		return nil, "", errNoCharts
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, q, nil
	}
	if limit <= 0 || limit > noaa.SearchLimitMax {
		limit = 20
	}

	ctx, cancel := context.WithTimeout(context.Background(), searchQueryTimeout)
	defer cancel()

	fetch := limit * searchOverFetch
	if fetch > noaa.SearchLimitMax {
		fetch = noaa.SearchLimitMax
	}

	// Both sources are tried at each rung of the fallback ladder, so a query
	// the chart can't answer but OSM can is answered in full rather than
	// silently narrowed first.
	qStart := time.Now()

	// The gazetteer answers from a text index, so it is fast and complete
	// where it has been built. When it returns nothing we still fall through
	// to the source collections — coverage may be regional, and a miss there
	// is not proof the name doesn't exist.
	if hits := r.searchPlaces(ctx, q, limit, origin); len(hits) > 0 {
		hits = dedupeSearchResults(hits)
		rankSearchResults(hits, q, origin)
		if len(hits) > limit {
			hits = hits[:limit]
		}
		r.logSlowQuery("places-search", time.Since(qStart), len(hits), -1, 0, 0, 0, 0)
		return hits, q, nil
	}

	var out []SearchResult
	matched := q
	for _, terms := range searchFallbacks(q) {
		// The two sources are independent, so run them together: OSM's budget
		// then overlaps the chart query instead of adding to it.
		var osmHits []SearchResult
		done := make(chan struct{})
		go func() {
			osmHits = r.searchOSM(ctx, terms, origin)
			close(done)
		}()
		docs, err := r.searchOnce(ctx, terms, strings.ToUpper(class), fetch, origin)
		<-done
		if err != nil {
			return nil, "", err
		}
		found := append(r.chartResults(docs, origin), osmHits...)
		if len(found) > 0 {
			out, matched = found, terms
			break
		}
	}
	r.logSlowQuery("noaa-search", time.Since(qStart), len(out), -1, 0, 0, 0, 0)

	// The same real-world object is charted in every cell that covers it (a
	// major light shows up in the coastal, approach and harbour cells), so a
	// raw result list is mostly duplicates. Keep one per name+class, preferring
	// the nearest — or, with no origin, whichever came first.
	out = dedupeSearchResults(out)

	sortSearchResults(out, origin)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, matched, nil
}

// chartResults converts ENC feature documents into search results.
func (r *ENCRenderer) chartResults(docs []noaa.FeatureDoc, origin *RoutePoint) []SearchResult {
	out := make([]SearchResult, 0, len(docs))
	for _, d := range docs {
		if d.Name == "" {
			continue
		}
		lon := (d.BBox[0] + d.BBox[2]) / 2
		lat := (d.BBox[1] + d.BBox[3]) / 2
		res := SearchResult{
			Name:           d.Name,
			Class:          d.ObjectClass,
			Label:          ClassLabel(d.ObjectClass),
			Source:         "chart",
			Cell:           d.Cell,
			Lat:            lat,
			Lng:            lon,
			BBox:           d.BBox,
			DistanceMeters: -1,
		}
		if origin != nil {
			res.DistanceMeters = haversineMeters(origin.Lat, origin.Lng, lat, lon)
		}
		out = append(out, res)
	}
	return out
}

// rankSearchResults orders gazetteer hits by how well the name answers the
// question, then by how near it is.
//
// Distance alone is wrong here, and visibly so: a text index matching "chandlers
// wharf" returns everything sharing either word, so sorting purely by distance
// puts a restaurant a mile away above the wharf of that name eight miles off.
// A name carrying every word the operator typed is what they asked for; among
// those, the near one wins.
func rankSearchResults(out []SearchResult, query string, origin *RoutePoint) {
	words := strings.Fields(strings.ToLower(query))
	for i, w := range words {
		words[i] = foldName(w)
	}
	matchesAll := func(name string) bool {
		folded := foldName(strings.ToLower(name))
		for _, w := range words {
			if w != "" && !strings.Contains(folded, w) {
				return false
			}
		}
		return true
	}
	sort.SliceStable(out, func(i, j int) bool {
		fi, fj := matchesAll(out[i].Name), matchesAll(out[j].Name)
		if fi != fj {
			return fi
		}
		if origin != nil && out[i].DistanceMeters != out[j].DistanceMeters {
			return out[i].DistanceMeters < out[j].DistanceMeters
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Class < out[j].Class
	})
}

// foldName strips punctuation so a name and a query match on their letters.
// The chart and OSM disagree with each other and with the operator about
// apostrophes and hyphens — the data says "Chandler's Wharf", a searcher types
// "chandlers wharf" — and treating that as a mismatch buries the exact answer
// under everything that merely shares a word.
func foldName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sortSearchResults orders hits nearest-first when there is somewhere to
// measure from, alphabetically otherwise.
func sortSearchResults(out []SearchResult, origin *RoutePoint) {
	sort.SliceStable(out, func(i, j int) bool {
		if origin != nil && out[i].DistanceMeters != out[j].DistanceMeters {
			return out[i].DistanceMeters < out[j].DistanceMeters
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Class < out[j].Class
	})
}

// placesFetch is how many gazetteer candidates to pull before ranking. The
// text index returns them by relevance, so this is a window over the best
// matches rather than an arbitrary alphabetical slice — which is exactly what
// the regex path could not do.
const placesFetch = 200

// searchPlaces queries the gazetteer.
func (r *ENCRenderer) searchPlaces(ctx context.Context, q string, limit int, origin *RoutePoint) []SearchResult {
	if r.placesColl == nil {
		return nil
	}
	hits, err := places.Search(ctx, r.placesColl, q, placesFetch)
	if err != nil {
		r.logger.Warnf("chart search: gazetteer lookup failed, falling back: %v", err)
		return nil
	}
	out := make([]SearchResult, 0, len(hits))
	for _, h := range hits {
		label := ClassLabel(h.Class)
		if h.Source == places.SourceOSM {
			label = osmKindLabel(h.Class)
		}
		res := SearchResult{
			Name:           h.Name,
			Class:          h.Class,
			Label:          label,
			Source:         h.Source,
			Cell:           h.Cell,
			Lat:            h.Lat,
			Lng:            h.Lng,
			BBox:           h.BBox,
			DistanceMeters: -1,
		}
		if origin != nil {
			res.DistanceMeters = haversineMeters(origin.Lat, origin.Lng, h.Lat, h.Lng)
		}
		out = append(out, res)
	}
	return out
}

// searchOSM looks the query up in the ingested OSM data, under its own short
// budget. Failures and timeouts are logged and swallowed: OSM is an
// enrichment, and losing it should degrade the search to chart-only rather
// than fail it or hold the response.
//
// That budget is doing real work. OSM name lookup here is an unanchored regex
// over a partial index covering every named feature in 274M documents, so its
// cost scales with the index, not with the number of matches: measured at
// 0.4 s for a common word and 15 s for a rare one, because a result limit makes
// the scan run until it has filled it. A tag pre-filter ("only marinas") makes
// it worse for the same reason — it is another post-filter the scan has to
// read past. The honest fix is an inverted index: a $text index on name, or a
// dedicated gazetteer collection of just the named places built at ingest.
// Until then this contributes when it can and gets out of the way when it
// can't.
const osmSearchBudget = 3 * time.Second

func (r *ENCRenderer) searchOSM(ctx context.Context, q string, origin *RoutePoint) []SearchResult {
	if r.osm == nil || q == "" {
		return nil
	}
	osmCtx, cancel := context.WithTimeout(ctx, osmSearchBudget)
	defer cancel()

	hits, err := osmtiler.SearchByName(osmCtx, r.osm, q, osmSearchFetch, nil, nil)
	if err != nil {
		r.logger.Debugf("chart search: osm lookup gave up (%v); chart-only results", err)
		return nil
	}

	out := make([]SearchResult, 0, len(hits))
	for _, h := range hits {
		res := SearchResult{
			Name:           h.Name,
			Class:          h.Kind,
			Label:          osmKindLabel(h.Kind),
			Source:         "osm",
			Lat:            h.Lat,
			Lng:            h.Lng,
			BBox:           h.BBox,
			DistanceMeters: -1,
		}
		if origin != nil {
			res.DistanceMeters = haversineMeters(origin.Lat, origin.Lng, h.Lat, h.Lng)
		}
		out = append(out, res)
	}
	return out
}

// osmSearchFetch is how many OSM candidates to pull before ranking by
// distance. Wide enough that a local hit usually makes the net, small enough
// that the scan stops early on a common word.
const osmSearchFetch = 120

// osmKindLabel renders an OSM "key=value" kind for a human.
func osmKindLabel(kind string) string {
	if l, ok := osmKindLabels[kind]; ok {
		return l
	}
	// Unknown kinds still read better as "Ferry terminal" than
	// "amenity=ferry_terminal", and the value is the informative half.
	if i := strings.IndexByte(kind, '='); i >= 0 {
		v := strings.ReplaceAll(kind[i+1:], "_", " ")
		if v == "" {
			return kind
		}
		return strings.ToUpper(v[:1]) + v[1:]
	}
	return kind
}

var osmKindLabels = map[string]string{
	"leisure=marina":         "Marina",
	"leisure=slipway":        "Slipway",
	"amenity=fuel":           "Fuel",
	"waterway=fuel":          "Fuel dock",
	"waterway=boatyard":      "Boatyard",
	"amenity=ferry_terminal": "Ferry terminal",
	"shop=boat":              "Boat shop",
	"shop=chandlery":         "Chandlery",
	"man_made=pier":          "Pier",
	"man_made=lighthouse":    "Lighthouse",
	"seamark:type=harbour":   "Harbour",
	"seamark:type=mooring":   "Mooring",
	"seamark:type=anchorage": "Anchorage",
	"harbour=yes":            "Harbour",
	"place=city":             "City",
	"place=town":             "Town",
	"place=village":          "Village",
	"place=hamlet":           "Hamlet",
	"place=island":           "Island",
	"place=islet":            "Islet",
}

// searchLocalRadiusM is how far around the operator a search looks before it
// looks anywhere. A chart's names are not unique — there are hundreds of
// "marina"s nationwide — and an index scan returns them alphabetically, so
// without a geographic pass first the fetch limit is spent on whatever sorts
// early and the boat's own harbour never appears at all. Wide enough to cover
// a cruise's worth of coast.
const searchLocalRadiusM = 150 * 1852

// searchOnce looks in the operator's own waters first and only then everywhere,
// merging the two so a local hit always outranks a distant one with the same
// name — while a search for somewhere you are planning to go still works.
func (r *ENCRenderer) searchOnce(ctx context.Context, q, class string, fetch int, origin *RoutePoint) ([]noaa.FeatureDoc, error) {
	if r.noaaColl == nil {
		return nil, nil // OSM-only deployment
	}
	var out []noaa.FeatureDoc
	seen := map[string]struct{}{}

	if origin != nil {
		box := pointBox(*origin, searchLocalRadiusM)
		local, err := noaa.SearchByName(ctx, r.noaaColl, q, class, fetch, &box)
		if err != nil {
			return nil, wrapChartQueryError(err, "chart search",
				"run `chartdiag search` to see whether the name_search index is missing")
		}
		for _, d := range local {
			seen[d.ID] = struct{}{}
			out = append(out, d)
		}
	}
	if len(out) >= fetch {
		return out, nil
	}

	global, err := noaa.SearchByName(ctx, r.noaaColl, q, class, fetch, nil)
	if err != nil {
		return nil, wrapChartQueryError(err, "chart search",
			"the name_search index is missing — run `datasync --indexes-only` against this database")
	}
	for _, d := range global {
		if _, dup := seen[d.ID]; dup {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// searchFallbacks is the query and then progressively shorter versions of it,
// dropping the trailing word each time. "booth bay marina" finds nothing on a
// chart that has no marina by that name, but "booth bay" finds Boothbay
// Harbor — which is what the operator was steering for. Reporting which terms
// matched keeps that honest rather than silently answering a different
// question.
func searchFallbacks(q string) []string {
	words := strings.Fields(q)
	out := []string{strings.TrimSpace(q)}
	for n := len(words) - 1; n >= 1; n-- {
		out = append(out, strings.Join(words[:n], " "))
	}
	return out
}

// dedupeSearchResults keeps one hit per (name, class), preferring the one
// closest to the origin when distances are known.
func dedupeSearchResults(in []SearchResult) []SearchResult {
	type key struct{ name, class string }
	best := make(map[key]int, len(in))
	out := make([]SearchResult, 0, len(in))
	for _, r := range in {
		k := key{strings.ToLower(r.Name), r.Class}
		if at, seen := best[k]; seen {
			// Distances are -1 when no origin was given, in which case the
			// first hit stands.
			if r.DistanceMeters >= 0 && r.DistanceMeters < out[at].DistanceMeters {
				out[at] = r
			}
			continue
		}
		best[k] = len(out)
		out = append(out, r)
	}
	return out
}

// classLabels are human wordings for the S-57 acronyms a search is likely to
// turn up. Anything missing falls back to the acronym itself — better an
// unfamiliar code than a wrong guess at what it means.
var classLabels = map[string]string{
	"LIGHTS": "Light", "LNDMRK": "Landmark", "WRECKS": "Wreck",
	"OBSTRN": "Obstruction", "UWTROC": "Rock", "SEAARE": "Sea area",
	"BOYLAT": "Lateral buoy", "BOYCAR": "Cardinal buoy", "BOYISD": "Isolated-danger buoy",
	"BOYSAW": "Safe-water buoy", "BOYSPP": "Special-purpose buoy", "BOYINB": "Installation buoy",
	"BCNLAT": "Lateral beacon", "BCNCAR": "Cardinal beacon", "BCNISD": "Isolated-danger beacon",
	"BCNSAW": "Safe-water beacon", "BCNSPP": "Special-purpose beacon",
	"DAYMAR": "Daymark", "FAIRWY": "Fairway", "ACHARE": "Anchorage",
	"ACHBRT": "Anchor berth", "RESARE": "Restricted area", "DRGARE": "Dredged area",
	"DEPARE": "Depth area", "BRIDGE": "Bridge", "CBLOHD": "Overhead cable",
	"PIPOHD": "Overhead pipe", "BUAARE": "Built-up area", "LNDARE": "Land",
	"RIVERS": "River", "LAKARE": "Lake", "CANALS": "Canal", "HRBFAC": "Harbour facility",
	"MORFAC": "Mooring", "PILPNT": "Pile", "SLCONS": "Shoreline construction",
	"BERTHS": "Berth", "SMCFAC": "Small-craft facility", "PONTON": "Pontoon",
	"DWRTPT": "Deep-water route", "TWRTPT": "Traffic-separation route",
	"RECTRC": "Recommended track", "NAVLNE": "Navigation line", "SBDARE": "Seabed area",
	"TSSLPT": "Traffic-separation lane", "CTNARE": "Caution area", "MIPARE": "Military area",
	"SPLARE": "Special area", "LOKBSN": "Lock basin",
}

// ClassLabel renders an S-57 object class for a human, falling back to the
// acronym when we have no wording for it.
func ClassLabel(class string) string {
	if l, ok := classLabels[class]; ok {
		return l
	}
	return class
}

// SearchBBoxSpanMeters is the diagonal of a result's extent — the frontend
// uses it to decide whether to zoom to the bbox or just centre on the point.
func (s SearchResult) SearchBBoxSpanMeters() float64 {
	if s.BBox[2] <= s.BBox[0] && s.BBox[3] <= s.BBox[1] {
		return 0
	}
	return haversineMeters(s.BBox[1], s.BBox[0], s.BBox[3], s.BBox[2])
}
