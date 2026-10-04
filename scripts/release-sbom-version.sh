#!/usr/bin/env bash
# scripts/release-sbom-version.sh
#
# Records this repository's own Go module version in an image's SBOMs.
#
# syft takes a Go binary's main-module version from its build info, and for the
# broker it finds none: Go keeps -ldflags out of the build info when building
# with -trimpath, and the build context has no .git for VCS stamping, so the
# module is listed as UNKNOWN. A release builds every binary from this
# repository at the release tag, so the version is known; this writes it into
# both SBOMs (version and purl). Entries that already carry a version, and all
# other packages, are left untouched, and a file with nothing to fill in is not
# rewritten.
#
# Usage: scripts/release-sbom-version.sh <version> <sbom.cdx.json> <sbom.spdx.json>
# Called by scripts/release-image.sh before the SBOM is attested. Needs jq.

# The jq programs are single-quoted on purpose: $m and $v are jq variables.
# shellcheck disable=SC2016

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

die() { printf '[release-sbom-version] ERROR: %s\n' "$*" >&2; exit 1; }

[[ $# -eq 3 ]] || die "usage: $0 <version> <sbom.cdx.json> <sbom.spdx.json>"
VERSION="$1"
CDX="$2"
SPDX="$3"
[[ -n "${VERSION}" ]] || die "empty version"
[[ -f "${CDX}" && -f "${SPDX}" ]] || die "SBOM not found: ${CDX} / ${SPDX}"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"

MODULE="$(awk '$1 == "module" { print $2; exit }' "${REPO_ROOT}/go.mod")"
[[ -n "${MODULE}" ]] || die "no module line in ${REPO_ROOT}/go.mod"

# Shared by both formats: what counts as "no version" and the purl to write.
JQ_DEFS='
  def unversioned: (. // "") | IN("", "UNKNOWN", "(devel)");
  def bare_purl: "pkg:golang/" + $m;
  def versioned_purl: bare_purl + "@" + $v;
'

CDX_FILTER="${JQ_DEFS}"'
  if .components == null then . else
    .components |= map(
      if .name == $m and (.version | unversioned) then
        .version = $v
        | if .purl == bare_purl then .purl = versioned_purl else . end
      else . end)
  end'

SPDX_FILTER="${JQ_DEFS}"'
  if .packages == null then . else
    .packages |= map(
      if .name == $m and (.versionInfo | unversioned) then
        .versionInfo = $v
        | .externalRefs |= ((. // []) | map(
            if .referenceType == "purl" and .referenceLocator == bare_purl
            then .referenceLocator = versioned_purl else . end))
      else . end)
  end'

# fill <file> <filter> <count filter>: rewrites the file only when an entry
# needs the version, and prints how many entries were filled in.
fill() {
  local file="$1" filter="$2" count_filter="$3" n
  n="$(jq --arg m "${MODULE}" --arg v "${VERSION}" "${JQ_DEFS} ${count_filter}" "${file}")"
  if [[ "${n}" -gt 0 ]]; then
    jq --arg m "${MODULE}" --arg v "${VERSION}" "${filter}" "${file}" > "${file}.tmp"
    mv "${file}.tmp" "${file}"
  fi
  echo "${n}"
}

cdx_count="$(fill "${CDX}" "${CDX_FILTER}" \
  '[.components[]? | select(.name == $m and (.version | unversioned))] | length')"
spdx_count="$(fill "${SPDX}" "${SPDX_FILTER}" \
  '[.packages[]? | select(.name == $m and (.versionInfo | unversioned))] | length')"
printf '[release-sbom-version] %s set to %s: %s CycloneDX, %s SPDX entries\n' \
  "${MODULE}" "${VERSION}" "${cdx_count}" "${spdx_count}"
