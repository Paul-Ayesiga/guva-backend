# Developer Walkthrough

End-to-end guide for working in this repository, from a fresh machine to your first verified change. Targeted at engineers joining the platform squad. Reading time: about ten minutes.

If anything in this guide is wrong or out of date, fix it in the same PR as the surrounding change — the walkthrough is part of the platform, not separate from it.

---

## 1. One-Time Machine Setup

You need:

| Tool | Minimum version | Notes |
|---|---|---|
| Docker (or compatible) | 4.20 / Engine 24 | Required. Compose v2 plugin must be installed. |
| GNU Make | 3.81+ | Pre-installed on macOS; on Linux: `apt install make`. |
| Go | 1.22 | Required to build / run services. `brew install go` or use [gvm](https://github.com/moovweb/gvm). |
| `golangci-lint` | 1.61+ | Lint. See <https://golangci-lint.run>. |
| `gofumpt` | 0.7+ | Stricter formatter. `go install mvdan.cc/gofumpt@latest`. |
| `goimports` | latest | `go install golang.org/x/tools/cmd/goimports@latest`. |
| `migrate` (golang-migrate) | v4.17+ | DB migrations. `brew install golang-migrate`. |
| `pre-commit` | 3+ | Optional but recommended. `pipx install pre-commit`. |

macOS one-liner (assumes Homebrew):

```bash
brew install make go golang-migrate golangci-lint gofumpt
brew install --cask docker
pipx install pre-commit
go install golang.org/x/tools/cmd/goimports@latest
```

---

## 2. Clone and Bootstrap

```bash
git clone <repo-url> guva
cd guva/guva-backend

# Copy .env, pull images, friendly preflight checks.
make bootstrap

# Optional but recommended: install pre-commit hooks.
pre-commit install
```

`make bootstrap` is idempotent — re-run it any time you suspect something is off with your local environment.

---

## 3. Bring Up the Stack

```bash
make up        # detached — runs in two phases (see below)
make status    # see service health
make urls      # print the local URLs cheat sheet
```

`make up` is two-phase by design: it brings up every service except APISIX and waits for Keycloak to be healthy, then renders APISIX's declarative config from `deploy/compose/apisix/apisix.yaml.tmpl` (substituting the realm public key fetched live from Keycloak), then starts APISIX. The rendered `apisix.yaml` is gitignored — secrets and rotating values never reach version control. APISIX standalone-mode hot-reloads the file within ~1s; `make refresh-keys` forces a re-render after Keycloak rotates its realm key.

First boot takes 30–90 seconds depending on machine. Keycloak and APISIX are the slowest; the rest are ready in under 15 seconds.

If a service shows `unhealthy` after two minutes, get its logs:

```bash
make logs-keycloak
```

Common gotchas are listed in [§9 Troubleshooting](#9-troubleshooting).

---

## 4. Run the Reference Service

The reference service proves the stack works end to end. It is not a real service; treat it as the template for new ones.

```bash
# In a second terminal
make run-reference
```

In a third terminal, exercise it. `/ping` requires the `verify:citizen` scope — the gateway rejects unauthenticated calls, and the service double-checks scope as defence in depth.

```bash
# Easiest path: fetch a token and call it in one go
make ping

# Or do it manually:
TOKEN=$(make token)                                               # fetch a JWT
curl -sH "Authorization: Bearer $TOKEN" \
  localhost:8000/v1/reference/ping | jq                           # through APISIX
curl -sH "Authorization: Bearer $TOKEN" \
  localhost:7070/ping | jq                                        # direct
curl -sH "Authorization: Bearer $TOKEN" \
  localhost:7070/v1/ping | jq                                     # backcompat alias

# Negative checks (both expected to 401):
curl -s -o /dev/null -w "%{http_code}\n" localhost:8000/v1/reference/ping
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer not.a.real.token" \
  localhost:8000/v1/reference/ping

# Unauthenticated probes (no scope needed):
curl -s localhost:7070/metrics | head
curl -s localhost:7070/healthz && echo
curl -s localhost:7070/readyz  && echo
```

A successful `make ping` response includes a `caller` block extracted from the JWT — the gateway has verified the signature, the service has parsed the claims and confirmed `verify:citizen` is present:

```json
{
  "service": "reference",
  "environment": "local",
  "timestamp": "2026-05-24T01:36:02.93775Z",
  "caller": {
    "client": "guva-reference",
    "subject": "e60cf536-1bf0-4292-80d9-2fad59edf76e",
    "scopes": ["verify:citizen", "audit:read"],
    "issuer": "http://localhost:8080/realms/guva"
  }
}
```

Then open:

- **Jaeger** — <http://localhost:16686>. Select service `reference` and look for the `GET /ping` span.
- **Grafana** — <http://localhost:3000> (admin/admin). The Prometheus datasource is provisioned; query `up{job="reference"}` to confirm scrape success.
- **APISIX admin GUI** — <http://localhost:8002>. The `reference` service, route, JWT consumer, and plugins are loaded from declarative config.

> **Why the jwt-auth plugin and not openid-connect (for now)?** APISIX's `openid-connect` plugin is fully free and supports JWKS discovery — but using it requires the token's `iss` claim to match what the OIDC discovery doc reports as the issuer. Out of the box, tokens fetched via `localhost:8080` have `iss=http://localhost:8080/realms/guva`, but APISIX inside Docker reaches Keycloak at `http://keycloak:8080` (different host). For Phase 1 we use the OSS `jwt-auth` plugin with a per-consumer public key rendered from `apisix.yaml.tmpl` at boot. **Phase 2 (in flight)** switches to `openid-connect` proper with `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` so the discovery `issuer` stays stable while internal endpoints stay reachable — and then ultimately moves to a local DNS + TLS layer (e.g. Traefik in front of `*.localhost`) so dev/staging/prod all share the same auth flow.

If all four checks pass, your stack is healthy. Stop the reference service with Ctrl-C.

---

## 5. Day-to-Day Workflow

### 5.1 Branch

```bash
git switch -c feat/<scope>-<short-description>
```

Trunk-based development; keep branches short-lived. Rebase rather than merge.

### 5.2 Code

- Edit under `services/<service>/`. The service is responsible for its OpenAPI spec, its migrations, and its tests.
- Shared utilities belong in `pkg/` only after the second consumer materialises. Premature factoring is discouraged.

### 5.3 Lint and Test

```bash
make fmt    # gofumpt + goimports
make lint   # golangci-lint
make test   # unit tests with race detector + coverage
```

### 5.4 Database Migrations

Each service owns migrations under `services/<service>/migrations/`. Create one with:

```bash
migrate create -ext sql -dir services/<service>/migrations -seq descriptive_name
```

Apply locally:

```bash
make migrate                                   # all services
tools/scripts/db-migrate.sh <service> up       # one service
tools/scripts/db-migrate.sh <service> down 1   # roll back one step
```

Convention: every migration has both an `up` and a `down`. Reviewers reject migrations that cannot be rolled back; the only exception is migrations that drop a column whose data cannot be reconstructed — those must include a `-- IRREVERSIBLE` marker and call this out in the PR description.

### 5.5 Commit

Conventional Commits, present-tense, short subject:

```text
feat(reference): expose /v1/ping echo endpoint
fix(consent): treat expired consents as denied at the gateway
chore(deps): bump otelhttp to v0.55.0
```

If you have `pre-commit` installed, the hooks run automatically on `git commit`. The same hooks run in CI as a backstop.

---

## 6. Adding a New Service

Use `services/reference/` as the template. The skeleton requires nine moves; everything else flows from them.

1. `cp -R services/reference services/<name>`
2. Rename the module: edit `go.mod` `module` line; replace `reference` with `<name>` across the source.
3. Add an entry to `go.work`:
   ```go
   use (
       ./services/reference
       ./services/<name>
   )
   ```
4. Add a database to `deploy/compose/postgres/initdb.d/00-databases.sql` and the matching extensions in `01-extensions.sql`.
5. Add an APISIX route and (if needed) consumer to `deploy/compose/apisix/apisix.yaml.tmpl`.
6. Add a Prometheus scrape target to `deploy/compose/prometheus/prometheus.yml`.
7. Add the service to `docker-compose.yml` (later — when you want it running in the compose stack rather than via `go run`).
8. Write the OpenAPI spec at `services/<name>/api/openapi.yaml` first. Endpoints follow once the contract is reviewed.
9. Open the PR. Include a one-line entry in this walkthrough's [§4](#4-run-the-reference-service) "verify it works" recipe so future readers can exercise your service the same way they exercised reference.

---

## 7. Talking to the Infrastructure

### Postgres

```bash
make psql                                 # interactive psql
# Or specific service DB:
docker compose exec postgres \
  psql -U guva -d guva_verification
```

### Redis

```bash
make redis-cli
> SET demo "hello"
> GET demo
```

### Kafka

The broker (Apache `kafka-native` 4.3) listens on `localhost:9094` from your host. The native (GraalVM-compiled) image is a single binary — it intentionally **does not ship the JVM shell scripts** (no `kafka-console-producer.sh`, `kafka-topics.sh`, etc.), so you can't `docker compose exec` your way to a CLI.

#### Default: `kcat` from your host

`kcat` (formerly `kafkacat`) is a ~5 MB native binary that does produce, consume, and metadata against any broker. Install once, use everywhere — no Docker round-trips.

```bash
brew install kcat                                       # macOS
# apt install kcat                                      # Debian/Ubuntu

echo 'hello kafka' | kcat -P -b localhost:9094 -t demo  # produce
kcat -C -b localhost:9094 -t demo -o beginning -e       # consume + exit
kcat -L -b localhost:9094                               # cluster metadata
```

#### Fallback: the Apache JVM image as a one-off sidecar

Only reach for this when you specifically need an official `kafka-*.sh` script (topic admin with non-trivial configs, ACL ops, consumer-group inspection, etc.). **First run pulls ~500 MB** — Docker downloads `apache/kafka:4.3.0` (the JVM image, *different* from the `kafka-native` one already in your stack); subsequent runs are cached.

```bash
docker run --rm -i --network guva apache/kafka:4.3.0 \
  /opt/kafka/bin/kafka-console-producer.sh \
    --bootstrap-server kafka:9092 --topic demo <<< 'hello kafka'

docker run --rm --network guva apache/kafka:4.3.0 \
  /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server kafka:9092 --topic demo --from-beginning --max-messages 1
```

> **Local Kafka data is ephemeral.** The Docker named volume that would back `/var/lib/kafka/data` clashes with the image's non-root uid 1000 on macOS, so the compose file omits the mount. Data survives container restarts but not container removal (`make down` is safe; `make reset` wipes). Production Kafka of course persists — that's handled by the Strimzi-managed PVCs in the infra repo.

### RabbitMQ

Open the management UI at <http://localhost:15672> (guva/guva). The `webhooks.delivery` queue and dead-letter exchange are pre-provisioned per the topology in §12.5.

### Vault

```bash
export VAULT_ADDR=http://localhost:8200
export VAULT_TOKEN=dev-root-token
vault kv put secret/services/reference/config greeting=howdy
vault kv get secret/services/reference/config
```

### Keycloak

Admin console: <http://localhost:8080> (admin/admin). The `guva` realm is imported on first boot with the scope catalogue and the `guva-reference` client (secret `reference-dev-secret`).

Get a service token:

```bash
curl -s -X POST http://localhost:8080/realms/guva/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=client_credentials' \
  -d 'client_id=guva-reference' \
  -d 'client_secret=reference-dev-secret' | jq
```

---

## 8. Tearing Down

```bash
make down       # stop everything, keep volumes (fast restart later)
make reset      # stop AND drop volumes (full clean slate)
```

`make reset` is destructive — your Postgres data, Kafka logs, and Keycloak users are gone. Don't run it on a flow you care about.

---

## 9. Troubleshooting

### Port already in use

The compose stack publishes a lot of ports. If one collides with something else on your machine, layer an override:

```bash
cp docker-compose.override.example.yml docker-compose.override.yml
# Edit the override to remap the offending port.
make restart
```

### Keycloak takes forever to come up

The first start does schema migrations against Postgres. Two minutes is normal on a cold cache; subsequent starts are under 30 seconds. Confirm progress via `make logs-keycloak`.

### `host.docker.internal` doesn't resolve

On Linux, Docker Desktop adds `host.docker.internal` automatically; on rootless Podman or some hardened Linux setups, it doesn't. Add this to a `docker-compose.override.yml`:

```yaml
services:
  apisix:
    extra_hosts:
      - "host.docker.internal:host-gateway"
  prometheus:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

### `make test` says "no Go files"

You're in the repo root, but the workspace can't see your service. Make sure your service is listed in `go.work`.

### Migrations error with "Dirty database"

Some migration failed mid-way. Inspect, then either fix-forward or force:

```bash
tools/scripts/db-migrate.sh <service> force <previous_version>
```

Only force when you know the schema state.

### Postgres container is `unhealthy` after `make up`

If `make logs-postgres` shows `Error: in 18+, these Docker images are configured to store database data in a format which is compatible with "pg_ctlcluster"`, the volume mount is pointing at the wrong path. Postgres 18+ stores data under a major-version subdirectory (`/var/lib/postgresql/18/docker/`), so the mount in `docker-compose.yml` must be at the **parent** `/var/lib/postgresql`, not `/var/lib/postgresql/data` (which was correct on Postgres ≤ 16). This is a one-line fix in the compose file — see the comment block on the `postgres` service.

### Reference service returns 404 on `/ping` (or 200 on `/v1/ping` only)

You're running a stale binary. `go run` does not auto-reload on source edits — Ctrl-C the `make run-reference` terminal and re-run it to pick up route changes.

---

## 10. Where Next

- [../README.md](../README.md) — high-level conventions and repository layout.
- [../../guva-docs/03-architecture/07-system-architecture.md](../../guva-docs/03-architecture/07-system-architecture.md) — what these components do at the system level.
- [../../guva-docs/09-delivery/03-task-list.md](../../guva-docs/09-delivery/03-task-list.md) — the work the platform squad is tracking; pick a task with a green dependency line.

Happy hacking.
