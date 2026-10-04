#!/usr/bin/env bash
# scripts/tests/release-scripts.test.sh
#
# Assert script for the release pipeline's local logic, runnable without a
# registry, a Docker daemon or a signing identity:
#
#   - scripts/release-assemble.sh, fed one fake image record per entry in
#     .github/release-images.json. The happy path doubles as a drift check: if
#     compose.yaml gains a service that is built from source but has no release
#     image, the overlay check fails here instead of at release time.
#   - scripts/verify-release.sh, against the assembled files, with a cosign test
#     double that logs its arguments and fails on request. That pins the
#     identity a signature must carry, and walks every failure path.
#
# Needs bash, git, jq and the docker compose CLI (config rendering only).
# Usage: bash scripts/tests/release-scripts.test.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ASSEMBLE="${ROOT}/scripts/release-assemble.sh"
VERIFY="${ROOT}/scripts/verify-release.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

VERSION="v9.9.9"
PREFIX="ghcr.io/example/aikonos"
REPO="example/aikonos"
IDENTITY="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${VERSION}"

pass=0
fail=0
LAST_OUT=""

# run_case <name> <expect: 0|nonzero> <command...>
run_case() {
  local name="$1" expect="$2" rc
  shift 2
  set +e
  LAST_OUT="$("$@" 2>&1)"
  rc=$?
  set -e
  if { [[ "${expect}" == "0" ]] && [[ "${rc}" -eq 0 ]]; } \
     || { [[ "${expect}" != "0" ]] && [[ "${rc}" -ne 0 ]]; }; then
    echo "PASS: ${name} (exit=${rc})"
    pass=$((pass + 1))
  else
    echo "FAIL: ${name} (exit=${rc}, expected ${expect})"
    printf -- '--- output ---\n%s\n--------------\n' "${LAST_OUT}"
    fail=$((fail + 1))
  fi
}

# assert_contains <name> <fixed string> [file] — greps LAST_OUT, or the file.
assert_contains() {
  local name="$1" pattern="$2" haystack
  if [[ $# -ge 3 ]]; then haystack="$(cat "$3")"; else haystack="${LAST_OUT}"; fi
  if grep -qF -- "${pattern}" <<< "${haystack}"; then
    echo "PASS: ${name}"
    pass=$((pass + 1))
  else
    echo "FAIL: ${name} (missing: \"${pattern}\")"
    printf -- '--- searched ---\n%s\n----------------\n' "${haystack}"
    fail=$((fail + 1))
  fi
}

# ---------------------------------------------------------------------------
# Fixtures: one fake release-image.sh output per release image.
# ---------------------------------------------------------------------------
IMAGES="${WORKDIR}/images"
mkdir -p "${IMAGES}"
jq -r '.[] | "\(.name) \(.service)"' "${ROOT}/.github/release-images.json" | tr -d '\r' \
  | while read -r name service; do
      digest="$(printf '%s' "${name}" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-64)"
      printf '%s %s/%s:%s@sha256:%s\n' "${service}" "${PREFIX}" "${name}" "${VERSION}" "${digest}" > "${IMAGES}/${name}.ref"
      echo '{"bomFormat":"CycloneDX","specVersion":"1.6"}' > "${IMAGES}/${name}.cdx.json"
      echo '{"spdxVersion":"SPDX-2.3"}' > "${IMAGES}/${name}.spdx.json"
    done

# assemble <images dir> <out dir>
assemble() {
  VERSION="${VERSION}" IMAGE_PREFIX="${PREFIX}" REPOSITORY="${REPO}" \
    IMAGES_DIR="$1" OUT_DIR="$2" NOTES_FILE="$2.notes.md" bash "${ASSEMBLE}"
}

# variant <name>: a copy of the fixture images, for a negative case to mutate.
variant() {
  rm -rf "${WORKDIR:?}/$1"
  cp -R "${IMAGES}" "${WORKDIR}/$1"
  echo "${WORKDIR}/$1"
}

echo "=== release-assemble.sh ==="
RELEASE="${WORKDIR}/release"
run_case "assemble: happy path, every variant" 0 assemble "${IMAGES}" "${RELEASE}"
assert_contains "assemble: local overlay check ran" "overlay OK for the local variant"
assert_contains "assemble: azure overlay check ran" "overlay OK for the azure variant"
assert_contains "assemble: onprem overlay check ran" "overlay OK for the onprem variant"
for f in compose.release.yaml images.txt SHA256SUMS "aikonos-${VERSION}-source.tar.gz" \
         "aikonos-broker-${VERSION}.cdx.json" "aikonos-broker-${VERSION}.spdx.json"; do
  if [[ -s "${RELEASE}/${f}" ]]; then
    echo "PASS: assemble: wrote ${f}"; pass=$((pass + 1))
  else
    echo "FAIL: assemble: did not write ${f}"; fail=$((fail + 1))
  fi
done
assert_contains "assemble: overlay drops the build" "build: !reset null" "${RELEASE}/compose.release.yaml"
assert_contains "assemble: overlay allows a mirror prefix" "image: \${AIKONOS_IMAGE_PREFIX:-${PREFIX}}/broker:${VERSION}@sha256:" "${RELEASE}/compose.release.yaml"
assert_contains "assemble: notes pin the signer identity" "ID=${IDENTITY}" "${RELEASE}.notes.md"
if grep -q 'SHA256SUMS$' "${RELEASE}/SHA256SUMS"; then
  echo "FAIL: assemble: SHA256SUMS lists itself"; fail=$((fail + 1))
else
  echo "PASS: assemble: SHA256SUMS does not list itself"; pass=$((pass + 1))
fi

dir="$(variant missing-image)"; rm "${dir}"/office-worker.*
run_case "assemble: refuses a built service with no release image" nonzero assemble "${dir}" "${dir}.out"
assert_contains "assemble: names the unreleased service" "still built from source, no release image: office-worker"

dir="$(variant unknown-service)"
printf 'no-such-service %s/docs-mcp:%s@sha256:%064d\n' "${PREFIX}" "${VERSION}" 0 > "${dir}/docs-mcp.ref"
run_case "assemble: refuses a service compose does not define" nonzero assemble "${dir}" "${dir}.out"
assert_contains "assemble: names the unknown service" "does not define: no-such-service"

dir="$(variant foreign-image)"
printf 'broker docker.io/someone/broker:%s@sha256:%064d\n' "${VERSION}" 0 > "${dir}/broker.ref"
run_case "assemble: refuses an image outside IMAGE_PREFIX" nonzero assemble "${dir}" "${dir}.out"

dir="$(variant no-digest)"
printf 'broker %s/broker:%s\n' "${PREFIX}" "${VERSION}" > "${dir}/broker.ref"
run_case "assemble: refuses an image not pinned by digest" nonzero assemble "${dir}" "${dir}.out"

dir="$(variant missing-sbom)"; rm "${dir}/webui.spdx.json"
run_case "assemble: refuses an image without its SBOMs" nonzero assemble "${dir}" "${dir}.out"
assert_contains "assemble: names the missing SBOM" "missing SBOM"

dir="$(variant duplicate)"
cp "${dir}/broker.ref" "${dir}/broker-copy.ref"
sed -i.bak "s#/broker:#/broker-copy:#" "${dir}/broker-copy.ref" && rm -f "${dir}/broker-copy.ref.bak"
cp "${dir}/broker.cdx.json" "${dir}/broker-copy.cdx.json"; cp "${dir}/broker.spdx.json" "${dir}/broker-copy.spdx.json"
run_case "assemble: refuses two images for one service" nonzero assemble "${dir}" "${dir}.out"
assert_contains "assemble: names the doubled service" "more than one image for service(s): broker"

mkdir -p "${WORKDIR}/occupied" && touch "${WORKDIR}/occupied/leftover"
run_case "assemble: refuses a non-empty output directory" nonzero assemble "${IMAGES}" "${WORKDIR}/occupied"

# ---------------------------------------------------------------------------
# verify-release.sh against the assembled release, with a cosign test double.
# ---------------------------------------------------------------------------
BIN="${WORKDIR}/bin"
mkdir -p "${BIN}"
cat > "${BIN}/cosign" <<'EOF'
#!/usr/bin/env bash
# cosign test double: logs every call; fails any call whose arguments contain
# FAKE_COSIGN_FAIL; reports FAKE_COSIGN_VERSION.
printf '%s\n' "$*" >> "${COSIGN_LOG}"
if [[ "$1" == "version" ]]; then
  echo "GitVersion:    ${FAKE_COSIGN_VERSION:-v3.1.3}"
  exit 0
fi
if [[ -n "${FAKE_COSIGN_FAIL:-}" && "$*" == *"${FAKE_COSIGN_FAIL}"* ]]; then
  echo "fake cosign: no matching signatures" >&2
  exit 1
fi
exit 0
EOF
chmod +x "${BIN}/cosign"
cat > "${BIN}/gh" <<'EOF'
#!/usr/bin/env bash
# gh test double: logs every call, succeeds.
printf 'gh %s\n' "$*" >> "${COSIGN_LOG}"
EOF
chmod +x "${BIN}/gh"
echo '{"fake":"bundle"}' > "${RELEASE}/SHA256SUMS.sigstore.json"

# run_verify [VAR=value...] -- <verify-release.sh args...>
# Runs verify-release.sh with the cosign double first on PATH, a fresh call log,
# and any extra environment (FAKE_COSIGN_FAIL, FAKE_COSIGN_VERSION).
run_verify() {
  local envs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
  shift
  : > "${WORKDIR}/cosign.log"
  env PATH="${BIN}:${PATH}" COSIGN_LOG="${WORKDIR}/cosign.log" ${envs[@]+"${envs[@]}"} \
    bash "${VERIFY}" "$@"
}

# verify <release dir> [verify-release.sh options...] — this version, this repo.
verify() {
  local dir="$1"
  shift
  run_verify -- "${VERSION}" --repo "${REPO}" --dir "${dir}" "$@"
}

# copy_release <name>: a copy of the assembled release for a case to tamper with.
copy_release() {
  rm -rf "${WORKDIR:?}/$1"
  cp -R "${RELEASE}" "${WORKDIR}/$1"
  echo "${WORKDIR}/$1"
}

# rehash <dir>: rewrite SHA256SUMS after a deliberate edit, as a forger would.
rehash() {
  local f sums="${WORKDIR}/rehash.tmp"
  ( cd "$1" && for f in *; do
      case "${f}" in SHA256SUMS|SHA256SUMS.sigstore.json) continue ;; esac
      { sha256sum -- "${f}" 2>/dev/null || shasum -a 256 -- "${f}"; }
    done ) > "${sums}"
  mv "${sums}" "$1/SHA256SUMS"
}

echo "=== verify-release.sh ==="
image_count="$(wc -l < "${RELEASE}/images.txt" | tr -d ' ')"
run_case "verify: happy path" 0 verify "${RELEASE}"
assert_contains "verify: reports success" "verified: ${VERSION} is what ${REPO}'s release workflow built and signed."
assert_contains "verify: SHA256SUMS checked against the release workflow at this tag" \
  "verify-blob --bundle ${RELEASE}/SHA256SUMS.sigstore.json --certificate-identity ${IDENTITY} --certificate-oidc-issuer https://token.actions.githubusercontent.com" \
  "${WORKDIR}/cosign.log"
assert_contains "verify: images checked against the same identity" \
  "verify --certificate-identity ${IDENTITY} --certificate-oidc-issuer https://token.actions.githubusercontent.com ${PREFIX}/broker@sha256:" \
  "${WORKDIR}/cosign.log"
assert_contains "verify: SBOM attestations checked" \
  "verify-attestation --type cyclonedx --certificate-identity ${IDENTITY}" "${WORKDIR}/cosign.log"
signature_calls="$(grep -c '^verify --certificate-identity' "${WORKDIR}/cosign.log" || true)"
attestation_calls="$(grep -c '^verify-attestation --type cyclonedx' "${WORKDIR}/cosign.log" || true)"
if [[ "${signature_calls}" == "${image_count}" && "${attestation_calls}" == "${image_count}" ]]; then
  echo "PASS: verify: every image (${image_count}) checked for signature and SBOM"; pass=$((pass + 1))
else
  echo "FAIL: verify: ${signature_calls} signature / ${attestation_calls} SBOM checks for ${image_count} images"; fail=$((fail + 1))
fi
if grep -qE -- '--certificate-identity-regexp|--insecure' "${WORKDIR}/cosign.log"; then
  echo "FAIL: verify: loosened identity or insecure flag passed to cosign"; fail=$((fail + 1))
else
  echo "PASS: verify: exact identity only, no insecure flags"; pass=$((pass + 1))
fi

RELEASE_ARGS=("${VERSION}" --repo "${REPO}" --dir "${RELEASE}")

run_case "verify: rejects a SHA256SUMS signature from another identity" nonzero \
  run_verify FAKE_COSIGN_FAIL="verify-blob" -- "${RELEASE_ARGS[@]}"
assert_contains "verify: says who was expected" "SHA256SUMS is not signed by ${IDENTITY}"

dir="$(copy_release tampered-file)"
echo '{"tampered":true}' > "${dir}/aikonos-webui-${VERSION}.cdx.json"
run_case "verify: rejects a file that does not match SHA256SUMS" nonzero verify "${dir}"
assert_contains "verify: names the checksum failure" "does not match SHA256SUMS"

dir="$(copy_release swapped-image)"
sed -i.bak "s#/broker:${VERSION}@sha256:[0-9a-f]*#/broker:${VERSION}@sha256:$(printf '%064d' 1)#" "${dir}/images.txt" && rm -f "${dir}/images.txt.bak"
rehash "${dir}"
run_case "verify: rejects an images.txt the overlay does not match" nonzero verify "${dir}"
assert_contains "verify: names the mismatch" "does not pin the same images as images.txt"

run_case "verify: rejects an unsigned image" nonzero \
  run_verify FAKE_COSIGN_FAIL="verify --certificate-identity ${IDENTITY} --certificate-oidc-issuer https://token.actions.githubusercontent.com ${PREFIX}/webui@" \
  -- "${RELEASE_ARGS[@]}"
assert_contains "verify: names the unsigned image" "no valid signature from ${IDENTITY} on ${PREFIX}/webui@sha256:"

run_case "verify: rejects an image without an SBOM attestation" nonzero \
  run_verify FAKE_COSIGN_FAIL="verify-attestation --type cyclonedx" -- "${RELEASE_ARGS[@]}"
assert_contains "verify: names the missing attestation" "no valid CycloneDX SBOM attestation"

run_case "verify: --provenance checks SLSA provenance with gh" 0 verify "${RELEASE}" --provenance
assert_contains "verify: provenance pinned to the same identity and tag" \
  "gh attestation verify oci://${PREFIX}/broker@sha256:$(printf '%s' broker | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-64) --bundle-from-oci --repo ${REPO} --cert-identity ${IDENTITY} --source-ref refs/tags/${VERSION} --deny-self-hosted-runners" \
  "${WORKDIR}/cosign.log"
provenance_calls="$(grep -c '^gh attestation verify' "${WORKDIR}/cosign.log" || true)"
if [[ "${provenance_calls}" == "${image_count}" ]]; then
  echo "PASS: verify: provenance checked for every image"; pass=$((pass + 1))
else
  echo "FAIL: verify: ${provenance_calls} provenance checks for ${image_count} images"; fail=$((fail + 1))
fi

run_case "verify: --image-prefix checks the mirror's copies" 0 verify "${RELEASE}" --image-prefix registry.example.org/mirror
assert_contains "verify: mirror ref passed to cosign" \
  "${IDENTITY} --certificate-oidc-issuer https://token.actions.githubusercontent.com registry.example.org/mirror/broker@sha256:" \
  "${WORKDIR}/cosign.log"

run_case "verify: a mirror on a port keeps its port" 0 verify "${RELEASE}" --image-prefix harbor.example.org:8443/aikonos
assert_contains "verify: port survives dropping the tag" \
  "${IDENTITY} --certificate-oidc-issuer https://token.actions.githubusercontent.com harbor.example.org:8443/aikonos/broker@sha256:" \
  "${WORKDIR}/cosign.log"

# The double accepts any signature, so this isolates the image-tag check: the
# real identity check would also fail, since the identity names the tag.
run_case "verify: rejects files of another version" nonzero \
  run_verify -- v9.9.8 --repo "${REPO}" --dir "${RELEASE}"
assert_contains "verify: names the version mismatch" "not a v9.9.8 image pinned by digest"

run_case "verify: refuses cosign 2.x" nonzero \
  run_verify FAKE_COSIGN_VERSION=v2.6.2 -- "${RELEASE_ARGS[@]}"
assert_contains "verify: explains the cosign version" "cosign v2.6.2 is too old"

run_case "verify: refuses a non-release version argument" nonzero run_verify -- main --repo "${REPO}"
assert_contains "verify: explains the version format" "not a release version: main"

echo
echo "release-scripts: ${pass} passed, ${fail} failed"
[[ "${fail}" -eq 0 ]]
