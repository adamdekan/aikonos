#!/usr/bin/env bash
# Network-segmentation proof: verifies the three-network topology isolates
# agent-gateway from raw datastores (backend internal network).
#
# Run from the repo root with the stack up (task compose:up).
#
# NEGATIVE assertions (assume-breach win): agent-gateway MUST NOT reach
#   postgres:5432, vault:8200, minio:9000  — these are backend-only.
# POSITIVE assertions: agent-gateway MUST reach
#   broker:9091, nats:4222               — mesh peers.
#
# The agent-gateway image has Node but no nc/wget. TCP probe via node:
#   resolve('REACHABLE') on connect, reject on error/timeout.
# Docker embedded DNS only resolves same-network peers → ENOTFOUND is the
# expected fast failure once segmented (no route / name not found).
#
# Broker→backend reachability is proven INDIRECTLY: the broker is distroless
# (no shell), so we don't exec it. We rely on the stack being healthy +
# scripts/compose-verify.sh green (broker connects to postgres/vault/openfga/
# opa/nats/minio at startup; unreachable backend → unhealthy broker).
# -e is intentionally omitted: the script tallies pass/fail across all probes and
# must not abort on the first legitimate failure — exit status is checked at the end.
set -uo pipefail

PASS=0
FAIL=0

ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }

# TCP probe via Node from within agent-gateway.
# Usage: tcp_probe <host> <port> <timeout_ms>
# Prints REACHABLE or UNREACHABLE, returns 0 always (caller checks).
tcp_probe() {
  local host="$1" port="$2" timeout_ms="${3:-3000}"
  docker compose exec -T agent-gateway node -e "
    const net = require('net');
    const sock = net.connect({ host: '${host}', port: ${port} });
    const timer = setTimeout(() => { sock.destroy(); console.log('UNREACHABLE'); process.exit(0); }, ${timeout_ms});
    sock.on('connect', () => { clearTimeout(timer); sock.destroy(); console.log('REACHABLE'); process.exit(0); });
    // 'error' and 'connect' are mutually terminal via process.exit — a post-connect error emission is harmless.
    sock.on('error',   () => { clearTimeout(timer);                  console.log('UNREACHABLE'); process.exit(0); });
  " 2>/dev/null
}

echo "[verify-netseg] network segmentation proof"
echo

echo "-- NEGATIVE: agent-gateway MUST NOT reach backend datastores --"

result=$(tcp_probe postgres 5432)
if [ "$result" = "UNREACHABLE" ]; then
  ok "postgres:5432 UNREACHABLE from agent-gateway (isolated on backend network)"
else
  bad "postgres:5432 REACHABLE from agent-gateway — segmentation NOT enforced"
fi

result=$(tcp_probe vault 8200)
if [ "$result" = "UNREACHABLE" ]; then
  ok "vault:8200 UNREACHABLE from agent-gateway (isolated on backend network)"
else
  bad "vault:8200 REACHABLE from agent-gateway — segmentation NOT enforced"
fi

result=$(tcp_probe minio 9000)
if [ "$result" = "UNREACHABLE" ]; then
  ok "minio:9000 UNREACHABLE from agent-gateway (isolated on backend network)"
else
  bad "minio:9000 REACHABLE from agent-gateway — segmentation NOT enforced"
fi

echo
echo "-- POSITIVE: agent-gateway MUST reach mesh peers --"

result=$(tcp_probe broker 9091)
if [ "$result" = "REACHABLE" ]; then
  ok "broker:9091 REACHABLE from agent-gateway (mesh peer)"
else
  bad "broker:9091 UNREACHABLE from agent-gateway — mesh broken"
fi

result=$(tcp_probe nats 4222)
if [ "$result" = "REACHABLE" ]; then
  ok "nats:4222 REACHABLE from agent-gateway (mesh peer)"
else
  bad "nats:4222 UNREACHABLE from agent-gateway — mesh broken"
fi

echo
echo "[verify-netseg] $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
