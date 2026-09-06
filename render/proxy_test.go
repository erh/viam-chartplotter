package render

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.viam.com/rdk/logging"
)

// newProxiedHandlers builds handlers with no chart collections attached and an
// upstream pointed at base — the configuration of every deployment that has no
// mongo_uri.
func newProxiedHandlers(t *testing.T, base string) *ENCHandlers {
	t.Helper()
	h := NewENCHandlers(NewENCRenderer(logging.NewTestLogger(t)), nil, nil, 6)
	p, err := NewUpstreamProxy(base, logging.NewTestLogger(t))
	if err != nil {
		t.Fatalf("NewUpstreamProxy: %v", err)
	}
	h.SetUpstream(p)
	return h
}

func TestProxyForwardsChartEndpoints(t *testing.T) {
	var gotPath, gotQuery, gotHop string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotHop = r.Header.Get(proxyHopHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[],"matched_query":"x"}`)
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	newProxiedHandlers(t, upstream.URL).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/noaa-enc/search?q=coimbra&limit=5", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/noaa-enc/search" {
		t.Errorf("upstream path = %q, want /noaa-enc/search", gotPath)
	}
	if gotQuery != "q=coimbra&limit=5" {
		t.Errorf("upstream query = %q, want the client's verbatim", gotQuery)
	}
	if gotHop != "1" {
		t.Errorf("hop header = %q, want 1 (loop guard)", gotHop)
	}
	if !strings.Contains(rec.Body.String(), "matched_query") {
		t.Errorf("body not relayed: %s", rec.Body.String())
	}
}

func TestProxyForwardsPOSTBody(t *testing.T) {
	var gotBody, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotMethod = string(b), r.Method
		_, _ = io.WriteString(w, `{"waypoints":[]}`)
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	newProxiedHandlers(t, upstream.URL).Register(mux)

	body := `{"waypoints":[{"lat":40.7,"lng":-74.0}]}`
	req := httptest.NewRequest("POST", "/noaa-enc/optimize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotMethod != "POST" || gotBody != body {
		t.Errorf("upstream got %s %q, want POST %q", gotMethod, gotBody, body)
	}
}

// A request that arrives already carrying the hop header has been forwarded
// once. Proxying it again would loop between two chartless servers, so it must
// be answered locally — as a plain "no charts here" error.
func TestProxyDoesNotForwardTwice(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forwarded a request that had already been proxied")
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	newProxiedHandlers(t, upstream.URL).Register(mux)

	req := httptest.NewRequest("GET", "/noaa-enc/search?q=coimbra", nil)
	req.Header.Set(proxyHopHeader, "1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want a local failure; body %s", rec.Body.String())
	}
}

// The frontend reads {"error": ...} off any failed chart response; a dead
// upstream must not break that contract.
func TestProxyReportsUnreachableUpstreamAsJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := upstream.URL
	upstream.Close() // nothing is listening now

	mux := http.NewServeMux()
	newProxiedHandlers(t, dead).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/noaa-enc/autoroute?startLat=40&startLon=-74&endLat=41&endLon=-71", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Errorf("no error message in %s", rec.Body.String())
	}
}

// With charts attached locally there is nothing to forward, even when an
// upstream is configured.
func TestProxyNotUsedWhenChartsAreLocal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forwarded %s despite local charts", r.URL.Path)
	}))
	defer upstream.Close()

	h := newProxiedHandlers(t, upstream.URL)
	// A non-nil collection handle is enough; the request fails on the (absent)
	// server, which is fine — the point is that it stayed here.
	local := true
	served := false
	mux := http.NewServeMux()
	mux.HandleFunc("/noaa-enc/search", h.orUpstream(func() bool { return local }, func(w http.ResponseWriter, r *http.Request) {
		served = true
	}))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/noaa-enc/search?q=x", nil))
	if !served {
		t.Errorf("local handler was bypassed")
	}
}

func TestUpstreamProxyBaseURL(t *testing.T) {
	for _, tc := range []struct{ in, wantHost, wantScheme string }{
		{"https://nycmaps.checkmatemaps.com/", "nycmaps.checkmatemaps.com", "https"},
		{"http://nycmaps.checkmatemaps.com", "nycmaps.checkmatemaps.com", "http"},
		{"nycmaps.checkmatemaps.com", "nycmaps.checkmatemaps.com", "https"},
	} {
		p, err := NewUpstreamProxy(tc.in, nil)
		if err != nil {
			t.Fatalf("NewUpstreamProxy(%q): %v", tc.in, err)
		}
		if p.Host() != tc.wantHost || p.base.Scheme != tc.wantScheme {
			t.Errorf("NewUpstreamProxy(%q) = %s://%s, want %s://%s", tc.in, p.base.Scheme, p.Host(), tc.wantScheme, tc.wantHost)
		}
	}
	if _, err := NewUpstreamProxy("  ", nil); err == nil {
		t.Errorf("empty base URL accepted")
	}
}

// The chart server is deployed separately and can predate an endpoint added
// here. Its bare 404 becomes a message naming that, so the app shows something
// an operator can act on instead of "auto-route failed (404)".
func TestProxyExplainsMissingUpstreamEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()

	mux := http.NewServeMux()
	newProxiedHandlers(t, upstream.URL).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/noaa-enc/autoroute?startLat=40&startLon=-74&endLat=41&endLon=-71", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (the upstream's own)", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, rec.Body.String())
	}
	if !strings.Contains(body.Error, "/noaa-enc/autoroute") || !strings.Contains(body.Error, "redeploying") {
		t.Errorf("unhelpful error: %q", body.Error)
	}
}

// A 404 an endpoint produced itself (a feature that isn't there) is a real
// answer and must be relayed untouched.
func TestProxyLeavesRealUpstream404Alone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no such cell"}`)
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	newProxiedHandlers(t, upstream.URL).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/noaa-enc/navaids?bbox=1,2,3,4", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such cell") {
		t.Errorf("upstream's own 404 was rewritten: %s", rec.Body.String())
	}
}
