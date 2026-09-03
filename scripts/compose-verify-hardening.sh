#!/usr/bin/env bash
# Hardening proof: verifies resource limits + read-only rootfs on all compose services.
#
# Run from the repo root with the stack up (task compose:up).
#
# Asserts per service:
#   ALL services:    PidsLimit > 0, Memory > 0  (fork-bomb + resource-exhaustion containment)
#   APP services:    ReadonlyRootfs == true       (assume-breach: can't write payload to own FS)
#   ALL services:    SecurityOpt contains no-new-privileges (re-affirm 3a)
#   ALL services:    CapDrop == [ALL]             (re-affirm 3a)
#
# APP_SERVICES: broker, agent-gateway, webui.
# Infra services that deliberately keep read_only=false are listed in SKIP_READONLY
# with a one-line reason tracked in the script and in compose.yaml.
#
# bash + docker inspect; ok/bad helpers; non-zero exit on any failure.
# Matches scripts/compose-verify-netseg.sh style.
#
# -e is intentionally omitted: script tallies pass/fail across all probes and must
# not abort on the first legitimate failure.
set -uo pipefail

PASS=0
FAIL=0

ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }

# All long-running services (one-shots exit before we run; skip them).
ALL_SERVICES=(
  postgres minio vault nats keycloak opa openfga
  broker agent-gateway webui
  otel-collector grafana loki tempo prometheus
)

# App services that MUST have read_only rootfs.
APP_READONLY=(broker agent-gateway webui)

# Infra/obs services that deliberately keep read_only=false.
# postgres  — writes data to rootfs paths (/var/lib/postgresql); volume-backed + backend-isolated → low marginal value
# minio     — writes data to /data (volume) but also rootfs tmp paths; already backend-isolated
# vault     — dev-mode; writes inmem but image touches rootfs; backend-isolated
# nats      — writes JetStream store to /data/jetstream (rootfs by default); mesh but low marginal value
# keycloak  — writes H2 DB + runtime state to rootfs; mesh but low marginal value
# otel-collector — writes to obs-archive volume but image is distroless+root; obs-isolated
# grafana   — writes session/plugin state to rootfs; obs-isolated
# loki      — writes index/chunk data to rootfs; obs-isolated
# tempo     — writes trace data to rootfs; obs-isolated
# prometheus — writes TSDB to rootfs; obs-isolated
SKIP_READONLY=(postgres minio vault nats keycloak opa openfga otel-collector grafana loki tempo prometheus)

in_array() {
  local needle="$1"; shift
  for item in "$@"; do [[ "$item" == "$needle" ]] && return 0; done
  return 1
}

echo "[verify-hardening] container hardening proof"
echo

echo "-- Resource limits: PidsLimit + Memory on all services --"
for svc in "${ALL_SERVICES[@]}"; do
  cname="aikonos-${svc}-1"
  # Skip services not running (e.g. obs profile not up)
  if ! docker inspect "$cname" &>/dev/null; then
    printf '  \033[33m-\033[0m %s: not running (skip)\n' "$svc"
    continue
  fi

  pids=$(docker inspect "$cname" --format '{{.HostConfig.PidsLimit}}')
  mem=$(docker inspect "$cname" --format '{{.HostConfig.Memory}}')

  if [[ "$pids" -gt 0 ]] 2>/dev/null; then
    ok "${svc}: PidsLimit=${pids} > 0"
  else
    bad "${svc}: PidsLimit=${pids} — no pids limit set"
  fi

  if [[ "$mem" -gt 0 ]] 2>/dev/null; then
    ok "${svc}: Memory=${mem} > 0"
  else
    bad "${svc}: Memory=${mem} — no memory limit set"
  fi
done

echo
echo "-- ReadonlyRootfs on app services (broker, agent-gateway, webui) --"
for svc in "${APP_READONLY[@]}"; do
  cname="aikonos-${svc}-1"
  if ! docker inspect "$cname" &>/dev/null; then
    bad "${svc}: not running"
    continue
  fi
  ro=$(docker inspect "$cname" --format '{{.HostConfig.ReadonlyRootfs}}')
  if [[ "$ro" == "true" ]]; then
    ok "${svc}: ReadonlyRootfs=true"
  else
    bad "${svc}: ReadonlyRootfs=${ro} — read_only not set"
  fi
done


echo
echo "-- ReadonlyRootfs skipped (infra/obs — write to rootfs paths; isolated per H1) --"
for svc in "${SKIP_READONLY[@]}"; do
  cname="aikonos-${svc}-1"
  if ! docker inspect "$cname" &>/dev/null; then
    printf '  \033[33m-\033[0m %s: not running (skip)\n' "$svc"
    continue
  fi
  ro=$(docker inspect "$cname" --format '{{.HostConfig.ReadonlyRootfs}}')
  printf '  \033[33m-\033[0m %s: ReadonlyRootfs=%s (deliberate — see compose.yaml comment)\n' "$svc" "$ro"
done

echo
echo "-- Re-affirm 3a: no-new-privileges + CapDrop=ALL on all services --"
for svc in "${ALL_SERVICES[@]}"; do
  cname="aikonos-${svc}-1"
  if ! docker inspect "$cname" &>/dev/null; then
    printf '  \033[33m-\033[0m %s: not running (skip)\n' "$svc"
    continue
  fi

  secopts=$(docker inspect "$cname" --format '{{json .HostConfig.SecurityOpt}}')
  capdrop=$(docker inspect "$cname" --format '{{json .HostConfig.CapDrop}}')

  if echo "$secopts" | grep -q 'no-new-privileges'; then
    ok "${svc}: no-new-privileges present"
  else
    bad "${svc}: no-new-privileges MISSING (SecurityOpt=${secopts})"
  fi

  if echo "$capdrop" | grep -q '"ALL"'; then
    ok "${svc}: CapDrop=[ALL]"
  else
    bad "${svc}: CapDrop missing ALL (got ${capdrop})"
  fi
done

echo
echo "[verify-hardening] ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
