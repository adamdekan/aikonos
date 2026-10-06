#!/usr/bin/env bash
# scripts/release-desktop.sh
#
# Writes the SBOMs of the Windows app (aikonOS for Windows, desktop/) and puts
# it through the release's vulnerability gate, the rule scripts/release-image.sh
# applies to every image. The release workflow builds aikonos.exe on Windows
# with `cargo auditable`, which embeds the app's dependency list in the
# executable, so the SBOM read back from it names the crates the exe was built
# from, for Windows only, not every crate Cargo.lock lists for every platform.
#
# Reads EXE (default dist/desktop/aikonos.exe) and writes into OUT_DIR (default
# dist/desktop), for scripts/release-assemble.sh:
#   desktop.cdx.json   CycloneDX 1.6 SBOM
#   desktop.spdx.json  SPDX 2.3 SBOM
# and the gate's full report to SCAN_DIR/desktop.grype.json (default dist/scans).
#
# The gate fails on a vulnerability rated Critical that has a fixed version
# available, as for the images (docs/14-signed-releases.md).
#
# Usage: VERSION=v1.2.3 scripts/release-desktop.sh
# Requires syft, grype and jq.

set -euo pipefail

log() { printf '[release-desktop] %s\n' "$*"; }
die() { printf '[release-desktop] ERROR: %s\n' "$*" >&2; exit 1; }

: "${VERSION:?VERSION is required, e.g. v1.2.3}"
EXE="${EXE:-dist/desktop/aikonos.exe}"
OUT_DIR="${OUT_DIR:-dist/desktop}"
SCAN_DIR="${SCAN_DIR:-dist/scans}"

for tool in syft grype jq; do
  command -v "${tool}" >/dev/null 2>&1 || die "${tool} not found on PATH"
done
[[ -s "${EXE}" ]] || die "no executable at ${EXE}"
mkdir -p "${OUT_DIR}" "${SCAN_DIR}"

# File metadata off and the same format versions as the images' SBOMs.
log "Writing SBOMs from ${EXE}..."
SYFT_CHECK_FOR_APP_UPDATE=false SYFT_FILE_METADATA_SELECTION=none \
  syft scan "file:${EXE}" \
    --quiet \
    --source-name aikonos-desktop \
    --source-version "${VERSION}" \
    --output "cyclonedx-json@1.6=${OUT_DIR}/desktop.cdx.json" \
    --output "spdx-json@2.3=${OUT_DIR}/desktop.spdx.json"

# Built without cargo auditable, the exe carries no dependency list, and the
# SBOM would name nothing but the file itself.
crates="$(jq '[.components[]? | select((.purl // "") | startswith("pkg:cargo/"))] | length' "${OUT_DIR}/desktop.cdx.json")"
[[ "${crates}" -gt 0 ]] || die "the SBOM lists no Rust crates; was ${EXE} built with cargo auditable?"
log "  ${crates} crates"

# The gate, as in scripts/release-image.sh: the report keeps every finding; the
# gate counts only Critical ones with a fixed version available.
log "Scanning the SBOM for known vulnerabilities..."
REPORT="${SCAN_DIR}/desktop.grype.json"
GRYPE_CHECK_FOR_APP_UPDATE=false \
  grype "sbom:${OUT_DIR}/desktop.cdx.json" --quiet --output "json=${REPORT}"
summary="$(jq -r '
  [.matches[].vulnerability | {severity, fixed: (.fix.state == "fixed")}]
  | group_by(.severity)
  | map("\(.[0].severity) \(length) (\(map(select(.fixed)) | length) with a fix)")
  | join(", ")' "${REPORT}")"
log "  ${summary:-no known vulnerabilities}"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  printf '**desktop**: %s\n\n' "${summary:-no known vulnerabilities}" >> "${GITHUB_STEP_SUMMARY}"
fi
blocking="$(jq -r '
  .matches[]
  | select(.vulnerability.severity == "Critical" and .vulnerability.fix.state == "fixed")
  | "\(.vulnerability.id) in \(.artifact.type) \(.artifact.name) \(.artifact.version), fixed in \(.vulnerability.fix.versions | join(" or "))"
  ' "${REPORT}" | sort -u)"
if [[ -n "${blocking}" ]]; then
  printf '[release-desktop] Critical vulnerabilities with a fix available:\n%s\n' "${blocking}" >&2
  die "aikonos.exe failed the vulnerability gate; update the crates above (cargo update -p <crate>) and rebuild (report: ${REPORT})"
fi

log "Done: ${OUT_DIR}/desktop.cdx.json, ${OUT_DIR}/desktop.spdx.json"
