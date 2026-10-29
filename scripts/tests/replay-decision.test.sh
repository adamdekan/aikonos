#!/usr/bin/env bash
# scripts/tests/replay-decision.test.sh
#
# Sandbox test for scripts/replay-decision.sh: an export is untrusted, and a
# self-consistent one (revision and hashes computed by whoever wrote it) still
# carries Rego that opa evaluates. A snapshot that calls opa.runtime() (the
# process environment) or http.send must fail to replay, not report a MATCH.
# A benign snapshot built the same way must MATCH, so a failure below is the
# sandbox and not a broken fixture.
#
# Usage: bash scripts/tests/replay-decision.test.sh   (needs bash, jq, opa)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REPLAY="${ROOT}/scripts/replay-decision.sh"
OPA_BIN="${OPA_BIN:-opa}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

sha() { sha256sum | cut -d' ' -f1; }

# make_evidence <name> <rego>: a decision export for query aikonos/t whose
# recorded result is {"allow": true}, hashed exactly as the broker does.
make_evidence() {
  local name="$1" rego="$2" snap rev input_sha
  snap="$(jq -n --arg c "$rego" --arg s "$(printf '%s' "$rego" | sha)" \
    '{kind: "aikonos.policy-bundle/v1", roots: ["aikonos"], files: [{path: "aikonos/t.rego", sha256: $s, content: $c}]}')"
  rev="sha256:$(jq -cjS '{files: ([.files[] | {path, sha256}] | sort_by(.path)), roots: (.roots | sort)}' <<<"$snap" | sha)"
  input_sha="sha256:$(jq -cjS -n '{}' | sha)"
  jq -n --argjson snap "$snap" --arg rev "$rev" --arg isha "$input_sha" '{
      kind: "aikonos.decision-evidence/v1",
      event: {event_id: "e1", event_type: "aikonos.broker.policy.decision", context: {
        kind: "test", outcome: {decision: "x", decided_by: "opa:aikonos/t"},
        opa: {query: "aikonos/t", revision: $rev, input: {}, input_sha256: $isha, result: {allow: true}}}},
      policies: [{revision: $rev, verified: true, snapshot: $snap}]
    }' > "${WORKDIR}/${name}.json"
}

pass=0
fail=0

# run_case <name> <expect: 0|nonzero>
run_case() {
  local name="$1" expect="$2" out rc
  set +e
  out="$(bash "$REPLAY" "${WORKDIR}/${name}.json" --opa "$OPA_BIN" 2>&1)"
  rc=$?
  set -e
  if { [ "$expect" = "0" ] && [ "$rc" -eq 0 ] && grep -q '^MATCH:' <<<"$out"; } \
    || { [ "$expect" != "0" ] && [ "$rc" -ne 0 ] && ! grep -q '^MATCH:' <<<"$out"; }; then
    echo "PASS: $name (exit=$rc)"
    pass=$((pass + 1))
  else
    echo "FAIL: $name (exit=$rc, expected $expect)"
    echo "$out"
    fail=$((fail + 1))
  fi
}

make_evidence benign 'package aikonos.t

import future.keywords.if

allow if { true }
'
run_case benign 0

make_evidence runtime 'package aikonos.t

import future.keywords.if

allow if { is_object(opa.runtime()) }
'
run_case runtime nonzero

make_evidence http-send 'package aikonos.t

import future.keywords.if

allow if { is_object(http.send({"method": "GET", "url": "http://127.0.0.1:1/", "raise_error": false, "timeout": "1s"})) }
'
run_case http-send nonzero

make_evidence net-lookup 'package aikonos.t

import future.keywords.if

allow if { is_set(net.lookup_ip_addr("localhost")) }
'
run_case net-lookup nonzero

echo ""
echo "=== summary: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
