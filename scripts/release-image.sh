#!/usr/bin/env bash
# scripts/release-image.sh
#
# Builds one first-party image for a release, writes its SBOMs, and (when
# publishing) pushes it to the registry, signs it and attaches the SBOM as a
# signed attestation. Called once per image by .github/workflows/release.yml;
# see docs/14-signed-releases.md for the whole pipeline.
#
#   PUBLISH=false (default) — dry run. Builds into the local Docker image store
#     and writes the SBOMs from that image. Nothing leaves the machine. Safe to
#     run locally to check that an image builds and what its SBOM contains.
#   PUBLISH=true — release. Pushes "${IMAGE_PREFIX}/<name>:${VERSION}", takes the
#     pushed digest from buildx's metadata, scans the image back from the
#     registry by that digest so the SBOM describes exactly what was pushed,
#     then signs the digest with cosign (keyless) and attests the CycloneDX SBOM
#     to it. Refuses to run outside GitHub Actions: release signatures are
#     verified against the release workflow's identity, so an image pushed and
#     signed from a laptop would only fail verification.
#
# Outputs, in OUT_DIR (default dist/images):
#   <name>.ref        "<compose service> <image>:<version>@<digest>"
#   <name>.cdx.json   CycloneDX 1.6 SBOM
#   <name>.spdx.json  SPDX 2.3 SBOM
# and, when GITHUB_OUTPUT is set, the step outputs `image` and `digest`.
# In a dry run the digest is the local image ID: there is no registry manifest.
#
# Usage:
#   IMAGE_PREFIX=ghcr.io/<owner>/aikonos VERSION=v1.2.3 \
#     scripts/release-image.sh --name broker --service broker \
#       --context . --dockerfile broker/Dockerfile [--build-arg KEY=VALUE ...]
#
# Requires docker with buildx, syft and jq; cosign when publishing.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

log() { printf '[release-image] %s\n' "$*"; }
die() { printf '[release-image] ERROR: %s\n' "$*" >&2; exit 1; }

NAME=""
SERVICE=""
CONTEXT=""
DOCKERFILE=""
BUILD_ARGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)       NAME="${2:?--name needs a value}"; shift 2 ;;
    --service)    SERVICE="${2:?--service needs a value}"; shift 2 ;;
    --context)    CONTEXT="${2:?--context needs a value}"; shift 2 ;;
    --dockerfile) DOCKERFILE="${2:?--dockerfile needs a value}"; shift 2 ;;
    --build-arg)  BUILD_ARGS+=(--build-arg "${2:?--build-arg needs KEY=VALUE}"); shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "${NAME}" && -n "${SERVICE}" && -n "${CONTEXT}" && -n "${DOCKERFILE}" ]] \
  || die "--name, --service, --context and --dockerfile are all required"
[[ "${NAME}" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || die "invalid image name: ${NAME}"
[[ -d "${CONTEXT}" ]] || die "build context not found: ${CONTEXT}"
[[ -f "${DOCKERFILE}" ]] || die "Dockerfile not found: ${DOCKERFILE}"

: "${IMAGE_PREFIX:?IMAGE_PREFIX is required, e.g. ghcr.io/<owner>/aikonos}"
: "${VERSION:?VERSION is required, e.g. v1.2.3}"
PUBLISH="${PUBLISH:-false}"
OUT_DIR="${OUT_DIR:-dist/images}"

# A Docker tag: no leading '.' or '-', at most 128 characters.
[[ "${VERSION}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "VERSION is not a valid image tag: ${VERSION}"
[[ "${IMAGE_PREFIX}" =~ ^[a-z0-9]([a-z0-9._/:-]*[a-z0-9])?$ ]] \
  || die "IMAGE_PREFIX must be a lowercase registry path, e.g. ghcr.io/<owner>/aikonos (got: ${IMAGE_PREFIX})"
case "${PUBLISH}" in
  true)
    [[ "${GITHUB_ACTIONS:-}" == "true" ]] \
      || die "PUBLISH=true runs only in the release workflow; use PUBLISH=false for a local dry run"
    command -v cosign >/dev/null 2>&1 || die "cosign not found on PATH"
    ;;
  false) ;;
  *) die "PUBLISH must be true or false, got: ${PUBLISH}" ;;
esac
for tool in docker syft jq; do
  command -v "${tool}" >/dev/null 2>&1 || die "${tool} not found on PATH"
done

IMAGE="${IMAGE_PREFIX}/${NAME}"
TAGGED="${IMAGE}:${VERSION}"
SOURCE_URL="${SOURCE_URL:-${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-adamdekan/aikonos}}"
REVISION="$(git rev-parse HEAD)"
# Commit time rather than build time, so rebuilding the same commit labels the
# image the same way.
CREATED="$(git log -1 --format=%cI HEAD)"

mkdir -p "${OUT_DIR}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# linux/amd64 only: the broker Dockerfile cross-compiles for amd64 explicitly.
# --provenance/--sbom off keep the pushed artifact a single image manifest; the
# provenance and SBOM attestations are produced and signed separately.
BUILD=(
  docker buildx build
  --platform linux/amd64
  --pull
  --provenance=false
  --sbom=false
  --file "${DOCKERFILE}"
  --tag "${TAGGED}"
  --label "org.opencontainers.image.title=aikonos-${NAME}"
  --label "org.opencontainers.image.source=${SOURCE_URL}"
  --label "org.opencontainers.image.revision=${REVISION}"
  --label "org.opencontainers.image.version=${VERSION}"
  --label "org.opencontainers.image.created=${CREATED}"
  --label "org.opencontainers.image.licenses=Apache-2.0"
  # Offered to every image; a Dockerfile that declares them uses them. The
  # broker records them in its binary, where the SBOM reads its own version.
  --build-arg "VERSION=${VERSION}"
  --build-arg "REVISION=${REVISION}"
  --metadata-file "${WORKDIR}/metadata.json"
)
if [[ ${#BUILD_ARGS[@]} -gt 0 ]]; then
  BUILD+=("${BUILD_ARGS[@]}")
fi

if [[ "${PUBLISH}" == "true" ]]; then
  log "Building and pushing ${TAGGED}..."
  "${BUILD[@]}" --push "${CONTEXT}"
  DIGEST="$(jq -r '."containerimage.digest" // empty' "${WORKDIR}/metadata.json")"
  SBOM_SOURCE="registry:${IMAGE}@${DIGEST}"
else
  log "Building ${TAGGED} (dry run, nothing is pushed)..."
  "${BUILD[@]}" --load "${CONTEXT}"
  DIGEST="$(docker image inspect --format '{{.Id}}' "${TAGGED}")"
  SBOM_SOURCE="docker:${TAGGED}"
fi
[[ "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "could not determine the digest of ${TAGGED} (got: '${DIGEST}')"
log "  ${TAGGED} -> ${DIGEST}"

# Package-level SBOMs. File metadata is off: listing every packaged file adds
# thousands of entries per image (LibreOffice alone) without naming a single
# additional component, and the signed image digest already covers file
# integrity. CycloneDX is pinned to 1.6 and SPDX to 2.3, the versions current
# SBOM tooling (Dependency-Track, grype, Trivy) reads.
log "Writing SBOMs from ${SBOM_SOURCE}..."
SYFT_CHECK_FOR_APP_UPDATE=false SYFT_FILE_METADATA_SELECTION=none \
  syft scan "${SBOM_SOURCE}" \
    --quiet \
    --source-name "${IMAGE}" \
    --source-version "${VERSION}" \
    --output "cyclonedx-json@1.6=${OUT_DIR}/${NAME}.cdx.json" \
    --output "spdx-json@2.3=${OUT_DIR}/${NAME}.spdx.json"

components="$(jq '[.components[]? | select(.type != "file")] | length' "${OUT_DIR}/${NAME}.cdx.json")"
[[ "${components}" -gt 0 ]] || die "the SBOM for ${TAGGED} lists no components; refusing to publish an empty SBOM"
log "  ${components} components"

if [[ "${PUBLISH}" == "true" ]]; then
  # Keyless: the signing certificate is issued to this workflow run's GitHub
  # OIDC identity and the signature is logged in Sigstore's public transparency
  # log. Both are stored in the registry as OCI referrers of the image digest.
  log "Signing ${IMAGE}@${DIGEST}..."
  cosign sign --yes "${IMAGE}@${DIGEST}"
  log "Attesting the CycloneDX SBOM to ${IMAGE}@${DIGEST}..."
  cosign attest --yes --type cyclonedx --predicate "${OUT_DIR}/${NAME}.cdx.json" "${IMAGE}@${DIGEST}"
fi

printf '%s %s@%s\n' "${SERVICE}" "${TAGGED}" "${DIGEST}" > "${OUT_DIR}/${NAME}.ref"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "image=${IMAGE}"
    echo "digest=${DIGEST}"
  } >> "${GITHUB_OUTPUT}"
fi

log "Done: ${OUT_DIR}/${NAME}.ref"
