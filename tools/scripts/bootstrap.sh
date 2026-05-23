#!/usr/bin/env bash
# =============================================================================
# bootstrap.sh — one-time setup for the local-development stack.
#
# Invoked by `make bootstrap`. Idempotent — safe to re-run.
# =============================================================================
set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log()  { printf "${GREEN}==>${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}!! ${NC} %s\n" "$*"; }
fail() { printf "${RED}xx ${NC} %s\n" "$*"; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

# ---- 1. Preflight ----------------------------------------------------------
log "Preflight checks"
command -v docker >/dev/null            || fail "docker is required"
docker compose version >/dev/null 2>&1  || fail "docker compose plugin is required"
command -v go >/dev/null                || warn "go not on PATH — install Go 1.22+ to build services locally"

# ---- 2. Environment file ---------------------------------------------------
if [[ ! -f .env ]]; then
  cp .env.example .env
  log "Created .env from .env.example"
else
  log ".env already exists; leaving it alone"
fi

# ---- 3. Pre-pull images so first `make up` is quick ------------------------
log "Pulling images (this can take a few minutes the first time)"
docker compose pull --quiet

# ---- 4. Friendly tail-message ---------------------------------------------
cat <<'EOF'

Bootstrap complete.

Next steps:
  make up         # bring up the stack
  make status     # see service health
  make urls       # print the local URLs
  make run-reference  # run the reference service against the stack
EOF
