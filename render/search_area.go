package render

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ---------------------------------------------------------------------------
// Search-result areas: every hit gets a "Newport, RI" so the operator can tell
// which of the dozens of "North Channel"s they are looking at before steering
// at one.
//
// The city is the nearest settlement from the gazetteer (the OSM place=city/
// town/village rows it already holds). The state is the OSM admin_level=4
// boundary polygon containing that settlement — resolved per settlement, not
// per hit, because a buoy five miles offshore is inside no state polygon but
// its harbour town always is. Both lookups are enrichment: any failure leaves
// the area blank rather than failing or slowing the search.
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

// settlementRetryAfter is how long to wait before re-attempting a failed
// settlement load, so a Mongo hiccup doesn't turn every search into a retry.
const settlementRetryAfter = time.Minute

// areaLookupBudget bounds the whole annotation pass. The state queries are
// indexed point-in-polygon lookups (fast, and cached per settlement), but the
// search response must never hang on an enrichment.
const areaLookupBudget = 2 * time.Second

type areaIndex struct {
	mu          sync.Mutex
	settlements []settlement
	loaded      bool
	lastAttempt time.Time
	// states caches the resolved state per settlement (index into
	// settlements). Present-but-empty means "looked, found none" — offshore
	// hamlet data errors exist, and retrying them every search is waste.
	states map[int]string
}

// loadSettlements reads the gazetteer's settlements into memory once,
// retrying a failed load at most every settlementRetryAfter.
func (r *ENCRenderer) loadSettlements(ctx context.Context) []settlement {
	a := &r.areas
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return a.settlements
	}
	if r.placesColl == nil || time.Since(a.lastAttempt) < settlementRetryAfter {
		return nil
	}
	a.lastAttempt = time.Now()

	classes := make([]string, 0, len(settlementWeights))
	for c := range settlementWeights {
		classes = append(classes, c)
	}
	cur, err := r.placesColl.Find(ctx,
		bson.M{"source": "osm", "class": bson.M{"$in": classes}},
		options.Find().SetProjection(bson.M{"name": 1, "class": 1, "lat": 1, "lng": 1}))
	if err != nil {
		r.logger.Warnf("chart search: settlement load failed, no areas this pass: %v", err)
		return nil
	}
	defer cur.Close(ctx)

	var out []settlement
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
	if err := cur.Err(); err != nil {
		r.logger.Warnf("chart search: settlement load broke off, no areas this pass: %v", err)
		return nil
	}
	a.settlements = out
	a.states = make(map[int]string)
	a.loaded = true
	r.logger.Infof("chart search: %d settlements loaded for area annotation", len(out))
	return out
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

// stateForSettlement resolves (and caches) the state containing a settlement,
// via a point-in-polygon lookup against the ingested OSM admin_level=4
// boundaries. Empty when unresolvable — offshore, no OSM attached, or a
// boundary the ingest didn't close.
func (r *ENCRenderer) stateForSettlement(ctx context.Context, idx int) string {
	a := &r.areas
	a.mu.Lock()
	if st, ok := a.states[idx]; ok {
		a.mu.Unlock()
		return st
	}
	s := a.settlements[idx]
	a.mu.Unlock()

	st := ""
	if r.osm != nil && r.osm.Overview != nil {
		var d struct {
			Name string `bson:"name"`
		}
		err := r.osm.Overview.FindOne(ctx, bson.M{
			"class":            "admin",
			"tags.admin_level": "4",
			"geometry": bson.M{"$geoIntersects": bson.M{"$geometry": bson.M{
				"type":        "Point",
				"coordinates": []float64{s.lng, s.lat},
			}}},
		}, options.FindOne().SetProjection(bson.M{"name": 1})).Decode(&d)
		if err == nil {
			st = stateAbbrev(d.Name)
		} else if ctx.Err() != nil {
			// Budget ran out — don't cache "no state" for a settlement we
			// never actually looked up.
			return ""
		}
	}
	a.mu.Lock()
	a.states[idx] = st
	a.mu.Unlock()
	return st
}

// annotateAreas fills in each hit's Area. Failures leave hits as they were.
func (r *ENCRenderer) annotateAreas(ctx context.Context, hits []SearchResult) {
	if len(hits) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, areaLookupBudget)
	defer cancel()
	settlements := r.loadSettlements(ctx)
	if len(settlements) == 0 {
		return
	}
	for i := range hits {
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

// stateAbbrev renders a state's postal code, falling back to the full name
// for anything not in the table (a Canadian province on a border chart reads
// better in full than dropped).
func stateAbbrev(name string) string {
	if ab, ok := stateAbbrevs[name]; ok {
		return ab
	}
	return name
}

var stateAbbrevs = map[string]string{
	"Alabama": "AL", "Alaska": "AK", "Arizona": "AZ", "Arkansas": "AR",
	"California": "CA", "Colorado": "CO", "Connecticut": "CT", "Delaware": "DE",
	"Florida": "FL", "Georgia": "GA", "Hawaii": "HI", "Idaho": "ID",
	"Illinois": "IL", "Indiana": "IN", "Iowa": "IA", "Kansas": "KS",
	"Kentucky": "KY", "Louisiana": "LA", "Maine": "ME", "Maryland": "MD",
	"Massachusetts": "MA", "Michigan": "MI", "Minnesota": "MN", "Mississippi": "MS",
	"Missouri": "MO", "Montana": "MT", "Nebraska": "NE", "Nevada": "NV",
	"New Hampshire": "NH", "New Jersey": "NJ", "New Mexico": "NM", "New York": "NY",
	"North Carolina": "NC", "North Dakota": "ND", "Ohio": "OH", "Oklahoma": "OK",
	"Oregon": "OR", "Pennsylvania": "PA", "Rhode Island": "RI", "South Carolina": "SC",
	"South Dakota": "SD", "Tennessee": "TN", "Texas": "TX", "Utah": "UT",
	"Vermont": "VT", "Virginia": "VA", "Washington": "WA", "West Virginia": "WV",
	"Wisconsin": "WI", "Wyoming": "WY",
	"District of Columbia": "DC", "Puerto Rico": "PR", "Guam": "GU",
	"United States Virgin Islands": "VI", "American Samoa": "AS",
	"Northern Mariana Islands": "MP",
}
