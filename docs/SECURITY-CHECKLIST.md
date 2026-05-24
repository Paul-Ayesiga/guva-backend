# Security Hardening Checklist

What's enforced, what's documented but not yet implemented, and what's the responsibility of the deploying environment.

The checklist is grouped by concern. Each item is one of:

- ✅ **Implemented** — present in this repository, verifiable today
- 📝 **Documented** — designed in `guva-docs/` or this folder, deferred until needed
- 🌐 **Environment** — must be configured per environment (local doesn't, prod must)

For threat-model background see [guva-docs/05-security/10-security-architecture.md](../../guva-docs/05-security/10-security-architecture.md).

---

## 1. Transport security (TLS)

| | Item | Source |
|---|---|---|
| ✅ | All public traffic to Keycloak is TLS-terminated by Caddy at `https://auth.guva.localhost` | `deploy/compose/caddy/Caddyfile` |
| ✅ | Caddy uses its internal CA for local dev; cert lifetimes are short, auto-rotated | Caddy default |
| 🌐 | Staging and production use ACME (Let's Encrypt) for valid public certs | [ENVIRONMENTS.md §2](./ENVIRONMENTS.md) |
| 📝 | TLS 1.3 only; TLS 1.2 supported only during a migration window | [§10.2](../../guva-docs/05-security/10-security-architecture.md) |
| 📝 | Cipher suites restricted to forward-secret only | [§10.2](../../guva-docs/05-security/10-security-architecture.md) |
| 🌐 | Mutual TLS for service-to-service calls within the cluster | Deferred; deploy via service mesh in production |

---

## 2. Token validation (at the gateway)

| | Item | Source |
|---|---|---|
| ✅ | All `/v1/*` routes require a valid bearer token | `deploy/compose/apisix/apisix.yaml` |
| ✅ | APISIX validates JWT signature against Keycloak's JWKS via OIDC discovery | `openid-connect` plugin |
| ✅ | Issuer (`iss`), expiry (`exp`), and audience (`azp`) checked | `openid-connect` plugin schema |
| ✅ | Algorithm allowlist enforced — only `RS256` accepted; `none` and unsupported algorithms rejected | `accept_none_alg: false`, `accept_unsupported_alg: false` |
| ✅ | JWKS refresh is automatic on unknown `kid`; no manual key rotation step | Phase 2.1 |
| 📝 | Token introspection (real-time revocation) — currently JWT-only validation | [AUTH.md §8](./AUTH.md) |
| 📝 | Audience claim enforcement via the `aud` claim (multi-audience scenarios) | [AUTH.md §8](./AUTH.md) |

---

## 3. Token validation (at the service)

| | Item | Source |
|---|---|---|
| ✅ | Services re-check scope server-side (defence in depth) | `services/reference/internal/auth` |
| ✅ | Token parsing does NOT re-verify signature (gateway is trusted) | Same; documented in `auth.go` header |
| ✅ | Missing or malformed Authorization header → 401 | `RequireScope` middleware |
| ✅ | Missing required scope → 403 with structured error | `RequireScope` middleware |
| 📝 | Audit log of authentication failures emitted to event bus | [§7.2.5](../../guva-docs/03-architecture/07-system-architecture.md) — deferred |

---

## 4. Identity provider hardening

| | Item | Source |
|---|---|---|
| ✅ | Realm has `bruteForceProtected: true` — Keycloak rate-limits failed logins per IP | `deploy/compose/keycloak/realm-export.json` |
| ✅ | Access token lifespan 1 hour, refresh-token reuse disabled (`refreshTokenMaxReuse: 0`) | Same |
| ✅ | SSO session idle timeout 30 min, max lifespan 10 hours | Same |
| ✅ | Email duplicates not allowed (`duplicateEmailsAllowed: false`) | Same |
| ✅ | Public registration disabled (`registrationAllowed: false`) | Same |
| 🌐 | Admin password rotated quarterly | [ENVIRONMENTS.md §3](./ENVIRONMENTS.md) |
| 🌐 | MFA required for all admin accounts | [§10.5](../../guva-docs/05-security/10-security-architecture.md) |
| 🌐 | Keycloak runs in production mode (`start`, not `start-dev`) in staging/prod | [ENVIRONMENTS.md §2](./ENVIRONMENTS.md) |
| 🌐 | Realm signing key rotation drilled twice a year | [RUNBOOK-AUTH.md §10](./RUNBOOK-AUTH.md) |

---

## 5. Rate limiting

| | Item | Source |
|---|---|---|
| ✅ | Per-IP rate limit on every `/v1/*` route (600/min, 5xx burst absorbed) | `apisix.yaml` `limit-count` plugin |
| ✅ | Failed-login brute-force protection at Keycloak (per IP) | `bruteForceProtected: true` |
| 📝 | Stricter limits for high-value endpoints (e.g. `/verify/land`) tuned during traffic baseline | [§6.1](../../guva-docs/02-requirements/06-non-functional-requirements.md) |
| 📝 | Distributed rate limiting via Redis (single counter across APISIX replicas) | `limit-count` `policy: redis` — switch when running multiple APISIX replicas |
| 🌐 | WAF (Cloudflare, AWS WAF, …) in front of Caddy in production | [§13.8](../../guva-docs/06-infrastructure/13-devops-deployment.md) |

---

## 6. Secret management

| | Item | Source |
|---|---|---|
| ✅ | No production secrets in version control (`pre-commit run gitleaks --all-files` enforced) | `.pre-commit-config.yaml` |
| ✅ | Local `.env` files are gitignored | `.gitignore` |
| ✅ | The Keycloak realm public key is fetched live, never pinned | Phase 2.1 |
| ✅ | The `guva-reference` client secret in the realm export is a dev-only value documented as such | `realm-export.json`, [ENVIRONMENTS.md §3](./ENVIRONMENTS.md) |
| 🌐 | Production secrets sourced from Vault, injected at process boot | [ENVIRONMENTS.md §3](./ENVIRONMENTS.md) |
| 🌐 | Vault unsealed by HSM, root token never used after init | [§10.4](../../guva-docs/05-security/10-security-architecture.md) |
| 📝 | Workload identity for service-to-Vault auth (not static tokens) | [§10.4](../../guva-docs/05-security/10-security-architecture.md) |

---

## 7. Logging and observability

| | Item | Source |
|---|---|---|
| ✅ | Every service emits structured JSON logs | `services/reference/internal/observability/logging.go` |
| ✅ | Correlation IDs propagated by APISIX `request-id` plugin and forwarded to upstream | `apisix.yaml` global_rules |
| ✅ | OpenTelemetry traces from APISIX → otel-collector → Jaeger | `apisix.yaml` `opentelemetry` plugin |
| ✅ | Auth-failure dashboard (401/403 rates, latency) provisioned in Grafana | `deploy/compose/grafana/dashboards/auth-overview.json` |
| 📝 | Prometheus alerts on auth-failure-share threshold | [AUTH.md §6](./AUTH.md) follow-up |
| 📝 | Tokens never logged in plaintext (only `sub`, `azp`, `jti`, `iss`) | Service-side convention; not yet linted |
| 🌐 | SIEM ingestion of audit + auth events | [§10.9](../../guva-docs/05-security/10-security-architecture.md) |

---

## 8. Supply chain

| | Item | Source |
|---|---|---|
| ✅ | All container images pinned to specific minor versions, not `latest` | `docker-compose.yml` |
| ✅ | Service container images built multi-stage on `distroless/static-debian12:nonroot` | `services/reference/Dockerfile` |
| ✅ | Container images signed with Cosign before push | CI pipeline (per `docs/DEVELOPMENT.md`) |
| ✅ | Dependencies vulnerability-scanned via pre-commit gitleaks + CI Trivy/Semgrep/CodeQL | `.pre-commit-config.yaml` + CI |
| 📝 | SBOM produced for every release | Deferred to CI hardening |
| 🌐 | Admission controller rejects unsigned images in production | [§13.3](../../guva-docs/06-infrastructure/13-devops-deployment.md) |

---

## 9. Network policy

| | Item | Source |
|---|---|---|
| 🌐 | Services reachable only through APISIX in production (NetworkPolicy) | Infra repo |
| 🌐 | Keycloak admin interface accessible only from a bastion / VPN | Infra repo |
| 🌐 | Outbound from APISIX restricted to Keycloak + upstream services | Infra repo |

---

## 10. Incident response

| | Item | Source |
|---|---|---|
| ✅ | Auth-specific incident runbook with symptom → cause → fix entries | [RUNBOOK-AUTH.md](./RUNBOOK-AUTH.md) |
| ✅ | Rollback procedure for each Phase 2 slice | [ROLLBACK-PHASE-2.md](./ROLLBACK-PHASE-2.md) |
| 📝 | Quarterly auth drills (key rotation, IdP failover, CA expiry) | [RUNBOOK-AUTH.md §10](./RUNBOOK-AUTH.md) |
| 🌐 | PDPO notification within the statutory window for personal-data breaches | [§10.11](../../guva-docs/05-security/10-security-architecture.md) |

---

## 11. What this checklist does NOT cover

By design:

- **Data-at-rest encryption** — covered in [§10.2](../../guva-docs/05-security/10-security-architecture.md); local Postgres is not encrypted at rest.
- **Application-level encryption of sensitive columns** (e.g. National ID hash) — Phase 3 work for the verification service, not this repository.
- **PKI for inter-agency integrations** — covered in [§10.3](../../guva-docs/05-security/10-security-architecture.md); operationalised when integrations land.
- **External penetration testing schedule** — operations concern; tracked in [09-delivery/03-task-list.md WS9-07](../../guva-docs/09-delivery/03-task-list.md).
- **OWASP API Security mapping** — narrative version in [§10.7](../../guva-docs/05-security/10-security-architecture.md).

---

## 12. How to use this checklist

Run through it before any:

- Architectural change touching the gateway, identity provider, or service auth surface.
- New environment provisioning (treat every 🌐 row as a deployment requirement).
- Security review or external audit.

The checklist is intentionally a tactical companion to the architecture doc, not a replacement. When you'd update one, update the other.
