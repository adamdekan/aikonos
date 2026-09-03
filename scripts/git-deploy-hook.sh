#!/usr/bin/env bash
# scripts/git-deploy-hook.sh
#
# Git post-receive hook: deploy Aikonos to ~/apps/aikonos on git push.
#
# INSTALL (run once on the on-prem host):
#   cp scripts/git-deploy-hook.sh ~/repos/aikonos.git/hooks/post-receive
#   chmod +x ~/repos/aikonos.git/hooks/post-receive
#
# FIRST-TIME SETUP (before the first push can succeed):
#   mkdir -p ~/apps/aikonos
#   cp deploy/compose/.env.onprem.example ~/apps/aikonos/.env
#   # Edit ~/apps/aikonos/.env — fill all placeholders and rotate secrets.
#   # Then seed the stack manually (see deploy/onprem/README.md).
#
# ROLLING DEPLOY (every subsequent push):
#   git push on-prem main     # or whatever branch
#   # The hook rebuilds webui (OIDC baked at build time) and runs compose up.
#
# Override the deploy path:
#   AIKONOS_DEPLOY_DIR=/custom/path git push ...
#
# SIGNED DEPLOYS (adoption path — see deploy/onprem/README.md "Signing setup"):
#   Before checkout, the pushed ref's tip commit (or tag) is verified against an
#   allowed-signers file. Until that file exists at AIKONOS_DEPLOY_ALLOWED_SIGNERS
#   (default /etc/aikonos/allowed_signers), the hook warns loudly and deploys
#   unverified — create the file to start enforcing.
#
#   AIKONOS_DEPLOY_ALLOWED_SIGNERS=/path/to/allowed_signers   # default: /etc/aikonos/allowed_signers
#   AIKONOS_DEPLOY_REQUIRE_SIGNED=false   # skip verification entirely
#   AIKONOS_DEPLOY_REQUIRE_SIGNED=true    # fail closed if the allowed-signers file is missing
#   AIKONOS_DEPLOY_VERIFY_ONLY=1          # run verification only, skip checkout/build/deploy (testing)

set -euo pipefail

DEPLOY_DIR="${AIKONOS_DEPLOY_DIR:-$HOME/apps/aikonos}"
GIT_REPO_DIR="${GIT_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"  # git sets GIT_DIR in hooks

ALLOWED_SIGNERS_FILE="${AIKONOS_DEPLOY_ALLOWED_SIGNERS:-/etc/aikonos/allowed_signers}"
REQUIRE_SIGNED="${AIKONOS_DEPLOY_REQUIRE_SIGNED:-}"

# The bare repo's symbolic HEAD (e.g. refs/heads/main) — the branch this hook
# deploys. Falls back to refs/heads/main if HEAD is somehow not a symref.
deploy_branch_ref() {
  git --git-dir="$1" symbolic-ref -q HEAD || echo "refs/heads/main"
}

# verify_signed_ref <git-dir> <sha> <refname>
# Returns 0 to proceed with deploy, 1 to refuse. Self-contained and callable
# standalone (see scripts/tests/git-deploy-hook-verify.test.sh) — reads only
# ALLOWED_SIGNERS_FILE / REQUIRE_SIGNED from the environment.
verify_signed_ref() {
  local git_dir="$1" sha="$2" refname="$3"

  if [ "$REQUIRE_SIGNED" = "false" ]; then
    echo "==> [aikonos-deploy] AIKONOS_DEPLOY_REQUIRE_SIGNED=false — skipping signature verification."
    return 0
  fi

  if [ ! -f "$ALLOWED_SIGNERS_FILE" ]; then
    if [ "$REQUIRE_SIGNED" = "true" ]; then
      echo ""
      echo "ERROR: [aikonos-deploy] AIKONOS_DEPLOY_REQUIRE_SIGNED=true but no allowed-signers"
      echo "       file found at: $ALLOWED_SIGNERS_FILE"
      echo ""
      echo "  Create it (one line per trusted signer):"
      echo "    <principal> ssh-ed25519 AAAA..."
      echo ""
      echo "  Or unset AIKONOS_DEPLOY_REQUIRE_SIGNED to fall back to the adoption warning path."
      echo ""
      return 1
    fi
    echo ""
    echo "WARNING: [aikonos-deploy] No allowed-signers file at $ALLOWED_SIGNERS_FILE —"
    echo "         signature verification is NOT enforced; deploying $sha unverified."
    echo "         Create the file to start enforcing signed deploys (see deploy/onprem/README.md)."
    echo ""
    return 0
  fi

  local verify_out
  verify_out="$(mktemp)"
  local ok=1
  case "$refname" in
    refs/tags/*)
      if git -c gpg.ssh.allowedSignersFile="$ALLOWED_SIGNERS_FILE" --git-dir="$git_dir" \
        verify-tag "$sha" >"$verify_out" 2>&1; then
        ok=0
      fi
      ;;
    *)
      if git -c gpg.ssh.allowedSignersFile="$ALLOWED_SIGNERS_FILE" --git-dir="$git_dir" \
        verify-commit "$sha" >"$verify_out" 2>&1; then
        ok=0
      fi
      ;;
  esac

  if [ "$ok" -ne 0 ]; then
    echo ""
    echo "ERROR: [aikonos-deploy] Signature verification FAILED for $sha ($refname) — deploy refused."
    echo ""
    sed 's/^/    /' "$verify_out"
    rm -f "$verify_out"
    echo ""
    echo "  To sign your commits with an SSH key:"
    echo "    git config gpg.format ssh"
    echo "    git config user.signingkey ~/.ssh/id_ed25519.pub"
    echo "    git config commit.gpgsign true      # or pass -S per commit"
    echo "    git commit -S -m '...'              # or: git tag -s ..."
    echo ""
    echo "  Ask the deploy operator to add your public key to:"
    echo "    $ALLOWED_SIGNERS_FILE"
    echo ""
    return 1
  fi

  rm -f "$verify_out"
  echo "==> [aikonos-deploy] Signature verified for $sha ($refname)."
  return 0
}

# unhealthy_services
# Reads `docker compose ps -a --format '{{.Service}}|{{.State}}|{{.Status}}'` on
# stdin and prints one line per service that should be up but is not. Empty
# output means the stack is healthy. Self-contained and callable standalone (see
# scripts/tests/git-deploy-hook-verify.test.sh), same as verify_signed_ref.
#
# One-shot services (migrate, dev-ca-mint, vault-init, workspace-init,
# openfga-migrate) are supposed to exit, so a zero exit code is success for them.
# Anything else — a non-zero exit, a container stuck in "created" by a failed
# rename, a restart loop — is a real failure.
unhealthy_services() {
  awk -F'|' '$2 == "running" { next }
             $2 == "exited" && $3 ~ /\(0\)/ { next }
             { print $1" — "$2" ("$3")" }'
}

# process_pushed_refs <deploy-branch-refname>
# Reads post-receive stdin ("<old> <new> <refname>" per line — one per
# updated ref) and verifies EVERY updated ref; a single unsigned/unverified
# ref refuses the whole push (a signed decoy ref must never wave through an
# unsigned deploy-branch update). Deleted refs (new sha is all zeros) are
# skipped — nothing to verify, nothing to deploy from. Sets the global
# DEPLOY_SHA to the deploy branch's new sha if the push touched it, else "".
# Falls back to the deploy branch's current tip when stdin carries no lines
# (manual/test invocation, mirroring old read_pushed_ref behavior).
process_pushed_refs() {
  local deploy_ref="$1"
  local old new refname
  local any_line=0 failed=0
  DEPLOY_SHA=""

  while read -r old new refname; do
    any_line=1
    if [[ "$new" =~ ^0+$ ]]; then
      echo "==> [aikonos-deploy] ref deleted, skipping verification: $refname"
      continue
    fi
    if ! verify_signed_ref "$GIT_REPO_DIR" "$new" "$refname"; then
      failed=1
      continue
    fi
    if [ "$refname" = "$deploy_ref" ]; then
      DEPLOY_SHA="$new"
    fi
  done

  if [ "$any_line" -eq 0 ]; then
    local head_sha
    head_sha="$(git --git-dir="$GIT_REPO_DIR" rev-parse "$deploy_ref" 2>/dev/null \
      || git --git-dir="$GIT_REPO_DIR" rev-parse HEAD)"
    if verify_signed_ref "$GIT_REPO_DIR" "$head_sha" "$deploy_ref"; then
      DEPLOY_SHA="$head_sha"
    else
      failed=1
    fi
  fi

  [ "$failed" -eq 0 ]
}

# Sourcing seam: `AIKONOS_DEPLOY_LOAD_ONLY=1 . git-deploy-hook.sh` defines the
# functions above and stops, so they can be exercised directly instead of
# through a whole simulated push (see scripts/tests/git-deploy-hook-verify.test.sh).
if [ "${AIKONOS_DEPLOY_LOAD_ONLY:-}" = "1" ]; then
  return 0 2>/dev/null || exit 0
fi

DEPLOY_BRANCH_REF="$(deploy_branch_ref "$GIT_REPO_DIR")"

if ! process_pushed_refs "$DEPLOY_BRANCH_REF"; then
  echo ""
  echo "ERROR: [aikonos-deploy] One or more pushed refs failed signature verification — push refused."
  echo ""
  exit 1
fi

if [ "${AIKONOS_DEPLOY_VERIFY_ONLY:-}" = "1" ]; then
  if [ -n "$DEPLOY_SHA" ]; then
    echo "==> [aikonos-deploy] Verified deploy sha: $DEPLOY_SHA ($DEPLOY_BRANCH_REF)"
  else
    echo "==> [aikonos-deploy] Push did not touch deploy branch $DEPLOY_BRANCH_REF — nothing to deploy."
  fi
  exit 0
fi

if [ -z "$DEPLOY_SHA" ]; then
  echo "==> [aikonos-deploy] Push did not update $DEPLOY_BRANCH_REF — all pushed refs verified, nothing to deploy."
  exit 0
fi

echo "==> [aikonos-deploy] Checking out $DEPLOY_SHA to $DEPLOY_DIR"
mkdir -p "$DEPLOY_DIR"

# Checkout the verified deploy-branch sha into the work tree (never bare
# HEAD — HEAD tracks whatever the bare repo's default branch points to,
# which must be bound to the sha we actually verified, not re-derived).
# .env is not tracked in git, so it is never overwritten.
GIT_WORK_TREE="$DEPLOY_DIR" GIT_DIR="$GIT_REPO_DIR" git checkout -f "$DEPLOY_SHA"

cd "$DEPLOY_DIR"

# Guard: refuse to start without secrets in place.
if [ ! -f .env ]; then
  echo ""
  echo "ERROR: $DEPLOY_DIR/.env not found — deploy aborted."
  echo ""
  echo "  1. Copy the template:  cp deploy/compose/.env.onprem.example $DEPLOY_DIR/.env"
  echo "  2. Fill all <...> placeholders and rotate every *-CHANGE-ME secret."
  echo "  3. Run the one-time seed commands (see deploy/onprem/README.md)."
  echo "  4. Re-push."
  echo ""
  exit 1
fi

# Rebuild ALL service images, not just webui. `up -d` never rebuilds, so a
# webui-only build shipped stale gateway/broker binaries on every deploy — a
# code push looked deployed while the backend silently ran old images (proto
# fields dropped at encode, missing RPCs). The layer cache keeps unchanged
# services near-free; webui always rebuilds anyway (OIDC baked in by Vite).
echo "==> [aikonos-deploy] Building images..."
docker compose build

# Bring up / update all services. `up -d` creates missing services, recreates
# any service whose image or config changed, and leaves unchanged services alone.
#
# Compose renames a container to <short-id>_<name> before recreating it. If that
# step is interrupted the daemon can lose track of the rename, and every later
# `up -d` aborts with "No such container: <short-id>_<name>". Because services
# are recreated in dependency order, the abort takes out everything after the
# failing one — a real deploy died on broker and left the stack with no broker
# container at all, while the push still looked like it had gone through.
#
# A plain retry adopts the orphaned container and succeeds, so try once more
# before giving up. Deliberately NOT `--force-recreate` as a fallback: that
# recreates every service including vault, which on-prem requires a manual
# unseal — the recovery would cause a worse outage than the failure.
echo "==> [aikonos-deploy] Starting / updating the stack..."
if ! docker compose up -d; then
  echo "==> [aikonos-deploy] 'up -d' failed — retrying once."
  docker compose up -d
fi

# Assert the stack actually came up. A zero exit from `up -d` is not proof: a
# service that dies on bad config is reported as started, and a container left
# in "created" by the rename failure above never runs at all. Without this check
# a broken deploy is silent until someone opens the app.
echo "==> [aikonos-deploy] Verifying services..."
sleep 5
NOT_RUNNING="$(docker compose ps -a --format '{{.Service}}|{{.State}}|{{.Status}}' | unhealthy_services)"
if [ -n "$NOT_RUNNING" ]; then
  echo ""
  echo "ERROR: [aikonos-deploy] Deploy INCOMPLETE — these services are not running:"
  echo "$NOT_RUNNING" | sed 's/^/         /'
  echo ""
  echo "  The code is checked out and the images are built, but the stack is only"
  echo "  partly up. Inspect and finish it on the host:"
  echo ""
  echo "    cd $DEPLOY_DIR"
  echo "    docker compose ps -a"
  echo "    docker compose logs --tail=50 <service>"
  echo "    docker compose up -d                     # usually enough"
  echo "    docker compose up -d --force-recreate <service>   # if it stays stuck"
  echo ""
  exit 1
fi
echo "    all services running"

echo ""
echo "==> [aikonos-deploy] Done."
echo "    Status:  docker compose ps"
echo "    Logs:    docker compose logs -f --tail=50"
echo "    Smoke:   bash scripts/compose-verify.sh"
echo ""
