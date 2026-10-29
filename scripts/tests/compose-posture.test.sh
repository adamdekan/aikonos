#!/usr/bin/env bash
# scripts/tests/compose-posture.test.sh
#
# Seeded regressions for scripts/check-compose-posture.sh. Each case copies the
# compose files into a temp root, applies one sed edit that breaks one rule, and
# expects exit 1 with that rule's POSTURE line, so a jq edit that makes a rule
# pass vacuously fails here. The unedited copy must pass, so a failure below is
# the seed and not a broken fixture.
#
# Usage: bash scripts/tests/compose-posture.test.sh   (needs bash, jq, docker compose CLI)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
POSTURE="${ROOT}/scripts/check-compose-posture.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

pass=0
fail=0

# run_case <name> <file to seed, "" = none> <sed expression> <expected POSTURE line, "" = exit 0>
run_case() {
  local name="$1" file="$2" expr="$3" want="$4" dir="${WORKDIR}/$1" out rc
  mkdir -p "${dir}/deploy/compose"
  cp "${ROOT}/compose.yaml" "${dir}/"
  cp "${ROOT}"/deploy/compose/compose.*.yaml "${ROOT}"/deploy/compose/.env.*.example "${dir}/deploy/compose/"
  if [ -n "$file" ]; then
    sed "$expr" "${dir}/${file}" > "${WORKDIR}/${name}.seeded"
    if cmp -s "${WORKDIR}/${name}.seeded" "${dir}/${file}"; then
      echo "FAIL: $name (the seed did not change ${file})"
      fail=$((fail + 1))
      return
    fi
    cp "${WORKDIR}/${name}.seeded" "${dir}/${file}"
  fi
  set +e
  out="$(bash "$POSTURE" --repo-root "$dir" 2>&1)"
  rc=$?
  set -e
  if { [ -z "$want" ] && [ "$rc" -eq 0 ]; } \
    || { [ -n "$want" ] && [ "$rc" -eq 1 ] && grep -qF "$want" <<<"$out"; }; then
    echo "PASS: $name (exit=$rc)"
    pass=$((pass + 1))
  else
    echo "FAIL: $name (exit=$rc, want: ${want:-exit 0})"
    echo "$out"
    fail=$((fail + 1))
  fi
}

run_case baseline "" "" ""
run_case local-trust-proxy deploy/compose/.env.local.example \
  's/^AIKONOS_EXTERNAL_TRUST_PROXY=0$/AIKONOS_EXTERNAL_TRUST_PROXY=1/' \
  'POSTURE [local] agent-gateway publishes 8090 directly but trusts X-Forwarded-For'
run_case local-entra-trust-proxy deploy/compose/.env.local-entra.example \
  's/^AIKONOS_EXTERNAL_TRUST_PROXY=0$/AIKONOS_EXTERNAL_TRUST_PROXY=1/' \
  'POSTURE [local-entra] agent-gateway publishes 8090 directly but trusts X-Forwarded-For'
run_case docs-mcp-host compose.yaml \
  '/^      HOST: "0.0.0.0"$/d' \
  'POSTURE [local] docs-mcp binds "" in its container'
run_case bundle-loopback compose.yaml \
  's/AIKONOS_POLICY_BUNDLE_HTTP_ADDR: *":9092"/AIKONOS_POLICY_BUNDLE_HTTP_ADDR: "127.0.0.1:9092"/' \
  'POSTURE [onprem] broker binds the policy bundle on "127.0.0.1:9092"'
run_case broker-tenant compose.yaml \
  's/\${AIKONOS_TENANT_ID:?[^}]*}/${AIKONOS_BROKER_TENANT_ID}/' \
  'POSTURE [azure] broker AIKONOS_TENANT_ID is "aikonos-dev"'
run_case dev-mode-mismatch compose.yaml \
  's/^\(      AIKONOS_DEV_MODE: \{11\}\)\${AIKONOS_DEV_MODE:-false}$/\1"false"/' \
  'POSTURE [local] broker and agent-gateway disagree on AIKONOS_DEV_MODE'
run_case unsigned-sessions-default compose.yaml \
  's/ALLOW_UNSIGNED_SESSIONS:-false}/ALLOW_UNSIGNED_SESSIONS:-true}/' \
  'POSTURE [local] broker AIKONOS_WORKSPACE_ALLOW_UNSIGNED_SESSIONS defaults to "true" when unset, want false'

echo "compose-posture: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
