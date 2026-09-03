#!/usr/bin/env bash
# scripts/tests/git-deploy-hook-verify.test.sh
#
# Red-first assert script for the signature-verification gate added to
# scripts/git-deploy-hook.sh (CP3.2). Self-contained: builds a throwaway git
# repo + throwaway ed25519 SSH signing key in a mktemp dir, and exercises the
# hook's verification path via AIKONOS_DEPLOY_VERIFY_ONLY=1 (skips checkout /
# build / deploy). Never touches the invoking user's real ~/.gitconfig or
# ~/.ssh — HOME is overridden and global/system git config is disabled for
# every invocation below.
#
# Usage: bash scripts/tests/git-deploy-hook-verify.test.sh

set -euo pipefail

HOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/git-deploy-hook.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

export HOME="$WORKDIR/home"
mkdir -p "$HOME"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

REPO="$WORKDIR/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.name "Test Signer"
git -C "$REPO" config user.email "signer@example.com"

# Throwaway ed25519 signing key — never touches ~/.ssh.
KEY="$WORKDIR/id_test_ed25519"
ssh-keygen -t ed25519 -N "" -f "$KEY" -q -C "signer@example.com"

git -C "$REPO" config gpg.format ssh
git -C "$REPO" config user.signingkey "$KEY.pub"

ALLOWED_SIGNERS="$WORKDIR/allowed_signers"
echo "signer@example.com $(cat "$KEY.pub")" > "$ALLOWED_SIGNERS"

ZERO="0000000000000000000000000000000000000000"

# --- Commit graph -------------------------------------------------------
# main:  UNSIGNED_SHA -> SIGNED_SHA
# other (branched off SIGNED_SHA): OTHER_SIGNED_SHA
# other2 (branched off SIGNED_SHA): OTHER_UNSIGNED_SHA

echo "v1" > "$REPO/file.txt"
git -C "$REPO" add file.txt
git -C "$REPO" commit -q -m "unsigned commit" --no-gpg-sign
UNSIGNED_SHA="$(git -C "$REPO" rev-parse HEAD)"

echo "v2" > "$REPO/file.txt"
git -C "$REPO" add file.txt
git -C "$REPO" commit -q -S -m "signed commit"
SIGNED_SHA="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" branch other "$SIGNED_SHA"
git -C "$REPO" branch other2 "$SIGNED_SHA"

git -C "$REPO" checkout -q other
echo "v3" > "$REPO/file.txt"
git -C "$REPO" add file.txt
git -C "$REPO" commit -q -S -m "other: signed commit"
OTHER_SIGNED_SHA="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" checkout -q other2
echo "v4" > "$REPO/file.txt"
git -C "$REPO" add file.txt
git -C "$REPO" commit -q -m "other2: unsigned commit" --no-gpg-sign
OTHER_UNSIGNED_SHA="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" checkout -q main

GIT_DIR="$REPO/.git"

pass=0
fail=0
LAST_OUT=""
LAST_RC=0

# run_case <name> <expect: 0|nonzero> <stdin-content> [env assignments...]
run_case() {
  local name="$1" expect="$2" stdin_content="$3"
  shift 3
  local out rc
  set +e
  # printf '%s\n' (not '%s') — stdin_content was built via $(...), which
  # strips the trailing newline off its last line; without restoring it,
  # bash's `while read` silently drops the final stdin record (classic
  # missing-trailing-newline pitfall), corrupting multi-ref test fixtures.
  out="$(printf '%s\n' "$stdin_content" \
    | env -i \
        PATH="$PATH" \
        HOME="$HOME" \
        GIT_CONFIG_GLOBAL="$GIT_CONFIG_GLOBAL" \
        GIT_CONFIG_NOSYSTEM="$GIT_CONFIG_NOSYSTEM" \
        GIT_DIR="$GIT_DIR" \
        AIKONOS_DEPLOY_VERIFY_ONLY=1 \
        "$@" \
        bash "$HOOK" 2>&1)"
  rc=$?
  set -e
  LAST_OUT="$out"
  LAST_RC="$rc"

  local ok=0
  if [ "$expect" = "0" ]; then
    [ "$rc" -eq 0 ] && ok=1
  else
    [ "$rc" -ne 0 ] && ok=1
  fi

  if [ "$ok" -eq 1 ]; then
    echo "PASS: $name (exit=$rc)"
    pass=$((pass + 1))
  else
    echo "FAIL: $name (exit=$rc, expected ${expect})"
    echo "--- output ---"
    echo "$out"
    echo "--------------"
    fail=$((fail + 1))
  fi
  echo "$out"
}

# assert_contains <name> <pattern> — greps LAST_OUT (fixed string).
assert_contains() {
  local name="$1" pattern="$2"
  if grep -qF -- "$pattern" <<<"$LAST_OUT"; then
    echo "PASS: $name (found: \"$pattern\")"
    pass=$((pass + 1))
  else
    echo "FAIL: $name (missing: \"$pattern\")"
    echo "--- output ---"
    echo "$LAST_OUT"
    echo "--------------"
    fail=$((fail + 1))
  fi
}

echo "=== signed + allowed-signers file present -> pass ==="
run_case "signed+file=pass" 0 "$(printf '%s %s refs/heads/main\n' "$ZERO" "$SIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "signed+file=pass: signature verified message" "Signature verified for $SIGNED_SHA"
assert_contains "signed+file=pass: deploy sha printed" "Verified deploy sha: $SIGNED_SHA"

echo "=== unsigned + allowed-signers file present -> refuse ==="
run_case "unsigned+file=refuse" nonzero "$(printf '%s %s refs/heads/main\n' "$ZERO" "$UNSIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "unsigned+file=refuse: FAILED message" "Signature verification FAILED"
assert_contains "unsigned+file=refuse: sign-instructions block" "git config gpg.format ssh"

echo "=== allowed-signers file absent -> warn and proceed ==="
run_case "nofile=warn+proceed" 0 "$(printf '%s %s refs/heads/main\n' "$ZERO" "$UNSIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$WORKDIR/does-not-exist"
assert_contains "nofile=warn+proceed: WARNING message" "WARNING"

echo "=== REQUIRE_SIGNED=false -> skip verification entirely ==="
run_case "require-signed-false=skip" 0 "$(printf '%s %s refs/heads/main\n' "$ZERO" "$UNSIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS" \
  AIKONOS_DEPLOY_REQUIRE_SIGNED=false
assert_contains "require-signed-false=skip: skipping message" "skipping signature verification"

echo "=== REQUIRE_SIGNED=true + allowed-signers file absent -> refuse (fail closed) ==="
run_case "require-signed-true+nofile=refuse" nonzero "$(printf '%s %s refs/heads/main\n' "$ZERO" "$SIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$WORKDIR/does-not-exist" \
  AIKONOS_DEPLOY_REQUIRE_SIGNED=true

echo "=== (a) multi-ref: unsigned deploy-branch + signed decoy ref -> REFUSED ==="
run_case "multiref-unsigned-main+signed-decoy=refuse" nonzero \
  "$(printf '%s %s refs/heads/main\n%s %s refs/heads/other\n' "$ZERO" "$UNSIGNED_SHA" "$ZERO" "$OTHER_SIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "(a): decoy ref itself was verified (signed, passes)" "Signature verified for $OTHER_SIGNED_SHA"
assert_contains "(a): deploy-branch ref failed verification" "Signature verification FAILED for $UNSIGNED_SHA"

echo "=== (b) multi-ref: signed deploy-branch + unsigned other-branch -> REFUSED ==="
run_case "multiref-signed-main+unsigned-other=refuse" nonzero \
  "$(printf '%s %s refs/heads/main\n%s %s refs/heads/other2\n' "$ZERO" "$SIGNED_SHA" "$ZERO" "$OTHER_UNSIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "(b): deploy-branch ref verified fine on its own" "Signature verified for $SIGNED_SHA"
assert_contains "(b): other-branch ref failed verification" "Signature verification FAILED for $OTHER_UNSIGNED_SHA"

echo "=== (c) deletion line alongside signed deploy-branch update -> passes, deploys signed sha ==="
run_case "deletion+signed-main=pass" 0 \
  "$(printf '%s %s refs/heads/main\n%s %s refs/heads/other\n' "$ZERO" "$SIGNED_SHA" "$OTHER_SIGNED_SHA" "$ZERO")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "(c): deleted ref logged, not verified" "ref deleted, skipping verification: refs/heads/other"
assert_contains "(c): deploy-branch ref verified" "Signature verified for $SIGNED_SHA"

echo "=== (d) VERIFY_ONLY prints exactly the pushed deploy-branch sha ==="
run_case "verify-only-sha-echo" 0 "$(printf '%s %s refs/heads/main\n' "$ZERO" "$SIGNED_SHA")" \
  AIKONOS_DEPLOY_ALLOWED_SIGNERS="$ALLOWED_SIGNERS"
assert_contains "(d): printed deploy sha matches pushed sha" "Verified deploy sha: $SIGNED_SHA (refs/heads/main)"

echo "=== (e) unhealthy_services: which services count as a failed deploy ==="
# WHY: a real deploy aborted mid-recreate and left the broker container in
# "created" — never running. `up -d` had already returned, so nothing noticed
# until someone opened the app. This function is what turns that into a failed
# push, so it must flag a stuck/crashed service and must NOT flag the one-shots
# (migrate, dev-ca-mint, vault-init, workspace-init, openfga-migrate) that are
# supposed to exit 0.
(
  # shellcheck source=/dev/null
  AIKONOS_DEPLOY_LOAD_ONLY=1 . "$HOOK"

  fixture() {
    printf 'broker|created|Created\n'
    printf 'agent-gateway|running|Up 2 minutes (healthy)\n'
    printf 'migrate|exited|Exited (0) 2 minutes ago\n'
    printf 'vault-init|exited|Exited (0) 2 minutes ago\n'
    printf 'opa|exited|Exited (1) 5 seconds ago\n'
    printf 'webui|restarting|Restarting (1) 2 seconds ago\n'
  }

  out="$(fixture | unhealthy_services)"

  for want in "broker" "opa" "webui"; do
    if grep -q "^${want} " <<<"$out"; then
      echo "PASS: (e): ${want} flagged as unhealthy"
    else
      echo "FAIL: (e): ${want} should be flagged — got: $out"
      exit 1
    fi
  done

  for unwanted in "agent-gateway" "migrate" "vault-init"; do
    if grep -q "^${unwanted} " <<<"$out"; then
      echo "FAIL: (e): ${unwanted} must NOT be flagged — got: $out"
      exit 1
    fi
    echo "PASS: (e): ${unwanted} correctly not flagged"
  done

  # A fully healthy stack must produce no output at all, or every deploy fails.
  healthy="$(printf 'broker|running|Up 1 minute\nmigrate|exited|Exited (0) 1 minute ago\n' | unhealthy_services)"
  if [ -n "$healthy" ]; then
    echo "FAIL: (e): healthy stack must yield empty output — got: $healthy"
    exit 1
  fi
  echo "PASS: (e): healthy stack yields no findings"
) || fail=$((fail + 1))
# The subshell prints its own PASS lines; count it as one aggregate case.
if [ "$fail" -eq 0 ]; then pass=$((pass + 1)); fi

echo ""
echo "=== summary: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
