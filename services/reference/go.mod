module github.com/guva-ug/guva-backend/services/reference

go 1.22

require (
	github.com/google/uuid v1.6.0
	github.com/prometheus/client_golang v1.20.4
	go.opentelemetry.io/otel v1.30.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.30.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.30.0
	go.opentelemetry.io/otel/sdk v1.30.0
	go.opentelemetry.io/otel/trace v1.30.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.55.0
)
