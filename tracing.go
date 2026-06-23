package vc

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.viam.com/rdk/logging"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation library name we tag spans with so
// they're distinguishable in trace UIs from spans emitted by other
// libraries in the same process (e.g. otelhttp).
const tracerName = "viam-chartplotter"

// initTracer wires up an OpenTelemetry TracerProvider whose exporter
// only emits spans that are interesting — i.e. spans that took longer
// than the configured threshold, or that ended with an Error status.
// The dump shape is a compact single-line summary (name + duration +
// key attributes), not the full SDK JSON envelope, so it's readable
// inline with the rest of the logs.
//
// Returns a shutdown func that flushes any buffered spans. The caller
// should defer it (or invoke it from resource.Close) so the last
// in-flight batch makes it out before the process exits.
func initTracer(logger logging.Logger) (func(context.Context) error, error) {
	exporter := &slowSpanExporter{logger: logger}
	// BatchSpanProcessor buffers spans and flushes in the background so
	// the exporter doesn't block the request path. Sampling defaults to
	// AlwaysSample — we collect every span and let the exporter decide
	// whether to *log* it, so a long-tail slow span never gets sampled
	// out before we can see it.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)
	// W3C TraceContext + Baggage propagation so traces can be linked
	// across services if anything upstream sends `traceparent`.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	logger.Infof("tracing: span exporter ready (logs errored spans only)")
	return tp.Shutdown, nil
}

// slowSpanExporter implements sdktrace.SpanExporter. It only logs spans
// that ended with an Error status — those are the ones worth a line in
// the logs. Slow-but-successful requests are already reported (more
// usefully, with status / bytes / x-cache) by slowRequestLog, so logging
// every slow span here too was just duplicate per-tile noise.
type slowSpanExporter struct {
	logger logging.Logger
}

func (e *slowSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, sp := range spans {
		if sp.Status().Code != codes.Error {
			continue
		}
		dur := sp.EndTime().Sub(sp.StartTime())
		var attrs strings.Builder
		for _, kv := range sp.Attributes() {
			attrs.WriteByte(' ')
			attrs.WriteString(string(kv.Key))
			attrs.WriteByte('=')
			attrs.WriteString(kv.Value.Emit())
		}
		e.logger.Warnf("span %s dur=%s ERR=%q%s",
			sp.Name(), dur.Round(time.Millisecond), sp.Status().Description, attrs.String())
	}
	return nil
}

func (e *slowSpanExporter) Shutdown(context.Context) error { return nil }

// tracer is the package-level tracer. Used by manual span sites
// (refreshNow, writeGzip, prewarm, etc.) so we don't have to thread a
// Tracer through every call.
func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// defaultSlowRequestThreshold is the default duration above which a
// request gets a WARN log line with its timing breakdown. Most chart
// tile / sprite fetches finish in well under this; anything slower is
// worth a look in the logs. Override with CHARTPLOTTER_SLOW_LOG_MS.
const defaultSlowRequestThreshold = 500 * time.Millisecond

// slowRequestThreshold returns the configured threshold, parsing the
// CHARTPLOTTER_SLOW_LOG_MS env var if set. Invalid values fall back
// to the default rather than erroring — this is a dev-debug knob,
// not a config gate.
func slowRequestThreshold() time.Duration {
	if v := os.Getenv("CHARTPLOTTER_SLOW_LOG_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultSlowRequestThreshold
}

// statusRecorder snoops on the status code + bytes written so the
// slow-request log can include both without needing the handler to
// cooperate. http.ResponseWriter's default WriteHeader hook is the
// only way to capture status; Write count is bumped in our wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		s.ResponseWriter.WriteHeader(code)
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// Mirror net/http's implicit-200-on-first-Write so we record the
		// status even when the handler never calls WriteHeader.
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// slowRequestLog wraps next so any request that exceeds the configured
// threshold gets logged with method, path (incl. raw query), status,
// duration, bytes written, and the X-Cache header (HIT/STALE/MISS) so
// you can immediately tell whether the slow request was a cache miss
// vs. a slow handler.
func slowRequestLog(logger logging.Logger, next http.Handler) http.Handler {
	threshold := slowRequestThreshold()
	if threshold <= 0 {
		// Logging disabled — return the handler unchanged so we don't
		// pay the wrapper's allocation overhead for nothing.
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)
		if elapsed < threshold {
			return
		}
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		logger.Warnf("slow %s %s status=%d %s bytes=%d x-cache=%q",
			r.Method, path, rec.status, elapsed.Round(time.Millisecond),
			rec.bytes, rec.Header().Get("X-Cache"))
	})
}

// withTracing wraps next so each request gets an otelhttp span (parent
// for any manual spans created inside handlers) AND, on a duration
// above the configured threshold, a WARN log line. The two layers
// nest — otelhttp inside slow-log — so the log line sees the elapsed
// time of the inner span and the otelhttp span sees the wall-clock of
// the handler proper (which is what trace UIs report).
func withTracing(logger logging.Logger, next http.Handler) http.Handler {
	// otelhttp builds the per-request span. We pass `""` for the span
	// name and let otelhttp build it from method + route via the
	// SpanNameFormatter; we want the request URL path in the span name
	// so traces are filterable by route in the UI.
	traced := otelhttp.NewHandler(next, "",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
	return slowRequestLog(logger, traced)
}
