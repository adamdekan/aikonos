#!/usr/bin/env bash
# scripts/replay-decision.sh
#
# Re-runs a recorded policy decision against the policy that made it, with the
# stock opa binary, and names the rules that decided it.
#
# Usage: scripts/replay-decision.sh <evidence.json> [--opa PATH] [--quiet]
#
# <evidence.json> is a decision export: Admin → Policy → Audit history →
# Evidence in the console, or the GetDecisionEvidence RPC. The script checks,
# in order:
#   1. the policy snapshot the decision names is in the export, and its files
#      hash to that revision, so they are the archived policy, unaltered;
#   2. the recorded OPA input matches its digest (skipped for fields the
#      record redacted; it says which);
#   3. opa, evaluating the recorded input against that snapshot, returns the
#      recorded result;
# then prints, for every decisive value in the result, the rule definitions
# that produced it, as file:line in the snapshot.
#
# The export is treated as untrusted input: snapshot paths, the query and rule
# names are validated before anything is written or evaluated.
#
# Exit status: 0 replayed and matched; 1 a check failed; 2 usage or missing
# tool. Needs bash, jq, opa (the version the server ran; the script warns on a
# mismatch) and sha256sum or shasum. Works offline. See
# docs/15-decision-replay.md.

# The jq programs are single-quoted on purpose: $k, $r, $i and $f are jq
# variables bound with --arg / --argjson / --slurpfile.
# shellcheck disable=SC2016

set -euo pipefail

EVIDENCE_KIND="aikonos.decision-evidence/v1"
SNAPSHOT_KIND="aikonos.policy-bundle/v1"

QUIET=false
OPA_BIN="${OPA_BIN:-opa}"
EVIDENCE=""

usage() { printf 'usage: %s <evidence.json> [--opa PATH] [--quiet]\n' "$0" >&2; exit 2; }
say() { [[ "${QUIET}" == true ]] || printf '%s\n' "$*"; }
fail() { printf 'MISMATCH: %s\n' "$*" >&2; exit 1; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit "${2:-1}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --opa)   OPA_BIN="${2:?--opa needs a path}"; shift 2 ;;
    --quiet) QUIET=true; shift ;;
    -h|--help) usage ;;
    -*) die "unknown option: $1" 2 ;;
    *) [[ -z "${EVIDENCE}" ]] || usage; EVIDENCE="$1"; shift ;;
  esac
done
[[ -n "${EVIDENCE}" ]] || usage
[[ -f "${EVIDENCE}" ]] || die "evidence file not found: ${EVIDENCE}" 2

command -v jq >/dev/null 2>&1 || die "jq not found on PATH" 2
command -v "${OPA_BIN}" >/dev/null 2>&1 || die "opa not found (install it or pass --opa PATH)" 2
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 | cut -d' ' -f1; }
else
  die "neither sha256sum nor shasum found" 2
fi

# jq -b: no CRLF translation on Windows builds; a no-op elsewhere.
jqr() { jq -b "$@"; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

jqr -e --arg k "${EVIDENCE_KIND}" '.kind == $k' "${EVIDENCE}" >/dev/null \
  || die "not a decision export (kind is not ${EVIDENCE_KIND})"

event_id="$(jqr -r '.event.event_id // ""' "${EVIDENCE}")"
event_type="$(jqr -r '.event.event_type // ""' "${EVIDENCE}")"
jqr '.event.context // {}' "${EVIDENCE}" > "${WORK}/record.json"

query="$(jqr -r '.opa.query // ""' "${WORK}/record.json")"
revision="$(jqr -r '.opa.revision // ""' "${WORK}/record.json")"
[[ -n "${query}" ]] || die "event ${event_id} (${event_type}) carries no OPA evaluation to replay"
[[ "${query}" =~ ^[A-Za-z0-9_]+(/[A-Za-z0-9_]+)*$ ]] || die "invalid query in the record: ${query}"
[[ -n "${revision}" ]] \
  || die "the decision was recorded without a policy revision (OPA was not loading the policy from the broker), so it cannot be replayed"

say "Decision ${event_id}"
say "  kind:       $(jqr -r '.kind // "?"' "${WORK}/record.json")"
say "  outcome:    $(jqr -r '"\(.outcome.decision // "?") (decided by \(.outcome.decided_by // "?"))"' "${WORK}/record.json")"
say "  query:      data.${query//\//.}"
say "  policy:     ${revision}"

# ── 1. The snapshot is the archived policy, unaltered ──────────────────────────
jqr -e --arg r "${revision}" '[.policies[]? | select(.revision == $r and .snapshot != null)] | length == 1' "${EVIDENCE}" >/dev/null \
  || die "the export does not include the snapshot of ${revision}"
jqr --arg r "${revision}" '.policies[] | select(.revision == $r) | .snapshot' "${EVIDENCE}" > "${WORK}/snapshot.json"
jqr -e --arg k "${SNAPSHOT_KIND}" '.kind == $k' "${WORK}/snapshot.json" >/dev/null \
  || die "the snapshot is not of kind ${SNAPSHOT_KIND}"

POLICY_DIR="${WORK}/snapshot-files"
mkdir -p "${POLICY_DIR}"
n_files="$(jqr '.files | length' "${WORK}/snapshot.json")"
[[ "${n_files}" -gt 0 ]] || fail "the snapshot has no files"
i=0
while [[ "${i}" -lt "${n_files}" ]]; do
  path="$(jqr -r --argjson i "${i}" '.files[$i].path' "${WORK}/snapshot.json")"
  want="$(jqr -r --argjson i "${i}" '.files[$i].sha256' "${WORK}/snapshot.json")"
  # Relative, no "..", no backslashes: a crafted export must not write
  # outside the scratch directory.
  if [[ ! "${path}" =~ ^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$ || "/${path}/" == *"/../"* || "/${path}/" == *"/./"* ]]; then
    die "unsafe file path in the snapshot: ${path}"
  fi
  mkdir -p "${POLICY_DIR}/$(dirname "${path}")"
  jqr -j --argjson i "${i}" '.files[$i].content' "${WORK}/snapshot.json" > "${POLICY_DIR}/${path}"
  got="$(sha256 < "${POLICY_DIR}/${path}")"
  [[ "${got}" == "${want}" ]] || fail "snapshot file ${path} does not match its sha256"
  i=$((i + 1))
done
computed="sha256:$(jqr -cjS '{files: ([.files[] | {path, sha256}] | sort_by(.path)), roots: (.roots | sort)}' "${WORK}/snapshot.json" | sha256)"
[[ "${computed}" == "${revision}" ]] || fail "the snapshot's files hash to ${computed}, not ${revision}"
say "  [ok] snapshot: ${n_files} files hash to the recorded revision"

# ── 2. The recorded input is the one evaluated ─────────────────────────────────
jqr '.opa.input' "${WORK}/record.json" > "${WORK}/input.json"
if jqr -e '(.opa.redacted // []) | length > 0' "${WORK}/record.json" >/dev/null; then
  say "  [--] input digest not rechecked: the record redacted"
  jqr -r '.opa.redacted[] | "         - " + .' "${WORK}/record.json" | while IFS= read -r line; do say "${line}"; done
else
  input_digest="sha256:$(jqr -cjS '.' "${WORK}/input.json" | sha256)"
  [[ "${input_digest}" == "$(jqr -r '.opa.input_sha256 // ""' "${WORK}/record.json")" ]] \
    || fail "the recorded input does not match its digest"
  say "  [ok] input matches its recorded digest"
fi

# ── 3. Same policy, same input, same result ────────────────────────────────────
server_version="$(jqr -r '.opa.opa_version // ""' "${WORK}/record.json")"
local_version="$("${OPA_BIN}" version 2>/dev/null | awk '/^Version:/ { print $2; exit }')"
if [[ -n "${server_version}" && "${server_version}" != "${local_version}" ]]; then
  say "  [!!] opa ${local_version:-unknown} here, ${server_version} on the server: a mismatch below may come from the engine, not the policy"
fi

ref="data.${query//\//.}"
"${OPA_BIN}" eval --format json --data "${POLICY_DIR}" --input "${WORK}/input.json" "${ref}" > "${WORK}/eval.json" \
  || die "opa could not evaluate ${ref} against the snapshot"
jqr '.result[0].expressions[0].value // {}' "${WORK}/eval.json" > "${WORK}/replayed.json"
jqr '.opa.result // {}' "${WORK}/record.json" > "${WORK}/recorded.json"

# Sets come back as arrays; compare them order-insensitively.
normalize='walk(if type == "array" then sort else . end)'
if [[ "$(jqr -cS "${normalize}" "${WORK}/recorded.json")" != "$(jqr -cS "${normalize}" "${WORK}/replayed.json")" ]]; then
  printf '  recorded: %s\n  replayed: %s\n' "$(jqr -cS . "${WORK}/recorded.json")" "$(jqr -cS . "${WORK}/replayed.json")" >&2
  fail "evaluating the recorded input against ${revision} does not reproduce the recorded result"
fi
say "  [ok] replay reproduces the recorded result"

# ── The rules that decided ─────────────────────────────────────────────────────
# Of the result fields the broker acts on (the record lists them; every field
# when it does not), each that is true or a non-empty set was produced by one
# or more rule definitions. OPA's trace names each definition that succeeded.
say ""
say "Decided by:"
jqr -c '.opa.fields // null' "${WORK}/record.json" > "${WORK}/fields.json"
jqr -r --slurpfile f "${WORK}/fields.json" '
  to_entries[]
  | select(($f[0] == null) or (.key as $k | $f[0] | index($k)))
  | select(.value == true or ((.value | type) == "array" and (.value | length) > 0) or ((.value | type) == "object" and (.value | length) > 0))
  | .key' "${WORK}/replayed.json" > "${WORK}/decisive.txt"
if [[ ! -s "${WORK}/decisive.txt" ]]; then
  say "  no rule produced a true or non-empty value: the decision is every rule's default (deny unless allowed)"
fi
while IFS= read -r rule; do
  [[ "${rule}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
  "${OPA_BIN}" eval --format json --explain full --data "${POLICY_DIR}" --input "${WORK}/input.json" "${ref}.${rule}" > "${WORK}/trace.json" \
    || die "opa could not trace ${ref}.${rule}"
  value="$(jqr -c --arg r "${rule}" '.[$r]' "${WORK}/replayed.json")"
  # Locations are reported relative to the snapshot (the scratch directory is
  # named snapshot-files; OPA on Windows prints it with backslashes).
  jqr -r '
    [.explanation[]?
      | select(.Op == "Exit" and .ParentID == 0 and (.Node | type) == "object" and (.Node | has("head")))
      | (.Location.file | gsub("\\\\"; "/") | sub("^.*/snapshot-files/"; "")) + ":" + (.Location.row | tostring)]
    | unique | .[]' "${WORK}/trace.json" > "${WORK}/where.txt"
  say "  ${rule} = ${value}"
  while IFS= read -r where; do say "      ${where}"; done < "${WORK}/where.txt"
done < "${WORK}/decisive.txt"

# The broker's own layers after OPA, when one of them set the outcome.
if jqr -e '(.layers // []) | length > 0' "${WORK}/record.json" >/dev/null; then
  say ""
  say "Then changed by the broker (not part of the OPA replay):"
  jqr -r '.layers[] | "  \(.layer) → \(.effect)"' "${WORK}/record.json" | while IFS= read -r line; do say "${line}"; done
fi

say ""
say "MATCH: the recorded result follows from policy ${revision} and the recorded input."
