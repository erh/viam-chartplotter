package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.viam.com/rdk/logging"
)

// Only one deployment holds the chart database. Every other one — a boat's
// module, a laptop running `make dev` — is configured with no mongo_uri, so its
// renderer has no feature store and every chart endpoint would answer
// errNoCharts. Rather than make each client know which server to ask, the
// local server forwards those requests itself: the app always talks to its own
// origin, and whether the data is a Mongo query away or an ocean away is a
// deployment detail.
//
// Forwarding rather than redirecting is what makes this work from a browser:
// /noaa-enc/autoroute and /noaa-enc/search are fetch()ed for JSON, so a
// cross-origin hop would need CORS on the far end. Same-origin needs nothing.

// upstreamResponseTimeout bounds how long we wait for the upstream to START
// answering. Routing is the slow endpoint — a Portland-to-Key-West plan reads
// thousands of navigability tiles — so this is minutes, not seconds. It only
// covers the response headers; a body already streaming is not cut off.
const upstreamResponseTimeout = 5 * time.Minute

// proxyHopHeader marks a request that has already been forwarded once, so a
// chain of chart servers can never form a loop.
const proxyHopHeader = "X-Chartplotter-Proxy"

// UpstreamProxy forwards chart-data requests to another chartplotter server —
// the one with the NOAA collections attached.
type UpstreamProxy struct {
	base   *url.URL
	proxy  *httputil.ReverseProxy
	logger logging.Logger
}

// NewUpstreamProxy builds a proxy to baseURL, which may include a path prefix.
// A bare host is assumed to be https.
func NewUpstreamProxy(baseURL string, logger logging.Logger) (*UpstreamProxy, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return nil, fmt.Errorf("upstream proxy: empty base URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	base, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: parse %q: %w", baseURL, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("upstream proxy: %q has no host", baseURL)
	}
	p := &UpstreamProxy{base: base, logger: logger}
	p.proxy = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      upstreamTransport(),
		ModifyResponse: p.explainMissingEndpoint,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The frontend reads {"error": ...} off a failed response, so a
			// dead upstream should look like every other endpoint failure
			// rather than an opaque proxy error page.
			if logger != nil {
				logger.Warnf("chart proxy: %s %s -> %s: %v", r.Method, r.URL.Path, base.Host, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("chart server %s is unreachable: %v", base.Host, err),
			})
		},
	}
	return p, nil
}

func upstreamTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = upstreamResponseTimeout
	t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// rewrite points the outbound request at the upstream, keeping the path and
// query the client asked for (under the base's own path prefix, if it has one).
func (p *UpstreamProxy) rewrite(r *httputil.ProxyRequest) {
	r.Out.URL.Scheme = p.base.Scheme
	r.Out.URL.Host = p.base.Host
	r.Out.URL.Path = strings.TrimSuffix(p.base.Path, "/") + r.In.URL.Path
	r.Out.URL.RawQuery = r.In.URL.RawQuery
	// Address the upstream by its own name; it may vhost.
	r.Out.Host = p.base.Host
	r.SetXForwarded()
	// Loop guard: if the upstream is itself missing charts (or DNS points it
	// back here), it must answer for itself rather than forward again.
	r.Out.Header.Set(proxyHopHeader, "1")
	// The upstream is a public map server and has no business seeing the app's
	// session — this is an anonymous data fetch.
	r.Out.Header.Del("Cookie")
	r.Out.Header.Del("Authorization")
}

// explainMissingEndpoint turns an upstream 404 into a message that names the
// actual problem. The chart server is deployed separately and can be older than
// this one, in which case an endpoint added since simply isn't there — and
// "auto-route failed (404)" tells an operator nothing about why.
func (p *UpstreamProxy) explainMissingEndpoint(resp *http.Response) error {
	if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Request.URL.Path, "/noaa-enc/") {
		return nil
	}
	// A 404 the endpoint itself produced (a tile outside coverage, say) is
	// real; only an unrouted path answers with Go's plain-text default.
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		return nil
	}
	if p.logger != nil {
		p.logger.Warnf("chart proxy: %s has no %s — it needs redeploying", p.base.Host, resp.Request.URL.Path)
	}
	body, err := json.Marshal(map[string]string{
		"error": fmt.Sprintf("the chart server %s does not have %s; it is running an older build and needs redeploying",
			p.base.Host, resp.Request.URL.Path),
	})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// Forward proxies one request upstream.
func (p *UpstreamProxy) Forward(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}

// Host is the upstream's host, for logging.
func (p *UpstreamProxy) Host() string { return p.base.Host }
