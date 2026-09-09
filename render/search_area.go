package render

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ---------------------------------------------------------------------------
// Search-result areas: every hit gets a "Newport, RI" so the operator can tell
// which of the dozens of "North Channel"s they are looking at before steering
// at one.
//
// The city is the nearest settlement from the gazetteer (the OSM place=city/
// town/village rows it already holds). The state comes from the settlement's
// own OSM document — the ", Rhode Island" suffix of its wikipedia tag when
// that names a known state, else the ingest region ("us-rhode-island") the
// document was loaded from. NOT from geometry: the ingested admin_level=4
// boundaries are LineString border segments, so point-in-polygon finds
// nothing, and a buoy offshore is outside every land polygon anyway.
//
// The settlement list is ~90k rows behind an unindexed class filter, a scan
// of half the gazetteer — tens of seconds — so it loads in the BACKGROUND,
// armed by the first search. Until it lands, searches simply have no areas.
// Everything here is enrichment: any failure leaves the area blank rather
// than failing or slowing the search.
// ---------------------------------------------------------------------------

// settlement is one gazetteer city/town/village, held in memory for nearest
// lookups. The whole national set is a few MB; a linear scan over it per hit
// is microseconds, which beats maintaining a spatial index nobody else needs.
type settlement struct {
	name   string
	lat    float64
	lng    float64
	weight float64
}

// settlementWeights biases the nearest-settlement choice toward places a
// reader will recognise: a city 10 km off names a light better than a village
// 6 km off. The weight multiplies distance, so lower wins ties of scale.
var settlementWeights = map[string]float64{
	"place=city":    0.5,
	"place=town":    0.75,
	"place=village": 1.0,
}

// settlementMaxDistMeters is how far a hit may be from the settlement that
// names it. Beyond ~60 km the association is a lie — better no area at all.
const settlementMaxDistMeters = 60_000

// settlementMatchRadiusMeters is how close an OSM place document must be to
// the gazetteer settlement to count as the SAME place during the state
// lookup — there are nine Newports, and only the one at these coordinates
// answers for this one.
const settlementMatchRadiusMeters = 5_000

// settlementLoadBudget bounds the background settlement load. The class
// filter has no index, so this is a deliberate scan of the gazetteer's OSM
// rows; measured around a minute on the full national set.
const settlementLoadBudget = 5 * time.Minute

// settlementRetryAfter is how long to wait before re-attempting a failed
// settlement load, so a Mongo outage doesn't spawn a scan per search.
const settlementRetryAfter = time.Minute

// areaLookupBudget bounds the per-search annotation pass. The state lookups
// are indexed name queries (~10 ms, cached per settlement), but the search
// response must never hang on an enrichment.
const areaLookupBudget = 2 * time.Second

type areaIndex struct {
	mu          sync.Mutex
	settlements []settlement
	loaded      bool
	loading     bool
	lastAttempt time.Time
	// states caches the resolved state per settlement (index into
	// settlements). Present-but-empty means "looked, found none", so an
	// unresolvable settlement is not re-queried every search.
	states map[int]string
}

// settlementsIfLoaded returns the in-memory settlement list, arming the
// background load the first time through (and again after a failure, at most
// every settlementRetryAfter). Nil until a load completes.
func (r *ENCRenderer) settlementsIfLoaded() []settlement {
	a := &r.areas
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return a.settlements
	}
	if r.placesColl == nil || a.loading || time.Since(a.lastAttempt) < settlementRetryAfter {
		return nil
	}
	a.loading = true
	a.lastAttempt = time.Now()
	go r.loadSettlements()
	return nil
}

// loadSettlements reads the gazetteer's settlements into memory, off the
// request path and under its own budget.
func (r *ENCRenderer) loadSettlements() {
	ctx, cancel := context.WithTimeout(context.Background(), settlementLoadBudget)
	defer cancel()

	classes := make([]string, 0, len(settlementWeights))
	for c := range settlementWeights {
		classes = append(classes, c)
	}
	began := time.Now()
	var out []settlement
	err := func() error {
		cur, err := r.placesColl.Find(ctx,
			bson.M{"source": "osm", "class": bson.M{"$in": classes}},
			options.Find().SetProjection(bson.M{"name": 1, "class": 1, "lat": 1, "lng": 1}))
		if err != nil {
			return err
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var d struct {
				Name  string  `bson:"name"`
				Class string  `bson:"class"`
				Lat   float64 `bson:"lat"`
				Lng   float64 `bson:"lng"`
			}
			if err := cur.Decode(&d); err != nil || d.Name == "" {
				continue
			}
			out = append(out, settlement{
				name:   d.Name,
				lat:    d.Lat,
				lng:    d.Lng,
				weight: settlementWeights[d.Class],
			})
		}
		return cur.Err()
	}()

	a := &r.areas
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loading = false
	if err != nil {
		r.logger.Warnf("chart search: settlement load failed after %s, no areas until retry: %v",
			time.Since(began).Round(time.Second), err)
		return
	}
	a.settlements = out
	a.states = make(map[int]string)
	a.loaded = true
	r.logger.Infof("chart search: %d settlements loaded for area annotation in %s",
		len(out), time.Since(began).Round(time.Second))
}

// nearestSettlement picks the settlement that best names a point: smallest
// weighted distance, hard-capped by the real distance. Returns -1 when
// nothing is close enough.
func nearestSettlement(settlements []settlement, lat, lng float64) int {
	best, bestScore := -1, 0.0
	for i, s := range settlements {
		d := haversineMeters(lat, lng, s.lat, s.lng)
		if d > settlementMaxDistMeters {
			continue
		}
		score := d * s.weight
		if best < 0 || score < bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// stateForSettlement resolves (and caches) a settlement's state. Empty when
// unresolvable — no OSM attached, or nothing by that name at those
// coordinates.
func (r *ENCRenderer) stateForSettlement(ctx context.Context, idx int) string {
	a := &r.areas
	a.mu.Lock()
	if st, ok := a.states[idx]; ok {
		a.mu.Unlock()
		return st
	}
	s := a.settlements[idx]
	a.mu.Unlock()

	st := r.lookupSettlementState(ctx, s)
	if ctx.Err() != nil {
		// Budget ran out mid-lookup — don't cache "no state" for a
		// settlement we never actually finished asking about.
		return ""
	}
	a.mu.Lock()
	a.states[idx] = st
	a.mu.Unlock()
	return st
}

// lookupSettlementState finds the settlement's own document in the OSM
// buckets by name (indexed) and proximity, and reads the state off it. The
// wikipedia tag outranks the region: a settlement inside another region's
// ingest buffer carries the wrong region, but its wikipedia title still
// names its real state.
func (r *ENCRenderer) lookupSettlementState(ctx context.Context, s settlement) string {
	if r.osm == nil {
		return ""
	}
	var nearestRegion string
	nearestDist := settlementMatchRadiusMeters + 1.0
	for _, coll := range []*mongo.Collection{r.osm.Overview, r.osm.Coastal, r.osm.Detail} {
		if coll == nil {
			continue
		}
		cur, err := coll.Find(ctx,
			bson.M{"name": s.name, "class": "place"},
			options.Find().
				SetProjection(bson.M{"region": 1, "bbox": 1, "tags.wikipedia": 1}).
				SetLimit(50))
		if err != nil {
			continue // enrichment: try the next bucket, else give up quietly
		}
		for cur.Next(ctx) {
			var d struct {
				Region string    `bson:"region"`
				BBox   []float64 `bson:"bbox"`
				Tags   struct {
					Wikipedia string `bson:"wikipedia"`
				} `bson:"tags"`
			}
			if err := cur.Decode(&d); err != nil || len(d.BBox) != 4 {
				continue
			}
			lat := (d.BBox[1] + d.BBox[3]) / 2
			lng := (d.BBox[0] + d.BBox[2]) / 2
			dist := haversineMeters(s.lat, s.lng, lat, lng)
			if dist > settlementMatchRadiusMeters {
				continue
			}
			if st := wikipediaState(d.Tags.Wikipedia); st != "" {
				cur.Close(ctx)
				return st
			}
			if dist < nearestDist {
				nearestDist, nearestRegion = dist, d.Region
			}
		}
		cur.Close(ctx)
	}
	return regionArea(nearestRegion)
}

// annotateAreas fills in each hit's Area. Failures leave hits as they were.
func (r *ENCRenderer) annotateAreas(ctx context.Context, hits []SearchResult) {
	if len(hits) == 0 {
		return
	}
	settlements := r.settlementsIfLoaded()
	if len(settlements) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, areaLookupBudget)
	defer cancel()
	for i := range hits {
		if hits[i].Area != "" {
			continue // the source knew its own address (Overture)
		}
		idx := nearestSettlement(settlements, hits[i].Lat, hits[i].Lng)
		if idx < 0 {
			continue
		}
		hits[i].Area = formatArea(hits[i].Name, settlements[idx].name,
			r.stateForSettlement(ctx, idx))
	}
}

// formatArea renders "City, ST". When the hit IS the city ("Newport" found by
// searching Newport), naming it after itself says nothing — the state alone
// does the placing.
func formatArea(hitName, city, state string) string {
	if strings.EqualFold(strings.TrimSpace(hitName), city) {
		return state
	}
	if state == "" {
		return city
	}
	return city + ", " + state
}

// wikipediaState reads the state off an English wikipedia tag —
// "en:Newport, Rhode Island" → "RI". Only a suffix naming a state (or
// province) we know is accepted; anything else ("en:Boston", a county, a
// disambiguation we don't recognise) returns "" and the region decides.
func wikipediaState(tag string) string {
	t, ok := strings.CutPrefix(tag, "en:")
	if !ok {
		return ""
	}
	i := strings.LastIndex(t, ", ")
	if i < 0 {
		return ""
	}
	if ab, ok := stateAbbrevs[strings.ToLower(strings.TrimSpace(t[i+2:]))]; ok {
		return ab
	}
	return ""
}

// regionArea turns an ingest region ("us-rhode-island", "canada-nova-scotia",
// "bahamas") into the state/province a result row should show. Unknown but
// well-formed names pass through whole — "Nova Scotia" reads better in full
// than dropped.
func regionArea(region string) string {
	if region == "" {
		return ""
	}
	name := strings.TrimPrefix(region, "canada-")
	name = strings.TrimPrefix(name, "us-")
	words := strings.Split(name, "-")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return stateAbbrev(strings.Join(words, " "))
}

// stateAbbrev renders a state's postal code, falling back to the full name
// for anything not in the table.
func stateAbbrev(name string) string {
	if ab, ok := stateAbbrevs[strings.ToLower(name)]; ok {
		return ab
	}
	return name
}

// stateAbbrevs maps lower-cased state/territory names — as they appear in
// wikipedia titles and title-cased ingest regions — to postal codes.
var stateAbbrevs = map[string]string{
	"alabama": "AL", "alaska": "AK", "arizona": "AZ", "arkansas": "AR",
	"california": "CA", "colorado": "CO", "connecticut": "CT", "delaware": "DE",
	"florida": "FL", "georgia": "GA", "hawaii": "HI", "idaho": "ID",
	"illinois": "IL", "indiana": "IN", "iowa": "IA", "kansas": "KS",
	"kentucky": "KY", "louisiana": "LA", "maine": "ME", "maryland": "MD",
	"massachusetts": "MA", "michigan": "MI", "minnesota": "MN", "mississippi": "MS",
	"missouri": "MO", "montana": "MT", "nebraska": "NE", "nevada": "NV",
	"new hampshire": "NH", "new jersey": "NJ", "new mexico": "NM", "new york": "NY",
	"north carolina": "NC", "north dakota": "ND", "ohio": "OH", "oklahoma": "OK",
	"oregon": "OR", "pennsylvania": "PA", "rhode island": "RI", "south carolina": "SC",
	"south dakota": "SD", "tennessee": "TN", "texas": "TX", "utah": "UT",
	"vermont": "VT", "virginia": "VA", "washington": "WA", "west virginia": "WV",
	"wisconsin": "WI", "wyoming": "WY",
	"district of columbia": "DC", "puerto rico": "PR", "guam": "GU",
	"virgin islands": "VI", "us virgin islands": "VI",
	"united states virgin islands": "VI",
	"american samoa": "AS", "northern mariana islands": "MP",
}
