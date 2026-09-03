#!/usr/bin/env bash
# scripts/proto-gen.sh
# Generate Go and TypeScript code from all .proto files.
# Run after any proto change: task proto:gen
#
# --ts-only: generate only the TypeScript (agent-gateway) stubs. Lets the
# Node-only CI job regenerate its gitignored stubs without a Go toolchain.
set -euo pipefail

TS_ONLY=false
if [ "${1:-}" = "--ts-only" ]; then
  TS_ONLY=true
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROTO_DIR="${ROOT}/proto"
GEN_GO="${ROOT}/gen/go"
GEN_TS="${ROOT}/agent-gateway/gen/ts"

mkdir -p "${GEN_GO}"

# TypeScript (ts-proto) — only when the agent-gateway has installed it. Stubs
# land under agent-gateway/gen/ts (gitignored), with grpc-js service clients.
TS_PLUGIN="${ROOT}/agent-gateway/node_modules/.bin/protoc-gen-ts_proto"
if [ -x "${TS_PLUGIN}" ]; then
  mkdir -p "${GEN_TS}"
fi

info() { echo "[proto-gen] $*"; }

# Verify tools — Go plugins only matter when generating Go stubs.
required_tools=(protoc)
if [ "${TS_ONLY}" = false ]; then
  required_tools+=(protoc-gen-go protoc-gen-go-grpc)
fi
for tool in "${required_tools[@]}"; do
  command -v "${tool}" &>/dev/null || {
    echo "[ERROR] ${tool} not found. Run: task deps"
    exit 1
  }
done

if [ "${TS_ONLY}" = true ] && [ ! -x "${TS_PLUGIN}" ]; then
  echo "[ERROR] ts_proto plugin not found at ${TS_PLUGIN}. Run: npm ci --prefix agent-gateway"
  exit 1
fi

info "Generating from proto files in ${PROTO_DIR}..."

for proto_file in "${PROTO_DIR}"/*.proto; do
  name=$(basename "${proto_file}")
  info "Processing ${name}..."

  # Go — strip module prefix so files land at gen/go/<pkg>/v1/
  if [ "${TS_ONLY}" = false ]; then
    protoc \
      --proto_path="${ROOT}" \
      --go_out="${ROOT}" \
      --go_opt=module=github.com/adamdekan/aikonos \
      --go-grpc_out="${ROOT}" \
      --go-grpc_opt=module=github.com/adamdekan/aikonos \
      "${proto_file}"
  fi

  # TypeScript (ts-proto + grpc-js) for the agent-gateway.
  if [ -x "${TS_PLUGIN}" ]; then
    protoc \
      --proto_path="${ROOT}" \
      --plugin=protoc-gen-ts_proto="${TS_PLUGIN}" \
      --ts_proto_out="${GEN_TS}" \
      --ts_proto_opt=outputServices=grpc-js,esModuleInterop=true,useExactTypes=false,useOptionals=messages \
      "${proto_file}"
  fi

done

if [ "${TS_ONLY}" = true ]; then
  info "Done. Generated TypeScript stubs in ${GEN_TS}"
  exit 0
fi

info "Done. Generated Go stubs in ${GEN_GO}"
