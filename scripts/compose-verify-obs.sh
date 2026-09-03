#!/usr/bin/env bash
# Smoke-test the obs pipeline: Tempo has broker traces, Loki logs are attributed
# by container id (not all unknown_service).
#
# Prerequisites: stack up with core + obs profiles; run scripts/compose-verify.sh
# first to exercise the broker and generate at least one trace.
#
# Usage: bash scripts/compose-verify-obs.sh
set -uo pipefail

TEMPO="${TEMPO_URL:-http://localhost:3200}"
LOKI="${LOKI_URL:-http://localhost:3100}"
PROM="${PROM_URL:-http://localhost:9095}"
PASS=0
FAIL=0

ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }

echo "[verify-obs] observability pipeline smoke test"

# ── C1: Tempo — aikonos-broker traces ─────────────────────────────────────────
# Exercise the broker first so there is at least one fresh trace.
echo "  → exercising broker via compose-verify.sh (generates traces)..."
bash "$(dirname "$0")/compose-verify.sh" > /dev/null 2>&1 || true
sleep 5

# Query Tempo search API for service aikonos-broker.
# Tempo search: GET /api/search?tags=service.name%3Daikonos-broker&limit=5
TEMPO_RESP=$(curl -sf --max-time 10 \
  "${TEMPO}/api/search?tags=service.name%3Daikonos-broker&limit=5" 2>/dev/null || echo "")

if [ -z "$TEMPO_RESP" ]; then
  bad "Tempo not reachable at ${TEMPO}"
else
  TRACE_COUNT=$(echo "$TEMPO_RESP" | grep -o '"traceID"' | wc -l | tr -d ' ')
  if [ "$TRACE_COUNT" -gt 0 ]; then
    ok "Tempo has ${TRACE_COUNT} trace(s) for service aikonos-broker"
  else
    bad "Tempo has 0 traces for aikonos-broker — set AIKONOS_OTEL_ENDPOINT=otel-collector:4317 in .env and recreate broker"
  fi
fi

# ── C2: Loki — container-id attribution ──────────────────────────────────────
# Query Loki label values for container_short (or container_id).
# If attribution is working, there should be >1 distinct value (multiple containers).
# Fall back to checking container_id if container_short is absent.

# Query Loki via query_range for streams that carry a non-empty container_short
# label. Loki's label-index endpoint (/label/.../values) may lag new stream
# creation; the query-range API is authoritative.
LOKI_HEALTH=$(curl -sf --max-time 5 "${LOKI}/ready" 2>/dev/null || echo "")
if [ -z "$LOKI_HEALTH" ]; then
  bad "Loki not reachable at ${LOKI}"
else
  START_NS=$(( ($(date +%s) - 300) ))000000000   # last 5 min
  END_NS=$(date +%s)000000000
  LOKI_QRESP=$(curl -sf --max-time 15 \
    --data-urlencode 'query={service_name="unknown_service"} | container_short != ""' \
    --data-urlencode 'limit=20' \
    --data-urlencode "start=${START_NS}" \
    --data-urlencode "end=${END_NS}" \
    "${LOKI}/loki/api/v1/query_range" 2>/dev/null || echo "")

  if [ -z "$LOKI_QRESP" ]; then
    bad "Loki query failed"
  else
    # Count distinct container_short values across returned streams
    DISTINCT=$(echo "$LOKI_QRESP" | grep -o '"container_short":"[a-f0-9]\{12\}"' | sort -u | wc -l | tr -d ' ')
    if [ "$DISTINCT" -gt 1 ]; then
      ok "Loki logs attributed by container_short (${DISTINCT} distinct containers)"
    elif [ "$DISTINCT" -eq 1 ]; then
      bad "Loki has only 1 distinct container_short — expected >1 (only one container logging?)"
    else
      bad "Loki logs not attributed by container_short — check filelog regex_parser in otel-collector-config-file.yaml"
    fi
  fi
fi

# ── C3: Prometheus — broker metric series ────────────────────────────────────
# Query for rpc_server_duration_milliseconds_count with exported_job=aikonos-broker.
# The OTel collector prometheus exporter maps service.name → exported_job (since
# "job" is a reserved Prometheus label); both otelgrpc RPC metrics and runtime
# metrics carry this label. Requires: broker up with AIKONOS_OTEL_ENDPOINT set,
# at least one RPC exercised (compose-verify.sh above does this), and a scrape
# interval (~15s) to have elapsed.
PROM_RESP=$(curl -sf --max-time 10 \
  "${PROM}/api/v1/query" \
  --data-urlencode 'query=rpc_server_duration_milliseconds_count{exported_job="aikonos-broker"}' \
  2>/dev/null || echo "")

if [ -z "$PROM_RESP" ]; then
  bad "Prometheus not reachable at ${PROM}"
else
  SERIES=$(echo "$PROM_RESP" | jq '.data.result | length' 2>/dev/null || echo 0)
  if [ "$SERIES" -gt 0 ]; then
    ok "Prometheus has ${SERIES} broker RPC metric series (rpc_server_duration_milliseconds_count{exported_job=aikonos-broker})"
  else
    bad "Prometheus has 0 broker metric series — ensure AIKONOS_OTEL_ENDPOINT=otel-collector:4317 in .env and broker rebuilt+recreated"
  fi
fi

echo
echo "[verify-obs] $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
