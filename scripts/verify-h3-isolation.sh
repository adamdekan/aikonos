#!/usr/bin/env bash
# verify-h3-isolation.sh — H3 Phase 1 isolation smoke tests.
# Mirrors the ok()/bad()/skip() helper pattern from scripts/verify-f3.sh.
# Run after `task compose:up` + `task compose:seed` (core profile).
#
# What is checked:
#   A. CreateGatewayTask with NO owner_grant → PermissionDenied
#   B. CreateGatewayTask with a forged owner_grant → PermissionDenied
#   C. (optional/best-effort) ClaimDueScheduledRuns returns a non-empty
#      owner_grant per run — scheduler path still works
#
# Gated skip: if grpcurl is missing, the broker south port (9091) is not
# reachable, or the gateway leaf cert is absent, the whole script skips
# cleanly with exit 0 and a clear message.

set -euo pipefail

BROKER_SOUTH="${BROKER_SOUTH:-localhost:9091}"
TLS_DIR="${TLS_DIR:-$(cd "$(dirname "$0")/.." && pwd)/deploy/compose/tls}"
CERT="${TLS_DIR}/agent-gateway.crt"
KEY="${TLS_DIR}/agent-gateway.key"
CA="${TLS_DIR}/ca.crt"

# dev tenant seeded by compose-seed-openfga.sh
TENANT_ID="${AIKONOS_TENANT_ID:-00000000-0000-0000-0000-000000000001}"

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

echo "=== H3 Phase 1 isolation checks ==="
echo ""

# ── Phase 1 prerequisites — gate only the Phase 1 gRPC checks ────────────────
PHASE1_SKIP=""

if ! command -v grpcurl >/dev/null 2>&1; then
  skip "grpcurl not found — install grpcurl to run south gRPC checks (Phase 1 skipped)"
  PHASE1_SKIP="grpcurl absent"
elif ! timeout 2 bash -c "</dev/tcp/127.0.0.1/9091" 2>/dev/null; then
  skip "broker south port 9091 not reachable — is the stack up? (task compose:up) (Phase 1 skipped)"
  PHASE1_SKIP="port 9091 unreachable"
elif [ ! -f "$CERT" ] || [ ! -f "$KEY" ] || [ ! -f "$CA" ]; then
  skip "gateway leaf cert absent (${TLS_DIR}) — run: bash scripts/compose-dev-ca.sh (Phase 1 skipped)"
  PHASE1_SKIP="certs absent"
fi

if [ -z "$PHASE1_SKIP" ]; then
  # grpcurl mTLS flags (gateway leaf cert, broker CA)
  GRPC_TLS_FLAGS="-cert ${CERT} -key ${KEY} -cacert ${CA}"
fi

# ── A, B, C — Phase 1 gRPC checks (skipped when Phase 1 prereqs absent) ──────
if [ -n "$PHASE1_SKIP" ]; then
  skip "A: Phase 1 prereqs absent (${PHASE1_SKIP}) — skipping gRPC checks"
  skip "B: Phase 1 prereqs absent — skipping"
  skip "C: Phase 1 prereqs absent — skipping"
else

# ── A — no owner_grant → PermissionDenied ──────────────────────────────────
echo "A. CreateGatewayTask with no owner_grant → PermissionDenied"

set +e
A_OUT=$(grpcurl ${GRPC_TLS_FLAGS} \
  -d "{\"tenant_id\":\"${TENANT_ID}\",\"owner_user_id\":\"user:verify-probe\",\"prompt\":\"hello\"}" \
  "${BROKER_SOUTH}" \
  aikonos.broker.v1.SandboxService/CreateGatewayTask 2>&1)
set -e

if echo "$A_OUT" | grep -qiE "PermissionDenied|permission_denied|owner grant required"; then
  ok "no owner_grant → PermissionDenied (impersonation rejected)"
else
  bad "expected PermissionDenied, got: $(echo "$A_OUT" | head -c 200)"
fi

# ── B — forged owner_grant → PermissionDenied ────────────────────────────────
echo ""
echo "B. CreateGatewayTask with forged owner_grant → PermissionDenied"

set +e
B_OUT=$(grpcurl ${GRPC_TLS_FLAGS} \
  -d "{\"tenant_id\":\"${TENANT_ID}\",\"owner_user_id\":\"user:verify-probe\",\"prompt\":\"hello\",\"owner_grant\":\"v1.garbage.forged.sig\"}" \
  "${BROKER_SOUTH}" \
  aikonos.broker.v1.SandboxService/CreateGatewayTask 2>&1)
set -e

if echo "$B_OUT" | grep -qiE "PermissionDenied|permission_denied"; then
  ok "forged owner_grant → PermissionDenied"
else
  bad "expected PermissionDenied for forged grant, got: $(echo "$B_OUT" | head -c 200)"
fi

# ── C — scheduler path (optional/best-effort) ────────────────────────────────
# Requires at least one seeded scheduled run with a due_at in the past.
# Skips gracefully when no due runs exist — does not fail the script.
echo ""
echo "C. ClaimDueScheduledRuns returns non-empty owner_grant (optional)"

set +e
C_OUT=$(grpcurl ${GRPC_TLS_FLAGS} \
  -d "{\"tenant_id\":\"${TENANT_ID}\"}" \
  "${BROKER_SOUTH}" \
  aikonos.broker.v1.SandboxService/ClaimDueScheduledRuns 2>&1)
set -e

if echo "$C_OUT" | grep -qiE "PermissionDenied|Unauthenticated|error"; then
  bad "ClaimDueScheduledRuns RPC failed: $(echo "$C_OUT" | head -c 200)"
elif echo "$C_OUT" | grep -q '"ownerGrant"'; then
  # At least one run returned; verify it carries a non-empty grant
  if echo "$C_OUT" | grep -qE '"ownerGrant"[[:space:]]*:[[:space:]]*"[^"]'; then
    ok "ClaimDueScheduledRuns returned run(s) with non-empty ownerGrant"
  else
    bad "ClaimDueScheduledRuns returned run(s) but ownerGrant is empty"
  fi
else
  skip "ClaimDueScheduledRuns: no due runs in tenant — seed a scheduled run to verify C end-to-end"
fi

fi  # end Phase 1 gRPC checks

# ── Phase 2 — forked-child credential isolation ──────────────────────────────
echo ""
echo "=== H3 Phase 2 isolation checks ==="
echo ""
echo "(Phase 2 requires the stack to be up AND at least one chat turn to have been"
echo " driven so the supervisor has spawned the child process. If no child exists"
echo " yet, these checks will skip cleanly — drive one prompt turn in the webui"
echo " first, then re-run.)"
echo ""

# Gate: is docker available and is the agent-gateway container running?
if ! command -v docker >/dev/null 2>&1; then
  skip "docker not found — cannot exec into agent-gateway for Phase 2 checks"
  echo ""
  echo "=== Phase 2: skipped (docker absent) ==="
else
  CONTAINER=$(docker compose ps -q agent-gateway 2>/dev/null | head -1)
  if [ -z "$CONTAINER" ]; then
    skip "agent-gateway container not running — is the stack up? (task compose:up)"
    echo ""
    echo "=== Phase 2: skipped (container not running) ==="
  else

    # D — pid/thread count evidence for pids_limit headroom
    echo "D. Container pid/thread count (pids_limit headroom evidence)"
    set +e
    PID_COUNT=$(docker compose exec -T agent-gateway sh -c \
      'ls /proc/*/task 2>/dev/null | grep -c task || echo 0' 2>/dev/null)
    set -e

    if [ -z "$PID_COUNT" ] || [ "$PID_COUNT" = "0" ]; then
      skip "D: could not read /proc/*/task inside container"
    else
      echo "  pid/thread count in container: ${PID_COUNT}"
      if [ "$PID_COUNT" -lt 256 ]; then
        ok "D: thread count ${PID_COUNT} < 256 — well under pids_limit:512"
      else
        bad "D: thread count ${PID_COUNT} >= 256 — approaching pids_limit:512; review mem_limit/pids_limit for Phase 3"
      fi
    fi

    # E — find the forked child Node process (child-entry.ts / AIKONOS_CHILD_ENTRY)
    echo ""
    echo "E. Forked child process has no provider key in env"
    set +e
    # The child is forked by the supervisor; it is a node process distinct from PID 1.
    # Match on "child-entry" in its cmdline (child-entry.ts is the child entrypoint).
    CHILD_PID=$(docker compose exec -T agent-gateway sh -c \
      'ps -eo pid,args 2>/dev/null | grep -i "child.entry" | grep -v grep | awk "{print \$1}" | head -1' \
      2>/dev/null | tr -d '[:space:]')
    set -e

    if [ -z "$CHILD_PID" ]; then
      skip "E: no forked child process found (child-entry not in ps output)."
      echo "  Drive one prompt turn in the webui first to lazily spawn the child, then re-run."
    else
      echo "  Found child pid: ${CHILD_PID}"

      # Assert child env has NO OPENROUTER_API_KEY
      set +e
      CHILD_ENV=$(docker compose exec -T agent-gateway sh -c \
        "cat /proc/${CHILD_PID}/environ 2>/dev/null | tr '\\0' '\\n'" 2>/dev/null)
      set -e

      if [ -z "$CHILD_ENV" ]; then
        skip "E: could not read /proc/${CHILD_PID}/environ (child may have exited)"
      else
        if echo "$CHILD_ENV" | grep -qE "^OPENROUTER_API_KEY=.+"; then
          bad "E: OPENROUTER_API_KEY found in child env — provider key was NOT scrubbed (Phase 2 regression)"
        elif echo "$CHILD_ENV" | grep -q "OPENROUTER_API_KEY="; then
          ok "E: OPENROUTER_API_KEY present in child env but is EMPTY (scrubbed — child holds no real key)"
        else
          ok "E: OPENROUTER_API_KEY absent from child env (scrubbed by supervisor spawn)"
        fi
      fi

      # F — verify the PARENT (pid 1) does have the key (sanity check)
      echo ""
      echo "F. Parent process (pid 1) retains provider key (sanity check)"
      set +e
      PARENT_ENV=$(docker compose exec -T agent-gateway sh -c \
        "cat /proc/1/environ 2>/dev/null | tr '\\0' '\\n'" 2>/dev/null)
      set -e

      if [ -z "$PARENT_ENV" ]; then
        skip "F: could not read /proc/1/environ"
      elif echo "$PARENT_ENV" | grep -qE "^OPENROUTER_API_KEY=.+"; then
        ok "F: parent (pid 1) retains OPENROUTER_API_KEY — credential-host contract holds"
      else
        # Key may be empty in dev (OPENROUTER_API_KEY= unset is normal in some setups)
        skip "F: OPENROUTER_API_KEY not set or empty in parent env — set it in .env to fully verify the key split"
      fi
    fi

  fi
fi

# ── Summary ──────────────────────────────────────────────────────────────────
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
