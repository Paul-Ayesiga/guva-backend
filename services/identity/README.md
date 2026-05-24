# identity

The platform's owner-of-record for the **scope catalogue** and **consumer registrations**.

Keycloak is the source of truth for credentials and the issuer of tokens. This service is the lifecycle-metadata layer beside Keycloak: it exposes the scope catalogue as a discoverable API, and it tracks who's onboarded as a consumer of the platform (who, when, by whom, for which scopes).

## What this version does

| Endpoint | Auth | Behaviour |
|---|---|---|
| `GET  /scopes`            | bearer + `audit:read` scope | Returns the hardcoded scope catalogue. |
| `POST /consumers`         | bearer + `audit:read` scope | Records a consumer registration intent in our DB. **Does not yet create the corresponding Keycloak client** — that's the next iteration. |
| `GET  /consumers/{id}`    | bearer + `audit:read` scope | Reads a registration back. |
| `GET  /healthz` / `/readyz` | none | Standard probes. |

## Run locally

```bash
# 1. Bring up the supporting stack (postgres, vault, keycloak, apisix, …)
make up                                    # from repo root

# 2. Apply this service's migrations
make migrate                               # creates consumer_registrations table

# 3. Run the service
make run-identity                          # listens on :7071
```

Test it:

```bash
TOKEN=$(make token)                        # fetch a JWT
curl -sH "Authorization: Bearer $TOKEN" \
  http://localhost:7071/scopes | jq

# Through the gateway:
curl -sH "Authorization: Bearer $TOKEN" \
  http://localhost:8000/v1/identity/scopes | jq

# Register a consumer:
curl -sH "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"agency_name":"Acacia Innovations","contact_email":"ops@acacia.example","keycloak_client_id":"acacia-onboarding","scopes":["verify:citizen"]}' \
  -X POST http://localhost:8000/v1/identity/consumers | jq
```

## Layout

```text
services/identity/
├── api/openapi.yaml             OpenAPI 3.1 spec (single source of truth)
├── cmd/server/main.go           Entry point — wires config, logger, db, server
├── internal/
│   ├── config/                  Env-driven config; Vault for the DB password
│   ├── server/                  HTTP handlers + the in-code scope catalogue
│   └── store/                   pgx-backed Postgres repository
├── migrations/                  golang-migrate SQL (000001 creates the table)
├── Dockerfile                   Multi-stage; distroless final image
└── README.md                    This file
```

## What's next

- **Keycloak admin-API integration**: when a consumer is registered, also create the corresponding Keycloak client. Probably an async outbox + worker so the API returns 202 + a status endpoint.
- **Pagination and listing** on `/consumers`.
- **Webhook subscriptions** owned by each registration.
- **Scope catalogue sync** with Keycloak (today it's hardcoded; eventually `/scopes` reads from Keycloak's `clientScopes` so the two can't drift).
- **Real auth scopes**: today read endpoints accept `audit:read` because that's what `guva-reference` carries; production uses a dedicated `identity:read` / `admin:consumers` split.
