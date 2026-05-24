package observability

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TracingConfig captures the per-service inputs to InitTracing.
type TracingConfig struct {
	ServiceName  string // e.g. "identity"
	Namespace    string // e.g. "guva"
	Environment  string // e.g. "local" / "staging" / "production"
	OTLPEndpoint string // e.g. "http://otel-collector:4317" or "otel-collector:4317"
}

// InitTracing configures the global OpenTelemetry tracer provider with an
// OTLP gRPC exporter pointed at the collector. The returned shutdown
// function MUST be called on process exit to flush the queue.
//
// Returns a no-op shutdown function and a non-nil error if anything fails;
// callers can choose to continue without tracing rather than abort.
func InitTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	endpoint, insecure, err := parseOTLPEndpoint(cfg.OTLPEndpoint)
	if err != nil {
		return func(context.Context) error { return nil }, err
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithTimeout(5 * time.Second),
	}
	if insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
	if err != nil {
		return func(context.Context) error { return nil }, fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceNamespace(cfg.Namespace),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return func(context.Context) error { return nil }, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return tp.Shutdown, nil
}

// parseOTLPEndpoint accepts both `http://host:port` and `host:port` forms
// and returns host:port plus whether plaintext should be used.
func parseOTLPEndpoint(raw string) (string, bool, error) {
	if raw == "" {
		return "", false, fmt.Errorf("empty OTLP endpoint")
	}
	if !hasScheme(raw) {
		return raw, true, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("invalid OTLP endpoint %q: %w", raw, err)
	}
	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("OTLP endpoint missing host: %q", raw)
	}
	return host, u.Scheme == "http", nil
}

func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ':' && i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/' {
			return true
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return false
}
