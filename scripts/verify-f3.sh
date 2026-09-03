#!/usr/bin/env bash
# verify-f3.sh — F3 external agent endpoint smoke tests.
# Mirrors the ok()/bad() helper pattern from scripts/compose-verify.sh.
# Run after `task compose:up` (core profile).
#
# What is checked:
#   1. Gateway external port :8090 reachable
#   2. Unauthenticated POST /v1/agents/x/invoke → 401
#   3. Garbage API key "Authorization: Bearer tk_bad" → 401
#   4. Happy-path (gated): create an approvalMode:auto agent → mint key →
#      invoke (live LLM SSE) → revoke → revoked-key→401. Auto-skips when its
#      prerequisites are absent: an admin token (AIKONOS_SMOKE_CLIENT_SECRET),
#      AIKONOS_API_KEY_PEPPER on the broker, and LLM egress (OPENROUTER_API_KEY).
#
# What is still skipped:
#   - tool invocation outside the agent's allowed skill set → 403
#
# Note: FGA-revoked-mid-run is an intentional non-guarantee. A running session
# holds a minted Biscuit token for each approved step; FGA revocation affects
# future InvokeTool calls only once the in-flight capability token expires or
# is not re-minted. This is by design — capability tokens are short-lived and
# the session re-mints per step, so revocation is effective within one step
# boundary (not instantaneous mid-step).

set -euo pipefail

GW_EXTERNAL="${GW_EXTERNAL:-http://localhost:8090}"
PASS=0
FAIL=0

ok() {
  echo "  [PASS] $1"
  PASS=$((PASS + 1))
}

bad() {
  echo "  [FAIL] $1"
  FAIL=$((FAIL + 1))
}

skip() {
  echo "  [SKIP] $1"
}

echo "=== F3 external agent endpoint checks ==="
echo ""

# ── 1. External port reachable ────────────────────────────────────────────────
echo "1. External port :8090 reachable"
if curl -sf --max-time 5 "${GW_EXTERNAL}/healthz" >/dev/null 2>&1; then
  ok ":8090 /healthz responded"
else
  # healthz may not exist on the external listener; a 4xx is still reachable
  status=$(curl -o /dev/null -w "%{http_code}" --max-time 5 "${GW_EXTERNAL}/healthz" 2>/dev/null || echo "000")
  if [ "$status" != "000" ]; then
    ok ":8090 reachable (HTTP $status)"
  else
    bad ":8090 not reachable (connection refused or timeout)"
  fi
fi

# ── 2. Unauthenticated invoke → 401 ──────────────────────────────────────────
echo ""
echo "2. Unauthenticated POST /v1/agents/test-agent/invoke → 401"
status=$(curl -o /dev/null -w "%{http_code}" --max-time 5 \
  -X POST "${GW_EXTERNAL}/v1/agents/test-agent/invoke" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"hello"}' 2>/dev/null || echo "000")
if [ "$status" = "401" ]; then
  ok "unauthenticated → 401"
else
  bad "expected 401, got $status"
fi

# ── 3. Garbage API key → 401 ──────────────────────────────────────────────────
echo ""
echo "3. Garbage API key → 401"
status=$(curl -o /dev/null -w "%{http_code}" --max-time 5 \
  -X POST "${GW_EXTERNAL}/v1/agents/test-agent/invoke" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer tk_bad" \
  -d '{"prompt":"hello"}' 2>/dev/null || echo "000")
if [ "$status" = "401" ]; then
  ok "garbage bearer → 401"
else
  bad "expected 401, got $status"
fi

# ── 4. Happy-path: mint → invoke (live LLM) → revoke (gated) ──────────────────
# Drives the full external lifecycle as a service principal (svc-<agentId>).
# Auto-skips when prerequisites are absent — these are operator-config gaps,
# not F3 correctness failures, so they skip rather than fail.
echo ""
echo "4. Happy-path (create auto agent → mint key → live invoke → revoke)"

GATEWAY="${GATEWAY:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:18080}"
SMOKE_SECRET="${AIKONOS_SMOKE_CLIENT_SECRET:-smoke-secret-local-dev}"
ADMIN_USER="${AIKONOS_ADMIN_USER:-admin@example.com}"

# Network-heavy block manages its own pass/fail accounting; a curl --max-time
# timeout must not abort the whole script under errexit.
set +e

# Prereq: an admin bearer via the confidential smoke client (ROPC). Absent → skip.
TOKEN=$(curl -s --max-time 8 \
  -d grant_type=password -d client_id=aikonos-smoke -d client_secret="${SMOKE_SECRET}" \
  -d username="${ADMIN_USER}" -d password=dev -d scope=openid \
  "${KEYCLOAK_URL}/realms/aikonos/protocol/openid-connect/token" 2>/dev/null \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

if [ -z "$TOKEN" ]; then
  skip "happy-path: no admin token (set AIKONOS_SMOKE_CLIENT_SECRET; Keycloak at ${KEYCLOAK_URL})"
else
  AGENT_ID=""
  cleanup_agent() {
    [ -n "$AGENT_ID" ] && curl -s --max-time 8 -X DELETE "${GATEWAY}/admin/agents/${AGENT_ID}" \
      -H "Authorization: Bearer ${TOKEN}" >/dev/null 2>&1
  }
  trap cleanup_agent EXIT

  # 4a. create an approvalMode:auto agent (only auto agents are externally drivable)
  #     gatewayEnabled:true — external reachability is deny-by-default (migration 022)
  AGENT_ID=$(curl -s --max-time 8 -X POST "${GATEWAY}/admin/agents" \
    -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
    -d '{"name":"f3-verify-bot","llmModel":"anthropic/claude-sonnet-4.6","approvalMode":"auto","skills":["web.fetch"],"gatewayEnabled":true}' 2>/dev/null \
    | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

  if [ -z "$AGENT_ID" ]; then
    bad "create approvalMode:auto agent failed (admin token rejected?)"
  else
    ok "created approvalMode:auto agent"

    # 4b. mint an API key
    KEY_JSON=$(curl -s --max-time 8 -X POST "${GATEWAY}/admin/agents/${AGENT_ID}/keys" \
      -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
      -d '{"label":"verify"}' 2>/dev/null)
    RAW_KEY=$(echo "$KEY_JSON" | sed -n 's/.*"rawKey":"\([^"]*\)".*/\1/p')
    KEY_ID=$(echo "$KEY_JSON" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

    if echo "$KEY_JSON" | grep -q 'pepper not configured'; then
      skip "happy-path: AIKONOS_API_KEY_PEPPER not set on the broker — cannot mint (operator config)"
    elif [ -z "$RAW_KEY" ]; then
      bad "mint API key failed: $KEY_JSON"
    else
      ok "minted API key (prefix ${RAW_KEY:0:8}…)"

      # 4c. invoke with the valid key → live LLM SSE stream
      STREAM=$(curl -sN --max-time 60 -X POST "${GW_EXTERNAL}/v1/agents/${AGENT_ID}/invoke" \
        -H "Authorization: Bearer ${RAW_KEY}" -H "Content-Type: application/json" \
        -d '{"prompt":"Reply with exactly the single word: pong"}' 2>/dev/null)
      if echo "$STREAM" | grep -qE '"type":"done"|"type":"text'; then
        ok "valid key → live SSE stream (ran as svc-${AGENT_ID})"
      else
        bad "invoke produced no stream — LLM egress / OPENROUTER_API_KEY? [$(echo "$STREAM" | head -c 160)]"
      fi

      # 4d. revoke the key → a subsequent invoke is rejected 401
      if [ -n "$KEY_ID" ]; then
        curl -s --max-time 8 -X DELETE "${GATEWAY}/admin/agents/${AGENT_ID}/keys/${KEY_ID}" \
          -H "Authorization: Bearer ${TOKEN}" >/dev/null 2>&1
        REVOKED_STATUS=$(curl -o /dev/null -w "%{http_code}" --max-time 8 \
          -X POST "${GW_EXTERNAL}/v1/agents/${AGENT_ID}/invoke" \
          -H "Authorization: Bearer ${RAW_KEY}" -H "Content-Type: application/json" \
          -d '{"prompt":"hi"}' 2>/dev/null)
        if [ "$REVOKED_STATUS" = "401" ]; then
          ok "revoked key → 401"
        else
          bad "expected 401 after revoke, got ${REVOKED_STATUS}"
        fi
      fi
    fi
  fi

  cleanup_agent
  AGENT_ID=""
  trap - EXIT
fi

# The broker-side `tool ∈ agent.Skills` enforcement is proven by the CP2 unit
# test TestSkillBoundary_OutOfSkill_Denied (broker/internal/broker/skill_boundary_test.go).
# Skipped here because the live smoke requires a south grpcurl invocation of a
# specific out-of-skill tool; the gate itself is not untested.
echo ""
echo "5. Skill-boundary (out-of-skill tool → 403)"
skip "tool outside agent skill set → 403 (broker gate covered by TestSkillBoundary_OutOfSkill_Denied; live check needs south grpcurl)"

# ── Summary ──────────────────────────────────────────────────────────────────
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
