package weather

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the OpenTelemetry instrumentation scope for weather spans.
const tracerName = "viam-chartplotter/weather"

// tracer returns the package tracer for the manual span sites (refreshNow,
// fetch, encode, gzip, prewarm). The provider is configured by the host
// process (see the root package's tracing setup); we just grab a named tracer.
func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
