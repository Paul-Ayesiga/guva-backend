# Phase 2 Rollback Procedures

How to back each Phase 2 slice out cleanly if it goes sideways. The migration was designed to be incremental and reversible — every slice can be reverted independently with a single `git revert` plus a stack reset.

Each section documents:

- **Trigger** — what would make you reach for this rollback.
- **Action** — the exact commands.
- **Side effects** — what's lost (rarely anything for these slices).
- **Recovery time** — how long until the platform is back to the previous slice's behaviour.

---

## Rollback: Slice 2.2 (Caddy + `*.localhost` TLS) → Slice 2.1

**Trigger.**

- Caddy's local CA conflicts with another tool in the dev environment.
- `*.localhost` resolution fails on a team member's machine and the workaround isn't pleasant.
- Some critical flow turns out to require non-TLS Keycloak access that the Caddy front blocks.

**Action.**

```bash
git revert 406bdd6                          # the Slice 2.2 commit
docker compose down caddy
docker volume rm guva_caddy-data guva_caddy-config 2>/dev/null
make untrust-ca
make reset                                  # drops Keycloak so KC_HOSTNAME takes effect cleanly
make up
```

**Side effects.** Token `iss` claims go back to `http://localhost:8080/realms/guva`. Existing tokens issued under the https issuer become invalid; clients need to re-fetch.

**Recovery time.** Under 5 minutes including `make reset`.

---

## Rollback: Slice 2.1 (openid-connect + JWKS) → pre-Phase-2 (jwt-auth + pinned key)

**Trigger.**

- The OIDC discovery flow is consistently failing in production and you need a known-good fallback.
- Keycloak is being replaced and the openid-connect plugin's behaviour is unproven against the new IdP.

**Action.**

```bash
git revert 5e446ac 406bdd6                  # revert both Slice 2.1 and 2.2 commits
# This restores deploy/compose/apisix/apisix.yaml.tmpl and the render-gateway-config.sh script.
make reset
make up                                     # which now re-runs the render script
```

**Side effects.** Loses the live-JWKS / automatic-rotation behaviour. Keys are pinned again; you'll need to run `make refresh-keys` after every Keycloak realm key rotation. The render script comes back as a dependency.

**Recovery time.** Under 10 minutes.

---

## Rollback: complete Phase 2 (back to jwt-auth + pinned key, no Caddy)

**Trigger.** Last-resort full revert. Some auth-related production incident that you can't quickly diagnose and you want every Phase 2 change off the platform.

**Action.**

```bash
# Revert Slices 2.2, 2.1 in topological order
git revert 406bdd6                          # 2.2 first (Caddy + TLS)
git revert 5e446ac                          # then 2.1 (openid-connect)

# Resolve any conflicts (these slices touch overlapping files; review carefully)
git status
# ... resolve, then:
git commit
git push origin main

# Clean redeploy
make reset
make up
```

**Side effects.** Full pre-Phase-2 behaviour restored: jwt-auth with pinned key, `http://localhost:8080` issuer, no Caddy in front of Keycloak.

**Recovery time.** 15–30 minutes depending on conflict complexity.

---

## What CAN'T be rolled back

- **Pre-commit hook changes** that disabled markdownlint MD031/MD034/MD040 — these were across Phase 1 and Phase 2; rolling them back would block subsequent commits. If you want them back, change the `.pre-commit-config.yaml` directly rather than reverting older commits.
- **Image version bumps** — Postgres 18 data directories are not backwards-compatible with Postgres 16; rolling Postgres back would require a `pg_dumpall` + restore. Document this risk explicitly when the next major version comes around.
- **Realm export changes** — once the realm export has been imported into a Keycloak DB, the DB has the state. Reverting the JSON doesn't un-create the realm; you'd need a `make reset` to drop the Postgres volume.

---

## Pre-rollback checklist (production)

Before running a rollback in production:

- [ ] Snapshot the current state: take a Postgres dump, copy `apisix.yaml`, copy Keycloak realm export.
- [ ] Announce in the operations channel; tag the on-call engineer.
- [ ] Confirm the rollback target commit SHA matches what was previously in production.
- [ ] Have a forward-fix plan ready in case the rollback also fails.
- [ ] Plan the post-rollback verification: `make ping`, smoke-test of every consumer.

After the rollback:

- [ ] Confirm all consumers can issue and validate tokens.
- [ ] Confirm alerts have cleared.
- [ ] File an incident report for the next architectural review.

---

## Cross-references

- [AUTH.md](./AUTH.md) — what Phase 2 changed and why.
- [ENVIRONMENTS.md](./ENVIRONMENTS.md) — environment configuration matrix.
- [RUNBOOK-AUTH.md](./RUNBOOK-AUTH.md) — incident response for auth-specific symptoms.
