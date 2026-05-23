#!/usr/bin/env bash
# =============================================================================
# db-migrate.sh — run migrations for a single service against the local
# Postgres container.
#
# Usage:
#   tools/scripts/db-migrate.sh <service> <up|down|version|force NN>
#
# Convention: each service owns its migrations under
#   services/<service>/migrations/*.sql
# and targets the database `guva_<service>` (per deploy/compose/postgres/initdb.d).
#
# Requires golang-migrate on PATH:
#   brew install golang-migrate
# or:
#   go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

SERVICE="${1:?usage: $0 <service> <up|down|version|force NN>}"
shift
COMMAND="${1:-up}"
shift || true

MIGRATIONS_DIR="services/${SERVICE}/migrations"
[[ -d "${MIGRATIONS_DIR}" ]] || {
  echo "no migrations directory for service '${SERVICE}' (${MIGRATIONS_DIR})" >&2
  exit 1
}

# Load .env without polluting the shell.
set -a
# shellcheck disable=SC1091
source .env
set +a

DB_NAME="guva_${SERVICE}"
DSN="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/${DB_NAME}?sslmode=disable"

command -v migrate >/dev/null || {
  echo "golang-migrate not installed. See header of this script for install commands." >&2
  exit 1
}

echo "==> migrate ${SERVICE} ${COMMAND} (${DB_NAME})"
exec migrate -path "${MIGRATIONS_DIR}" -database "${DSN}" "${COMMAND}" "$@"
