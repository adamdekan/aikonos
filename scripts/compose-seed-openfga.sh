#!/usr/bin/env bash
# Seed the OpenFGA store, authorization model, and dev tuples for the docker
# compose deployment, then write the resulting store id into .env so the broker
# leaves dev allow-all stub mode and actually enforces ReBAC.
#
# Compose analogue of scripts/seed-openfga.sh (which targets the k8s cluster).
# Idempotent: the store is located by name (find-or-create); re-running rewrites
# the model + tuples and refreshes the .env store id.
#
# The fga CLI runs in compose's `fga-cli` one-off, on the internal backend
# network next to OpenFGA. So OpenFGA needs no host port (current Docker
# releases publish none for a service that is only on an internal network, and
# the azure overlay removes it anyway) and the host needs no fga CLI.
#
# Requires: docker compose and jq. Run from the repo root with the compose
# stack already up.
set -euo pipefail

STORE_NAME="${STORE_NAME:-aikonos}"
MODEL_FILE="policies/fga/model.fga"
TUPLES_FILE="policies/fga/tuples/dev-seed.yaml"
ENV_FILE="${ENV_FILE:-.env}"

[[ -f "$MODEL_FILE" ]] || { echo "run from repo root ($MODEL_FILE not found)"; exit 1; }
[[ -f "$ENV_FILE" ]]   || { echo "$ENV_FILE not found — run: cp deploy/compose/.env.local.example .env"; exit 1; }

# ── Preflight: fail fast (before any network wait) if a required tool is missing.
for bin in docker jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    case "$bin" in
      docker) hint="https://docs.docker.com/engine/install/" ;;
      jq) hint="brew install jq   |   apt install jq   |   winget install jqlang.jq" ;;
    esac
    echo "[seed] ERROR: required tool '$bin' not found on PATH. Install: $hint" >&2
    exit 1
  fi
done

# fga runs the CLI inside the compose network. compose.yaml mounts policies/fga
# at /policies/fga, so file arguments take that path. MSYS_NO_PATHCONV stops Git
# Bash on Windows rewriting those container paths into Windows ones.
fga() { MSYS_NO_PATHCONV=1 docker compose run --rm -T --no-deps fga-cli "$@"; }

echo "[seed] waiting for OpenFGA ..."
for attempt in $(seq 1 30); do
  fga store list >/dev/null 2>&1 && break
  if [[ "$attempt" -eq 30 ]]; then
    echo "[seed] ERROR: OpenFGA did not answer. Is the stack up? (docker compose ps openfga)" >&2
    exit 1
  fi
  sleep 2
done

# Find-or-create the store by name (avoids duplicate stores on re-run).
STORE_ID="$(fga store list | jq -r --arg n "$STORE_NAME" '.stores[]? | select(.name==$n) | .id' | head -n1)"
if [[ -z "$STORE_ID" || "$STORE_ID" == "null" ]]; then
  STORE_ID="$(fga store create --name "$STORE_NAME" | jq -r '.store.id')"
  echo "[seed] created store $STORE_ID"
else
  echo "[seed] reusing store $STORE_ID"
fi

MODEL_ID="$(fga model write --store-id "$STORE_ID" --file "/$MODEL_FILE" | jq -r '.authorization_model_id')"
echo "[seed] wrote model $MODEL_ID"

# dev-seed.yaml contains Keycloak demo accounts (alice@example.com,
# bob@example.com, admin@example.com) that are for LOCAL DEV
# / DEMO only. Seeding them on an on-prem or production environment grants those
# identities real tenant access, so writing them is gated behind an explicit
# opt-in and defaults OFF (fail closed). The dev `task compose:seed` path sets
# AIKONOS_SEED_DEMO_TUPLES=1; on-prem/production must leave it unset and grant
# real Entra OIDs via the admin console or a deployment-specific tuples file.
SEED_DEMO_TUPLES="${AIKONOS_SEED_DEMO_TUPLES:-0}"
if [[ "$SEED_DEMO_TUPLES" == "1" || "$SEED_DEMO_TUPLES" == "true" ]]; then
  # fga lists rejected tuples in its output and still exits 0. A tuple that
  # already exists is a re-run; any other rejection means the file and the
  # model disagree, so stop rather than leave a grant silently missing.
  WRITE_OUT="$(fga tuple write --store-id "$STORE_ID" --file "/$TUPLES_FILE")"
  REJECTED="$(jq -r '.failed[]? | select(.reason | test("already exists") | not)
    | "  \(.tuple_key.user) \(.tuple_key.relation) \(.tuple_key.object): \(.reason)"' <<< "$WRITE_OUT")"
  if [[ -n "$REJECTED" ]]; then
    echo "[seed] ERROR: OpenFGA rejected dev-seed tuples:" >&2
    echo "$REJECTED" >&2
    exit 1
  fi
  echo "[seed] wrote dev-seed demo tuples, $(jq -r '.successful_count' <<< "$WRITE_OUT") new (AIKONOS_SEED_DEMO_TUPLES=1)"
else
  echo "[seed] skipped demo tuples — store + model only (set AIKONOS_SEED_DEMO_TUPLES=1 for local dev/demo)"
fi

# Point the broker at the store via .env (compose substitutes it into the env).
if grep -q '^AIKONOS_POLICY_OPENFGA_STORE_ID=' "$ENV_FILE"; then
  sed -i "s|^AIKONOS_POLICY_OPENFGA_STORE_ID=.*|AIKONOS_POLICY_OPENFGA_STORE_ID=${STORE_ID}|" "$ENV_FILE"
else
  printf '\nAIKONOS_POLICY_OPENFGA_STORE_ID=%s\n' "$STORE_ID" >> "$ENV_FILE"
fi
echo "[seed] set AIKONOS_POLICY_OPENFGA_STORE_ID in ${ENV_FILE}"
echo "[seed] done — store ${STORE_ID}. Recreate the broker to enable enforcement:"
echo "        docker compose up -d broker"
