import { downloadFile, uploadFile, deleteFile, listFiles } from "./files.js";

const SESSION_DIR = ".agent/Sessions";
const SESSION_PREFIX = `${SESSION_DIR}/`;
const MANIFEST_PATH = `${SESSION_PREFIX}index.json`;
// ListDir (broker/internal/workspacefs) returns paths relative to the workspace root
// regardless of scope, so a dir-scoped listing of .agent/Sessions still yields full
// paths like ".agent/Sessions/x.json" — the regex remains a shape guard, not a filter
// that does the scoping (scoping is now the server's job).
const SESSION_PATTERN = /^\.agent\/Sessions\/(?!index\.json$).+\.json$/;

const CHUNK_SIZE = 0x8000;

function encodeJson(obj) {
  const json = JSON.stringify(obj);
  const bytes = new TextEncoder().encode(json);
  let binary = "";
  for (let i = 0; i < bytes.length; i += CHUNK_SIZE) {
    binary += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK_SIZE));
  }
  return btoa(binary);
}

function decodeJson(b64) {
  return JSON.parse(decodeURIComponent(escape(atob(b64))));
}

export async function listSessionFiles() {
  const result = await listFiles({ dir: SESSION_DIR, includeHidden: true });
  if (result && result.forbidden) return [];
  const files = result?.files ?? [];
  return files.filter((f) => SESSION_PATTERN.test(f.path));
}

export async function readSession(id) {
  const result = await downloadFile(`${SESSION_PREFIX}${id}.json`);
  if (!result || result.forbidden) return null;
  if (!result.contentBase64) return null;
  try {
    return decodeJson(result.contentBase64);
  } catch {
    return null;
  }
}

export async function writeSession(record) {
  return uploadFile(`${SESSION_PREFIX}${record.id}.json`, encodeJson(record));
}

export async function deleteSession(id) {
  return deleteFile(`${SESSION_PREFIX}${id}.json`);
}

export async function readManifest() {
  const result = await downloadFile(MANIFEST_PATH);
  if (!result || result.forbidden) return [];
  if (!result.contentBase64) return [];
  try {
    const parsed = decodeJson(result.contentBase64);
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

export async function writeManifest(arr) {
  return uploadFile(MANIFEST_PATH, encodeJson(arr));
}

// One-time migration: moves legacy Sessions/* files to .agent/Sessions/*.
// Idempotent; swallows all errors so it never blocks chat.
export async function migrateLegacySessions() {
  const LEGACY_DIR = "Sessions";
  const LEGACY_PREFIX = `${LEGACY_DIR}/`;
  try {
    // Two scoped fetches — the legacy dir and the new dir — replace the old single
    // full-tree listing now that the server no longer offers one call for both.
    const [legacyResult, newResult] = await Promise.all([
      listFiles({ dir: LEGACY_DIR, includeHidden: true }),
      listFiles({ dir: SESSION_DIR, includeHidden: true }),
    ]);
    const legacyFiles = (legacyResult?.files ?? []).filter((f) => f.path.startsWith(LEGACY_PREFIX));
    if (legacyFiles.length === 0) return;

    const newPaths = new Set(
      (newResult?.files ?? []).filter((f) => f.path.startsWith(SESSION_PREFIX)).map((f) => f.path)
    );

    for (const file of legacyFiles) {
      const basename = file.path.slice(LEGACY_PREFIX.length);
      const newPath = `${SESSION_PREFIX}${basename}`;

      let copied = newPaths.has(newPath); // already exists → treat as copied
      if (!copied) {
        try {
          const dl = await downloadFile(file.path);
          if (dl && dl.contentBase64) {
            await uploadFile(newPath, dl.contentBase64);
            copied = true;
          }
        } catch (err) {
          console.warn("migrateLegacySessions: failed to copy", file.path, err);
        }
      }

      if (copied) {
        try {
          await deleteFile(file.path);
        } catch {
          // swallow
        }
      }
    }
  } catch (err) {
    console.warn("migrateLegacySessions: unexpected error", err);
  }
}
