/**
 * corpus.js — pure read-only corpus reader.
 *
 * All functions accept an absolute `root` path (the CORPUS_ROOT).
 * No caching: every call reads from disk live.
 * No path ever escapes the corpus root — guards are enforced at read time.
 */

import fs from 'node:fs';
import path from 'node:path';

// ---------------------------------------------------------------------------
// CorpusError
// ---------------------------------------------------------------------------

export class CorpusError extends Error {
  /**
   * @param {string} message  Safe message — must NOT contain absolute host paths.
   * @param {string} code     Machine-readable code, e.g. 'NOT_FOUND', 'FORBIDDEN', 'BAD_INPUT'.
   */
  constructor(message, code) {
    super(message);
    this.name = 'CorpusError';
    this.code = code;
  }
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/**
 * Return the real (symlink-resolved) corpus root. Throws if the root itself
 * cannot be resolved (caller catches and re-throws as CorpusError).
 */
function realRoot(root) {
  return fs.realpathSync(root);
}

/**
 * Extract the first Markdown H1 from file content, or null if none.
 * Matches `# Title` (one or more spaces after #, at start of a line).
 */
function extractH1(content) {
  const m = content.match(/^#\s+(.+)$/m);
  return m ? m[1].trim() : null;
}

/**
 * Recursively collect all *.md files under `dir`, returning their absolute paths.
 */
function collectMdFiles(dir) {
  const results = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      // Symlinked subdirs are intentionally not traversed — corpus mounts are real dirs.
      results.push(...collectMdFiles(full));
    } else if (entry.isFile() && entry.name.endsWith('.md')) {
      results.push(full);
    }
  }
  return results;
}

// ---------------------------------------------------------------------------
// listDocs(root) → Array<{ path, title }>
// ---------------------------------------------------------------------------

/**
 * List all .md files under `root`, sorted by path ascending.
 * `path` is relative to `root` with forward slashes.
 * `title` = first H1 in the file, else the basename without extension.
 *
 * @param {string} root  Absolute path to the corpus root.
 * @returns {Array<{ path: string, title: string }>}
 */
export async function listDocs(root) {
  const rr = realRoot(root);
  const absFiles = collectMdFiles(rr);

  const docs = absFiles.map((abs) => {
    const rel = path.relative(rr, abs).split(path.sep).join('/');
    const content = fs.readFileSync(abs, 'utf8');
    const title = extractH1(content) ?? path.basename(abs, '.md');
    return { path: rel, title };
  });

  docs.sort((a, b) => a.path.localeCompare(b.path));
  return docs;
}

// ---------------------------------------------------------------------------
// readDoc(root, relPath) → { path, content }
// ---------------------------------------------------------------------------

/**
 * Read a single document by its relative path within the corpus.
 *
 * Path guard: resolves against the real root and rejects anything that
 * escapes it — including `..` segments, absolute paths, and symlinks pointing
 * outside. Rejects non-.md paths. Throws CorpusError on any violation or
 * missing file.
 *
 * Safe errors: messages never contain the absolute host root path.
 *
 * @param {string} root     Absolute path to the corpus root.
 * @param {string} relPath  Relative path requested by the caller.
 * @returns {{ path: string, content: string }}
 */
export async function readDoc(root, relPath) {
  // Reject absolute inputs up front — path.resolve would silently treat them
  // as absolute and they would likely escape the corpus.
  if (path.isAbsolute(relPath)) {
    throw new CorpusError('Path must be relative', 'BAD_INPUT');
  }

  // Reject non-.md files before any filesystem touch.
  if (!relPath.endsWith('.md')) {
    throw new CorpusError('Only .md files are served', 'BAD_INPUT');
  }

  const rr = realRoot(root);
  // Candidate absolute path (may still contain symlinks — realpathSync below).
  const candidate = path.resolve(rr, relPath);

  // Guard 1: the normalized candidate must start with the real root.
  // This catches `..` traversal before touching the filesystem.
  if (!candidate.startsWith(rr + path.sep) && candidate !== rr) {
    throw new CorpusError('Path is outside the corpus', 'FORBIDDEN');
  }

  // Guard 2: resolve symlinks to catch symlinks pointing outside.
  let real;
  try {
    real = fs.realpathSync(candidate);
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new CorpusError('Document not found', 'NOT_FOUND');
    }
    // EACCES, ELOOP, etc. — distinct from missing.
    throw new CorpusError('Document could not be read', 'IO');
  }

  if (!real.startsWith(rr + path.sep) && real !== rr) {
    throw new CorpusError('Path is outside the corpus', 'FORBIDDEN');
  }

  let content;
  try {
    content = fs.readFileSync(real, 'utf8');
  } catch {
    throw new CorpusError('Document not found', 'NOT_FOUND');
  }

  const relOut = path.relative(rr, real).split(path.sep).join('/');
  return { path: relOut, content };
}

// ---------------------------------------------------------------------------
// searchDocs(root, query, limit) → Array<{ path, score, snippet }>
// ---------------------------------------------------------------------------

const SNIPPET_MAX_CHARS = 400;
const SNIPPET_LINES = 3;

/**
 * Search the corpus for `query` and return ranked results.
 *
 * Scoring:
 *   - Tokenize `query` on whitespace; each token is matched case-insensitively.
 *   - `score` = total occurrences of all tokens across the entire file content.
 *   - Title matches count double (a title hit is more signal than a body hit).
 * Sort: score descending, then path ascending for ties.
 * Limit is clamped to [1, 20].
 * Files with score 0 are excluded.
 * Empty query (no non-space tokens) → [].
 *
 * @param {string} root   Absolute corpus root.
 * @param {string} query  Free-text query string.
 * @param {number} limit  Maximum results to return (clamped to [1, 20]).
 * @returns {Array<{ path: string, score: number, snippet: string }>}
 */
export async function searchDocs(root, query, limit) {
  const clampedLimit = Math.max(1, Math.min(20, limit ?? 8));

  const tokens = query
    .split(/\s+/)
    .map(t => t.toLowerCase())
    .filter(t => t.length > 0);

  if (tokens.length === 0) return [];

  const rr = realRoot(root);
  const absFiles = collectMdFiles(rr);

  const results = [];

  for (const abs of absFiles) {
    const rel = path.relative(rr, abs).split(path.sep).join('/');
    const content = fs.readFileSync(abs, 'utf8');
    const lower = content.toLowerCase();
    const title = extractH1(content) ?? path.basename(abs, '.md');
    const titleLower = title.toLowerCase();

    let score = 0;
    for (const token of tokens) {
      // Body hits
      let idx = 0;
      while ((idx = lower.indexOf(token, idx)) !== -1) {
        score += 1;
        idx += token.length;
      }
      // Title bonus: each title hit counts double, but we already counted it
      // in the body scan above — add one more point per title occurrence.
      let tidx = 0;
      while ((tidx = titleLower.indexOf(token, tidx)) !== -1) {
        score += 1; // extra point for title match
        tidx += token.length;
      }
    }

    if (score === 0) continue;

    // Snippet: collect up to SNIPPET_LINES lines that contain any token.
    const lines = content.split('\n');
    const matchingLines = [];
    for (const line of lines) {
      if (matchingLines.length >= SNIPPET_LINES) break;
      const lineLower = line.toLowerCase();
      if (tokens.some(t => lineLower.includes(t))) {
        matchingLines.push(line.trim());
      }
    }
    let snippet = matchingLines.join(' … ');
    if (snippet.length > SNIPPET_MAX_CHARS) {
      snippet = snippet.slice(0, SNIPPET_MAX_CHARS) + '…';
    }

    results.push({ path: rel, score, snippet });
  }

  // Sort: score descending, then path ascending for ties.
  results.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score;
    return a.path.localeCompare(b.path);
  });

  return results.slice(0, clampedLimit);
}
