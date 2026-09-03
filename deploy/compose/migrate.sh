#!/usr/bin/env sh
# deploy/compose/migrate.sh
#
# Idempotent migration runner for the compose `migrate` one-shot service.
# Ported from Taskfile.yml db:migrate (lines 172-181).
#
# Runs inside postgres:16-alpine — uses psql from the image path.
# Mounted at: /migrations (broker/internal/db/migrations/*.sql)
# Env required: PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE
#
# Also creates the `openfga` database if absent (replaces the k8s initdb
# ConfigMap that does not translate to compose).

set -eu

# The broker connects as the non-superuser role aikonos_app (provisioned below)
# so RLS actually enforces. Its password comes from the environment — never a
# committed literal. Fail loud if the operator did not set it.
: "${AIKONOS_DB_APP_PASSWORD:?AIKONOS_DB_APP_PASSWORD must be set (broker app-role password)}"

# Grafana connects as the SELECT-only role aikonos_grafana (provisioned below) to
# read the LLM-spend analytics tables. Same rule as above: password from the
# environment, never a committed literal.
: "${AIKONOS_GRAFANA_DB_PASSWORD:?AIKONOS_GRAFANA_DB_PASSWORD must be set (Grafana read-only role password)}"

MIG_DIR=/migrations

echo "[migrate] waiting for postgres to be ready..."
until pg_isready -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}"; do
  sleep 1
done
echo "[migrate] postgres is ready"

# Create openfga DB if absent. CREATE DATABASE cannot run inside a transaction
# and \gexec is a psql meta-command (not valid via -c), so guard it shell-side:
# check existence, create only when missing.
if [ "$(psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d postgres -tAc \
    "SELECT 1 FROM pg_database WHERE datname = 'openfga'")" != "1" ]; then
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d postgres -c "CREATE DATABASE openfga"
fi
echo "[migrate] openfga database ensured"

# Apply aikonos migrations idempotently
for f in $(find "${MIG_DIR}" -maxdepth 1 -name '*.sql' | sort -V); do
  ver=$(basename "$f" | cut -d_ -f1)
  applied=$(psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -tAc "SELECT 1 FROM schema_migrations WHERE version='${ver}'" 2>/dev/null || echo "")
  if [ "${applied}" = "1" ]; then
    echo "[migrate]   ${ver}  already applied, skip"
    continue
  fi
  echo "[migrate]   ${ver}  applying $(basename "$f") ..."
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -f "$f"
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -c \
    "INSERT INTO schema_migrations (version, description) VALUES ('${ver}', '$(basename "$f")') ON CONFLICT (version) DO NOTHING;"
done

# ── Broker application role ──────────────────────────────────────────────────
# The broker connects as aikonos_app: LOGIN, NOSUPERUSER, NOBYPASSRLS — so the
# RLS policies on every tenant-scoped table actually enforce (a superuser/owner
# role bypasses RLS unconditionally). Migrations above ran as the owner
# (${PGUSER}); this role only gets DML + the two SECURITY DEFINER carve-outs.
# Provisioning is idempotent so re-running migrate on an existing DB is safe.
if [ "$(psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" -tAc \
    "SELECT 1 FROM pg_roles WHERE rolname = 'aikonos_app'")" = "1" ]; then
  echo "[migrate] app role aikonos_app exists — syncing password + attributes"
  # NB: psql -c does NOT perform :'var' interpolation — must read from stdin.
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -v app_pw="${AIKONOS_DB_APP_PASSWORD}" <<'SQL'
ALTER ROLE aikonos_app WITH LOGIN PASSWORD :'app_pw' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
SQL
else
  echo "[migrate] creating app role aikonos_app"
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -v app_pw="${AIKONOS_DB_APP_PASSWORD}" <<'SQL'
CREATE ROLE aikonos_app WITH LOGIN PASSWORD :'app_pw' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
SQL
fi

echo "[migrate] granting DML + definer-function execute to aikonos_app"
psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" -v ON_ERROR_STOP=1 <<SQL
GRANT CONNECT ON DATABASE "${PGDATABASE}" TO aikonos_app;
GRANT USAGE ON SCHEMA public TO aikonos_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO aikonos_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO aikonos_app;
GRANT EXECUTE ON FUNCTION baseline_distinct_agents(timestamptz), baseline_prune_windows(timestamptz) TO aikonos_app;
GRANT EXECUTE ON FUNCTION resolve_agent_api_key(text) TO aikonos_app;
GRANT EXECUTE ON FUNCTION rate_limit_policies_all() TO aikonos_app;
GRANT EXECUTE ON FUNCTION llm_usage_prune_events(timestamptz) TO aikonos_app;
-- Future tables/sequences created by the owner auto-grant to the app role.
ALTER DEFAULT PRIVILEGES FOR ROLE "${PGUSER}" IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO aikonos_app;
ALTER DEFAULT PRIVILEGES FOR ROLE "${PGUSER}" IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO aikonos_app;
SQL
echo "[migrate] app role ready"

# ── Grafana read-only analytics role ─────────────────────────────────────────
# The "LLM Spend" dashboard queries
# Postgres directly. aikonos_grafana is LOGIN, NOSUPERUSER, NOBYPASSRLS with
# SELECT on exactly six tables and no DML anywhere — the narrowest grant that
# answers the dashboard's questions. Deliberately no ALTER DEFAULT PRIVILEGES:
# a future table must be named here to become visible.
if [ "$(psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" -tAc \
    "SELECT 1 FROM pg_roles WHERE rolname = 'aikonos_grafana'")" = "1" ]; then
  echo "[migrate] grafana role aikonos_grafana exists — syncing password + attributes"
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -v gf_pw="${AIKONOS_GRAFANA_DB_PASSWORD}" <<'SQL'
ALTER ROLE aikonos_grafana WITH LOGIN PASSWORD :'gf_pw' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
SQL
else
  echo "[migrate] creating grafana role aikonos_grafana"
  psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
    -v ON_ERROR_STOP=1 -v gf_pw="${AIKONOS_GRAFANA_DB_PASSWORD}" <<'SQL'
CREATE ROLE aikonos_grafana WITH LOGIN PASSWORD :'gf_pw' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
SQL
fi

echo "[migrate] granting read-only analytics SELECT to aikonos_grafana"
psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" -v ON_ERROR_STOP=1 <<SQL
GRANT CONNECT ON DATABASE "${PGDATABASE}" TO aikonos_grafana;
GRANT USAGE ON SCHEMA public TO aikonos_grafana;
GRANT SELECT ON llm_usage_events, llm_spend_counters, spend_caps, llm_providers, agents, user_directory TO aikonos_grafana;
-- All six tables are RLS-enabled, so a SELECT grant alone yields zero rows.
-- The dashboard is deliberately cross-tenant (operator view); scoping stays a
-- visible per-table SELECT policy rather than a blanket BYPASSRLS, and
-- aikonos_app's own tenant_isolation policies are untouched (policies are
-- per-role). DROP-then-CREATE keeps a re-run idempotent.
DROP POLICY IF EXISTS grafana_read ON llm_usage_events;
CREATE POLICY grafana_read ON llm_usage_events FOR SELECT TO aikonos_grafana USING (true);
DROP POLICY IF EXISTS grafana_read ON llm_spend_counters;
CREATE POLICY grafana_read ON llm_spend_counters FOR SELECT TO aikonos_grafana USING (true);
DROP POLICY IF EXISTS grafana_read ON spend_caps;
CREATE POLICY grafana_read ON spend_caps FOR SELECT TO aikonos_grafana USING (true);
DROP POLICY IF EXISTS grafana_read ON llm_providers;
CREATE POLICY grafana_read ON llm_providers FOR SELECT TO aikonos_grafana USING (true);
DROP POLICY IF EXISTS grafana_read ON agents;
CREATE POLICY grafana_read ON agents FOR SELECT TO aikonos_grafana USING (true);
DROP POLICY IF EXISTS grafana_read ON user_directory;
CREATE POLICY grafana_read ON user_directory FOR SELECT TO aikonos_grafana USING (true);
SQL
echo "[migrate] grafana role ready"

echo "[migrate] done. applied versions:"
psql -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" -d "${PGDATABASE}" \
  -tAc "SELECT version FROM schema_migrations ORDER BY version" | tr '\n' ' '
echo
