#!/usr/bin/env bash
# =============================================================================
# seed-schemas.sh — register the audit envelope JSON Schema in Apicurio.
#
# Idempotent: re-running with no schema changes is a no-op (Apicurio's
# `ifExists=RETURN_OR_UPDATE&canonical=true` returns the existing artifact
# unchanged); if the schema body has drifted, a new version is created.
#
# Producers (pkg/platform/audit) fetch the latest version at startup and
# validate every envelope before commit. The Go binary also embeds the
# schema file, so a registry outage falls back to the embedded copy.
#
# Usage:
#   tools/scripts/seed-schemas.sh           # uses APICURIO_URL or default
#   APICURIO_URL=... tools/scripts/seed-schemas.sh
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
APICURIO_URL="${APICURIO_URL:-http://localhost:8081}"
GROUP="guva-audit"
SCHEMA_DIR="${REPO_ROOT}/pkg/platform/audit/schemas"

echo "==> seeding schemas into ${APICURIO_URL} (group=${GROUP})"

# Wait briefly for Apicurio to be ready (compose 'up' calls us right after
# bringing the stack online).
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS "${APICURIO_URL}/apis/registry/v2/system/info" >/dev/null 2>&1; then
    break
  fi
  if [[ "$i" -eq 10 ]]; then
    echo "Apicurio did not become reachable at ${APICURIO_URL} within ~10s" >&2
    exit 1
  fi
  sleep 1
done

shopt -s nullglob
schemas=("${SCHEMA_DIR}"/*.json)
if [[ ${#schemas[@]} -eq 0 ]]; then
  echo "No JSON schemas found under ${SCHEMA_DIR}; nothing to register." >&2
  exit 0
fi

for schema_path in "${schemas[@]}"; do
  base="$(basename "${schema_path}" .json)"
  # File `audit-event-envelope-v1.json` -> artifactId `audit-event-envelope`,
  # version label preserved in the schema's $id.
  artifact_id="${base%-v[0-9]*}"
  echo "  - ${artifact_id}  (${base}.json)"

  http_status=$(curl -sS -o /tmp/apicurio-seed.out -w '%{http_code}' \
    -X POST "${APICURIO_URL}/apis/registry/v2/groups/${GROUP}/artifacts?ifExists=RETURN_OR_UPDATE&canonical=true" \
    -H "Content-Type: application/json" \
    -H "X-Registry-ArtifactId: ${artifact_id}" \
    -H "X-Registry-ArtifactType: JSON" \
    --data-binary "@${schema_path}")

  case "${http_status}" in
    200|201)
      version=$(python3 -c 'import sys,json;d=json.load(open("/tmp/apicurio-seed.out"));print(d.get("version","?"))')
      echo "      registered version ${version}"
      ;;
    *)
      echo "      failed: HTTP ${http_status}" >&2
      cat /tmp/apicurio-seed.out >&2
      exit 1
      ;;
  esac
done

echo "==> all schemas registered"
