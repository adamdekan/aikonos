#!/usr/bin/env bash
set -euo pipefail

# scripts/check-compose-posture.sh
#
# Renders the compose config for each variant (local / local-entra / azure /
# onprem) from its .env example, every profile enabled, and fails on network-exposure
# regressions the source files hide (a plain `ports:` list in an overlay merges
# with the base file's, so only the rendered config tells the truth):
#
#   - azure/onprem publish no host port except Traefik 80/443 and
#     127.0.0.1-bound operator ports
#   - OpenFGA (unauthenticated) is on `backend` only, with no published port
#   - the broker's policy-bundle port 9092 is never published, but the broker
#     binds it beyond loopback (:9092) so OPA's own container can fetch it
#   - docs-mcp is published only on loopback in local, while its container
#     bind (HOST) is 0.0.0.0 so the mesh and that publish can reach it
#   - a gateway that publishes :8090 directly trusts no X-Forwarded-For hop
#   - broker and gateway serve the same AIKONOS_TENANT_ID and AIKONOS_DEV_MODE
#   - broker and gateway mount only their own TLS dir (never ca.key)
#   - dev-ca-mint is digest-pinned with no network; office-worker has an init
#   - NATS requires a token; an unset/blank AIKONOS_DEV_MODE renders false on
#     broker and gateway, and so do the broker's fail-open migration switches
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
  # local and local-entra are the base file alone.
  [[ "$variant" != local* ]] && files+=(-f "deploy/compose/compose.${variant}.yaml")
  docker compose --env-file "$env_file" "${files[@]}" "${PROFILES[@]}" config --format json
}

# jq over the rendered config; prints one finding per line.
violations() {
  jq -r --arg variant "$1" '
    .services as $s
    | ( if ($variant | startswith("local") | not) then
          $s | to_entries[] | .key as $svc | (.value.ports // [])[]
          | select((.host_ip // "") != "127.0.0.1")
          | select(($svc == "traefik") and ((.published|tostring) == "80" or (.published|tostring) == "443") | not)
          | "\($svc) publishes \(.published) on \(.host_ip // "every interface")"
        else empty end ),
      ( if ($s.openfga.networks | keys) != ["backend"] then "openfga networks are \($s.openfga.networks | keys | join(",")), want backend only" else empty end ),
      ( if ($s.openfga.ports // []) != [] then "openfga publishes a host port" else empty end ),
      ( ($s.broker.ports // [])[] | select(.target == 9092) | "broker publishes the policy bundle port 9092" ),
      ( ($s.broker.environment.AIKONOS_POLICY_BUNDLE_HTTP_ADDR // "") as $a
        | if ($a | test("^(0\\.0\\.0\\.0)?:9092$")) then empty
          else "broker binds the policy bundle on \"\($a)\", want :9092 (OPA fetches http://broker:9092 from its own container)" end ),
      ( ($s["aikonos-docs-mcp"].ports // [])[] | select((.host_ip // "") != "127.0.0.1") | "docs-mcp publishes \(.published) beyond loopback" ),
      ( ($s["aikonos-docs-mcp"].environment.HOST // "") as $h
        | if $h == "0.0.0.0" then empty
          else "docs-mcp binds \"\($h)\" in its container, want 0.0.0.0 (mesh peers and the port publish cannot reach loopback)" end ),
      ( $s["agent-gateway"] as $g
        | if any(($g.ports // [])[]; .target == 8090)
             and (["0", "false"] | index($g.environment.AIKONOS_EXTERNAL_TRUST_PROXY // "1") | not)
          then "agent-gateway publishes 8090 directly but trusts X-Forwarded-For (AIKONOS_EXTERNAL_TRUST_PROXY=\($g.environment.AIKONOS_EXTERNAL_TRUST_PROXY // "unset: 1")), want 0"
          else empty end ),
      ( ($s.broker.environment.AIKONOS_TENANT_ID // "") as $t
        | if $t != "" and $t == ($s["agent-gateway"].environment.AIKONOS_TENANT_ID // "") then empty
          else "broker AIKONOS_TENANT_ID is \"\($t)\", want the same non-empty value as agent-gateway (the tenant claim every north call is checked against)" end ),
      ( if $s.broker.environment.AIKONOS_DEV_MODE == $s["agent-gateway"].environment.AIKONOS_DEV_MODE then empty
        else "broker and agent-gateway disagree on AIKONOS_DEV_MODE" end ),
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

for variant in local local-entra azure onprem; do
  env_file="deploy/compose/.env.${variant}.example"
  out="$(render "$variant" "$env_file" | violations "$variant")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && bad "$variant" "$line"
  done <<< "$out"
done

# An omitted or blank AIKONOS_DEV_MODE must mean production, and an omitted
# migration switch must stay off (fail closed).
grep -vE '^AIKONOS_(DEV_MODE|WORKSPACE_ALLOW_UNSIGNED_SESSIONS|VAULT_ALLOW_INSECURE_HTTP)=' \
  deploy/compose/.env.local.example > "$WORKDIR/unset.env"
out="$(render local "$WORKDIR/unset.env" | jq -r '
  .services as $s
  | [["broker", "AIKONOS_DEV_MODE"], ["agent-gateway", "AIKONOS_DEV_MODE"],
     ["broker", "AIKONOS_WORKSPACE_ALLOW_UNSIGNED_SESSIONS"], ["broker", "AIKONOS_VAULT_ALLOW_INSECURE_HTTP"]][]
  | . as [$svc, $key] | ($s[$svc].environment[$key] // "unset")
  | select(. != "false") | "\($svc) \($key) defaults to \"\(.)\" when unset, want false"')"
while IFS= read -r line; do
  [[ -n "$line" ]] && bad local "$line"
done <<< "$out"

if [[ "$fail" -eq 1 ]]; then
  echo "check-compose-posture: FAIL" >&2
  exit 1
fi
echo "check-compose-posture: OK — local/local-entra/azure/onprem rendered configs keep the network posture."
