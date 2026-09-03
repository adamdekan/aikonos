#!/usr/bin/env bash
set -euo pipefail

# scripts/check-env-drift.sh
#
# Fails loud when the AIKONOS_* env-var surface documented for a compose
# variant (local / azure / onprem) drifts from what compose.yaml + that
# variant's overlay actually wire up via ${VAR} substitution:
#
#   - "missing"  = a key compose substitutes (${AIKONOS_X}) has no entry
#                  (live or commented-documented) in that variant's template.
#   - "extra"    = a key documented in the template is never substituted by
#                  compose for that variant.
#
# Deliberate per-variant differences are carried in the ALLOWLIST array below,
# one line per (variant, direction, key), each with a one-line reason as a
# preceding comment. New drift not covered by the allowlist exits 1 naming
# the variant + keys + direction.
#
# Usage: scripts/check-env-drift.sh [--repo-root DIR]
#   --repo-root DIR   run against a scratch copy instead of the real repo
#                     (used to prove the check goes red on a seeded violation).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${1:-}" == "--repo-root" ]]; then
  ROOT="$(cd "$2" && pwd)"
fi
cd "$ROOT"

COMPOSE_BASE="compose.yaml"
COMPOSE_AZURE="deploy/compose/compose.azure.yaml"
COMPOSE_ONPREM="deploy/compose/compose.onprem.yaml"
TMPL_LOCAL="deploy/compose/.env.local.example"
TMPL_AZURE="deploy/compose/.env.azure.example"
TMPL_ONPREM="deploy/compose/.env.onprem.example"

# Deliberate differences. Format: "<variant>:<direction>:<KEY>"
# direction: missing = compose references it, template has no entry for it
#            extra   = template documents it, compose never substitutes it
ALLOWLIST=(
  # Vestigial ROPC-era / mTLS-path vars: compose.yaml hardcodes a literal
  # container-internal value for these (e.g. AIKONOS_BROKER_TLS_CERT_FILE:
  # /tls/broker.crt) or never forwards them into any service's environment
  # block at all, so setting them in .env has no observable effect on any
  # variant. They stay in every template as historical documentation, same
  # convention as the templates' own "harmless, kept because compose still
  # references the base" comments for the Keycloak block.
  "local:extra:AIKONOS_BROKER_TLS_CERT_FILE"
  "local:extra:AIKONOS_BROKER_TLS_KEY_FILE"
  "local:extra:AIKONOS_BROKER_TLS_CA_FILE"
  "local:extra:AIKONOS_EXTERNAL_PORT"
  "local:extra:AIKONOS_DEMO_PASSWORD"
  "local:extra:AIKONOS_KEYCLOAK_URL"
  "local:extra:AIKONOS_KEYCLOAK_REALM"
  "local:extra:AIKONOS_KEYCLOAK_CLIENT"
  "azure:extra:AIKONOS_BROKER_TLS_CERT_FILE"
  "azure:extra:AIKONOS_BROKER_TLS_KEY_FILE"
  "azure:extra:AIKONOS_BROKER_TLS_CA_FILE"
  "azure:extra:AIKONOS_EXTERNAL_PORT"
  "azure:extra:AIKONOS_DEMO_PASSWORD"
  "azure:extra:AIKONOS_KEYCLOAK_URL"
  "azure:extra:AIKONOS_KEYCLOAK_REALM"
  "azure:extra:AIKONOS_KEYCLOAK_CLIENT"
  "onprem:extra:AIKONOS_BROKER_TLS_CERT_FILE"
  "onprem:extra:AIKONOS_BROKER_TLS_KEY_FILE"
  "onprem:extra:AIKONOS_BROKER_TLS_CA_FILE"
  "onprem:extra:AIKONOS_EXTERNAL_PORT"
  "onprem:extra:AIKONOS_DEMO_PASSWORD"
  "onprem:extra:AIKONOS_KEYCLOAK_URL"
  "onprem:extra:AIKONOS_KEYCLOAK_REALM"
  "onprem:extra:AIKONOS_KEYCLOAK_CLIENT"

  # AIKONOS_SMOKE_CLIENT_SECRET is read directly by scripts/compose-verify.sh,
  # scripts/verify-f3.sh, scripts/verify-llm-providers.sh via shell env
  # expansion after `.env` is sourced — never through a compose ${}
  # substitution, so it's invisible to a compose-file-only scan by design.
  "local:extra:AIKONOS_SMOKE_CLIENT_SECRET"
  "azure:extra:AIKONOS_SMOKE_CLIENT_SECRET"
  "onprem:extra:AIKONOS_SMOKE_CLIENT_SECRET"

  # AIKONOS_PROVISIONING_SEED_FILE is read by the broker (provisioningseed/seed.go)
  # via os.Getenv but compose.yaml never forwards it into the broker service's
  # environment block on any variant — pre-existing gap; fixing it is a broker
  # compose-service change, out of scope for this drift-check-only batch.
  "local:extra:AIKONOS_PROVISIONING_SEED_FILE"
  "azure:extra:AIKONOS_PROVISIONING_SEED_FILE"
  "onprem:extra:AIKONOS_PROVISIONING_SEED_FILE"

  # AIKONOS_WEBUI_OIDC_SCOPE: compose defaults it to "openid profile" (Keycloak
  # stamps aud=aikonos-broker via a client protocol mapper, so no audience scope
  # is requested), so the local template has no need to set it; azure/onprem
  # override it for Entra's api://... scope.
  "local:missing:AIKONOS_WEBUI_OIDC_SCOPE"

  # Optional obs-archive-to-Azure-Blob config, obs-archive-to-SIEM config, the
  # MCP-private-dial dev-only flag, and the Docker-root override are
  # documented once (commented, as examples) in the local template and not
  # repeated per-variant — they are orthogonal to which compose overlay is
  # active (the obs profile and the
  # dev-only MCP flag apply the same way regardless of deployment variant).
  "azure:missing:AIKONOS_OBS_AZURE_AUTH_TYPE"
  "azure:missing:AIKONOS_OBS_AZURE_ACCOUNT_URL"
  "azure:missing:AIKONOS_OBS_AZURE_CLIENT_ID"
  "azure:missing:AIKONOS_OBS_AZURE_CLIENT_SECRET"
  "azure:missing:AIKONOS_OBS_AZURE_CONNECTION_STRING"
  "azure:missing:AIKONOS_OBS_AZURE_TENANT_ID"
  "azure:missing:AIKONOS_DOCKER_CONTAINERS_DIR"
  "azure:missing:AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE"
  "azure:missing:AIKONOS_SIEM_OTLP_ENDPOINT"
  "azure:missing:AIKONOS_SIEM_OTLP_AUTH_HEADER"
  "onprem:missing:AIKONOS_OBS_AZURE_AUTH_TYPE"
  "onprem:missing:AIKONOS_OBS_AZURE_ACCOUNT_URL"
  "onprem:missing:AIKONOS_OBS_AZURE_CLIENT_ID"
  "onprem:missing:AIKONOS_OBS_AZURE_CLIENT_SECRET"
  "onprem:missing:AIKONOS_OBS_AZURE_CONNECTION_STRING"
  "onprem:missing:AIKONOS_OBS_AZURE_TENANT_ID"
  "onprem:missing:AIKONOS_DOCKER_CONTAINERS_DIR"
  "onprem:missing:AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE"
  "onprem:missing:AIKONOS_SIEM_OTLP_ENDPOINT"
  "onprem:missing:AIKONOS_SIEM_OTLP_AUTH_HEADER"
)

is_allowlisted() {
  local variant="$1" direction="$2" key="$3" entry
  for entry in "${ALLOWLIST[@]}"; do
    [[ "$entry" == "${variant}:${direction}:${key}" ]] && return 0
  done
  return 1
}

# Keys compose substitutes as ${AIKONOS_X} or ${AIKONOS_X:-default}.
referenced_keys() {
  grep -ohE '\$\{AIKONOS_[A-Z0-9_]+' "$@" | sed 's/^\${//' | sort -u
}

# Keys documented in a template, live or commented-out (commented = optional,
# still counts as "documented" for drift purposes).
template_keys() {
  grep -ohE '^[[:space:]]*#?[[:space:]]*AIKONOS_[A-Z0-9_]+=' "$1" \
    | grep -oE 'AIKONOS_[A-Z0-9_]+' | sort -u
}

fail=0

check_variant() {
  local variant="$1" template="$2"
  shift 2
  local compose_files=("$@")

  local ref_file tmpl_file
  ref_file="$(mktemp)"
  tmpl_file="$(mktemp)"
  referenced_keys "${compose_files[@]}" > "$ref_file"
  template_keys "$template" > "$tmpl_file"

  local missing extra key
  missing="$(comm -23 "$ref_file" "$tmpl_file")"
  extra="$(comm -13 "$ref_file" "$tmpl_file")"
  rm -f "$ref_file" "$tmpl_file"

  local bad_missing=() bad_extra=()
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    is_allowlisted "$variant" "missing" "$key" || bad_missing+=("$key")
  done <<< "$missing"
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    is_allowlisted "$variant" "extra" "$key" || bad_extra+=("$key")
  done <<< "$extra"

  if [[ ${#bad_missing[@]} -gt 0 ]]; then
    echo "DRIFT [$variant] compose references but $template is missing: ${bad_missing[*]}" >&2
    fail=1
  fi
  if [[ ${#bad_extra[@]} -gt 0 ]]; then
    echo "DRIFT [$variant] $template documents but no compose file substitutes: ${bad_extra[*]}" >&2
    fail=1
  fi
}

check_variant local  "$TMPL_LOCAL"  "$COMPOSE_BASE"
check_variant azure  "$TMPL_AZURE"  "$COMPOSE_BASE" "$COMPOSE_AZURE"
check_variant onprem "$TMPL_ONPREM" "$COMPOSE_BASE" "$COMPOSE_ONPREM"

if [[ "$fail" -eq 1 ]]; then
  echo "check-env-drift: FAIL — add a deliberate allowlist entry (with a reason) or fix the template/compose file." >&2
  exit 1
fi

echo "check-env-drift: OK — local/azure/onprem AIKONOS_* env surfaces match their compose wiring."
