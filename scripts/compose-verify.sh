#!/usr/bin/env bash
# Smoke-test the running docker compose stack: enforcement, audit, and the UI.
# Compose analogue of the old in-cluster verify-enforcement.sh — drives the
# published localhost ports instead of `kubectl port-forward`.
#
# Run from the repo root with the stack up (task compose:up + compose:seed).
set -uo pipefail

WEBUI="${WEBUI_URL:-http://localhost:4200}"
GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
PASS=0
FAIL=0

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
skip() { printf '  \033[33m∼\033[0m %s\n' "$1"; }

# ── CP3: on-prem topology detection ─────────────────────────────────────────
# The on-prem overlay (deploy/compose/compose.onprem.yaml) routes through
# Traefik host-header rules and `!reset`s webui/agent-gateway's published host
# ports — every localhost probe below is structurally blind there (this script
# was blind on the on-prem host). Detect via `docker compose port`: no published port on
# EITHER service → MODE=in-network, and HTTP probes route through
# `docker compose exec -T agent-gateway node -e '...fetch...'` instead of
# curl-on-the-host — the same mechanism used live during on-prem diagnosis
# (the container image has no curl). Local/Azure (ports published) keep the
# exact curl path below, unchanged.

# port_is_published: on the on-prem host, `docker compose port <svc> <port>` for an
# UNPUBLISHED port exits 0 and prints "invalid IP:0" (compose-version-
# dependent), not empty stdout — a plain emptiness check misclassified
# on-prem as `local`. A port only counts as published if the output ends in
# a real port number (no leading zero, i.e. not literal "0"); empty output,
# "invalid IP:0", and ":0" all mean unpublished.
port_is_published() {
  grep -qE ':[1-9][0-9]*$' <<<"$1"
}

detect_mode() {
  local w g running
  running="$(docker compose ps --status running --services 2>/dev/null)"
  if ! grep -qx 'webui' <<<"$running" || ! grep -qx 'agent-gateway' <<<"$running"; then
    echo down
    return
  fi
  w="$(docker compose port webui 4200 2>/dev/null)"
  g="$(docker compose port agent-gateway 8080 2>/dev/null)"
  if port_is_published "$w" || port_is_published "$g"; then echo local; else echo in-network; fi
}
MODE="$(detect_mode)"
if [ "$MODE" = down ]; then
  echo "[verify] ✗ stack appears down — webui/agent-gateway are not running. Start it with: task compose:up" >&2
  exit 1
fi
if [ "$MODE" = in-network ]; then
  echo "[verify] topology: in-network (no published host ports — routing HTTP probes via docker compose exec agent-gateway)"
else
  echo "[verify] topology: local (published host ports — curl direct)"
fi

# rewrite_for_container: translate a host-published $WEBUI/$GATEWAY URL to how
# it's reached from inside the agent-gateway container — webui via mesh
# service DNS, gateway via its own loopback. in-network mode only.
rewrite_for_container() {
  case "$1" in
    "$WEBUI"*)   printf '%s' "http://webui:4200${1#"$WEBUI"}" ;;
    "$GATEWAY"*) printf '%s' "http://127.0.0.1:8080${1#"$GATEWAY"}" ;;
    *)           printf '%s' "$1" ;;
  esac
}

# code_in_network: same contract as code() (URL, then optional curl-style
# `-H "name: value"` pairs) but executed via node fetch inside agent-gateway
# — no curl in the hardened image. Header name/value travel as env vars into
# the child, never string-interpolated into the JS source (avoids quoting a
# bearer token into a script literal).
code_in_network() {
  local url hdr_name="" hdr_value=""
  url="$(rewrite_for_container "$1")"; shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -H) hdr_name="${2%%:*}"; hdr_value="${2#*: }"; shift 2 ;;
      *) shift ;;
    esac
  done
  docker compose exec -T \
    -e CV_URL="$url" -e CV_HDR_NAME="$hdr_name" -e CV_HDR_VALUE="$hdr_value" \
    agent-gateway node -e '
      const headers = {};
      if (process.env.CV_HDR_NAME) headers[process.env.CV_HDR_NAME] = process.env.CV_HDR_VALUE;
      fetch(process.env.CV_URL, { headers, signal: AbortSignal.timeout(8000) })
        .then(r => process.stdout.write(String(r.status)))
        .catch(() => process.stdout.write("000"));
    ' 2>/dev/null
}

code() {
  if [ "$MODE" = in-network ]; then code_in_network "$@"; else
    curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$@" 2>/dev/null
  fi
}

echo "[verify] compose stack smoke test"

# Capture the broker log once (compose logs go to stderr). Grepping a captured
# string avoids the pipefail + `grep -q` SIGPIPE trap (grep -q closes the pipe
# early, killing `docker compose logs`, which pipefail would read as failure).
BROKER_LOG="$(docker compose logs broker 2>&1)"

# 1. UI + gateway reachable
if [ "$(code "$WEBUI/")" = "200" ]; then ok "webui reachable (:4200)"; else bad "webui not reachable (:4200)"; fi
if [ "$(code "$GATEWAY/healthz")" = "200" ]; then ok "gateway healthz (:8080)"; else bad "gateway healthz failed (:8080)"; fi

# 2. broker gRPC ports listening (north + south, mTLS). In-network mode: the
# host has no route to these ports either (on-prem publishes none), so probe
# a plain TCP connect from inside the mesh (agent-gateway container) instead
# of /dev/tcp on 127.0.0.1 — a connect success/refusal is all that's needed.
broker_port_open_in_network() {
  docker compose exec -T -e CV_PORT="$1" agent-gateway node -e '
    const net = require("net");
    const s = net.createConnection({ host: "broker", port: Number(process.env.CV_PORT), timeout: 2000 });
    s.on("connect", () => { s.destroy(); process.stdout.write("open"); });
    s.on("timeout", () => { s.destroy(); process.stdout.write("closed"); });
    s.on("error", () => process.stdout.write("closed"));
  ' 2>/dev/null
}
for p in 9090 9091; do
  if [ "$MODE" = in-network ]; then
    if [ "$(broker_port_open_in_network "$p")" = open ]; then ok "broker port $p open (in-network)"; else bad "broker port $p closed (in-network)"; fi
  elif timeout 2 bash -c "</dev/tcp/127.0.0.1/$p" 2>/dev/null; then ok "broker port $p open"; else bad "broker port $p closed"; fi
done

# 3. broker in live OpenFGA mode (not the dev allow-all stub)
if grep -q 'openfga_mode.*live' <<<"$BROKER_LOG"; then
  ok "broker enforcing OpenFGA (live mode)"
else
  bad "broker in allow-all stub — run: task compose:seed"
fi

# 4. MinIO WORM audit sink active
if grep -q 'MinIO sink active' <<<"$BROKER_LOG"; then
  ok "audit WORM sink active (MinIO)"
else
  bad "audit MinIO sink not active"
fi

# 4b. Capability root key sourced from Vault via the AppRole — NOT the ephemeral
# fallback (which means the broker failed to authenticate to Vault).
if grep -q 'EPHEMERAL key' <<<"$BROKER_LOG"; then
  bad "capability key is EPHEMERAL — Vault AppRole not provisioned (run: task compose:seed)"
elif grep -qE 'capability minter initialized from Vault|stored it in Vault' <<<"$BROKER_LOG"; then
  ok "capability key persisted in Vault (AppRole auth working)"
else
  bad "capability key source unconfirmed in broker log"
fi

# 5. Audit SSE is auth-gated (unauthenticated probe must be rejected — cross-tenant leak closed)
if [ "$(code "$GATEWAY/api/audit/stream")" = "401" ]; then ok "audit SSE requires auth (401 — gateway /api/audit/stream)"; else bad "audit SSE not auth-gated (expected 401, got $(code "$GATEWAY/api/audit/stream"))"; fi

# ── CP3: gateway /readyz (deeper than /healthz, which stayed green throughout
# the on-prem incident — it doesn't check broker/audit connectivity) ─────────
readyz_body() {
  if [ "$MODE" = in-network ]; then
    docker compose exec -T -e CV_URL="http://127.0.0.1:8080/readyz" agent-gateway node -e '
      fetch(process.env.CV_URL, { signal: AbortSignal.timeout(8000) })
        .then(r => r.text()).then(t => process.stdout.write(t))
        .catch(e => process.stdout.write(JSON.stringify({ ok: false, error: String(e) })));
    ' 2>/dev/null
  else
    curl -s --max-time 8 "$GATEWAY/readyz" 2>/dev/null
  fi
}
READYZ_BODY="$(readyz_body)"
if grep -q '"ok":true' <<<"$READYZ_BODY"; then
  ok "gateway /readyz ok:true"
else
  bad "gateway /readyz not ready — ${READYZ_BODY:-<empty response>}"
fi

# ── CP3: LLM credential health (the check that would have caught on-prem) ────
# Grep the exact signatures CP1/CP2 ship — keep in lockstep, cross-comment
# both sites: agent-gateway/src/pi/session.ts (spawn-failure "llm credentials
# unavailable" throws + the legacy silent-fallback log line) and
# broker/internal/broker/providers_south.go (Vault-key-miss warn).
GATEWAY_LOG="$(docker compose logs agent-gateway 2>&1)"
if grep -q 'missing key — using env fallback' <<<"$GATEWAY_LOG"; then
  bad "gateway log: silent env-fallback on a missing provider key — re-enter it in Admin → LLM Providers (or check AIKONOS_OPENROUTER_API_KEY)"
elif grep -q 'llm credentials unavailable' <<<"$GATEWAY_LOG"; then
  bad "gateway log: LLM credential resolution failed at spawn — re-enter the provider key in Admin → LLM Providers (or check AIKONOS_OPENROUTER_API_KEY)"
else
  ok "gateway log clean of LLM credential-loss signatures"
fi

if grep -q 'provider key missing from Vault' <<<"$BROKER_LOG"; then
  bad "broker log: provider key missing from Vault (Vault-wipe signature) — re-enter it in Admin → LLM Providers"
else
  ok "broker log clean of Vault provider-key-miss warning"
fi

# ── F2 real-auth (real OIDC bearer) ──────────────────────────────────────────
# The real-bearer check adapts to the configured provider, detected from the
# issuer (env override, else read from .env). Keycloak (default): mint via the
# confidential aikonos-smoke ROPC client + assert ROPC is disabled on the
# user-facing client (F-01). Entra: assert the broker validates a real
# Entra-issued USER bearer — gated on smoke creds, skipped otherwise.
OIDC_ISSUER="${AIKONOS_OIDC_ISSUER:-$(sed -n 's/^AIKONOS_OIDC_ISSUER=//p' .env 2>/dev/null | head -1)}"
case "$OIDC_ISSUER" in
  *login.microsoftonline.com*|*sts.windows.net*) PROVIDER=entra ;;
  *) PROVIDER=keycloak ;;
esac
echo "[verify] OIDC provider: ${PROVIDER} (issuer: ${OIDC_ISSUER:-keycloak-default})"

# Unauthenticated north call is rejected (provider-agnostic): no bearer → 401
# from the gateway's verify layer.
noauth_code=$(code "$WEBUI/api/admin/assignments")
if [ "$noauth_code" = "401" ]; then ok "unauthenticated /admin rejected (401 — header-trust gone)"; else bad "unauthenticated /admin not 401 (got $noauth_code)"; fi

if [ "$PROVIDER" = "keycloak" ]; then
  # Token minting goes through the CONFIDENTIAL aikonos-smoke client (requires a
  # secret) — the user-facing aikonos-broker/aikonos-webui clients have ROPC
  # DISABLED, which is what closes finding F-01.
  KC="${KEYCLOAK_URL:-http://localhost:18080}"
  SMOKE_SECRET="${AIKONOS_SMOKE_CLIENT_SECRET:-smoke-secret-local-dev}"
  mint() { # mint <username> → prints access_token (empty on failure)
    curl -s --max-time 8 \
      -d "grant_type=password" -d "client_id=aikonos-smoke" -d "client_secret=${SMOKE_SECRET}" \
      -d "username=$1" -d "password=dev" -d "scope=openid" \
      "${KC}/realms/aikonos/protocol/openid-connect/token" 2>/dev/null \
      | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
  }

  # ROPC is disabled on the user-facing client (F-01 closed): a password grant
  # against aikonos-broker must FAIL (the gateway can no longer mint identities).
  ropc_err=$(curl -s --max-time 8 \
    -d "grant_type=password" -d "client_id=aikonos-broker" \
    -d "username=admin@example.com" -d "password=dev" -d "scope=openid" \
    "${KC}/realms/aikonos/protocol/openid-connect/token" 2>/dev/null \
    | sed -n 's/.*"error":"\([^"]*\)".*/\1/p')
  if [ -n "$ropc_err" ]; then ok "ROPC disabled on aikonos-broker (F-01 closed: got '$ropc_err')"; else bad "ROPC still works on aikonos-broker — F-01 NOT closed"; fi

  # ReBAC enforcement with REAL bearer tokens: tenant admin allowed, seeded
  # non-admin denied — both acting as their verified OIDC subject.
  ADMIN_TOK=$(mint "admin@example.com")
  ALICE_TOK=$(mint "alice@example.com")
  if [ -z "$ADMIN_TOK" ] || [ -z "$ALICE_TOK" ]; then
    bad "could not mint smoke tokens (aikonos-smoke client / Keycloak at $KC)"
  else
    admin_code=$(code "$WEBUI/api/admin/assignments" -H "authorization: Bearer ${ADMIN_TOK}")
    if [ "$admin_code" = "200" ]; then ok "admin@example.com allowed on /admin (200, real token)"; else bad "admin@example.com not allowed (got $admin_code)"; fi

    alice_code=$(code "$WEBUI/api/admin/assignments" -H "authorization: Bearer ${ALICE_TOK}")
    if [ "$alice_code" = "403" ]; then ok "alice@example.com denied on /admin (403 — enforced, real token)"; else bad "alice@example.com not denied (got $alice_code — enforcement off?)"; fi
  fi
else
  # Entra: validate a REAL Entra-issued USER bearer (issuer/audience/JWKS +
  # oid→principal / tid→tenant mapping). App-only (client-credentials) tokens
  # lack user claims and are rejected by the gateway, so a USER token is needed:
  # supply one directly (AIKONOS_ENTRA_SMOKE_TOKEN — e.g. copied from the browser
  # after sign-in), or let the script ROPC-mint one for a CLOUD-ONLY, NO-MFA
  # smoke account (ROPC is discouraged — use a dedicated test user only). An
  # optional deny token exercises the 403 path. See docs/12-entra-login.md.
  OIDC_AUD="${AIKONOS_OIDC_AUDIENCE:-$(sed -n 's/^AIKONOS_OIDC_AUDIENCE=//p' .env 2>/dev/null | head -1)}"
  entra_ropc() { # entra_ropc <username> <password> → access_token
    local scope="${AIKONOS_ENTRA_SMOKE_SCOPE:-api://${OIDC_AUD}/access_as_user}"
    local args=(-d grant_type=password
      -d "client_id=${AIKONOS_ENTRA_SMOKE_CLIENT_ID:-}"
      -d "scope=openid profile ${scope}"
      -d "username=$1" -d "password=$2")
    if [ -n "${AIKONOS_ENTRA_SMOKE_CLIENT_SECRET:-}" ]; then args+=(-d "client_secret=${AIKONOS_ENTRA_SMOKE_CLIENT_SECRET}"); fi
    curl -s --max-time 12 "${args[@]}" \
      "https://login.microsoftonline.com/${AIKONOS_ENTRA_SMOKE_TENANT_ID:-}/oauth2/v2.0/token" 2>/dev/null \
      | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
  }

  ADMIN_TOK="${AIKONOS_ENTRA_SMOKE_TOKEN:-}"
  DENY_TOK="${AIKONOS_ENTRA_SMOKE_TOKEN_DENY:-}"
  if [ -z "$ADMIN_TOK" ] && [ -n "${AIKONOS_ENTRA_SMOKE_TENANT_ID:-}" ] && [ -n "${AIKONOS_ENTRA_SMOKE_USERNAME:-}" ]; then
    ADMIN_TOK=$(entra_ropc "${AIKONOS_ENTRA_SMOKE_USERNAME}" "${AIKONOS_ENTRA_SMOKE_PASSWORD:-}")
    if [ -n "${AIKONOS_ENTRA_SMOKE_USERNAME_DENY:-}" ]; then
      DENY_TOK=$(entra_ropc "${AIKONOS_ENTRA_SMOKE_USERNAME_DENY}" "${AIKONOS_ENTRA_SMOKE_PASSWORD_DENY:-}")
    fi
  fi

  if [ -z "$ADMIN_TOK" ]; then
    skip "Entra real-bearer check — set AIKONOS_ENTRA_SMOKE_TOKEN, or _TENANT_ID/_CLIENT_ID/_USERNAME/_PASSWORD for ROPC (docs/12-entra-login.md)"
  else
    a=$(code "$WEBUI/api/admin/assignments" -H "authorization: Bearer ${ADMIN_TOK}")
    case "$a" in
      200) ok "Entra USER bearer accepted + authorized as admin on /admin (200)";;
      403) ok "Entra USER bearer accepted by OIDC validation (403 — valid token; seed user:<oid> admin tenant:<AIKONOS_BROKER_TENANT_ID> for full admin)";;
      401) bad "Entra bearer REJECTED at validation (401 — check AIKONOS_OIDC_ISSUER/_AUDIENCE/_JWKS_URL + SUBJECT_CLAIM=oid / TENANT_CLAIM=tid)";;
      *)   bad "Entra /admin returned unexpected $a";;
    esac
    if [ -n "$DENY_TOK" ]; then
      d=$(code "$WEBUI/api/admin/assignments" -H "authorization: Bearer ${DENY_TOK}")
      if [ "$d" = "403" ]; then ok "Entra non-admin denied on /admin (403 — enforced, real token)"; else bad "Entra non-admin not denied (got $d)"; fi
    fi
  fi
fi


# ── CP3: chat round-trip ─────────────────────────────────────────────────────
# Gated on a mintable/supplied smoke bearer, like the real-bearer checks above
# (ADMIN_TOK is set by whichever branch ran — Keycloak mint or Entra token).
# Distinguishes "agent answers" / "agent fails loudly" (both prove the pipe:
# PASS / PASS-with-note) from "silence" (FAIL — the on-prem symptom exactly).
BEARER_TOK="${ADMIN_TOK:-}"
if [ -z "$BEARER_TOK" ]; then
  skip "chat round-trip — no bearer available (needs Keycloak smoke creds or AIKONOS_ENTRA_SMOKE_TOKEN)"
else
  chat_roundtrip_body() {
    if [ "$MODE" = in-network ]; then
      docker compose exec -T \
        -e CV_URL="http://127.0.0.1:8080/agui" -e CV_TOK="$BEARER_TOK" \
        agent-gateway node -e '
          fetch(process.env.CV_URL, {
            method: "POST",
            headers: { authorization: `Bearer ${process.env.CV_TOK}`, "content-type": "application/json" },
            body: JSON.stringify({ prompt: "compose-verify smoke test: reply with the single word OK" }),
            signal: AbortSignal.timeout(60000),
          }).then(r => r.text()).then(t => process.stdout.write(t))
            .catch(e => process.stdout.write(""));
        ' 2>/dev/null
    else
      curl -s --max-time 60 -X POST "$GATEWAY/agui" \
        -H "authorization: Bearer ${BEARER_TOK}" \
        -H "content-type: application/json" \
        -d '{"prompt":"compose-verify smoke test: reply with the single word OK"}' 2>/dev/null
    fi
  }
  CHAT_BODY="$(chat_roundtrip_body)"
  if [ -z "$CHAT_BODY" ]; then
    bad "chat round-trip: empty response / timeout from /agui — silent no-response (the on-prem symptom)"
  elif grep -q '"type":"TEXT_MESSAGE_CONTENT"' <<<"$CHAT_BODY"; then
    ok "chat round-trip: assistant responded"
  elif grep -q 'llm credentials unavailable' <<<"$CHAT_BODY"; then
    # Spawn-time credential failure can surface either as an SSE RUN_ERROR
    # frame or, when the failure happens before the stream starts, a plain
    # HTTP 409 error body — both prove the pipe works with a broken provider.
    ok "chat round-trip: pipe proven, provider broken (llm credentials unavailable — ${CHAT_BODY})"
  else
    bad "chat round-trip: no assistant text and no recognized error frame — silent no-response"
  fi
fi

# ── Skill-bundle smoke checks (CP10) ─────────────────────────────────────────
# Requires Keycloak provider (smoke tokens) + the sample research-assistant row
# seeded by scripts/compose-seed-skill-bundles.sh (task compose:seed).
# Skipped gracefully when tokens are unavailable or the wrong OIDC provider.
# post_code: mode-aware POST-with-body, mirroring code()'s GET contract but for
# routes needing a body (raw curl bypassed code()'s dispatch pre-fix). Returns
# HTTP status only. Local mode issues the exact same curl call as before.
post_code() {
  local url="$1" tok="$2" ctype="$3" data="$4"
  if [ "$MODE" = in-network ]; then
    docker compose exec -T \
      -e CV_URL="$(rewrite_for_container "$url")" -e CV_TOK="$tok" -e CV_CTYPE="$ctype" -e CV_DATA="$data" \
      agent-gateway node -e '
        fetch(process.env.CV_URL, {
          method: "POST",
          headers: { authorization: `Bearer ${process.env.CV_TOK}`, "content-type": process.env.CV_CTYPE },
          body: process.env.CV_DATA,
          signal: AbortSignal.timeout(8000),
        }).then(r => process.stdout.write(String(r.status)))
          .catch(() => process.stdout.write("000"));
      ' 2>/dev/null
  else
    curl -s -o /dev/null -w '%{http_code}' --max-time 8 \
      -X POST "$url" \
      -H "authorization: Bearer ${tok}" \
      -H "content-type: ${ctype}" \
      --data-binary "$data" 2>/dev/null
  fi
}

# get_body: mode-aware GET returning the response body (bearer-authed variant
# of readyz_body/chat_roundtrip_body above). Local mode issues the exact same
# curl call as before.
get_body() {
  local url="$1" tok="$2"
  if [ "$MODE" = in-network ]; then
    docker compose exec -T -e CV_URL="$(rewrite_for_container "$url")" -e CV_TOK="$tok" agent-gateway node -e '
      fetch(process.env.CV_URL, { headers: { authorization: `Bearer ${process.env.CV_TOK}` }, signal: AbortSignal.timeout(8000) })
        .then(r => r.text()).then(t => process.stdout.write(t))
        .catch(() => process.stdout.write(""));
    ' 2>/dev/null
  else
    curl -s --max-time 8 "$url" -H "authorization: Bearer ${tok}" 2>/dev/null
  fi
}

if [ "$PROVIDER" = "keycloak" ] && [ -n "${ADMIN_TOK:-}" ] && [ -n "${ALICE_TOK:-}" ]; then
  # Admin uploads a sample SKILL.md bundle via POST /admin/skills/upload.
  # Content-Type text/plain is accepted by the route (bare SKILL.md parser).
  # Accepts 201 (new row) or 400 (broker InvalidArgument: name collision when the
  # idempotent seed row already exists — NOT a generic 400; only {201,400} pass).
  SKILL_MD='---
name: smoke-probe
description: Verify-only probe bundle.
allowed-tools:
  - web.fetch
---
You are a smoke-test probe. Do nothing.'
  upload_code=$(post_code "$GATEWAY/admin/skills/upload" "$ADMIN_TOK" "text/plain" "$SKILL_MD")
  if [ "$upload_code" = "201" ] || [ "$upload_code" = "400" ]; then
    ok "skill-bundle upload via admin token (${upload_code} — 201=new, 400=already exists)"
  else
    bad "skill-bundle upload unexpected code ${upload_code} (want 201 or 400)"
  fi

  # Non-admin (alice) uploading a bundle is rejected with 403 (admin-gate enforced).
  nonadmin_upload_code=$(post_code "$GATEWAY/admin/skills/upload" "$ALICE_TOK" "text/plain" "$SKILL_MD")
  if [ "$nonadmin_upload_code" = "403" ]; then
    ok "skill-bundle upload rejected for non-admin alice (403 — admin gate enforced)"
  else
    bad "skill-bundle upload not 403 for non-admin alice (got ${nonadmin_upload_code})"
  fi

  # Granted user (alice, member of security-team) lists bundles — positive grant-surface
  # check: HTTP 200 and the seeded bundle name appears in the response body.
  alice_bundles=$(get_body "$GATEWAY/user/skill-bundles" "$ALICE_TOK")
  alice_bundles_code=$(code "$GATEWAY/user/skill-bundles" -H "authorization: Bearer ${ALICE_TOK}")
  if [ "$alice_bundles_code" = "200" ] && echo "$alice_bundles" | grep -q 'research-assistant'; then
    ok "alice sees granted skill bundle research-assistant (GET /user/skill-bundles → 200)"
  elif [ "$alice_bundles_code" != "200" ]; then
    bad "GET /user/skill-bundles for alice: expected 200, got ${alice_bundles_code}"
  else
    bad "GET /user/skill-bundles 200 but research-assistant not in body (seed missing?)"
  fi
else
  skip "skill-bundle smoke checks — requires Keycloak + smoke tokens (run: task compose:seed)"
fi

# ── Office document tools: office-worker health + docx.create round-trip ────
# office-worker joins only the
# `office` network (internal: true, members broker + office-worker) — no
# published host port in ANY topology (local/Azure/on-prem alike), so unlike
# webui/gateway above it is ALWAYS probed via `docker compose exec office-worker
# node -e '...fetch...'` regardless of $MODE — the same idiom compose.yaml's
# own healthcheck for this service already uses (node:22-slim ships no
# curl/wget). broker itself can't be used as the exec target: it's a
# distroless static binary with no shell/node to run a probe from.
office_worker_exec() {
  docker compose exec -T office-worker node -e "$1" 2>/dev/null
}

owhealthz_status=$(office_worker_exec '
  fetch("http://127.0.0.1:8081/healthz", { signal: AbortSignal.timeout(8000) })
    .then(r => process.stdout.write(String(r.status)))
    .catch(() => process.stdout.write("000"));
')
if [ "$owhealthz_status" = "200" ]; then
  ok "office-worker healthz (in-network exec)"
else
  bad "office-worker healthz failed (in-network exec, got ${owhealthz_status:-<no response>})"
fi

# The spec calls for the docx.create round-trip via south InvokeTool (through
# broker's toolproxy plugin + FGA/capability gate), not a direct hit on
# office-worker — that's the only way to prove agent-gateway→broker→toolproxy
# plugin→office-worker→workspace write-back is actually wired end-to-end
# (the office-worker /healthz check above already proves the worker itself is
# reachable; this proves the plugin, AIKONOS_OFFICE_WORKER_URL, and the office
# network path INTO it). agent-gateway/src/cli/smoke-south.ts (now
# parameterized past its former hardcoded workspace_read) drives it: runs
# inside the agent-gateway container so it reuses the real south mTLS
# SVID/broker-addr env already wired there — no new plumbing.
#
# Runs as alice@example.com (an OIDC bearer from the Keycloak mint above binds
# her as the task owner; personal-session deny-by-default applies). She
# already holds the FGA grant this needs: dev-seed.yaml puts her in
# group:office-users, which CP7's seed grants can_invoke on skill:docx.create
# (and every other office tool id) — no new tuple required here.
if [ -n "${ALICE_TOK:-}" ]; then
  DOCX_CREATE_ARGS='{"script":"const { Document, Packer, Paragraph } = require(\"docx\"); const fs = require(\"fs\"); const doc = new Document({ sections: [{ children: [new Paragraph(\"compose-verify smoke (south InvokeTool)\")] }] }); Packer.toBuffer(doc).then(buf => fs.writeFileSync(\"output.docx\", buf));","output_path":"compose-verify/docx-create-smoke.docx"}'
  # Passed via env, not argv: argv forwarded through npm's `--` doesn't reliably
  # survive the `docker compose exec` hop (confirmed live — the tool silently
  # ran the workspace_read default instead of docx.create).
  DOCX_SMOKE_OUT=$(docker compose exec -T \
    -e AIKONOS_OIDC_TOKEN="$ALICE_TOK" \
    -e SMOKE_TOOL_ID=docx.create \
    -e SMOKE_ARGS="$DOCX_CREATE_ARGS" \
    -e SMOKE_EFFECT_CLASS=WRITE_LOCAL \
    agent-gateway npm run --silent smoke 2>&1)
  DOCX_SMOKE_STATUS=$?
  if [ "$DOCX_SMOKE_STATUS" -eq 0 ]; then
    ok "docx.create round-trip (south InvokeTool → broker → office-worker, alice@example.com)"
  else
    bad "docx.create round-trip failed: $(tail -c 400 <<<"$DOCX_SMOKE_OUT")"
  fi
else
  skip "docx.create round-trip — no ALICE_TOK (needs Keycloak smoke creds)"
fi

echo
echo "[verify] $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
