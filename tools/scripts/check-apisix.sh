#!/usr/bin/env bash
# =============================================================================
# check-apisix.sh — guard against the two APISIX gotchas we keep hitting.
#
# Gotcha 1: Plugin allowlist parity.
#   APISIX standalone won't load any plugin that isn't named in
#   config.yaml's top-level `plugins:` list, even if it's referenced
#   from apisix.yaml. Symptom: a single startup error
#   `unknown plugin [foo]`, then the gateway serves traffic with the
#   plugin silently disabled. This script greps every plugin name out
#   of apisix.yaml and asserts each one is in the allowlist.
#
# Gotcha 2: Bind-mount drift on macOS.
#   Docker Desktop's VirtioFS sometimes serves a stale or truncated
#   copy of apisix.yaml / config.yaml to the running container. The
#   container's view diverges silently from the host edit. This script
#   md5sums both sides and restarts APISIX if they disagree.
#
# If anything is wrong this exits non-zero so `make up` halts before
# the developer wastes time debugging mystery 404s.
#
# Usage:
#   tools/scripts/check-apisix.sh
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

APISIX_DIR="deploy/compose/apisix"
HOST_APISIX_YAML="${APISIX_DIR}/apisix.yaml"
HOST_CONFIG_YAML="${APISIX_DIR}/config.yaml"
CONTAINER="${APISIX_CONTAINER:-guva-apisix}"
# COMPOSE may be multi-word ("docker compose"); split into an array so
# bash invokes it as command + args, not a single literal command name.
read -r -a COMPOSE_CMD <<< "${COMPOSE:-docker compose}"

red()    { printf "\033[31m%s\033[0m\n" "$*"; }
green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }

# ---- 1. allowlist parity ---------------------------------------------------
#
# Plugin names appear in apisix.yaml under `plugins:` blocks; the allowlist
# lives in config.yaml under a top-level `plugins:` list. We extract every
# referenced plugin from apisix.yaml and check membership in the allowlist.

# Allowed plugins: every line under `plugins:` in config.yaml that starts
# with `- name`. Stored as a newline-separated set we grep against (no
# associative arrays — macOS still ships bash 3.2).
allow_list=$(awk '
  /^plugins:[[:space:]]*$/ { flag=1; next }
  /^[a-zA-Z_]+:/           { flag=0 }
  flag {
    if (match($0, /-[[:space:]]+[a-z0-9_-]+/)) {
      s = substr($0, RSTART, RLENGTH)
      sub(/^-[[:space:]]+/, "", s)
      print s
    }
  }
' "${HOST_CONFIG_YAML}")

# Plugin references in apisix.yaml are 6-space-indented keys directly under
# a `plugins:` block. Examples:
#   plugins:
#       request-id:
#         header_name: ...
referenced=$(awk '
  /^[[:space:]]+plugins:[[:space:]]*$/ { in_plugins=1; indent=length($0)-length("plugins:"); next }
  in_plugins {
    if ($0 ~ /^[[:space:]]*$/) { next }
    line_indent=match($0,/[^ ]/) - 1
    if (line_indent <= indent) { in_plugins=0; next }
    if (line_indent == indent+2 && $1 ~ /:$/) {
      n=$1; sub(/:$/,"",n); print n
    }
  }
' "${HOST_APISIX_YAML}" | sort -u)

missing=()
for plugin in $referenced; do
  if ! printf '%s\n' "${allow_list}" | grep -qx "${plugin}"; then
    missing+=("$plugin")
  fi
done

if (( ${#missing[@]} > 0 )); then
  red   "FAIL: apisix.yaml references plugins not in config.yaml's allowlist:"
  for m in "${missing[@]}"; do
    red   "      - $m"
  done
  red   "      Add them under \`plugins:\` in ${HOST_CONFIG_YAML} and rerun."
  red   "      Without this, APISIX silently disables them at startup."
  exit 2
fi
green "OK: every plugin referenced in apisix.yaml is in the config.yaml allowlist."

# ---- 2. bind-mount parity --------------------------------------------------
#
# Only matters if APISIX is running. If the container isn't up, skip — the
# user is probably running this script standalone for validation.

if [[ "$(docker inspect -f '{{.State.Running}}' "${CONTAINER}" 2>/dev/null)" != "true" ]]; then
  yellow "SKIP: ${CONTAINER} is not running; bind-mount parity check skipped."
  exit 0
fi

check_bind_mount() {
  local host_path="$1"
  local container_path="$2"
  local label="$3"

  local host_md5 container_md5
  host_md5=$(md5 -q "${host_path}" 2>/dev/null || md5sum "${host_path}" | awk '{print $1}')
  container_md5=$(docker exec "${CONTAINER}" md5sum "${container_path}" 2>/dev/null | awk '{print $1}')

  if [[ -z "${container_md5}" ]]; then
    red "FAIL: could not read ${container_path} inside ${CONTAINER}."
    return 2
  fi

  if [[ "${host_md5}" != "${container_md5}" ]]; then
    yellow "${label} differs: host=${host_md5} container=${container_md5}"
    return 1
  fi

  green "OK: ${label} md5 matches between host and container."
  return 0
}

drift=0
check_bind_mount "${HOST_APISIX_YAML}" "/usr/local/apisix/conf/apisix.yaml" "apisix.yaml" || drift=$?
check_bind_mount "${HOST_CONFIG_YAML}" "/usr/local/apisix/conf/config.yaml" "config.yaml" || drift=$?

if (( drift == 2 )); then
  exit 2
fi

if (( drift == 1 )); then
  yellow "Restarting ${CONTAINER} to re-read bind-mounted configs…"
  docker restart "${CONTAINER}" >/dev/null
  # Give it a couple of seconds to settle.
  for i in 1 2 3 4 5; do
    if [[ "$(docker inspect -f '{{.State.Running}}' "${CONTAINER}" 2>/dev/null)" == "true" ]]; then
      sleep 2; break
    fi
    sleep 1
  done

  drift=0
  check_bind_mount "${HOST_APISIX_YAML}" "/usr/local/apisix/conf/apisix.yaml" "apisix.yaml (after restart)" || drift=$?
  check_bind_mount "${HOST_CONFIG_YAML}" "/usr/local/apisix/conf/config.yaml" "config.yaml (after restart)" || drift=$?

  if (( drift != 0 )); then
    red "FAIL: bind-mount still divergent after restart. Inspect Docker Desktop file sharing."
    exit 2
  fi
fi

# ---- 3. startup log scan ---------------------------------------------------
#
# Even when the files are right, APISIX may have flagged a config problem
# during boot (e.g. a route reference we just removed). Surface those.

# Scope to logs since the current container start so we don't trip on
# historical errors from before a fix was applied. Docker stamps
# StartedAt in RFC3339Nano; --since accepts that directly.
started_at=$(docker inspect -f '{{.State.StartedAt}}' "${CONTAINER}" 2>/dev/null || true)
if [[ -n "${started_at}" ]]; then
  # `|| true` so grep-finds-nothing (exit 1) doesn't trip set -e/pipefail.
  errors=$(docker logs --since "${started_at}" "${CONTAINER}" 2>&1 \
    | grep -iE 'unknown plugin|failed to check item data|invalid plugin' \
    | tail -3 || true)
  if [[ -n "${errors}" ]]; then
    yellow "WARN: APISIX startup logs (since ${started_at}) contain config errors:"
    printf '  %s\n' "${errors}"
    yellow "Inspect ${HOST_APISIX_YAML} / ${HOST_CONFIG_YAML} and rerun."
    exit 1
  fi
fi

green "OK: APISIX startup logs are clean."
