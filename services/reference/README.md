# reference

Reference service for the GUVA backend monorepo. This service has no business purpose. It exists to:

1. Validate that the local development stack works end to end — gateway routing, OAuth handshake (TBD), metrics scrape, OTel traces, structured logs.
2. Serve as the template that every new service is scaffolded from.

## Run locally

```bash
# 1. Bring up the supporting stack
make up                              # from repo root

# 2. Run this service
make run-reference                   # from repo root
# ... or:
cd services/reference && go run ./cmd/server
```

Then exercise it:

```bash
# Directly (flat route on the service)
curl -s localhost:7070/ping | jq

# Through Kong (gateway adds the /v1/reference public prefix)
curl -s localhost:8000/v1/reference/ping | jq

# Metrics
curl -s localhost:7070/metrics | head

# Probes
curl -s localhost:7070/healthz
curl -s localhost:7070/readyz
```

Traces appear in Jaeger at <http://localhost:16686>. Metrics in Prometheus at <http://localhost:9090> and Grafana at <http://localhost:3000>.

## Layout

```text
services/reference/
├── api/openapi.yaml              Single source of truth for the API
├── cmd/server/main.go            Entry point — wires config, logger, server
├── internal/
│   ├── config/                   Env-driven config with validation
│   ├── health/                   Liveness / readiness state
│   ├── httpserver/               HTTP server, routes, middleware
│   └── observability/            slog, OTel tracing
├── migrations/                   golang-migrate SQL (empty for now)
├── Dockerfile                    Multi-stage, distroless, non-root
└── README.md                     This file
```

## Conventions

- All configuration loaded from environment; defaults match `.env.example`.
- All HTTP handlers wrapped in correlation-ID middleware.
- All logs structured JSON via `slog`.
- All outgoing requests propagate W3C trace context via OpenTelemetry.
- No business logic in `cmd/`; no HTTP code in `internal/domain` (none yet).
