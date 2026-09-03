#!/usr/bin/env bash
# Seed the OpenFGA store, authorization model, and dev tuples for the docker
# compose deployment, then write the resulting store id into .env so the broker
# leaves dev allow-all stub mode and actually enforces ReBAC.
#
# Compose analogue of scripts/seed-openfga.sh (which targets the k8s cluster).
# Idempotent: the store is located by name (find-or-create); re-running rewrites
# the model + tuples and refreshes the .env store id.
#
# Requires: fga CLI, jq, python3, curl. Run from the repo root with the compose
# stack already up (the openfga HTTP port must be reachable).
set -euo pipefail

STORE_NAME="${STORE_NAME:-aikonos}"
FGA_API_URL="${FGA_API_URL:-http://127.0.0.1:8082}"
MODEL_FILE="policies/fga/model.fga"
TUPLES_FILE="policies/fga/tuples/dev-seed.yaml"
ENV_FILE="${ENV_FILE:-.env}"
export FGA_API_URL

[[ -f "$MODEL_FILE" ]] || { echo "run from repo root ($MODEL_FILE not found)"; exit 1; }
[[ -f "$ENV_FILE" ]]   || { echo "$ENV_FILE not found — run: cp deploy/compose/.env.local.example .env"; exit 1; }

# ── Preflight: fail fast (before any network wait) if a required tool is missing.
for bin in fga jq curl; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    case "$bin" in
      fga) hint="brew install openfga/tap/fga  (or: https://github.com/openfga/cli#installation)" ;;
      jq) hint="brew install jq   |   apt install jq" ;;
      curl) hint="brew install curl   |   apt install curl" ;;
    esac
    echo "[seed] ERROR: required tool '$bin' not found on PATH. Install: $hint" >&2
    exit 1
  fi
done
# python3 + PyYAML are preflighted lazily inside the demo-tuple block below: they
# are only used to reshape dev-seed.yaml, so a store+model-only seed (the default,
# AIKONOS_SEED_DEMO_TUPLES unset — the on-prem/production path) needs neither.

echo "[seed] waiting for OpenFGA at ${FGA_API_URL} ..."
for _ in $(seq 1 30); do
  curl -sf "${FGA_API_URL}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

# Find-or-create the store by name (avoids duplicate stores on re-run).
STORE_ID="$(curl -s "${FGA_API_URL}/stores" | jq -r --arg n "$STORE_NAME" '.stores[]? | select(.name==$n) | .id' | head -n1)"
if [[ -z "$STORE_ID" || "$STORE_ID" == "null" ]]; then
  STORE_ID="$(fga store create --name "$STORE_NAME" | jq -r '.store.id')"
  echo "[seed] created store $STORE_ID"
else
  echo "[seed] reusing store $STORE_ID"
fi

MODEL_ID="$(fga model write --store-id "$STORE_ID" --file "$MODEL_FILE" | jq -r '.authorization_model_id')"
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
  # Reshaping dev-seed.yaml needs python3 + PyYAML — preflight here (not globally)
  # so a store+model-only seed doesn't require them.
  command -v python3 >/dev/null 2>&1 || {
    echo "[seed] ERROR: writing demo tuples needs python3. Install: brew install python3 | apt install python3" >&2; exit 1; }
  if ! python3 -c "import yaml" >/dev/null 2>&1; then
    echo "[seed] ERROR: writing demo tuples needs the python3 'yaml' module. Install: pip install pyyaml | brew install pyyaml | apt install python3-yaml" >&2
    exit 1
  fi
  # dev-seed.yaml wraps the tuple list under `tuples:`; fga wants a flat array.
  # The fga CLI infers input format from the file EXTENSION, so the temp file MUST
  # end in .yaml — a bare `mktemp` name is rejected ("unsupported file format
  # .<rand>"). Use a temp dir + fixed name (portable across GNU/BSD mktemp).
  TUPLES_DIR="$(mktemp -d)"
  TUPLES_TMP="$TUPLES_DIR/tuples.yaml"
  trap 'rm -rf "$TUPLES_DIR"' EXIT
  python3 -c "import yaml; yaml.safe_dump(yaml.safe_load(open('$TUPLES_FILE'))['tuples'], open('$TUPLES_TMP','w'))"
  fga tuple write --store-id "$STORE_ID" --file "$TUPLES_TMP" >/dev/null
  echo "[seed] wrote dev-seed demo tuples (AIKONOS_SEED_DEMO_TUPLES=1)"
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
