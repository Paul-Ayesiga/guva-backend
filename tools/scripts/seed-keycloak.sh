#!/usr/bin/env bash
# =============================================================================
# seed-keycloak.sh — idempotent admin-API seeding for the guva realm.
#
# realm-export.json only loads on first boot. After that, scope/client
# additions need to go through the Admin API. This script reads the
# scopes + client we currently expect to be present and creates anything
# missing. Re-running with no changes is a no-op.
#
# Called by `make up` after Keycloak reports healthy.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

KC_URL="${KC_URL:-http://localhost:8080}"
KC_ADMIN="${KC_ADMIN:-admin}"
KC_ADMIN_PASSWORD="${KC_ADMIN_PASSWORD:-admin}"
REALM="${REALM:-guva}"

echo "==> seeding Keycloak realm '${REALM}' at ${KC_URL}"

# Obtain an admin token from the master realm.
TOKEN=$(curl -fsS -X POST "${KC_URL}/realms/master/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d "grant_type=password" -d "client_id=admin-cli" \
  -d "username=${KC_ADMIN}" -d "password=${KC_ADMIN_PASSWORD}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
if [[ -z "${TOKEN}" ]]; then
  echo "  failed to obtain admin token" >&2
  exit 1
fi

api() {
  # api METHOD PATH [extra curl args ...]
  local method="$1"; shift
  local path="$1"; shift
  curl -fsS -X "${method}" "${KC_URL}${path}" -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' "$@"
}

# ---- 1. scopes ------------------------------------------------------------
declare -a SCOPES_TO_ENSURE=(
  "admin:scopes|Read and manage the platform scope catalogue"
  "admin:audit|Privileged audit operations: bulk export bundles, manage anchors"
  "admin:keys|Rotate the audit signing key and other platform secrets"
  "admin:webhooks|Inspect and replay any consumer's webhook deliveries"
)

existing_scopes_json=$(api GET "/admin/realms/${REALM}/client-scopes")

ensure_scope() {
  local name="$1" desc="$2"
  if python3 -c '
import json, sys
name = sys.argv[1]
existing = json.loads(sys.stdin.read())
for s in existing:
    if s.get("name") == name:
        sys.exit(0)
sys.exit(1)
' "${name}" <<<"${existing_scopes_json}"; then
    echo "  ✓ scope ${name} (already present)"
    return
  fi
  api POST "/admin/realms/${REALM}/client-scopes" \
    --data "{\"name\":\"${name}\",\"protocol\":\"openid-connect\",\"description\":\"${desc}\"}" \
    >/dev/null
  echo "  + scope ${name}"
}

for entry in "${SCOPES_TO_ENSURE[@]}"; do
  IFS='|' read -r name desc <<<"${entry}"
  ensure_scope "${name}" "${desc}"
done

# Refresh the existing scopes list after creation.
existing_scopes_json=$(api GET "/admin/realms/${REALM}/client-scopes")

# ---- 2. admin client -----------------------------------------------------
ADMIN_CLIENT_ID="guva-platform-admin"
ADMIN_CLIENT_SECRET="platform-admin-dev-secret"
ADMIN_DEFAULT_SCOPES=(audit:read admin:consumers admin:scopes admin:audit admin:keys admin:webhooks)

existing_clients_json=$(api GET "/admin/realms/${REALM}/clients?clientId=${ADMIN_CLIENT_ID}")
admin_internal_id=$(python3 -c '
import json, sys
data = json.loads(sys.stdin.read())
print(data[0]["id"] if data else "")
' <<<"${existing_clients_json}")

if [[ -z "${admin_internal_id}" ]]; then
  api POST "/admin/realms/${REALM}/clients" --data "{
    \"clientId\": \"${ADMIN_CLIENT_ID}\",
    \"name\": \"GUVA Platform Admin (local)\",
    \"enabled\": true,
    \"protocol\": \"openid-connect\",
    \"publicClient\": false,
    \"secret\": \"${ADMIN_CLIENT_SECRET}\",
    \"serviceAccountsEnabled\": true,
    \"standardFlowEnabled\": false,
    \"directAccessGrantsEnabled\": false
  }" >/dev/null
  echo "  + client ${ADMIN_CLIENT_ID}"
  # Re-fetch its internal id.
  admin_internal_id=$(api GET "/admin/realms/${REALM}/clients?clientId=${ADMIN_CLIENT_ID}" \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
else
  echo "  ✓ client ${ADMIN_CLIENT_ID} (already present)"
fi

# ---- 3. attach default scopes to the admin client ------------------------
ensure_default_scope() {
  local scope_name="$1"
  local scope_id
  scope_id=$(python3 -c '
import json, sys
name = sys.argv[1]
data = json.loads(sys.stdin.read())
for s in data:
    if s.get("name") == name:
        print(s["id"]); sys.exit(0)
' "${scope_name}" <<<"${existing_scopes_json}")
  if [[ -z "${scope_id}" ]]; then
    echo "  ! scope ${scope_name} not found in realm; cannot attach" >&2
    return
  fi
  # PUT is idempotent — returns 204 either way.
  api PUT "/admin/realms/${REALM}/clients/${admin_internal_id}/default-client-scopes/${scope_id}" \
    -o /dev/null >/dev/null
  echo "    attach default scope ${scope_name} -> ${ADMIN_CLIENT_ID}"
}

for scope in "${ADMIN_DEFAULT_SCOPES[@]}"; do
  ensure_default_scope "${scope}"
done

echo "==> Keycloak realm seeding complete"
