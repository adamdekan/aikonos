#!/usr/bin/env bash
set -euo pipefail

# scripts/check-compose-posture.sh
#
# Renders the compose config for each variant (local / azure / onprem) from
# its .env example, every profile enabled, and fails on network-exposure
# regressions the source files hide (a plain `ports:` list in an overlay merges
# with the base file's, so only the rendered config tells the truth):
#
#   - azure/onprem publish no host port except Traefik 80/443 and
#     127.0.0.1-bound operator ports
#   - OpenFGA (unauthenticated) is on `backend` only, with no published port
#   - the broker's policy-bundle port 9092 is never published; docs-mcp only
#     on loopback in local
#   - broker and gateway mount only their own TLS dir (never ca.key)
#   - dev-ca-mint is digest-pinned with no network; office-worker has an init
#   - NATS requires a token; an unset/blank AIKONOS_DEV_MODE renders false
#
# Needs jq and the docker compose CLI (config rendering only, no daemon calls).
# Usage: scripts/check-compose-posture.sh [--repo-root DIR]

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${1:-}" == "--repo-root" ]]; then
  ROOT="$(cd "$2" && pwd)"
fi
cd "$ROOT"

PROFILES=(--profile core --profile obs --profile docs-mcp --profile dev --profile mcp-grafana)
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail=0
bad() { echo "POSTURE [$1] $2" >&2; fail=1; }

render() {
  local variant="$1" env_file="$2"
  local files=(-f compose.yaml)
  [[ "$variant" != local ]] && files+=(-f "deploy/compose/compose.${variant}.yaml")
  docker compose --env-file "$env_file" "${files[@]}" "${PROFILES[@]}" config --format json
}

# jq over the rendered config; prints one finding per line.
violations() {
  jq -r --arg variant "$1" '
    .services as $s
    | ( if $variant != "local" then
          $s | to_entries[] | .key as $svc | (.value.ports // [])[]
          | select((.host_ip // "") != "127.0.0.1")
          | select(($svc == "traefik") and ((.published|tostring) == "80" or (.published|tostring) == "443") | not)
          | "\($svc) publishes \(.published) on \(.host_ip // "every interface")"
        else empty end ),
      ( if ($s.openfga.networks | keys) != ["backend"] then "openfga networks are \($s.openfga.networks | keys | join(",")), want backend only" else empty end ),
      ( if ($s.openfga.ports // []) != [] then "openfga publishes a host port" else empty end ),
      ( ($s.broker.ports // [])[] | select(.target == 9092) | "broker publishes the policy bundle port 9092" ),
      ( ($s["aikonos-docs-mcp"].ports // [])[] | select((.host_ip // "") != "127.0.0.1") | "docs-mcp publishes \(.published) beyond loopback" ),
      ( ["broker", "agent-gateway"][] as $svc
        | ($s[$svc].volumes // [])[] | select(.target == "/tls")
        | select(.source | endswith("/deploy/compose/tls/" + $svc) | not)
        | "\($svc) mounts \(.source) at /tls, want deploy/compose/tls/\($svc) (keeps ca.key out)" ),
      ( if ($s["dev-ca-mint"].image | contains("@sha256:")) then empty else "dev-ca-mint image is not digest-pinned" end ),
      ( if $s["dev-ca-mint"].network_mode == "none" then empty else "dev-ca-mint has network access" end ),
      ( if $s["office-worker"].init == true then empty else "office-worker runs without init (zombie reaping)" end ),
      ( if ($s.nats.command | map(select(startswith("--auth="))) | length) == 1 then empty else "nats runs without --auth" end )
  '
}

for variant in local azure onprem; do
  env_file="deploy/compose/.env.${variant}.example"
  out="$(render "$variant" "$env_file" | violations "$variant")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && bad "$variant" "$line"
  done <<< "$out"
done

# An omitted or blank AIKONOS_DEV_MODE must mean production (fail closed).
grep -v '^AIKONOS_DEV_MODE=' deploy/compose/.env.local.example > "$WORKDIR/no-dev-mode.env"
dev_mode="$(render local "$WORKDIR/no-dev-mode.env" | jq -r '.services.broker.environment.AIKONOS_DEV_MODE')"
[[ "$dev_mode" == "false" ]] || bad local "AIKONOS_DEV_MODE defaults to '$dev_mode' when unset, want false"

if [[ "$fail" -eq 1 ]]; then
  echo "check-compose-posture: FAIL" >&2
  exit 1
fi
echo "check-compose-posture: OK — local/azure/onprem rendered configs keep the network posture."
