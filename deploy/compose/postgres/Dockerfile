# Builds a local PostgreSQL image preloaded with the extensions called for in
# guva-docs/06-infrastructure/19-recommended-tech-stack.md §19.3:
#   - pgcrypto              column-level encryption
#   - pg_partman            automated partition lifecycle
#   - pgaudit               fine-grained audit logging
#   - pg_stat_statements    query performance analysis
FROM postgres:18

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      postgresql-18-partman \
      postgresql-18-pgaudit \
 && rm -rf /var/lib/apt/lists/*

# pgcrypto and pg_stat_statements ship with core Postgres; they are enabled
# at initdb time via the SQL in initdb.d/.
