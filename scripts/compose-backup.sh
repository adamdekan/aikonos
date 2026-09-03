#!/usr/bin/env bash
# scripts/compose-backup.sh
#
# Backup and restore the durable compose stores:
#   - Postgres (aikonos + openfga databases)
#   - MinIO WORM audit bucket
#   - workspace-data volume
#   - obs-archive volume
#   - vault-data volume (azure/onprem overlays only — skipped if absent, e.g. local dev)
#
# Usage:
#   compose-backup.sh backup  [OUTPUT_DIR]
#   compose-backup.sh restore  BACKUP_DIR [--yes]
#
# OUTPUT_DIR defaults to ./backups/<timestamp>/
# --yes skips the confirmation prompt on restore.
#
# Restore ordering (load-bearing):
#   1. Postgres dumps first — broker and OpenFGA read this on startup.
#   2. MinIO bucket second — audit chain head depends on object metadata.
#   3. Workspace volume third — agents read this at runtime, not at startup.
#   4. obs-archive volume — no load-bearing order; read only by the (optional)
#      obs profile, never by broker startup.
#   5. vault-data volume (azure/onprem only) — restore before starting `vault`;
#      it comes back sealed and needs a manual unseal (docs/OPS-RUNBOOK.md)
#      before the broker's Vault client can authenticate.
#   Start broker + other apps only AFTER all steps complete.
#
#   Before starting the broker, also reconcile AIKONOS_POLICY_OPENFGA_STORE_ID
#   in .env against the restored backup's value: the OpenFGA store id is
#   seed-written at first `compose:seed`, not re-derivable from the Postgres
#   dump alone, and a mismatch makes the broker query the wrong (or a
#   nonexistent) FGA store.
#
# Volume snapshot (coarse fallback):
#   docker run --rm -v "${COMPOSE_PROJECT_NAME:-aikonos}_postgres-data:/data" -v "$OUT":/out alpine \
#     tar czf /out/postgres-data.tgz -C /data .
#   Repeat for minio-data, workspace-data, obs-archive (same project-prefixed name).
#   Useful when a pg_dump is not feasible, but does not give a consistent logical backup.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

usage() {
  cat >&2 <<'EOF'
Usage:
  compose-backup.sh backup  [OUTPUT_DIR]
  compose-backup.sh restore  BACKUP_DIR [--yes]

Subcommands:
  backup   Dump Postgres (aikonos + openfga), mirror MinIO audit bucket,
           and tar the workspace-data and obs-archive volumes into OUTPUT_DIR.
           OUTPUT_DIR defaults to ./backups/<timestamp>/ inside the repo.

  restore  Load the dumps back into a fresh Postgres, mirror the bucket,
           and untar the workspace and obs-archive volumes from BACKUP_DIR.
           Requires --yes (or interactive confirmation) to proceed.
           Start the broker and other apps only AFTER restore completes.
           Also reconcile AIKONOS_POLICY_OPENFGA_STORE_ID in .env from the
           backup — it is seed-written, not re-derivable.

Environment (read from .env if present):
  COMPOSE_FILE        path to compose.yaml (default: repo-root/compose.yaml)
  POSTGRES_USER       (default: aikonos)
  POSTGRES_PASSWORD   (default: dev-password-change-me)
  POSTGRES_DB         (default: aikonos)
  MINIO_ROOT_USER     (default: minioadmin)
  MINIO_ROOT_PASSWORD (default: minioadmin)
  AIKONOS_AUDIT_BUCKET (default: aikonos-audit)
EOF
  exit 1
}

log() { printf '[compose-backup] %s\n' "$*"; }
die() { printf '[compose-backup] ERROR: %s\n' "$*" >&2; exit 1; }

# Load .env if present (best-effort; variables already set in env win).
load_dotenv() {
  local env_file="${REPO_ROOT}/.env"
  if [[ -f "${env_file}" ]]; then
    # Export only lines that are KEY=VALUE (skip comments and blanks).
    set -a
    # shellcheck source=/dev/null
    source <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "${env_file}" || true)
    set +a
  fi
}

# ---------------------------------------------------------------------------
# Config (with defaults)
# ---------------------------------------------------------------------------

config_defaults() {
  COMPOSE_FILE="${COMPOSE_FILE:-${REPO_ROOT}/compose.yaml}"
  PG_USER="${POSTGRES_USER:-aikonos}"
  PG_PASSWORD="${POSTGRES_PASSWORD:-dev-password-change-me}"
  # PG_DB is not used directly: pg_dumpall captures all databases.
  # POSTGRES_DB is documented in .env.local.example for reference only.
  MINIO_USER="${MINIO_ROOT_USER:-minioadmin}"
  MINIO_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
  AUDIT_BUCKET="${AIKONOS_AUDIT_BUCKET:-aikonos-audit}"
}

# Compose command — honour COMPOSE_FILE env.
dc() {
  docker compose -f "${COMPOSE_FILE}" "$@"
}

# ---------------------------------------------------------------------------
# backup
# ---------------------------------------------------------------------------

cmd_backup() {
  local out_dir="${1:-${REPO_ROOT}/backups/$(date +%Y%m%dT%H%M%S)}"
  mkdir -p "${out_dir}"
  out_dir="$(cd "${out_dir}" && pwd)"

  log "Backup destination: ${out_dir}"

  # -- 1. Postgres: pg_dumpall for a consistent cross-DB dump ---------------
  log "Dumping Postgres (aikonos + openfga)..."
  # pg_dumpall captures all databases, roles, tablespaces in one file.
  # PGPASSWORD is consumed by psql/pg_dumpall inside the container.
  dc exec -T \
    -e PGPASSWORD="${PG_PASSWORD}" \
    postgres \
    pg_dumpall -U "${PG_USER}" \
    > "${out_dir}/postgres-all.sql"
  log "  wrote ${out_dir}/postgres-all.sql ($(du -sh "${out_dir}/postgres-all.sql" | cut -f1))"

  # -- 2. MinIO audit bucket -------------------------------------------------
  log "Mirroring MinIO audit bucket '${AUDIT_BUCKET}'..."
  local bucket_out="${out_dir}/minio-${AUDIT_BUCKET}"
  mkdir -p "${bucket_out}"
  # Run mc (MinIO client) as a one-shot container, sharing the network so it
  # can reach the minio service by its compose service name.
  docker run --rm \
    --network "${COMPOSE_PROJECT_NAME:-aikonos}_backend" \
    -v "${bucket_out}:/out" \
    -e MC_ALIAS_LOCAL_URL="http://minio:9000" \
    -e MC_ALIAS_LOCAL_ACCESS_KEY="${MINIO_USER}" \
    -e MC_ALIAS_LOCAL_SECRET_KEY="${MINIO_PASSWORD}" \
    --entrypoint /bin/sh \
    minio/mc:latest \
    -c "mc alias set local http://minio:9000 \"${MINIO_USER}\" \"${MINIO_PASSWORD}\" --quiet && \
           mc mirror --preserve local/${AUDIT_BUCKET} /out/ --quiet" \
    || die "MinIO mirror step failed — audit bucket backup is incomplete or empty; aborting to avoid a silent data-durability gap"
  log "  wrote ${bucket_out}/"

  # -- 3. workspace-data volume ----------------------------------------------
  # Volume names are compose-project-prefixed, same derivation as the MinIO
  # network above — a bare "workspace-data" mount silently creates and tars
  # an empty, wrongly-named volume instead of the real one.
  log "Archiving workspace-data volume..."
  docker run --rm \
    -v "${COMPOSE_PROJECT_NAME:-aikonos}_workspace-data:/src:ro" \
    -v "${out_dir}:/out" \
    alpine \
    tar czf /out/workspace.tgz -C /src .
  log "  wrote ${out_dir}/workspace.tgz ($(du -sh "${out_dir}/workspace.tgz" | cut -f1))"

  # -- 4. obs-archive volume --------------------------------------------------
  log "Archiving obs-archive volume..."
  docker run --rm \
    -v "${COMPOSE_PROJECT_NAME:-aikonos}_obs-archive:/src:ro" \
    -v "${out_dir}:/out" \
    alpine \
    tar czf /out/obs-archive.tgz -C /src .
  log "  wrote ${out_dir}/obs-archive.tgz ($(du -sh "${out_dir}/obs-archive.tgz" | cut -f1))"

  # -- 5. vault-data volume (azure/onprem overlays only) ---------------------
  # Absent on local dev (Vault stays -dev/in-memory, no volume) — skip, not an
  # error. Vault should be sealed or stopped for a consistent file-storage
  # snapshot; this script does not stop it for you.
  local vault_vol="${COMPOSE_PROJECT_NAME:-aikonos}_vault-data"
  if docker volume inspect "${vault_vol}" >/dev/null 2>&1; then
    log "Archiving vault-data volume..."
    log "  (ensure Vault is sealed or stopped for a consistent snapshot)"
    docker run --rm \
      -v "${vault_vol}:/src:ro" \
      -v "${out_dir}:/out" \
      alpine \
      tar czf /out/vault-data.tgz -C /src .
    log "  wrote ${out_dir}/vault-data.tgz ($(du -sh "${out_dir}/vault-data.tgz" | cut -f1))"
  else
    log "vault-data volume not found (local dev or Vault dev-mode) — skipping."
  fi

  # -- Summary ---------------------------------------------------------------
  log "Backup complete. Artifacts:"
  ls -lh "${out_dir}/"
}

# ---------------------------------------------------------------------------
# restore
# ---------------------------------------------------------------------------

cmd_restore() {
  local backup_dir="${1:-}"
  local force=0

  shift || true
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes|-y) force=1 ;;
      *) die "Unknown option: $1" ;;
    esac
    shift
  done

  [[ -n "${backup_dir}" ]] || die "BACKUP_DIR is required for restore. Run with --help for usage."
  [[ -d "${backup_dir}" ]] || die "BACKUP_DIR '${backup_dir}' does not exist."
  backup_dir="$(cd "${backup_dir}" && pwd)"

  # Confirm unless --yes supplied.
  if [[ "${force}" -eq 0 ]]; then
    printf '[compose-backup] WARNING: restore will overwrite all data in the running containers.\n'
    printf '  Backup dir: %s\n' "${backup_dir}"
    printf '  Proceed? (yes/no): '
    read -r answer
    [[ "${answer}" == "yes" ]] || { log "Aborted."; exit 0; }
  fi

  # -- Ordering note (see file header) --------------------------------------
  log "=== Restore ordering: Postgres, MinIO, workspace, obs-archive ==="
  log "=== Reconcile AIKONOS_POLICY_OPENFGA_STORE_ID in .env before starting the broker. ==="
  log "=== Start broker + apps only AFTER this script completes.        ==="

  # -- 1. Postgres restore ---------------------------------------------------
  local pg_dump="${backup_dir}/postgres-all.sql"
  [[ -f "${pg_dump}" ]] || die "postgres-all.sql not found in ${backup_dir}"

  log "Restoring Postgres from ${pg_dump}..."
  # pg_dumpall output is loaded via psql against the postgres (superuser) DB.
  # ON_ERROR_STOP=1 aborts on first error so partial restores are visible.
  dc exec -T \
    -e PGPASSWORD="${PG_PASSWORD}" \
    postgres \
    psql -U "${PG_USER}" -d postgres -v ON_ERROR_STOP=1 \
    < "${pg_dump}"
  log "  Postgres restore complete."

  # -- 2. MinIO bucket restore -----------------------------------------------
  local bucket_in="${backup_dir}/minio-${AUDIT_BUCKET}"
  if [[ -d "${bucket_in}" ]]; then
    log "Restoring MinIO audit bucket '${AUDIT_BUCKET}' from ${bucket_in}..."
    docker run --rm \
      --network "${COMPOSE_PROJECT_NAME:-aikonos}_backend" \
      -v "${bucket_in}:/src:ro" \
      -e MC_ALIAS_LOCAL_URL="http://minio:9000" \
      -e MC_ALIAS_LOCAL_ACCESS_KEY="${MINIO_USER}" \
      -e MC_ALIAS_LOCAL_SECRET_KEY="${MINIO_PASSWORD}" \
      --entrypoint /bin/sh \
      minio/mc:latest \
      -c "mc alias set local http://minio:9000 \"${MINIO_USER}\" \"${MINIO_PASSWORD}\" --quiet && \
             mc mb --ignore-existing local/${AUDIT_BUCKET} --quiet && \
             mc mirror --preserve /src/ local/${AUDIT_BUCKET} --quiet" \
      || log "WARNING: MinIO restore step failed — check minio container is running."
    log "  MinIO restore complete."
  else
    log "WARNING: ${bucket_in} not found, skipping MinIO restore."
  fi

  # -- 3. workspace-data volume restore --------------------------------------
  local ws_tgz="${backup_dir}/workspace.tgz"
  if [[ -f "${ws_tgz}" ]]; then
    log "Restoring workspace-data volume from ${ws_tgz}..."
    docker run --rm \
      -v "${COMPOSE_PROJECT_NAME:-aikonos}_workspace-data:/dst" \
      -v "${backup_dir}:/src:ro" \
      alpine \
      tar xzf /src/workspace.tgz -C /dst
    log "  workspace-data restore complete."
  else
    log "WARNING: workspace.tgz not found in ${backup_dir}, skipping."
  fi

  # -- 4. obs-archive volume restore -----------------------------------------
  local obs_tgz="${backup_dir}/obs-archive.tgz"
  if [[ -f "${obs_tgz}" ]]; then
    log "Restoring obs-archive volume from ${obs_tgz}..."
    docker run --rm \
      -v "${COMPOSE_PROJECT_NAME:-aikonos}_obs-archive:/dst" \
      -v "${backup_dir}:/src:ro" \
      alpine \
      tar xzf /src/obs-archive.tgz -C /dst
    log "  obs-archive restore complete."
  else
    log "WARNING: obs-archive.tgz not found in ${backup_dir}, skipping."
  fi

  # -- 5. vault-data volume restore (azure/onprem overlays only) ------------
  local vault_tgz="${backup_dir}/vault-data.tgz"
  if [[ -f "${vault_tgz}" ]]; then
    log "Restoring vault-data volume from ${vault_tgz}..."
    docker run --rm \
      -v "${COMPOSE_PROJECT_NAME:-aikonos}_vault-data:/dst" \
      -v "${backup_dir}:/src:ro" \
      alpine \
      tar xzf /src/vault-data.tgz -C /dst
    log "  vault-data restore complete. Vault will come back SEALED — see"
    log "  docs/OPS-RUNBOOK.md \"Vault operations\" to unseal before recreating the broker."
  else
    log "vault-data.tgz not found in ${backup_dir} — skipping (expected for local dev)."
  fi

  log "=== Restore complete. Reconcile AIKONOS_POLICY_OPENFGA_STORE_ID in .env, then start broker and other services. ==="
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

load_dotenv
config_defaults

case "${1:-}" in
  backup)  shift; cmd_backup  "$@" ;;
  restore) shift; cmd_restore "$@" ;;
  --help|-h|help) usage ;;
  *)
    printf '[compose-backup] subcommand required: backup | restore\n' >&2
    usage
    ;;
esac
