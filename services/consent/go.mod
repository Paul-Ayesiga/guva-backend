module github.com/guva-ug/guva-backend/services/consent

go 1.24.0

toolchain go1.24.0

require (
	github.com/google/uuid v1.6.0
	github.com/guva-ug/guva-backend/pkg/platform v0.0.0-00010101000000-000000000000
	github.com/guva-ug/guva-backend/pkg/secrets v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.7.1
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.55.0
)

replace github.com/guva-ug/guva-backend/pkg/platform => ../../pkg/platform

replace github.com/guva-ug/guva-backend/pkg/secrets => ../../pkg/secrets
