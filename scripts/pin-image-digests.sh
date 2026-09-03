#!/usr/bin/env bash
# scripts/pin-image-digests.sh
#
# Enumerates every pulled (non-locally-built) `image:` reference across the
# base compose file plus the azure/onprem overlays, `docker pull`s each one,
# resolves its content digest, and writes/refreshes the checked-in
#   deploy/compose/compose.digests.yaml
# — a pure `services.<name>.image: <ref>@sha256:...` override file. It never
# edits compose.yaml or the overlays; prod deploys layer it on top with an
# extra `-f`.
#
# Service enumeration is done via `docker compose ... config --format json`
# (not grep) so env-substituted image tags resolve to their real values, and
# every profile is force-enabled so profile-gated services (docs-mcp,
# dev-ca-mint, migrate, ...) are included regardless of which
# profile an operator normally runs. Dummy env files are used — this script
# never reads or echoes the real .env.
#
# A service with a `build:` key has no upstream pulled image to pin and is
# skipped. Any image that can't be pulled or whose digest can't be resolved
# is a hard failure naming the image — this script never emits a partial
# file.
#
# Usage: scripts/pin-image-digests.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

OUT_FILE="deploy/compose/compose.digests.yaml"
ALL_PROFILES=(--profile core --profile full --profile obs --profile dev --profile docs-mcp)

log() { printf '[pin-image-digests] %s\n' "$*"; }
die() { printf '[pin-image-digests] ERROR: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"

# ---------------------------------------------------------------------------
# Dummy env files — never the real .env. Cleaned up on exit.
# ---------------------------------------------------------------------------
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

cp deploy/compose/.env.local.example "${WORKDIR}/local.env"
cp deploy/compose/.env.azure.example "${WORKDIR}/azure.env"
cp deploy/compose/.env.onprem.example "${WORKDIR}/onprem.env"

# service_name<TAB>image_ref for every non-build service, across all three
# variants (dedup happens naturally: same service+image collapses via sort -u).
collect_service_images() {
  local env_file="$1"
  shift
  docker compose --env-file "${env_file}" "$@" "${ALL_PROFILES[@]}" config --format json \
    | jq -r '.services | to_entries[] | select(.value.build == null) | select(.value.image != null) | [.key, .value.image] | @tsv'
}

log "Enumerating services (local + azure + onprem variants, all profiles)..."
SERVICE_IMAGES="$(
  {
    collect_service_images "${WORKDIR}/local.env" -f compose.yaml
    collect_service_images "${WORKDIR}/azure.env" -f compose.yaml -f deploy/compose/compose.azure.yaml
    collect_service_images "${WORKDIR}/onprem.env" -f compose.yaml -f deploy/compose/compose.onprem.yaml
  } | sort -u
)"

[[ -n "${SERVICE_IMAGES}" ]] || die "no pullable image: references found across any variant"

# Sanity check: the same service name must not resolve to two different
# images across variants (that would mean the merged overlays disagree on
# what the digest override should pin, which this script cannot resolve).
CONFLICTS="$(printf '%s\n' "${SERVICE_IMAGES}" | awk -F'\t' '{print $1}' | sort | uniq -d)"
if [[ -n "${CONFLICTS}" ]]; then
  die "service(s) resolve to different images across variants, cannot pin unambiguously: ${CONFLICTS}"
fi

log "Found $(printf '%s\n' "${SERVICE_IMAGES}" | wc -l | tr -d ' ') service/image pairs."

# ---------------------------------------------------------------------------
# Pull + resolve digest per unique image ref.
# ---------------------------------------------------------------------------
UNIQUE_IMAGES="$(printf '%s\n' "${SERVICE_IMAGES}" | awk -F'\t' '{print $2}' | sort -u)"

declare -A DIGEST_FOR_IMAGE

while IFS= read -r image; do
  [[ -z "${image}" ]] && continue
  log "Pulling ${image}..."
  docker pull --quiet "${image}" >/dev/null || die "docker pull failed for image: ${image}"

  # Repository part (before the tag) — used to pick the matching RepoDigest
  # entry when the local image cache holds digests for other tags/repos too.
  local_repo="${image%:*}"

  repo_digests="$(docker image inspect "${image}" --format '{{join .RepoDigests ","}}' 2>/dev/null || true)"
  [[ -n "${repo_digests}" ]] || die "docker image inspect returned no RepoDigests for image: ${image}"

  digest=""
  IFS=',' read -ra digest_list <<< "${repo_digests}"
  for entry in "${digest_list[@]}"; do
    entry_repo="${entry%@*}"
    if [[ "${entry_repo}" == "${local_repo}" ]]; then
      digest="${entry#*@}"
      break
    fi
  done

  [[ -n "${digest}" ]] || die "could not resolve a matching RepoDigest for image: ${image} (got: ${repo_digests})"
  DIGEST_FOR_IMAGE["${image}"]="${digest}"
  log "  ${image} -> ${digest}"
done <<< "${UNIQUE_IMAGES}"

# ---------------------------------------------------------------------------
# Write the digest overlay — deterministic (sorted by service name) so diffs
# stay reviewable.
# ---------------------------------------------------------------------------
{
  echo "# deploy/compose/compose.digests.yaml"
  echo "#"
  echo "# Generated by scripts/pin-image-digests.sh — do not hand-edit."
  echo "# Refresh on every dependency bump (any base image version change)."
  echo "#"
  echo "# Prod deploys append this file with an extra -f, e.g.:"
  echo "#   docker compose -f compose.yaml -f deploy/compose/compose.azure.yaml \\"
  echo "#     -f deploy/compose/compose.digests.yaml up -d"
  echo "# Local dev never includes it (mutable tags are fine for dev iteration)."
  echo "services:"
  while IFS=$'\t' read -r service image; do
    [[ -z "${service}" ]] && continue
    echo "  ${service}:"
    echo "    image: ${image}@${DIGEST_FOR_IMAGE[${image}]}"
  done <<< "$(printf '%s\n' "${SERVICE_IMAGES}" | sort -t $'\t' -k1,1)"
} > "${OUT_FILE}"

log "Wrote ${OUT_FILE}"
