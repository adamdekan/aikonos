#!/usr/bin/env bash
# scripts/tests/nats-auth.test.sh
#
# Live check of deploy/compose/nats/nats.conf (F114) with the nats image and
# the quoted env values compose.yaml uses: no anonymous client; the gateway
# user reads aikonos.audit.> but publishes nothing and reads nothing else; the
# broker user publishes and subscribes. Needs a docker daemon; skips without.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then
  echo "nats-auth.test: SKIP (no docker daemon)"
  exit 0
fi
CONF="$ROOT/deploy/compose/nats/nats.conf"
# A missing bind source would make docker create a root-owned dir in its place.
[ -f "$CONF" ] || { echo "FAIL: $CONF missing"; exit 1; }
IMAGE="$(awk '/^  nats:/ { s = 1 } s && $1 == "image:" { print $2; exit }' "$ROOT/compose.yaml")"
# 1e5… is the hex shape NATS mis-lexes as a number when unquoted.
BPW="1e5$(openssl rand -hex 16)"
GPW="$(openssl rand -hex 16)"

name="aikonos-nats-auth-test-$$"
docker run -d --rm --name "$name" -p 127.0.0.1::4222 \
  -e AIKONOS_NATS_BROKER_PASSWORD="\"$BPW\"" -e AIKONOS_NATS_GATEWAY_PASSWORD="\"$GPW\"" \
  -v "$CONF:/etc/nats/aikonos.conf:ro" \
  "$IMAGE" --config=/etc/nats/aikonos.conf >/dev/null
trap 'docker rm -f "$name" >/dev/null 2>&1' EXIT
port="$(docker port "$name" 4222/tcp | head -1)"
port="${port##*:}"
[ -n "$port" ] || { echo "FAIL: nats did not start:"; docker logs "$name" 2>&1 | tail -5; exit 1; }

# session <user> <pass> <protocol lines>: prints the server's replies up to PONG.
session() {
  exec 3<>"/dev/tcp/127.0.0.1/$port"
  printf 'CONNECT {"verbose":false,"pedantic":false,"user":"%s","pass":"%s"}\r\n%bPING\r\n' "$1" "$2" "$3" >&3
  local line
  while IFS= read -r -t 3 line <&3; do
    echo "$line"
    [[ "$line" == PONG* ]] && break
  done
  exec 3<&-
}
for _ in $(seq 1 50); do
  (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break
  sleep 0.1
done

fail=0
expect() { # <label> <output> <grep -E pattern that must match> [pattern that must not]
  grep -qE "$3" <<<"$2" || { echo "FAIL: $1 (missing /$3/):"; echo "$2"; fail=1; }
  if [ -n "${4:-}" ] && grep -qE "$4" <<<"$2"; then echo "FAIL: $1 (unexpected /$4/):"; echo "$2"; fail=1; fi
}

expect "anonymous client rejected" "$(session "" "" "")" "Authorization Violation"
expect "wrong broker password rejected" "$(session broker "$GPW" "")" "Authorization Violation"
out="$(session gateway "$GPW" 'SUB aikonos.audit.> 1\r\nPUB aikonos.audit.t1 2\r\nhi\r\nSUB aikonos.task.> 2\r\n')"
expect "gateway may not publish" "$out" "Permissions Violation for Publish to \"aikonos.audit.t1\""
expect "gateway reads only the audit stream" "$out" "Permissions Violation for Subscription to \"aikonos.task.>\"" \
  "Subscription to \"aikonos.audit"
expect "gateway session stays up" "$out" "^PONG"
out="$(session broker "$BPW" 'SUB aikonos.task.> 1\r\nPUB aikonos.audit.t1 2\r\nhi\r\n')"
expect "broker publishes and subscribes" "$out" "^PONG" "ERR"

[ "$fail" -eq 0 ] || exit 1
echo "nats-auth.test: OK"
