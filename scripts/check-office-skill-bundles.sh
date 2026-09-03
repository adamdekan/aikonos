#!/usr/bin/env bash
set -euo pipefail

# scripts/check-office-skill-bundles.sh
#
# Asserts each office-tools skill bundle is under the agent_skill.body DB cap (256 KiB) — a bundle
# exceeding it fails to upload via POST /admin/skills/upload.
#
# Usage: scripts/check-office-skill-bundles.sh

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MAX_BYTES=$((256 * 1024))

BUNDLES=(
  "docs/skills/docx/SKILL.md"
  "docs/skills/xlsx/SKILL.md"
  "docs/skills/pptx/SKILL.md"
  "docs/skills/pdf/SKILL.md"
)

status=0
for f in "${BUNDLES[@]}"; do
  if [[ ! -f "$f" ]]; then
    echo "MISSING: $f" >&2
    status=1
    continue
  fi
  size=$(wc -c < "$f")
  if (( size > MAX_BYTES )); then
    echo "TOO LARGE: $f is $size bytes, exceeds cap of $MAX_BYTES bytes" >&2
    status=1
  else
    echo "OK: $f ($size bytes)"
  fi
done

exit "$status"
