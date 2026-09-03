#!/usr/bin/env bash
# Seed a sample agent_skill bundle row into Postgres for the dev compose stack.
# The FGA can_use grant for this bundle is seeded by compose-seed-openfga.sh
# (policies/fga/tuples/dev-seed.yaml, CP4).
#
# The broker checks FGA by the bundle's UUID (CheckFGA "agentskill:"+row.ID), so
# the row's id MUST be a FIXED, known UUID that the dev-seed grant references —
# a random/generated id would never match the static grant. SAMPLE_BUNDLE_ID is
# that fixed id; it must stay in sync with the agentskill object in dev-seed.yaml.
#
# Idempotent: uses INSERT … ON CONFLICT DO NOTHING (keyed on tenant_id + name).
# Run from the repo root with the compose stack already up.
set -euo pipefail

PG_USER="${POSTGRES_USER:-aikonos}"
PG_PASS="${POSTGRES_PASSWORD:-dev-password-change-me}"
PG_DB="${POSTGRES_DB:-aikonos}"
# Dev tenant UUID — matches AIKONOS_TENANT_ID in deploy/compose/.env.local.example.
TENANT_ID="${AIKONOS_TENANT_ID:-11111111-1111-1111-1111-111111111111}"
# Fixed sample-bundle UUID — MUST match agentskill:<id> in policies/fga/tuples/dev-seed.yaml.
SAMPLE_BUNDLE_ID="a5111d00-0000-4000-8000-000000000001"

echo "[seed-skill-bundles] inserting sample agent_skill row (research-assistant) ..."

PGPASSWORD="$PG_PASS" docker compose exec -T postgres \
  psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 <<SQL
-- Set RLS tenant context so the policy allows the write.
SET LOCAL app.current_tenant = '${TENANT_ID}';

INSERT INTO agent_skill (
    id,
    tenant_id,
    name,
    description,
    body,
    allowed_tools,
    context_fork,
    disable_model_invocation,
    extras,
    created_by
)
VALUES (
    '${SAMPLE_BUNDLE_ID}',
    '${TENANT_ID}',
    'research-assistant',
    'A sample research assistant bundle that narrows the tool surface to web fetch only.',
    E'You are a focused research assistant. Use web.fetch to retrieve information and summarise findings concisely. Do not perform any write operations.',
    '["web.fetch"]'::jsonb,
    false,
    false,
    '{}'::jsonb,
    'dev-seed'
)
ON CONFLICT (tenant_id, name) DO NOTHING;
SQL

echo "[seed-skill-bundles] done."
