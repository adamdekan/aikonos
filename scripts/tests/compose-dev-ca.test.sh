#!/usr/bin/env bash
# scripts/tests/compose-dev-ca.test.sh
#
# Regression test for scripts/compose-dev-ca.sh: a re-run over an already
# populated per-service dir it cannot write (dev-ca-mint runs as
# AIKONOS_HOST_UID, the checkout may belong to another user) must not rewrite
# the identical copies, or compose never starts broker and agent-gateway.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "$(id -u)" = 0 ]; then
  echo "compose-dev-ca.test: SKIP (root ignores file modes)"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK"; rm -rf "$WORK"' EXIT
mkdir -p "$WORK/scripts"
cp "$ROOT/scripts/compose-dev-ca.sh" "$WORK/scripts/"
TLS="$WORK/deploy/compose/tls"

bash "$WORK/scripts/compose-dev-ca.sh" >/dev/null 2>&1
for svc in broker agent-gateway; do
  for f in "$svc.crt" "$svc.key" ca.crt; do
    cmp -s "$TLS/$f" "$TLS/$svc/$f" || { echo "FAIL: $svc/$f is not a copy of $f"; exit 1; }
  done
  [ ! -e "$TLS/$svc/ca.key" ] || { echo "FAIL: ca.key copied into $svc/"; exit 1; }
  # Another owner's populated dir: nothing in it is writable by the mint user.
  chmod a-w "$TLS/$svc" "$TLS/$svc"/*
done

if ! out="$(bash "$WORK/scripts/compose-dev-ca.sh" 2>&1)"; then
  echo "FAIL: idempotent re-run over read-only per-service dirs exited non-zero:"
  echo "$out"
  exit 1
fi
echo "compose-dev-ca.test: OK"
