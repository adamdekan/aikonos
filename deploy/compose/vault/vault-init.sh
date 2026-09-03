#!/bin/sh
# deploy/compose/vault/vault-init.sh
#
# Auto-init + auto-unseal for the LOCAL-DEV durable Vault. Runs as the compose `vault-init` one-shot after `vault` has
# started.
#
# Enterprise (azure/onprem) deploys unseal MANUALLY — unseal keys must never
# touch disk there. This sidecar is gated on AIKONOS_VAULT_AUTO_UNSEAL: it exists
# in every compose variant (so broker's depends_on is uniform) but no-ops unless
# the flag is exactly "true", which only the local .env sets.
#
# POSIX sh only, busybox-safe: the hashicorp/vault image ships no jq, so status
# and init output are parsed with awk against Vault's stable table columns.

set -eu

export VAULT_ADDR="${VAULT_ADDR:-http://vault:8200}"
STATE=/vault/file/init.env

if [ "${AIKONOS_VAULT_AUTO_UNSEAL:-false}" != "true" ]; then
  echo "[vault-init] AIKONOS_VAULT_AUTO_UNSEAL != true — manual unseal expected" \
       "(enterprise posture: unseal keys never touch disk). No-op."
  exit 0
fi

# Wait for the listener to answer. `vault status` exits 0 when unsealed and 2
# when sealed/uninitialized — both mean the listener is up, so stop waiting on
# either. Exit 1 (or connection refused) means not yet reachable.
#
# Capture the status code with errexit disabled around the probe: `set -e` would
# abort on the non-zero exit, and an `if`-condition can't be used to read the
# code (a false `if` with no `else` yields exit 0, masking the 2).
echo "[vault-init] waiting for vault listener at ${VAULT_ADDR} ..."
i=0
while true; do
  set +e
  vault status >/dev/null 2>&1
  rc=$?
  set -e
  if [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]; then
    break
  fi
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "[vault-init] vault unreachable after 60s" >&2
    exit 1
  fi
  sleep 1
done

initialized="$(vault status 2>/dev/null | awk '/^Initialized/ {print $2}')"
if [ "$initialized" = "false" ]; then
  echo "[vault-init] Vault is uninitialized — initializing (1 key share, threshold 1)"
  out="$(vault operator init -key-shares=1 -key-threshold=1)"
  key="$(printf '%s\n' "$out" | awk -F': ' '/^Unseal Key 1:/ {print $2}')"
  tok="$(printf '%s\n' "$out" | awk -F': ' '/^Initial Root Token:/ {print $2}')"
  if [ -z "$key" ] || [ -z "$tok" ]; then
    echo "[vault-init] failed to parse init output" >&2
    exit 1
  fi
  # umask 077: the persisted unseal material is dev-only and never leaves the
  # vault-data volume. Strictly no weaker than the retired committed dev root
  # token it replaces.
  ( umask 077; printf 'VAULT_UNSEAL_KEY=%s\nVAULT_ROOT_TOKEN=%s\n' "$key" "$tok" >"$STATE" )
  echo "[vault-init] initialized; unseal material persisted to ${STATE} (dev only)"
fi

if [ ! -f "$STATE" ]; then
  echo "[vault-init] ${STATE} missing but Vault is already initialized — cannot" \
       "auto-unseal. The vault-data volume was likely reset out of band; run" \
       "'docker compose down -v' to start clean." >&2
  exit 1
fi
# shellcheck disable=SC1090
. "$STATE"

sealed="$(vault status 2>/dev/null | awk '/^Sealed/ {print $2}')"
if [ "$sealed" = "true" ]; then
  vault operator unseal "$VAULT_UNSEAL_KEY" >/dev/null
  echo "[vault-init] unsealed"
else
  echo "[vault-init] already unsealed"
fi

echo "[vault-init] ready"
