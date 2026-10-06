#!/usr/bin/env bash
# scripts/verify-release.sh
#
# Verifies an aikonOS release before you deploy it. Checks, in order, and stops
# at the first failure:
#
#   1. SHA256SUMS was signed by this repository's release workflow running for
#      exactly this tag (cosign keyless, Sigstore's public-good instance).
#   2. Every release file matches SHA256SUMS.
#   3. compose.release.yaml pins exactly the images listed in images.txt, all
#      tagged with this version.
#   4. Every image carries a signature and a CycloneDX SBOM attestation from
#      that same workflow identity.
#   5. With --provenance: every image's SLSA build provenance verifies with the
#      GitHub CLI (gh attestation verify).
#
# Files missing from the release directory are downloaded from the GitHub
# release first, so a bare run fetches and checks everything. The directory is
# kept: it holds the verified compose.release.yaml and SBOMs you deploy with.
#
# Usage:
#   scripts/verify-release.sh <version> [options]
#
#   --dir DIR             release directory (default ./aikonos-<version>)
#   --repo OWNER/NAME     source repository (default adamdekan/aikonos)
#   --image-prefix PREFIX verify the images at a mirror instead of the release
#                         registry, e.g. harbor.example.org/aikonos; the digests
#                         must be the same, so copy them with their referrers
#   --provenance          also verify SLSA build provenance (needs gh)
#
# Needs cosign 3.x, curl (only to download), and sha256sum or shasum.
# docs/14-signed-releases.md explains each check and how to do it by hand.

set -euo pipefail

log() { printf '[verify-release] %s\n' "$*"; }
die() { printf '[verify-release] FAILED: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '/^# Usage:/,/^# Needs/p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

VERSION=""
DIR=""
REPO="adamdekan/aikonos"
IMAGE_PREFIX=""
PROVENANCE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)          DIR="${2:?--dir needs a value}"; shift 2 ;;
    --repo)         REPO="${2:?--repo needs a value}"; shift 2 ;;
    --image-prefix) IMAGE_PREFIX="${2:?--image-prefix needs a value}"; shift 2 ;;
    --provenance)   PROVENANCE=true; shift ;;
    -h|--help)      usage ;;
    -*)             die "unknown option: $1" ;;
    *)
      [[ -z "${VERSION}" ]] || die "unexpected argument: $1"
      VERSION="$1"; shift ;;
  esac
done

[[ -n "${VERSION}" ]] || usage
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "not a release version: ${VERSION} (expected vMAJOR.MINOR.PATCH)"
[[ "${REPO}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo must be OWNER/NAME, got: ${REPO}"
DIR="${DIR:-aikonos-${VERSION}}"

command -v cosign >/dev/null 2>&1 || die "cosign not found on PATH (https://docs.sigstore.dev/cosign/system_config/installation/)"
cosign_version="$(cosign version 2>/dev/null | awk '/^GitVersion:/ {print $2}')"
case "${cosign_version}" in
  v0.*|v1.*|v2.*) die "cosign ${cosign_version} is too old: these signatures need cosign 3.x" ;;
  v*) ;;
  *) log "warning: could not read the cosign version; cosign 3.x is required" ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
  SHA256=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA256=(shasum -a 256)
else
  die "neither sha256sum nor shasum found on PATH"
fi
if [[ "${PROVENANCE}" == "true" ]]; then
  command -v gh >/dev/null 2>&1 || die "--provenance needs the GitHub CLI (gh) on PATH"
fi

# The only identity a release signature may carry: the release workflow of this
# repository, run for this exact tag. A signature made from a branch, a fork, a
# different workflow file or a different tag fails here.
IDENTITY="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${VERSION}"
ISSUER="https://token.actions.githubusercontent.com"
IDENTITY_ARGS=(--certificate-identity "${IDENTITY}" --certificate-oidc-issuer "${ISSUER}")
DOWNLOAD_BASE="https://github.com/${REPO}/releases/download/${VERSION}"

mkdir -p "${DIR}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fetch() {
  local name="$1"
  [[ -s "${DIR}/${name}" ]] && return 0
  command -v curl >/dev/null 2>&1 || die "${name} is not in ${DIR} and curl is not available to download it"
  log "downloading ${name}"
  curl -fsSL --retry 3 -o "${DIR}/${name}" "${DOWNLOAD_BASE}/${name}" \
    || die "could not download ${DOWNLOAD_BASE}/${name}"
}

log "release ${VERSION} of ${REPO}, files in ${DIR}"
log "expected signer: ${IDENTITY}"

# 1. The checksum list, and its signature.
fetch SHA256SUMS
fetch SHA256SUMS.sigstore.json
cosign verify-blob --bundle "${DIR}/SHA256SUMS.sigstore.json" "${IDENTITY_ARGS[@]}" \
  "${DIR}/SHA256SUMS" > "${WORKDIR}/out" 2>&1 < /dev/null \
  || { cat "${WORKDIR}/out" >&2; die "SHA256SUMS is not signed by ${IDENTITY}"; }
log "OK  SHA256SUMS signature"

# 2. Every file it lists. Names come from the signed list, but are still checked
#    before they become paths.
names="$(sed -E 's/^[0-9a-f]{64} [ *]//' "${DIR}/SHA256SUMS")"
[[ -n "${names}" ]] || die "SHA256SUMS lists no files"
while IFS= read -r name; do
  [[ "${name}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "unexpected file name in SHA256SUMS: '${name}'"
  fetch "${name}"
done <<< "${names}"
for required in compose.release.yaml images.txt; do
  grep -qxF "${required}" <<< "${names}" || die "the release lists no ${required}"
done
( cd "${DIR}" && "${SHA256[@]}" -c SHA256SUMS ) > "${WORKDIR}/out" 2>&1 < /dev/null \
  || { cat "${WORKDIR}/out" >&2; die "a release file does not match SHA256SUMS"; }
log "OK  $(wc -l <<< "${names}" | tr -d ' ') files match SHA256SUMS"

# 3. The overlay deploys exactly the listed images, all of this version.
images="$(grep -v '^[[:space:]]*$' "${DIR}/images.txt" | LC_ALL=C sort)"
[[ -n "${images}" ]] || die "images.txt lists no images"
pinned="$(sed -nE 's/^[[:space:]]+image:[[:space:]]+\$\{AIKONOS_IMAGE_PREFIX:-([^}]+)\}(.*)$/\1\2/p' \
  "${DIR}/compose.release.yaml" | LC_ALL=C sort)"
[[ "${pinned}" == "${images}" ]] || die "compose.release.yaml does not pin the same images as images.txt"
while IFS= read -r image; do
  [[ "${image}" =~ ^[a-z0-9][a-z0-9._/:-]*/[a-z0-9-]+:[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}$ \
     && "${image}" == *":${VERSION}@sha256:"* ]] \
    || die "not a ${VERSION} image pinned by digest: ${image}"
done <<< "${images}"
log "OK  compose.release.yaml pins the $(wc -l <<< "${images}" | tr -d ' ') images in images.txt"

# 4 and 5. Each image, at the release registry or the given mirror.
while IFS= read -r image; do
  if [[ -n "${IMAGE_PREFIX}" ]]; then
    image="${IMAGE_PREFIX%/}/${image##*/}"
  fi
  # Address the image by digest alone: the digest is what the signatures bind,
  # and not every tool accepts a tag and a digest in one reference.
  by_digest="${image%@*}"
  by_digest="${by_digest%:*}@${image##*@}"

  cosign verify "${IDENTITY_ARGS[@]}" "${by_digest}" > /dev/null 2> "${WORKDIR}/out" < /dev/null \
    || { cat "${WORKDIR}/out" >&2; die "no valid signature from ${IDENTITY} on ${by_digest}"; }
  cosign verify-attestation --type cyclonedx "${IDENTITY_ARGS[@]}" "${by_digest}" \
    > /dev/null 2> "${WORKDIR}/out" < /dev/null \
    || { cat "${WORKDIR}/out" >&2; die "no valid CycloneDX SBOM attestation from ${IDENTITY} on ${by_digest}"; }

  if [[ "${PROVENANCE}" == "true" ]]; then
    gh attestation verify "oci://${by_digest}" --bundle-from-oci \
      --repo "${REPO}" \
      --cert-identity "${IDENTITY}" \
      --source-ref "refs/tags/${VERSION}" \
      --deny-self-hosted-runners > "${WORKDIR}/out" 2>&1 < /dev/null \
      || { cat "${WORKDIR}/out" >&2; die "SLSA provenance did not verify for ${by_digest}"; }
    log "OK  ${image##*/}: signature, SBOM attestation, provenance"
  else
    log "OK  ${image##*/}: signature, SBOM attestation"
  fi
done <<< "${images}"

if [[ "${PROVENANCE}" != "true" ]]; then
  log "provenance not checked (add --provenance; needs gh)"
fi
log "verified: ${VERSION} is what ${REPO}'s release workflow built and signed."
log "deploy with: -f ${DIR}/compose.release.yaml (see docs/14-signed-releases.md)"
