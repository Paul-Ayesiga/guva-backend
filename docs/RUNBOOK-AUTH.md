# Auth Operational Runbook

What to do when authentication misbehaves. Every entry has a **symptom** (what you see), a **root cause** (why it happens), and a **fix** (the exact steps).

This runbook covers local, staging, and production unless otherwise stated. Where commands differ, both are shown.

For architectural background, see [AUTH.md](./AUTH.md). For per-environment configuration matrix, see [ENVIRONMENTS.md](./ENVIRONMENTS.md). For rolling back a Phase 2 change, see [ROLLBACK-PHASE-2.md](./ROLLBACK-PHASE-2.md).

---

## 1. Symptoms quick-reference

| Symptom | Section |
|---|---|
| Clients getting `401 invalid_token: signature verification failed` after Keycloak restart or key rotation | [§2](#2-jwks-stale-after-key-rotation) |
| Clients getting `401 invalid_token: iss returned failure` | [§3](#3-issuer-mismatch) |
| Clients getting `401 invalid_token: No bearer token found in request` for a request that *did* include `Authorization` | [§4](#4-bearer-header-stripped-or-malformed) |
| Burst of 401s from a single client | [§5](#5-compromised-or-expired-client-credentials) |
| Keycloak admin console works but token issuance fails for a specific client | [§6](#6-disabled-client--missing-service-account) |
| Keycloak unreachable from APISIX | [§7](#7-idp-outage-or-network-partition) |
| APISIX healthcheck failing while traffic still flows | [§8](#8-apisix-healthcheck-noise) |
| `make trust-ca` doesn't take effect | [§9](#9-make-trust-ca-doesnt-take-effect) |

---

## 2. JWKS stale after key rotation

**Symptom.** Clients suddenly start receiving `401 invalid_token: signature verification failed` even though they're using a freshly issued token. The Keycloak admin console works fine.

**Root cause.** APISIX caches the JWKS from Keycloak in-memory. When Keycloak rotates the realm key, new tokens are signed by the new key but APISIX's cache only has the old one. APISIX *does* auto-refresh on unknown `kid`, but if the rotation removed the old key entirely (rather than keeping both during a grace window), tokens issued just before the rotation also fail.

**Fix (local / staging / prod).**

```bash
# Restart APISIX to drop its JWKS cache; it will refetch on next request.
docker compose restart apisix                                   # local
kubectl rollout restart deploy/apisix -n guva-system            # k8s
```

**Prevent recurrence.** Keep at least the previous key active during the grace window when rotating. Keycloak does this by default; verify in the Realm Settings → Keys tab that both `state=active` and `state=passive` keys exist for `RS256` during rotation.

---

## 3. Issuer mismatch

**Symptom.** `401 invalid_token: iss returned failure`. The token's `iss` claim doesn't match what APISIX expects from the OIDC discovery doc.

**Root cause.** One of:

- `KC_HOSTNAME` was changed but APISIX wasn't restarted — its cached discovery doc still has the old issuer.
- Clients are fetching tokens via a URL that differs from `KC_HOSTNAME`, and `KC_HOSTNAME_BACKCHANNEL_DYNAMIC` is *false* (so Keycloak issues tokens with `iss` derived from the request host, not from `KC_HOSTNAME`).

**Fix.**

```bash
# 1. Confirm what iss the tokens carry:
TOKEN=$(make token)
python3 -c "import base64,json; t='$TOKEN'.split('.')[1]; t += '='*(-len(t)%4); print(json.loads(base64.urlsafe_b64decode(t))['iss'])"

# 2. Compare against the discovery doc APISIX sees:
docker compose exec apisix bash -c 'wget -qO- http://keycloak:8080/realms/guva/.well-known/openid-configuration' | python3 -c 'import sys,json; print(json.load(sys.stdin)["issuer"])'

# 3. If they differ, the iss in the token wins (it's set in the JWT).
#    Verify KC_HOSTNAME on the Keycloak deployment matches the token iss:
docker compose exec keycloak printenv KC_HOSTNAME
docker compose exec keycloak printenv KC_HOSTNAME_BACKCHANNEL_DYNAMIC

# 4. Restart APISIX so its cached discovery aligns:
docker compose restart apisix
```

**Prevent recurrence.** Treat `KC_HOSTNAME` as part of the platform's release contract. Document every change in the env's release notes.

---

## 4. Bearer header stripped or malformed

**Symptom.** Client logs show `Authorization: Bearer <jwt>` being sent, but APISIX returns `401 No bearer token found in request`.

**Root cause.** One of:

- A misconfigured proxy in front of the client (load balancer, CDN, service mesh sidecar) strips the `Authorization` header.
- The client is using a non-standard header name.
- The header value has a typo (e.g., `Bearer<jwt>` with no space).

**Fix.**

```bash
# Use curl -v to confirm the header reaches APISIX:
curl -v -H "Authorization: Bearer $TOKEN" https://api.guva.localhost/v1/reference/ping 2>&1 | grep -i 'authorization'
```

If the header is present but APISIX still says "No bearer token", check for a service-mesh sidecar (Istio, Linkerd) — they sometimes intercept and re-sign requests with their own auth.

---

## 5. Compromised or expired client credentials

**Symptom.** A burst of 401s from a single `azp` value, or unusual traffic patterns from a single client.

**Root cause.** Either:

- Token expired; a well-behaved client should refresh, but if it's hammering with an expired token, that's a bug.
- Client credentials were rotated and not propagated to the consumer.
- Credentials are leaked and someone else is now using them (rare but possible).

**Fix.**

1. **Suspend the client** in Keycloak admin (Clients → guva-reference → Enabled: off). New token requests will fail immediately; existing tokens remain valid until `exp` (max 1h by default).
2. **Rotate the secret**: regenerate the client secret, store in Vault, hand off to the consumer team out-of-band.
3. **Force-revoke active tokens**: requires either short token TTLs (default 1h is usually enough) or token introspection. Introspection isn't yet enabled in our APISIX config — flagged in [AUTH.md §7](./AUTH.md) as a follow-up.

---

## 6. Disabled client / missing service account

**Symptom.** A specific client can't obtain a token; gets `401 unauthorized_client` or `invalid_client`.

**Root cause.**

- `Service accounts enabled` is off on the client (required for `client_credentials` grant).
- Client is disabled (`Enabled: off`).
- Client secret in the consumer's config doesn't match Keycloak's.

**Fix.**

Check the realm export or Keycloak admin: every client used for service-to-service must have `serviceAccountsEnabled: true` and `enabled: true`. For local dev, the `guva-reference` client in [deploy/compose/keycloak/realm-export.json](../deploy/compose/keycloak/realm-export.json) shows the canonical shape.

---

## 7. IdP outage or network partition

**Symptom.** All token-protected routes return `401` or `502`. Keycloak unreachable from APISIX.

**Root cause.** Keycloak is down, or there's a network partition between APISIX and Keycloak.

**Fix (immediate impact mitigation).**

1. **APISIX continues to serve traffic for cached tokens whose signature it can still verify** — until its in-memory JWKS expires (default ~30s). Then everything starts 401-ing.
2. **Restart Keycloak**, or fail traffic over to the standby Keycloak instance.
3. **In production, the alert should already have fired** via the auth-failure dashboard ([§4 of the observability docs, once Phase 2.4 lands]).

**Longer term.** Run Keycloak HA (multiple replicas, shared Infinispan cache, external Postgres). The infra repo's Keycloak chart values document the production topology.

---

## 8. APISIX healthcheck noise

**Symptom.** `guva-apisix` shows `unhealthy` in `docker compose ps`, but `curl https://api.guva.localhost/v1/reference/ping` works fine.

**Root cause.** Our local healthcheck uses bash's `/dev/tcp/localhost/9080` to probe whether the proxy port accepts connections. False negatives can happen if the bash builtin is unavailable in the image's path; APISIX itself is healthy.

**Fix.** Tail APISIX logs (`docker compose logs apisix --tail 50`) — if you see request lines and 2xx/3xx responses, the gateway is fine. The healthcheck command can be tweaked in `docker-compose.yml`; restart with `docker compose up -d --force-recreate apisix` after changes.

---

## 9. `make trust-ca` doesn't take effect

**Symptom.** After running `make trust-ca`, `curl https://auth.guva.localhost/realms/guva` still says "self signed certificate".

**Root cause.** One of:

- The cert was installed but the OS hasn't refreshed its trust store yet.
- A previous Caddy CA is still cached in the trust store and conflicts.
- Caddy regenerated its CA (e.g., the `caddy-data` volume was dropped) — the trust store has the old root.

**Fix.**

```bash
# Refresh by re-running:
make untrust-ca
make trust-ca

# If Caddy regenerated its CA recently, the volume needs to outlive
# `make reset`. Inspect:
docker volume inspect guva_caddy-data

# Worst case, fully drop and reissue:
docker compose down caddy
docker volume rm guva_caddy-data
docker compose up -d caddy
make trust-ca
```

---

## 10. Drills

Production-eligible drills, run quarterly:

1. **Key rotation drill** — rotate the realm signing key in staging during business hours, observe APISIX's JWKS refresh, confirm no client-visible errors. Document the timing.
2. **IdP failover drill** — kill the primary Keycloak instance, confirm clients fail over (or that the runbook's manual fix takes under 10 minutes).
3. **CA expiry drill** — simulate Caddy's local root cert expiring on a dev machine; confirm `make trust-ca` reinstalls cleanly.

Logging every drill in `docs/incidents/` builds the platform's institutional memory.
