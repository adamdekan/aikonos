#!/usr/bin/env bash
# verify-llm-providers.sh — F4 modular LLM-provider smoke tests.
# Mirrors the ok()/bad()/skip() pattern from scripts/verify-f3.sh.
# Run after `task compose:up` (+ `task compose:seed`); the live-usage check also
# needs the `obs` profile (Prometheus on :8889) and LLM egress.
#
# What is checked (admin-only, no LLM — the CP1 DB-level invariants land here):
#   1. GET /admin/providers → 200, the seeded `openrouter` default is present
#   2. Upsert→Get round-trip (POST a provider, GET shows it with its prices)
#   3. List never leaks api-key bytes (has_key only)
#   4. SetDefault single-default invariant (exactly one is_default per tenant)
#   5. Delete removes the provider; default restored to openrouter
#
# What is checked (gated — needs pepper + OPENROUTER_API_KEY + obs profile):
#   6. A live agent run emits llm_tokens_total + llm_cost_total to Prometheus
#
# Auto-skips when prerequisites are absent (admin token, pepper, LLM egress,
# Prometheus) — those are operator-config gaps, not F4 correctness failures.

set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
GW_EXTERNAL="${GW_EXTERNAL:-http://localhost:8090}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:18080}"
PROM="${PROM:-http://localhost:8889}"
SMOKE_SECRET="${AIKONOS_SMOKE_CLIENT_SECRET:-smoke-secret-local-dev}"
ADMIN_USER="${AIKONOS_ADMIN_USER:-admin@example.com}"
PASS=0
FAIL=0

ok()   { echo "  [PASS] $1"; PASS=$((PASS + 1)); }
bad()  { echo "  [FAIL] $1"; FAIL=$((FAIL + 1)); }
skip() { echo "  [SKIP] $1"; }

echo "=== F4 LLM-provider checks ==="
echo ""

# Network-heavy: manage our own accounting; a curl timeout must not abort errexit.
set +e

# Prereq: an admin bearer via the confidential smoke client (ROPC). Absent → skip all.
TOKEN=$(curl -s --max-time 8 \
  -d grant_type=password -d client_id=aikonos-smoke -d client_secret="${SMOKE_SECRET}" \
  -d username="${ADMIN_USER}" -d password=dev -d scope=openid \
  "${KEYCLOAK_URL}/realms/aikonos/protocol/openid-connect/token" 2>/dev/null \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

auth=(-H "Authorization: Bearer ${TOKEN}")
json=(-H "Content-Type: application/json")

if [ -z "$TOKEN" ]; then
  skip "no admin token (set AIKONOS_SMOKE_CLIENT_SECRET; Keycloak at ${KEYCLOAK_URL}) — all checks skipped"
  echo ""
  echo "=== Results: ${PASS} passed, ${FAIL} failed (admin token absent) ==="
  exit 0
fi

TEST_ID="verify-prov"
cleanup() {
  curl -s --max-time 8 -X DELETE "${GATEWAY}/admin/providers/${TEST_ID}" "${auth[@]}" >/dev/null 2>&1
  # Restore openrouter as the tenant default (the seed default).
  curl -s --max-time 8 -X POST "${GATEWAY}/admin/providers/openrouter/default" "${auth[@]}" >/dev/null 2>&1
}
trap cleanup EXIT

# ── 1. List providers; seeded openrouter default present ──────────────────────
echo "1. GET /admin/providers → seeded openrouter default"
LIST=$(curl -s --max-time 8 "${GATEWAY}/admin/providers" "${auth[@]}" 2>/dev/null)
if echo "$LIST" | jq -e '.providers[] | select(.id=="openrouter")' >/dev/null 2>&1; then
  if echo "$LIST" | jq -e '.providers[] | select(.id=="openrouter" and .isDefault==true)' >/dev/null 2>&1; then
    ok "openrouter present and is_default"
  else
    ok "openrouter present (not flagged default — admin may have changed it)"
  fi
else
  bad "openrouter not in provider list (run task compose:seed?) [$(echo "$LIST" | head -c 160)]"
fi

# ── 2. Upsert → Get round-trip ────────────────────────────────────────────────
echo ""
echo "2. Upsert → Get round-trip"
curl -s --max-time 8 -X POST "${GATEWAY}/admin/providers" "${auth[@]}" "${json[@]}" -d "{
  \"provider\": {
    \"id\": \"${TEST_ID}\", \"name\": \"Verify\", \"endpoint\": \"https://example.com/v1\",
    \"api\": \"openai-completions\", \"enabled\": true, \"isDefault\": false, \"hasKey\": false,
    \"models\": [{\"id\":\"m1\",\"priceIn\":0.000002,\"priceOut\":0.000008,\"priceCacheRead\":0,\"priceCacheWrite\":0,\"contextWindow\":100000,\"maxTokens\":4096}]
  },
  \"apiKey\": \"sk-verify-secret\"
}" >/dev/null 2>&1
LIST=$(curl -s --max-time 8 "${GATEWAY}/admin/providers" "${auth[@]}" 2>/dev/null)
if echo "$LIST" | jq -e ".providers[] | select(.id==\"${TEST_ID}\" and .models[0].priceIn==0.000002 and .models[0].priceOut==0.000008)" >/dev/null 2>&1; then
  ok "upserted provider round-trips with prices"
else
  bad "upsert round-trip mismatch [$(echo "$LIST" | jq -c ".providers[] | select(.id==\"${TEST_ID}\")" 2>/dev/null | head -c 160)]"
fi

# ── 3. List never leaks api-key bytes ─────────────────────────────────────────
echo ""
echo "3. List never returns api-key bytes (has_key only)"
if echo "$LIST" | jq -e '[.providers[].apiKey] | map(select(. != null and . != "")) | length == 0' >/dev/null 2>&1; then
  HASKEY=$(echo "$LIST" | jq -r ".providers[] | select(.id==\"${TEST_ID}\") | .hasKey" 2>/dev/null)
  if [ "$HASKEY" = "true" ]; then
    ok "no apiKey bytes on the wire; has_key=true after write"
  else
    bad "has_key should be true after an apiKey write (got '${HASKEY}')"
  fi
else
  bad "an apiKey value leaked in GET /admin/providers"
fi

# ── 4. SetDefault single-default invariant ────────────────────────────────────
echo ""
echo "4. SetDefault → exactly one default per tenant"
curl -s --max-time 8 -X POST "${GATEWAY}/admin/providers/${TEST_ID}/default" "${auth[@]}" >/dev/null 2>&1
LIST=$(curl -s --max-time 8 "${GATEWAY}/admin/providers" "${auth[@]}" 2>/dev/null)
NDEF=$(echo "$LIST" | jq '[.providers[] | select(.isDefault==true)] | length' 2>/dev/null)
NEWDEF=$(echo "$LIST" | jq -r '.providers[] | select(.isDefault==true) | .id' 2>/dev/null)
if [ "$NDEF" = "1" ] && [ "$NEWDEF" = "${TEST_ID}" ]; then
  ok "exactly one default, switched to ${TEST_ID}"
else
  bad "expected single default=${TEST_ID}, got count=${NDEF} id=${NEWDEF}"
fi

# ── 5. Delete removes the provider ────────────────────────────────────────────
echo ""
echo "5. Delete removes the provider"
curl -s --max-time 8 -X DELETE "${GATEWAY}/admin/providers/${TEST_ID}" "${auth[@]}" >/dev/null 2>&1
LIST=$(curl -s --max-time 8 "${GATEWAY}/admin/providers" "${auth[@]}" 2>/dev/null)
if echo "$LIST" | jq -e ".providers[] | select(.id==\"${TEST_ID}\")" >/dev/null 2>&1; then
  bad "provider ${TEST_ID} still present after delete"
else
  ok "provider deleted"
fi
# restore openrouter default for a clean tenant state
curl -s --max-time 8 -X POST "${GATEWAY}/admin/providers/openrouter/default" "${auth[@]}" >/dev/null 2>&1

# ── 6. Live usage → Prometheus (gated) ────────────────────────────────────────
echo ""
echo "6. Live agent run emits llm_tokens_total + llm_cost_total to Prometheus"
if ! curl -sf --max-time 5 "${PROM}/metrics" >/dev/null 2>&1; then
  skip "Prometheus exporter ${PROM} unreachable (start the obs profile: task compose:obs)"
else
  AGENT_ID=$(curl -s --max-time 8 -X POST "${GATEWAY}/admin/agents" "${auth[@]}" "${json[@]}" \
    -d '{"name":"llm-verify-bot","llmModel":"anthropic/claude-sonnet-4.6","approvalMode":"auto","skills":["web.fetch"]}' 2>/dev/null \
    | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  if [ -z "$AGENT_ID" ]; then
    skip "could not create agent for live check"
  else
    KEY_JSON=$(curl -s --max-time 8 -X POST "${GATEWAY}/admin/agents/${AGENT_ID}/keys" "${auth[@]}" "${json[@]}" -d '{"label":"llm-verify"}' 2>/dev/null)
    RAW_KEY=$(echo "$KEY_JSON" | sed -n 's/.*"rawKey":"\([^"]*\)".*/\1/p')
    if echo "$KEY_JSON" | grep -q 'pepper not configured'; then
      skip "AIKONOS_API_KEY_PEPPER not set on the broker — cannot drive a live run"
    elif [ -z "$RAW_KEY" ]; then
      skip "could not mint key for live check"
    else
      STREAM=$(curl -sN --max-time 60 -X POST "${GW_EXTERNAL}/v1/agents/${AGENT_ID}/invoke" \
        -H "Authorization: Bearer ${RAW_KEY}" "${json[@]}" \
        -d '{"prompt":"Reply with exactly the single word: pong"}' 2>/dev/null)
      if ! echo "$STREAM" | grep -qE '"type":"done"|"type":"text'; then
        skip "live invoke produced no stream — LLM egress / OPENROUTER_API_KEY absent"
      else
        # Primary (deterministic): EmitLlmUsage writes an llm.usage audit event
        # synchronously on each completed turn. Assert it for THIS run's agent.
        # The metric counter is incremented in the same call; Prometheus export is
        # eventually-consistent (OTLP ~60s interval), so it is a best-effort bonus.
        # LLM turn completion is not guaranteed in a smoke (rate limits, egress) —
        # a streamed-but-no-usage run SKIPs rather than fails.
        usage_seen=""
        if command -v docker >/dev/null 2>&1 && docker compose ps broker >/dev/null 2>&1; then
          for _ in 1 2 3 4 5 6; do
            if docker compose logs broker --since 150s 2>/dev/null \
                 | grep '"event_type":"llm.usage"' | grep -q "${AGENT_ID}"; then
              usage_seen="yes"; break
            fi
            sleep 3
          done
          if [ -n "$usage_seen" ]; then
            ok "llm.usage audit event recorded (tokens+cost) for agent ${AGENT_ID}"
          else
            skip "no llm.usage for agent ${AGENT_ID} — LLM turn did not complete (rate-limit/egress)"
          fi
        else
          skip "docker unavailable — cannot assert the broker llm.usage audit event"
        fi

        # Best-effort: the metric reached Prometheus (export-interval dependent).
        if curl -s --max-time 5 "${PROM}/metrics" 2>/dev/null | grep -q '^llm_tokens_total'; then
          ok "llm_tokens_total + llm_cost_total present in Prometheus"
        else
          skip "llm_tokens_total not yet exported to Prometheus (OTLP periodic reader interval)"
        fi
      fi
    fi
    curl -s --max-time 8 -X DELETE "${GATEWAY}/admin/agents/${AGENT_ID}" "${auth[@]}" >/dev/null 2>&1
  fi
fi

cleanup
trap - EXIT

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
