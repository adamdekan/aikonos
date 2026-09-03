#!/usr/bin/env bash
# Provision the broker's Vault AppRole + least-privilege policy in the compose
# Vault, then write the resulting role_id/secret_id into .env so the broker
# authenticates with a scoped AppRole instead of the all-powerful root token.
#
# Zero-trust: ONLY this seed script touches the root token (to create the policy
# and role). The broker itself never sees it — it logs in with an AppRole bound
# to deploy/compose/vault-broker-policy.hcl, which grants only the broker's own
# KV paths. Local dev's Vault now uses durable `file` storage, so its auth config persists across restart — seeding is
# first-boot-only, not every-restart (safe to re-run any time; it is idempotent).
# The azure/onprem overlays use durable `file` storage — a sealed or uninitialized Vault there is a manual first-boot
# state, not a wipe; this script detects it and points at the runbook instead of
# failing cryptically against auth/policy calls a sealed store can't serve.
#
# Idempotent: enabling approle / writing the policy / writing the role all
# overwrite cleanly, and a fresh secret_id is minted each run.
#
# Requires: docker compose with the `vault` service up. Run from the repo root.
set -euo pipefail

ENV_FILE="${ENV_FILE:-.env}"
POLICY_FILE="deploy/compose/vault-broker-policy.hcl"
POLICY_NAME="aikonos-broker"
ROLE_NAME="aikonos-broker"

[[ -f "$POLICY_FILE" ]] || { echo "run from repo root ($POLICY_FILE not found)"; exit 1; }
[[ -f "$ENV_FILE" ]]    || { echo "$ENV_FILE not found — run: cp deploy/compose/.env.local.example .env"; exit 1; }

# The root token is only used here, to bootstrap the AppRole. Resolution order:
#   1. VAULT_TOKEN env — highest priority. The azure/onprem first-boot procedure
#      (docs/OPS-RUNBOOK.md) passes the `vault operator init` root token this way.
#   2. The durable local-dev token: vault-init persists it to init.env on the
#      vault-data volume. Read it from inside
#      the vault container.
#   3. Legacy dev-mode fallback (kept harmless; no longer exercised once durable).
ROOT_TOKEN="${VAULT_TOKEN:-}"
if [[ -z "$ROOT_TOKEN" ]]; then
  # Exec as the vault user (uid 100): init.env is mode 0600 owned by vault, and
  # the container's cap_drop=ALL strips CAP_DAC_OVERRIDE, so even root cannot
  # read it — only the owning uid can.
  ROOT_TOKEN="$(docker compose exec -T -u vault vault sh -c \
    'test -f /vault/file/init.env && . /vault/file/init.env && printf %s "$VAULT_ROOT_TOKEN"' \
    2>/dev/null || true)"
fi
if [[ -z "$ROOT_TOKEN" ]]; then
  ROOT_TOKEN="$(grep -E '^VAULT_DEV_ROOT_TOKEN_ID=' "$ENV_FILE" | head -n1 | cut -d= -f2-)"
  ROOT_TOKEN="${ROOT_TOKEN:-root-token-local-dev}"
fi

# vex runs the vault CLI inside the vault container, authenticated as root and
# pointed at the in-container listener. -T: no TTY (script context).
vex() {
  docker compose exec -T \
    -e VAULT_ADDR=http://127.0.0.1:8200 \
    -e VAULT_TOKEN="$ROOT_TOKEN" \
    vault vault "$@"
}

echo "[vault-seed] waiting for Vault ..."
status_json=""
status_exit=1
for _ in $(seq 1 30); do
  # -format=json: stable field names, unlike the table renderer's column widths.
  # Guarded form: under `set -e`, a bare `status_json="$(vex status ...)"` would
  # abort the whole script the instant vex returns nonzero (sealed=2,
  # unreachable=1) — the sealed-detection branch below would never run.
  status_json="$(vex status -format=json 2>&1)" && status_exit=0 || status_exit=$?
  # 0 = unsealed (dev-mode default, unchanged). 2 = the listener answered but
  # Vault is sealed or uninitialized — stop waiting either way and diagnose
  # below instead of falling through to auth/policy calls that fail cryptically
  # against a sealed store.
  [[ "$status_exit" -eq 0 || "$status_exit" -eq 2 ]] && break
  sleep 1
done

if [[ "$status_exit" -ne 0 ]]; then
  if grep -q '"initialized": *false' <<<"$status_json"; then
    echo "[vault-seed] Vault is UNINITIALIZED (expected on a fresh durable-storage" \
         "deploy — azure/onprem). Run the first-boot init/unseal procedure, then" \
         "re-run this script: see docs/OPS-RUNBOOK.md \"Vault operations\" section." >&2
    exit 1
  fi
  if grep -q '"sealed": *true' <<<"$status_json"; then
    echo "[vault-seed] Vault is SEALED. Unseal it, then re-run this script:" \
         "see docs/OPS-RUNBOOK.md \"Vault operations\" section." >&2
    exit 1
  fi
  echo "[vault-seed] Vault did not become reachable after 30s:" >&2
  echo "$status_json" >&2
  exit 1
fi

# Enable the KV-v2 secrets engine at secret/. Dev-mode Vault auto-mounts this;
# durable `file` storage (local dev + azure/onprem) does NOT, and the broker's
# whole Vault surface lives under secret/ (broker/*, providers/*, workspaces/*,
# mcp/*) — without this every broker Vault op 403s. Idempotent: skip if present.
if ! vex secrets list 2>/dev/null | grep -qE '^secret/'; then
  vex secrets enable -path=secret kv-v2 >/dev/null
  echo "[vault-seed] enabled kv-v2 secrets engine at secret/"
fi

# Enable the approle auth backend (already-enabled is not an error here).
if ! vex auth list 2>/dev/null | grep -q '^approle/'; then
  vex auth enable approle >/dev/null
  echo "[vault-seed] enabled approle auth backend"
fi

# Write the least-privilege policy from the committed HCL (piped over stdin).
vex policy write "$POLICY_NAME" - < "$POLICY_FILE" >/dev/null
echo "[vault-seed] wrote policy $POLICY_NAME (scoped to broker KV paths)"

# Bind a role to the policy. Tokens are short-lived and renewable; secret_id does
# not expire (dev) so a single seed lasts the Vault lifetime.
vex write "auth/approle/role/${ROLE_NAME}" \
  token_policies="$POLICY_NAME" \
  token_ttl=1h \
  token_max_ttl=4h \
  secret_id_ttl=0 \
  secret_id_num_uses=0 >/dev/null
echo "[vault-seed] wrote role $ROLE_NAME -> policy $POLICY_NAME"

ROLE_ID="$(vex read -field=role_id "auth/approle/role/${ROLE_NAME}/role-id")"
SECRET_ID="$(vex write -f -field=secret_id "auth/approle/role/${ROLE_NAME}/secret-id")"
[[ -n "$ROLE_ID" && -n "$SECRET_ID" ]] || { echo "[vault-seed] failed to mint approle credentials"; exit 1; }

set_env() {
  local key="$1" val="$2"
  if grep -q "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=${val}|" "$ENV_FILE"
  else
    printf '\n%s=%s\n' "$key" "$val" >> "$ENV_FILE"
  fi
}
set_env AIKONOS_VAULT_AUTH_METHOD approle
set_env AIKONOS_VAULT_ROLE_ID   "$ROLE_ID"
set_env AIKONOS_VAULT_SECRET_ID "$SECRET_ID"
echo "[vault-seed] set AIKONOS_VAULT_{AUTH_METHOD,ROLE_ID,SECRET_ID} in ${ENV_FILE}"
echo "[vault-seed] done. Recreate the broker to pick up the AppRole:"
echo "        docker compose up -d broker"
