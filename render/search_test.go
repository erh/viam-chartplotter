package render

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/test"
)

func TestClassLabel(t *testing.T) {
	test.That(t, ClassLabel("LIGHTS"), test.ShouldEqual, "Light")
	test.That(t, ClassLabel("BOYLAT"), test.ShouldEqual, "Lateral buoy")
	// An acronym we have no wording for comes back as itself rather than as a
	// guess at what it means.
	test.That(t, ClassLabel("XYZZY9"), test.ShouldEqual, "XYZZY9")
}

func TestDedupeSearchResultsKeepsTheNearest(t *testing.T) {
	// The same light charted in three cells (coastal, approach, harbour).
	in := []SearchResult{
		{Name: "Brenton Reef Light", Class: "LIGHTS", Cell: "US3RI1AA", DistanceMeters: 5000},
		{Name: "Brenton Reef Light", Class: "LIGHTS", Cell: "US5RI1BB", DistanceMeters: 120},
		{Name: "brenton reef light", Class: "LIGHTS", Cell: "US4RI1CC", DistanceMeters: 900},
		{Name: "Brenton Reef", Class: "SEAARE", Cell: "US4RI1CC", DistanceMeters: 700},
	}
	out := dedupeSearchResults(in)
	// One entry per name+class: the light collapses to one, the sea area is
	// a different class so it survives alongside.
	test.That(t, len(out), test.ShouldEqual, 2)
	test.That(t, out[0].Cell, test.ShouldEqual, "US5RI1BB") // the nearest copy
	test.That(t, out[0].DistanceMeters, test.ShouldAlmostEqual, 120.0, 0.001)
	test.That(t, out[1].Class, test.ShouldEqual, "SEAARE")
}

func TestDedupeSearchResultsWithoutDistances(t *testing.T) {
	// No origin: distances are -1 and the first hit stands rather than being
	// replaced by an arbitrary later duplicate.
	in := []SearchResult{
		{Name: "Hudson Canyon", Class: "SEAARE", Cell: "A", DistanceMeters: -1},
		{Name: "Hudson Canyon", Class: "SEAARE", Cell: "B", DistanceMeters: -1},
	}
	out := dedupeSearchResults(in)
	test.That(t, len(out), test.ShouldEqual, 1)
	test.That(t, out[0].Cell, test.ShouldEqual, "A")
}

func TestSearchResultSpan(t *testing.T) {
	// A point feature has no extent; an area does, so the frontend can tell
	// "centre on this" from "frame this".
	point := SearchResult{BBox: [4]float64{-71.4, 41.4, -71.4, 41.4}}
	test.That(t, point.SearchBBoxSpanMeters(), test.ShouldAlmostEqual, 0.0, 0.001)

	area := SearchResult{BBox: [4]float64{-71.5, 41.4, -71.4, 41.5}}
	test.That(t, area.SearchBBoxSpanMeters(), test.ShouldBeGreaterThan, 1000)
}

func TestSearchHandlerNeedsAQuery(t *testing.T) {
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/noaa-enc/search", nil))
	test.That(t, rec.Code, test.ShouldEqual, http.StatusBadRequest)
}

func TestSearchHandlerNoCharts(t *testing.T) {
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/noaa-enc/search?q=brenton", nil))
	test.That(t, rec.Code, test.ShouldEqual, http.StatusServiceUnavailable)
	test.That(t, rec.Body.String(), test.ShouldContainSubstring, "mongo_uri")
}

func TestWrapChartQueryError(t *testing.T) {
	// The driver's read-deadline error arrives as a plain wrapped string, not
	// as context.DeadlineExceeded, so the substring path is the one that
	// actually fires in production. Both must classify as a timeout.
	raw := errors.New("connection(host:27017[-13]) incomplete read of full message: context deadline exceeded: read tcp: i/o timeout")
	wrapped := wrapChartQueryError(raw, "chart search", "run datasync --indexes-only")
	test.That(t, errors.Is(wrapped, ErrChartQueryTimeout), test.ShouldBeTrue)
	test.That(t, wrapped.Error(), test.ShouldContainSubstring, "chart search")
	test.That(t, wrapped.Error(), test.ShouldContainSubstring, "indexes-only")

	typed := wrapChartQueryError(fmt.Errorf("find: %w", context.DeadlineExceeded), "auto-route", "hint")
	test.That(t, errors.Is(typed, ErrChartQueryTimeout), test.ShouldBeTrue)

	// Anything else keeps its own identity — a real failure must not be
	// reported to the operator as "your index is missing".
	other := errors.New("connection refused")
	test.That(t, errors.Is(wrapChartQueryError(other, "chart search", "hint"), ErrChartQueryTimeout), test.ShouldBeFalse)
	test.That(t, wrapChartQueryError(other, "chart search", "hint").Error(), test.ShouldContainSubstring, "connection refused")
}

func TestSearchFallbacksDropTrailingWords(t *testing.T) {
	// "booth bay marina" finds nothing on a chart with no marina by that name,
	// so the search falls back to "booth bay" — which finds Boothbay Harbor,
	// what the operator was actually steering for.
	test.That(t, searchFallbacks("booth bay marina"), test.ShouldResemble,
		[]string{"booth bay marina", "booth bay", "booth"})
	// A single word has nothing to fall back to.
	test.That(t, searchFallbacks("boothbay"), test.ShouldResemble, []string{"boothbay"})
	test.That(t, searchFallbacks("  padded  "), test.ShouldResemble, []string{"padded"})
}

func TestPointBoxIsSquareAndCentred(t *testing.T) {
	b := pointBox(RoutePoint{Lat: 41.5, Lng: -71.3}, 1500)
	test.That(t, (b[0]+b[2])/2, test.ShouldAlmostEqual, -71.3, 1e-9)
	test.That(t, (b[1]+b[3])/2, test.ShouldAlmostEqual, 41.5, 1e-9)
	// ~3 km across each way, with the latitude correction applied to longitude.
	test.That(t, haversineMeters(b[1], b[0], b[3], b[0]), test.ShouldAlmostEqual, 3000.0, 50)
	test.That(t, haversineMeters(b[1], b[0], b[1], b[2]), test.ShouldAlmostEqual, 3000.0, 50)
}

func TestFoldNameIgnoresPunctuation(t *testing.T) {
	// The data says "Chandler's Wharf"; a searcher types "chandlers wharf".
	// Treating that as a mismatch buries the exact answer.
	test.That(t, foldName("chandler's wharf"), test.ShouldEqual, "chandlerswharf")
	test.That(t, foldName("point judith (n)"), test.ShouldEqual, "pointjudithn")
	test.That(t, foldName("st. george"), test.ShouldEqual, "stgeorge")
}

func TestRankPutsFullNameMatchesFirst(t *testing.T) {
	// A text index matching "chandlers wharf" returns everything sharing
	// either word. Sorting purely by distance puts a restaurant a mile away
	// above the wharf of that name further off — which is not what was asked.
	origin := &RoutePoint{Lat: 43.85, Lng: -69.63}
	out := []SearchResult{
		{Name: "Robinson's Wharf", DistanceMeters: 1852},
		{Name: "Chandler's Wharf", DistanceMeters: 53708}, // 29 nm
		{Name: "Chandler Road", DistanceMeters: 3704},
	}
	rankSearchResults(out, "chandlers wharf", origin)
	test.That(t, out[0].Name, test.ShouldEqual, "Chandler's Wharf")

	// Among equally good name matches, the near one wins.
	both := []SearchResult{
		{Name: "Chandlers Wharf", DistanceMeters: 90000},
		{Name: "Chandler's Wharf", DistanceMeters: 1000},
	}
	rankSearchResults(both, "chandlers wharf", origin)
	test.That(t, out[0].Name, test.ShouldEqual, "Chandler's Wharf")
	test.That(t, both[0].DistanceMeters, test.ShouldAlmostEqual, 1000.0, 1)
}
