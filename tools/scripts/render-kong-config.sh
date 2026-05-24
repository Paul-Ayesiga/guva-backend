#!/usr/bin/env bash
# =============================================================================
# render-kong-config.sh
#
# Fetches Keycloak's realm public key and renders deploy/compose/kong/kong.yml
# from kong.yml.tmpl. The rendered output is gitignored — Kong reads it on
# (re)start.
#
# Prereqs:
#   - Keycloak must be reachable at $KEYCLOAK_URL_FOR_BOOTSTRAP (default
#     http://localhost:8080), with the `guva` realm imported.
#
# Usage:
#   tools/scripts/render-kong-config.sh                  # render if missing
#   tools/scripts/render-kong-config.sh --force          # always re-render
#
# Invoked by:
#   - make refresh-keys           (forced re-render)
#   - make up                     (auto, only when output missing)
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

TEMPLATE=deploy/compose/kong/kong.yml.tmpl
OUTPUT=deploy/compose/kong/kong.yml
REALM_URL="${KEYCLOAK_URL_FOR_BOOTSTRAP:-http://localhost:8080}/realms/guva"

FORCE=0
[[ "${1:-}" == "--force" ]] && FORCE=1

if [[ -f "${OUTPUT}" && "${FORCE}" -eq 0 ]]; then
  echo "==> ${OUTPUT} already exists (re-run with --force or make refresh-keys to regenerate)"
  exit 0
fi

[[ -f "${TEMPLATE}" ]] || { echo "missing template: ${TEMPLATE}" >&2; exit 1; }

echo "==> Fetching realm public key from ${REALM_URL}"
PUBKEY_B64=$(curl -fsS "${REALM_URL}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["public_key"])')

if [[ -z "${PUBKEY_B64}" ]]; then
  echo "Got empty public key from Keycloak — is the realm imported and Keycloak healthy?" >&2
  exit 1
fi

# Render. Python handles multi-line substitution cleanly across BSD/GNU
# awk + sed differences, and we already require python3 for JSON parsing
# above. The Python block wraps the single-line base64 into a 64-char-
# per-line PEM block indented 10 spaces (matches the YAML column under the
# `rsa_public_key: |` literal in kong.yml.tmpl), then substitutes the
# placeholder.
python3 - "${TEMPLATE}" "${OUTPUT}" "${PUBKEY_B64}" <<'PY'
import sys, textwrap
template_path, output_path, pubkey = sys.argv[1], sys.argv[2], sys.argv[3]
indent = "          "
body = "\n".join(indent + line for line in textwrap.wrap(pubkey, 64))
block = f"{indent}-----BEGIN PUBLIC KEY-----\n{body}\n{indent}-----END PUBLIC KEY-----"
with open(template_path) as f:
    rendered = f.read().replace("__KEYCLOAK_REALM_PUBKEY_PEM__", block)
with open(output_path, "w") as f:
    f.write(rendered)
PY

echo "==> Wrote ${OUTPUT}"
echo "    Restart Kong to pick up the new config:"
echo "      docker compose restart kong"
