# GUVA Backend

Monorepo for the Government Unified Verification API backend services and the local development infrastructure that supports them.

This repository implements the architecture documented in [../guva-docs](../guva-docs). Cross-references in this README take the form `[##-name.md §X.Y](../guva-docs/...)` so that every implementation choice can be traced back to the design that mandated it.

---

## Repository Layout

```
guva-backend/
├── services/                 Microservices, one directory per service
│   └── reference/            Reference service used to validate the platform
├── pkg/                      Shared Go libraries (logging, OTel, errors)
├── deploy/
│   └── compose/              Local-development infrastructure stack
│       ├── postgres/         PostgreSQL with pgcrypto, pg_partman, pgaudit
│       ├── keycloak/         Realm export and bootstrap
│       ├── kong/             Declarative gateway configuration
│       ├── vault/            Dev-mode policies and seed
│       ├── prometheus/       Scrape configuration
│       ├── grafana/          Datasources and starter dashboards
│       ├── otel/             OpenTelemetry collector pipeline
│       └── rabbitmq/         Definitions for queues, exchanges, users
├── tools/
│   └── scripts/              Bootstrap, migration, seed scripts
├── docs/
│   └── DEVELOPMENT.md        End-to-end developer walkthrough
├── docker-compose.yml        Full local stack (entrypoint for developers)
├── docker-compose.override.example.yml
├── Makefile                  Common workflows (up, down, lint, test, ...)
├── go.work                   Go workspace tying services together
├── .editorconfig
├── .gitignore
├── .gitattributes
├── .golangci.yml
├── .pre-commit-config.yaml
└── .env.example              Local environment defaults
```

The layout reflects two architectural commitments from the design:

- **Database-per-service** ([§8.1](../guva-docs/03-architecture/08-database-design.md)). Each service under `services/` owns its own schema; cross-service data access goes via API or events, never via shared tables.
- **API-first** ([§7.1](../guva-docs/03-architecture/07-system-architecture.md)). Every service ships an OpenAPI 3.1 spec under `services/<name>/api/openapi.yaml`, and contract tests in CI verify that the live endpoints match.

---

## Quick Start

Requirements: Docker Desktop (≥ 4.20) or compatible engine, GNU Make, Go ≥ 1.22.

```bash
# One-time bootstrap (copies .env, generates dev secrets, pulls images)
make bootstrap

# Bring the whole stack up
make up

# Tail logs
make logs

# Stop everything (keeps volumes)
make down

# Nuke everything (drops volumes)
make reset
```

After `make up`, the following endpoints are reachable on `localhost`:

| Component | URL | Default credentials | Anchor |
|---|---|---|---|
| APISIX Gateway (proxy) | http://localhost:8000 | — | [§7.2.1](../guva-docs/03-architecture/07-system-architecture.md) |
| APISIX Prom metrics | http://localhost:9091 | — | [§19.6](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) |
| Caddy (TLS edge) | https://auth.guva.localhost | — (auto-issued local cert) | run `make trust-ca` once |
| Keycloak (canonical, via Caddy) | https://auth.guva.localhost | `admin` / `admin` (dev only) | [§7.2.2](../guva-docs/03-architecture/07-system-architecture.md) |
| Keycloak (raw, debugging) | http://localhost:8080 | `admin` / `admin` (dev only) | bypasses Caddy |
| Vault UI | http://localhost:8200 | token: `dev-root-token` | [§10.4](../guva-docs/05-security/10-security-architecture.md) |
| PostgreSQL | localhost:5432 | `guva` / `guva` | [§19.3](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) |
| Redis | localhost:6379 | — | [§17.2](../guva-docs/03-architecture/17-scalability-strategy.md) |
| Kafka broker | localhost:9092 | — | [§12.1](../guva-docs/03-architecture/12-event-driven-messaging.md) |
| RabbitMQ management | http://localhost:15672 | `guva` / `guva` | [§12.1](../guva-docs/03-architecture/12-event-driven-messaging.md) |
| Apicurio Registry | http://localhost:8081 | — | [§19.5](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) |
| MinIO Console | http://localhost:9001 | `guva` / `guvaguva` | [§19.3](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) |
| Prometheus | http://localhost:9090 | — | [§14](../guva-docs/06-infrastructure/14-monitoring-observability.md) |
| Grafana | http://localhost:3000 | `admin` / `admin` | [§14](../guva-docs/06-infrastructure/14-monitoring-observability.md) |
| Jaeger | http://localhost:16686 | — | [§19.9](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) |
| Reference service | http://localhost:7070 | — | This repo, `services/reference` |

All credentials above are **development defaults only**. Production credentials are issued by HashiCorp Vault ([§10.4](../guva-docs/05-security/10-security-architecture.md)) and are never checked into the repository.

---

## What This Repository Is, and What It Is Not

This repository **is**:

- The home of every backend service, its OpenAPI contract, its database migrations, its tests, and its Dockerfile.
- The single source of truth for the local development environment. Anything that runs on a developer laptop runs through `docker-compose.yml` or `make`.
- The substrate against which CI runs. Pipelines in [13-devops-deployment.md §13.4](../guva-docs/06-infrastructure/13-devops-deployment.md) execute the same `make` targets that developers use locally.

This repository **is not**:

- Production infrastructure-as-code. Terraform modules and Kubernetes manifests for non-local environments live in a separate `guva-infra` repository (to be created in Phase 0; see [09-delivery/03-task-list.md WS1-08](../guva-docs/09-delivery/03-task-list.md)).
- The frontend. The Admin Dashboard and Developer Portal live in `guva-frontend` (also to be created).
- A staging or production deployment. The compose stack is sized and credentialed for a single developer machine. It is **not** a substitute for the Kubernetes deployment described in [§13.3](../guva-docs/06-infrastructure/13-devops-deployment.md).

---

## Engineering Conventions

### Language and frameworks

- **Go ≥ 1.22** for new microservices ([§19.1](../guva-docs/06-infrastructure/19-recommended-tech-stack.md)). The `services/reference` skeleton demonstrates the expected structure.
- **Node.js with NestJS** is permitted for administrative and integration services where iteration speed outweighs raw throughput.
- **Java with Spring Boot** is permitted only for adapters to legacy government systems whose existing client libraries are predominantly Java.

### Code organisation per service

```
services/<name>/
├── api/openapi.yaml          OpenAPI 3.1 spec — single source of truth
├── cmd/server/main.go        Entry point
├── internal/                 Service-private packages
│   ├── config/               Configuration loading and validation
│   ├── http/                 HTTP handlers, middleware, routing
│   ├── domain/               Business logic, free of transport details
│   ├── store/                Repository implementations
│   └── observability/        Tracing, metrics, structured logging
├── migrations/               golang-migrate SQL files
├── Dockerfile                Multi-stage build, minimal final image
├── go.mod
└── README.md                 Service-specific operator notes
```

### Style and quality gates

- `golangci-lint` configured in `.golangci.yml`; runs in `make lint`.
- `gofumpt` for formatting (stricter than `gofmt`).
- `go test ./...` with coverage gates per service.
- Pre-commit hooks in `.pre-commit-config.yaml`: trailing-whitespace, end-of-file-fixer, yaml/json linting, `gofumpt`, `golangci-lint`, `markdownlint`.

### Commit and branch hygiene

- Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`).
- Trunk-based development. Feature branches short-lived; PRs merged via the rebased fast-forward strategy.
- No direct pushes to `main`. CI gates every merge.

---

## Further Reading

| Document | Audience |
|---|---|
| [docs/DEVELOPMENT.md](./docs/DEVELOPMENT.md) | Full developer walkthrough — first day to first commit |
| [../guva-docs/09-delivery/01-implementation-plan.md](../guva-docs/09-delivery/01-implementation-plan.md) | Workstream organisation that this repo implements |
| [../guva-docs/03-architecture/07-system-architecture.md](../guva-docs/03-architecture/07-system-architecture.md) | The architectural shape the services collectively form |
| [../guva-docs/06-infrastructure/19-recommended-tech-stack.md](../guva-docs/06-infrastructure/19-recommended-tech-stack.md) | Why we use what we use |

---

## Status

| Field | Value |
|---|---|
| Version | 0.1.0 |
| Status | Phase 0 — Foundation Setup |
| Owner | Platform Engineering squad ([§1.3.1](../guva-docs/09-delivery/01-implementation-plan.md)) |
