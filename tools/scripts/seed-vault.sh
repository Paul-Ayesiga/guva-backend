#!/usr/bin/env bash
# =============================================================================
# seed-vault.sh — seed the local Vault with dev-only secrets every service
# can fetch at startup.
#
# Vault runs in dev mode (in-memory storage) so this needs to run after
# every `make up`. `make up` invokes it automatically after Vault reports
# healthy.
#
# Secrets seeded:
#   secret/services/reference/config :greeting=hello-from-vault
#   secret/services/identity/config  :db-password=guva
#                                    :keycloak-admin-password=admin
#                                    :keycloak-admin-username=admin
#   secret/services/audit/config     :db-writer-password=audit-writer-dev
#                                    :db-reader-password=audit-reader-dev
#
# All values are local-only. Production secrets are sourced from a real
# secret store via the path documented in docs/ENVIRONMENTS.md §3.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

VAULT_ADDR="${VAULT_ADDR:-http://localhost:8200}"
VAULT_TOKEN="${VAULT_TOKEN:-dev-root-token}"

write_kv() {
  local path="$1"; shift
  # Construct {data: {key1: val1, ...}} JSON from the kv pairs the caller passed.
  local json
  json=$(python3 -c '
import json, sys
out = {"data": {}}
for kv in sys.argv[1:]:
    k, v = kv.split("=", 1)
    out["data"][k] = v
json.dump(out, sys.stdout)
' "$@")
  curl -fsS -X POST \
    -H "X-Vault-Token: ${VAULT_TOKEN}" \
    -H "Content-Type: application/json" \
    --data "${json}" \
    "${VAULT_ADDR}/v1/secret/data/${path}" > /dev/null
  echo "  ✓ secret/${path}"
}

# Wait for Vault to answer; `make up` already --wait's on the
# healthcheck, but defend against the script being run standalone.
for i in 1 2 3 4 5; do
  if curl -fsS "${VAULT_ADDR}/v1/sys/health" >/dev/null 2>&1; then
    break
  fi
  echo "  …waiting for Vault at ${VAULT_ADDR} (${i}/5)"
  sleep 2
done

echo "==> Seeding Vault (${VAULT_ADDR}) with dev-only secrets"
write_kv "services/reference/config" "greeting=hello-from-vault"
write_kv "services/identity/config" \
  "db-password=guva" \
  "keycloak-admin-username=admin" \
  "keycloak-admin-password=admin"
write_kv "services/audit/config" \
  "db-writer-password=audit-writer-dev" \
  "db-reader-password=audit-reader-dev"
write_kv "services/verification/config" \
  "db-password=guva"

write_kv "services/consent/config" \
  "db-password=guva"
echo "==> Done. Inspect with:"
echo "    VAULT_ADDR=${VAULT_ADDR} VAULT_TOKEN=${VAULT_TOKEN} vault kv get secret/services/reference/config"
