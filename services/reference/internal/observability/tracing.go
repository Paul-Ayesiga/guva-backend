package observability

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/guva-ug/guva-backend/services/reference/internal/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitTracing configures the global tracer provider with an OTLP gRPC
// exporter pointed at the collector. The returned shutdown function must be
// called from main on process exit to flush the queue.
func InitTracing(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	endpoint, insecure, err := parseOTLPEndpoint(cfg.OTLPEndpoint)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceNamespace("guva"),
			semconv.DeploymentEnvironmentName(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
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

// parseOTLPEndpoint accepts both `http://host:port` and `host:port` forms and
// returns the host:port plus whether plaintext should be used.
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
