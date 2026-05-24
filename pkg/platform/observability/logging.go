// Package observability bundles structured logging and OpenTelemetry
// tracing helpers used by every service.
//
// Logs: JSON-encoded slog, written to stdout, at the level configured
// per service. The Docker / Kubernetes log collector picks them up.
//
// Traces: OTLP gRPC export to the OpenTelemetry Collector, which fans
// out to Jaeger. The OTLP endpoint defaults to localhost:4317 and can
// be overridden by OTEL_EXPORTER_OTLP_ENDPOINT.
package observability

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON-encoding slog logger writing to stdout at the
// given level. Use slog.SetDefault on the result to make it the package-
// default logger.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
	}))
}
