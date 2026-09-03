#!/usr/bin/env bash
# scripts/compose-dev-ca.sh
#
# DEV-ONLY — mints a static root CA + per-service leaf certs for compose mTLS.
# Output: deploy/compose/tls/{ca,broker,agent-gateway}.{crt,key}
#
# Each leaf carries BOTH a URI-SAN (SPIFFE ID) and a DNS-SAN (compose service
# name) so the broker's trust-domain check AND gRPC's hostname verification both
# pass without SPIRE.
#
# DO NOT use these certs in any non-development environment.
# Idempotent: skips generation if certs exist unless --force is passed.

set -euo pipefail

FORCE=false
for arg in "$@"; do
  if [ "$arg" = "--force" ]; then
    FORCE=true
  fi
done

# $0 works in both bash (host) and ash (alpine/openssl container); BASH_SOURCE
# is bash-only and undefined in ash, which makes the dirname calculation fail.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TLS_DIR="${REPO_ROOT}/deploy/compose/tls"
mkdir -p "${TLS_DIR}"

# ── helpers ───────────────────────────────────────────────────────────────────

need_regen() {
  local name="$1"
  if [ "${FORCE}" = true ]; then return 0; fi
  if [ ! -f "${TLS_DIR}/${name}.crt" ] || [ ! -f "${TLS_DIR}/${name}.key" ]; then
    return 0
  fi
  return 1
}

# ── CA ────────────────────────────────────────────────────────────────────────

if need_regen "ca"; then
  echo "[compose-dev-ca] generating dev root CA"
  openssl ecparam -name prime256v1 -genkey -noout \
    -out "${TLS_DIR}/ca.key"
  openssl req -new -x509 \
    -key "${TLS_DIR}/ca.key" \
    -out "${TLS_DIR}/ca.crt" \
    -days 3650 \
    -subj "/CN=aikonos-dev-ca/O=Aikonos Dev" \
    -extensions v3_ca \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"
  echo "[compose-dev-ca] CA written to ${TLS_DIR}/ca.crt"
else
  echo "[compose-dev-ca] CA exists, skipping (pass --force to regenerate)"
fi

# ── leaf cert helper ──────────────────────────────────────────────────────────
# mint_leaf <name> <spiffe-id> <dns-san>

mint_leaf() {
  local name="$1"
  local spiffe_id="$2"
  local dns_san="$3"

  if ! need_regen "${name}"; then
    echo "[compose-dev-ca] ${name} cert exists, skipping"
    return 0
  fi

  echo "[compose-dev-ca] generating leaf cert for ${name}"

  # Generate leaf key in PKCS#8 ("PRIVATE KEY") form. `openssl ecparam -genkey`
  # emits SEC1 ("EC PRIVATE KEY"), which go-spiffe's x509svid.Load does not
  # recognize ("no private key found"); genpkey emits PKCS#8, which it parses.
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
    -out "${TLS_DIR}/${name}.key"

  # Write SAN config — both URI (SPIFFE) and DNS (compose service name)
  local san_cfg
  san_cfg="$(mktemp /tmp/san-XXXXXX.cnf)"
  cat > "${san_cfg}" <<EOF
[req]
distinguished_name = req_dn
req_extensions     = v3_req
prompt             = no

[req_dn]
CN = ${name}
O  = Aikonos Dev

[v3_req]
subjectAltName = URI:${spiffe_id},DNS:${dns_san}
extendedKeyUsage = clientAuth,serverAuth
keyUsage = critical,digitalSignature,keyEncipherment
EOF

  # CSR
  openssl req -new \
    -key "${TLS_DIR}/${name}.key" \
    -out "${TLS_DIR}/${name}.csr" \
    -config "${san_cfg}"

  # Sign with dev CA; embed the SANs from the CSR via the same config
  openssl x509 -req \
    -in "${TLS_DIR}/${name}.csr" \
    -CA "${TLS_DIR}/ca.crt" \
    -CAkey "${TLS_DIR}/ca.key" \
    -CAcreateserial \
    -out "${TLS_DIR}/${name}.crt" \
    -days 3650 \
    -extensions v3_req \
    -extfile "${san_cfg}"

  rm -f "${TLS_DIR}/${name}.csr" "${san_cfg}"
  echo "[compose-dev-ca] ${name} cert written to ${TLS_DIR}/${name}.crt"
}

# ── mint per-service leaves ───────────────────────────────────────────────────

mint_leaf "broker"        "spiffe://aikonos.com/broker"        "broker"
mint_leaf "agent-gateway" "spiffe://aikonos.com/agent-gateway" "agent-gateway"

# DEV ONLY: the broker (uid 65532) and gateway run as non-root and mount this
# dir read-only; openssl writes leaf keys 0600 owned by the mint user, which the
# service users then cannot read. These are throwaway dev certs (not production
# secrets) — make the leaf keys world-readable so the services can load them.
# Runs unconditionally (also on the idempotent skip path) to fix pre-existing perms.
chmod 0644 "${TLS_DIR}/broker.key" "${TLS_DIR}/agent-gateway.key" 2>/dev/null || true

echo "[compose-dev-ca] done. Files in ${TLS_DIR}:"
ls -1 "${TLS_DIR}"
