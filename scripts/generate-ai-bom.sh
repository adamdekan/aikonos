#!/usr/bin/env bash
# scripts/generate-ai-bom.sh
#
# Generates docs/AI-BOM.md — an AI bill-of-materials covering:
#   - LLM providers (name, dialect, model ids, vision capability, has_key
#     boolean) — never key material.
#   - Skill bundles (from skills/*/skill.yaml) with their sbom_ref, when set.
#   - Pi harness (agent-gateway) version + broker version (git describe/sha).
#
# LLM providers are per-tenant/per-deployment runtime state in Postgres
# (llm_providers), not something a static script — or a committed artifact —
# should pin. Default behavior emits a documented placeholder for that
# section. Pass --live-db to opt into querying a reachable compose stack's
# postgres container instead (read-only SELECT of non-secret columns only:
# id, name, api, api_version, models, vision_capable, has_key — never any
# key material; keys live in Vault, not this table, but we still never touch
# anything that could be one). This is a point-in-time snapshot of whichever
# stack was reachable at generation time — regenerate at release against the
# target deployment. The committed docs/AI-BOM.md is generated WITHOUT
# --live-db (placeholder form); --live-db is for operators regenerating
# against their own deployment before a release.
#
# Usage: scripts/generate-ai-bom.sh [--out FILE] [--live-db]

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT="docs/AI-BOM.md"
LIVE_DB=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) OUT="$2"; shift 2 ;;
    --live-db) LIVE_DB=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

# Cheap markdown-table-cell guard: a raw `|` in an interpolated value would
# split the row. Not full markdown escaping — just enough to keep the table
# well-formed for any provider/model/skill field pulled from the DB or YAML.
esc() { tr '|' '/' <<< "$1"; }

GIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
GIT_DESCRIBE="$(git describe --tags --always 2>/dev/null || echo "$GIT_SHA")"
GENERATED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
BROKER_VERSION="$GIT_DESCRIBE"
HARNESS_VERSION="$(grep -m1 '"version"' agent-gateway/package.json | sed -E 's/.*"version": *"([^"]+)".*/\1/')"

# --- LLM providers: --live-db opts into a live compose stack query --------
providers_md=""
providers_source="none"

if [[ "$LIVE_DB" -eq 1 ]] && docker compose ps --format '{{.Name}} {{.State}}' 2>/dev/null | grep -q 'postgres.*running'; then
  PGU="$(docker compose exec -T postgres printenv POSTGRES_USER 2>/dev/null | tr -d '\r' || true)"
  PGD="$(docker compose exec -T postgres printenv POSTGRES_DB 2>/dev/null | tr -d '\r' || true)"
  if [[ -n "$PGU" && -n "$PGD" ]]; then
    # Non-secret projection only — never selects key material. Field sep is
    # \x1f (unit separator), not tab — bash `read` collapses consecutive
    # IFS-whitespace delimiters (tab included) even when IFS is set
    # explicitly, which misaligns columns on empty fields (e.g. api_version).
    rows="$(docker compose exec -T postgres psql -U "$PGU" -d "$PGD" -tAF $'\x1f' -c \
      "SELECT tenant_id, id, name, api, api_version, models, vision_capable, has_key
       FROM llm_providers ORDER BY tenant_id, id;" 2>/dev/null || true)"
    if [[ -n "$rows" ]]; then
      providers_source="live-db"
      providers_md+="| Tenant | Provider ID | Name | Dialect | API Version | Model IDs | Vision Capable | Has Key |"$'\n'
      providers_md+="|---|---|---|---|---|---|---|---|"$'\n'
      while IFS=$'\x1f' read -r tenant id name api apiver models vision haskey; do
        [[ -z "$tenant" ]] && continue
        model_ids="$(echo "$models" | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' | sed -E 's/.*"([^"]*)"$/\1/' | paste -sd, - || true)"
        [[ -z "$model_ids" ]] && model_ids="—"
        [[ -z "$apiver" ]] && apiver="—"
        providers_md+="| $(esc "$tenant") | $(esc "$id") | $(esc "$name") | $(esc "$api") | $(esc "$apiver") | $(esc "$model_ids") | $(esc "$vision") | $(esc "$haskey") |"$'\n'
      done <<< "$rows"
    fi
  fi
fi

if [[ "$providers_source" == "none" ]]; then
  providers_md="_Providers are per-deployment Postgres (\`llm_providers\`) runtime state and are_
_deliberately NOT pinned in this committed artifact — dev/test fixture rows would_
_otherwise leak into a canonical BOM._

_Regenerate against a live deployment with \`scripts/generate-ai-bom.sh --live-db\`_
_(requires a reachable compose stack's \`postgres\` service) before treating this_
_section as current for that environment._"
fi

# --- Skill bundles ----------------------------------------------------------
skills_md="| Skill | Version | Effect Class | SBOM Ref |"$'\n'
skills_md+="|---|---|---|---|"$'\n'
for f in $(find skills -maxdepth 2 -name skill.yaml | sort); do
  name="$(grep -m1 '^\s*name:' "$f" | sed -E 's/^\s*name:\s*//; s/\s*#.*$//')"
  version="$(grep -m1 '^\s*version:' "$f" | sed -E 's/^\s*version:\s*//; s/\s*#.*$//')"
  effect="$(grep -m1 '^\s*effect_class:' "$f" | sed -E 's/^\s*effect_class:\s*//; s/\s*#.*$//')"
  sbom_ref="$(grep -m1 '^\s*sbom_ref:' "$f" | sed -E 's/^\s*sbom_ref:\s*//; s/\s*#.*$//' | tr -d '"')"
  [[ -z "$sbom_ref" ]] && sbom_ref="—"
  skills_md+="| $(esc "$name") | $(esc "$version") | $(esc "$effect") | $(esc "$sbom_ref") |"$'\n'
done

# --- Assemble ----------------------------------------------------------------
{
  echo "# AI Bill of Materials"
  echo
  echo "Generated: $GENERATED_AT — git $GIT_SHA"
  echo
  echo "Committed artifact per \`\` CP3.3; regenerate at"
  echo "release with \`scripts/generate-ai-bom.sh\`. Contains no key material — provider"
  echo "rows carry a \`Has Key\` boolean only, never key bytes/values."
  echo
  echo "## LLM providers"
  echo
  echo "$providers_md"
  echo
  echo "## Skill bundles"
  echo
  echo "$skills_md"
  echo "## Versions"
  echo
  echo "| Component | Version |"
  echo "|---|---|"
  echo "| Pi harness (agent-gateway) | $(esc "$HARNESS_VERSION") |"
  echo "| Broker | $(esc "$BROKER_VERSION") |"
} > "$OUT"

echo "wrote $OUT"
