#!/usr/bin/env bash
# scripts/tests/compose-backup.test.sh
#
# compose-backup.sh's guards (F109): required credentials, no secret on any
# docker argv (ps), init.env left out of the vault archive, and a 0700 output
# directory that is never re-moded when the caller supplied it. A stub `docker`
# records its argv instead of running anything. The script runs from a copy in
# a temp tree, so it never sources a real .env.
#
# Usage: bash scripts/tests/compose-backup.test.sh

set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/compose-backup.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/scripts" "$WORK/bin"
cp "$SRC" "$WORK/scripts/compose-backup.sh"
LOG="$WORK/docker.argv"
cat > "$WORK/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
printf '%s\n' "${PGPASSWORD:-}" >> "$DOCKER_LOG.pgpassword"
EOF
chmod +x "$WORK/bin/docker"

PW='pg-Secret-123'
MPW='minio-Secret-456'
CREDS=(POSTGRES_PASSWORD="$PW" MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD="$MPW")
RUN_ENV=()
pass=0
fail=0
rc=0

# run <subcommand> [args...]: one script run against the stub, with only
# RUN_ENV in its environment; sets rc.
run() {
  : > "$LOG"
  : > "$LOG.pgpassword"
  rc=0
  env -i PATH="$WORK/bin:$PATH" HOME="$WORK" DOCKER_LOG="$LOG" ${RUN_ENV[@]+"${RUN_ENV[@]}"} \
    bash "$WORK/scripts/compose-backup.sh" "$@" > "$WORK/out.txt" 2>&1 || rc=$?
}
ok() {
  local name="$1"
  shift
  if "$@"; then echo "PASS: $name"; pass=$((pass + 1))
  else echo "FAIL: $name"; sed 's/^/    /' "$WORK/out.txt"; fail=$((fail + 1)); fi
}
mode_is() { [ -n "$(find "$1" -maxdepth 0 -perm "$2")" ]; }
no_secret_on_argv() { ! grep -qF -e "$PW" -e "$MPW" "$LOG"; }

echo "=== credentials unset -> refused before any docker call ==="
run backup "$WORK/b1"
ok "no credentials: non-zero exit" test "$rc" -ne 0
ok "no credentials: docker never called" test ! -s "$LOG"

echo "=== backup into a new directory ==="
RUN_ENV=("${CREDS[@]}")
run backup "$WORK/b2"
ok "backup: exit 0" test "$rc" -eq 0
ok "backup: new output dir is 0700" mode_is "$WORK/b2" 700
ok "backup: postgres dump is 0600" mode_is "$WORK/b2/postgres-all.sql" 600
ok "backup: no secret on any docker argv" no_secret_on_argv
ok "backup: postgres password passed by name" grep -qF -- "-e PGPASSWORD postgres pg_dumpall" "$LOG"
ok "backup: postgres password reaches compose by env" grep -qxF "$PW" "$LOG.pgpassword"
ok "backup: vault archive excludes init.env" grep -qF -- "--exclude=./init.env" "$LOG"

echo "=== existing caller-supplied 0755 directory -> refused, left as is ==="
mkdir -m 755 "$WORK/b3"
run backup "$WORK/b3"
ok "existing 0755 dir: non-zero exit" test "$rc" -ne 0
ok "existing 0755 dir: mode untouched" mode_is "$WORK/b3" 755
ok "existing 0755 dir: docker never called" test ! -s "$LOG"

echo "=== existing caller-supplied 0700 directory -> accepted ==="
mkdir -m 700 "$WORK/b4"
run backup "$WORK/b4"
ok "existing 0700 dir: exit 0" test "$rc" -eq 0

echo "=== restore ==="
run restore "$WORK/b2" --yes
ok "restore: exit 0" test "$rc" -eq 0
ok "restore: no secret on any docker argv" no_secret_on_argv
ok "restore: postgres password passed by name" grep -qF -- "-e PGPASSWORD postgres psql" "$LOG"
ok "restore: postgres password reaches compose by env" grep -qxF "$PW" "$LOG.pgpassword"

echo ""
echo "=== summary: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
