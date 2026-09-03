// Parser for SKILL.md bundles (bare markdown or zip).
//
// Zip traversal uses ZIP local-file-header walking (magic PK\x03\x04). STORED
// (0) and DEFLATE (8) entries are supported; DEFLATE is inflated with the Node
// built-in zlib, so the gateway footprint stays zero-added-packages.
//
// Bundles are inert: a bundle is the SKILL.md body (system-prompt text) plus an
// allowed-tools list (parsed for spec compliance; nothing narrows on it — see
//  D5). The gateway executes nothing from a
// bundle. Every file under SKILL.md's parent directory — scripts/, assets/,
// nested subdirs, anything — is captured verbatim as `extras` (see below) and
// forwarded; `scripts/` stays inert (stored and readable, never run). Uploads
// are tenant-admin-gated upstream.

import { inflateRawSync } from "node:zlib";
import { posix } from "node:path";

export interface ParsedSkill {
  name: string;
  description: string;
  allowedTools: string[];
  contextFork: boolean;
  disableModelInvocation: boolean;
  body: string;
  /**
   * Every non-directory file under SKILL.md's parent directory except
   * SKILL.md itself, keyed by path relative to that directory. Binary-safe — never decoded to text.
   */
  extras: Record<string, Buffer>;
  // rawSkillMd: the exact SKILL.md
  // text found (bare input, or the located zip entry), frontmatter included.
  // Admin bundle upload (routes/admin.ts) stores fields in separate DB
  // columns and ignores this; personal-skill import (routes/skills.ts)
  // writes it verbatim to Skills/<name>/SKILL.md — round-tripping through
  // `body`/`allowedTools` etc. would silently drop any frontmatter field this
  // parser doesn't model (e.g. `keywords`).
  rawSkillMd: string;
}

// ── ZIP local-file traversal ──────────────────────────────────────────────────
// Local file header layout (little-endian):
//   0-3   signature  PK\x03\x04
//   4-5   version needed
//   6-7   flags
//   8-9   compression  (0=stored, 8=deflate)
//   10-11 mod time
//   12-13 mod date
//   14-17 crc-32
//   18-21 compressed size
//   22-25 uncompressed size
//   26-27 file name length
//   28-29 extra field length
//   30+   file name, extra field, file data

const ZIP_SIG = 0x04034b50; // PK\x03\x04 LE

// The 5 MiB upload cap (routes/admin.ts) bounds only the *compressed* body; a
// crafted entry can inflate to arbitrary memory (zip bomb). Cap decompressed
// output per entry — sized so no legitimate bundle entry is ever rejected,
// since the whole compressed upload is already ≤ 5 MiB.
const MAX_ENTRY_DECOMPRESSED_BYTES = 5 * 1024 * 1024;

// Caps how many extras files a single skill's zip may carry — mirrors the broker's maxSkillBundleFiles
// (skill_file_path.go), the admin-bundle-side twin of this same cap.
export const MAX_SKILL_FILES = 100;

// Bounds the aggregate decompressed size of every extras entry in one parse
// call. The per-entry cap alone doesn't stop a zip with up to MAX_SKILL_FILES
// entries each near MAX_ENTRY_DECOMPRESSED_BYTES from allocating ~500 MiB
// inside a single parseSkillMd call.
export const MAX_SKILL_TOTAL_BYTES = 20 * 1024 * 1024;

const DRIVE_PREFIX_RE = /^[A-Za-z]:/;

// isValidSkillFilePath mirrors the broker's validSkillFilePath rule
// (broker/internal/broker/skill_file_path.go, ):
// a root-relative extras path must be non-empty, non-absolute, contain no
// ".." segment, no backslash, no NUL byte, no Windows drive prefix, and must
// equal its own posix.normalize() form (rejects "a/./b", "a//b", and other
// non-canonical forms — the broker's path.Clean(rel) == rel check).
// Exported so read-skill-file.ts (the runtime read tool) reuses this exact
// rule instead of duplicating it — the parser and the tool must reject the
// same paths, and the broker re-validates independently either way.
export function isValidSkillFilePath(rel: string): boolean {
  if (rel === "" || rel.startsWith("/") || rel.includes("\\") || rel.includes("\0")) return false;
  if (DRIVE_PREFIX_RE.test(rel)) return false;
  if (rel.split("/").some((seg) => seg === "..")) return false;
  return posix.normalize(rel) === rel;
}

/**
 * Returns the list of entry paths found in a ZIP buffer.
 * Throws with a descriptive message when the buffer is not a valid ZIP.
 */
export function listZipEntries(buf: Buffer): string[] {
  if (buf.length < 4 || buf.readUInt32LE(0) !== ZIP_SIG) {
    throw new Error("invalid zip: missing local file header signature");
  }

  const paths: string[] = [];
  let offset = 0;

  while (offset + 30 <= buf.length) {
    const sig = buf.readUInt32LE(offset);
    if (sig !== ZIP_SIG) break; // end of local entries (central dir or EOF)

    const compressedSize = buf.readUInt32LE(offset + 18);
    const nameLen = buf.readUInt16LE(offset + 26);
    const extraLen = buf.readUInt16LE(offset + 28);

    // A truncated or crafted header that claims a name extending past the buffer
    // is a parse error, not a clean EOF — fail loud to prevent bypass via partial list.
    if (offset + 30 + nameLen > buf.length) {
      throw new Error("invalid zip: local file header truncated (name extends past buffer)");
    }
    const entryName = buf.toString("utf8", offset + 30, offset + 30 + nameLen);
    paths.push(entryName);

    offset += 30 + nameLen + extraLen + compressedSize;
  }

  if (paths.length === 0) {
    throw new Error("invalid zip: no entries found");
  }

  return paths;
}

/**
 * Returns the uncompressed content of a STORED (0) or DEFLATE (8) entry by name.
 * DEFLATE entries are inflated via zlib (zip stores raw deflate, no zlib header).
 */
function readZipEntry(buf: Buffer, targetName: string): Buffer | null {
  let offset = 0;

  while (offset + 30 <= buf.length) {
    if (buf.readUInt32LE(offset) !== ZIP_SIG) break;

    const compression = buf.readUInt16LE(offset + 8);
    const compressedSize = buf.readUInt32LE(offset + 18);
    const uncompressedSize = buf.readUInt32LE(offset + 22);
    const nameLen = buf.readUInt16LE(offset + 26);
    const extraLen = buf.readUInt16LE(offset + 28);

    const entryName = buf.toString("utf8", offset + 30, offset + 30 + nameLen);
    const dataStart = offset + 30 + nameLen + extraLen;

    if (entryName === targetName) {
      const raw = buf.subarray(dataStart, dataStart + compressedSize);
      if (compression === 0) {
        if (uncompressedSize > MAX_ENTRY_DECOMPRESSED_BYTES) {
          throw new Error(
            `zip entry "${targetName}" decompresses beyond the ${MAX_ENTRY_DECOMPRESSED_BYTES}-byte limit`,
          );
        }
        return raw.subarray(0, uncompressedSize);
      }
      if (compression === 8) {
        try {
          return inflateRawSync(raw, { maxOutputLength: MAX_ENTRY_DECOMPRESSED_BYTES });
        } catch (err) {
          // Do not leak the raw zlib stack — rethrow descriptive. Node throws a
          // RangeError (ERR_BUFFER_TOO_LARGE) specifically when maxOutputLength
          // is exceeded; any other failure is genuine stream corruption and
          // must not be mislabeled as a size-cap breach.
          if (err instanceof RangeError) {
            throw new Error(
              `zip entry "${targetName}" decompresses beyond the ${MAX_ENTRY_DECOMPRESSED_BYTES}-byte limit`,
            );
          }
          const message = err instanceof Error ? err.message : String(err);
          throw new Error(`zip entry "${targetName}" failed to decompress: ${message}`);
        }
      }
      throw new Error(
        `zip entry "${targetName}" uses unsupported compression ${compression} (only STORED and DEFLATE)`,
      );
    }

    offset = dataStart + compressedSize;
  }

  return null;
}

// ── Frontmatter parser ────────────────────────────────────────────────────────
// Minimal YAML-subset parser for the SKILL.md frontmatter block.
// Handles: scalar strings, boolean literals, block sequences (- item).
// Does NOT pull in a full YAML library — keeps the zero-new-dep invariant.

interface FrontmatterRaw {
  name?: string;
  description?: string;
  "allowed-tools"?: string[];
  context?: string;
  "disable-model-invocation"?: boolean;
}

function parseFrontmatter(block: string): FrontmatterRaw {
  const result: FrontmatterRaw = {};
  const lines = block.split("\n");
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    const keyMatch = line.match(/^([a-z][a-z0-9-]*):\s*(.*)?$/);
    if (!keyMatch) { i++; continue; }

    const key = keyMatch[1];
    const rest = (keyMatch[2] ?? "").trim();

    if (rest === "" || rest === "|" || rest === ">") {
      // block sequence or block scalar — collect following "- item" lines
      const items: string[] = [];
      i++;
      while (i < lines.length && lines[i].match(/^\s+-\s+/)) {
        items.push(lines[i].replace(/^\s+-\s+/, "").trim());
        i++;
      }
      if (items.length > 0) {
        (result as Record<string, unknown>)[key] = items;
      }
      continue;
    }

    // inline sequence [a, b]
    if (rest.startsWith("[")) {
      const inner = rest.slice(1, rest.lastIndexOf("]"));
      (result as Record<string, unknown>)[key] = inner
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
      i++;
      continue;
    }

    // boolean
    if (rest === "true") { (result as Record<string, unknown>)[key] = true; i++; continue; }
    if (rest === "false") { (result as Record<string, unknown>)[key] = false; i++; continue; }

    // scalar string (strip optional quotes)
    (result as Record<string, unknown>)[key] = rest.replace(/^['"]|['"]$/g, "");
    i++;
  }

  return result;
}

// ── Main entry point ──────────────────────────────────────────────────────────

export interface ParseOptions {
  /** true → input is a ZIP buffer; false → input is bare SKILL.md text/buffer */
  zip: boolean;
}

/**
 * Parse a SKILL.md bundle (bare markdown or zip).
 *
 * Throws with a typed message on:
 * - Missing/invalid frontmatter
 * - Missing `name` field
 * - Invalid zip format
 */
export function parseSkillMd(input: Buffer | string, opts: ParseOptions): ParsedSkill {
  let mdText: string;

  // zipPaths is populated once for zip input and reused for both SKILL.md lookup and extras build.
  let zipBuf: Buffer | null = null;
  let zipPaths: string[] | null = null;
  // root anchors extras extraction at SKILL.md's parent directory: "" when
  // SKILL.md is at the zip's top level, else the "<dir>/" prefix of the
  // one-deep match (skill-creator's package_skill.py wraps the tree this way).
  let root: string | null = null;

  if (opts.zip) {
    zipBuf = Buffer.isBuffer(input) ? input : Buffer.from(input);
    zipPaths = listZipEntries(zipBuf); // throws on invalid zip

    // Locate SKILL.md (top-level or inside one directory)
    const skillPath =
      zipPaths.find((p) => p === "SKILL.md") ??
      zipPaths.find((p) => p.endsWith("/SKILL.md"));

    if (!skillPath) {
      throw new Error("invalid zip: no SKILL.md entry found");
    }

    root = skillPath === "SKILL.md" ? "" : skillPath.slice(0, skillPath.length - "SKILL.md".length);

    const content = readZipEntry(zipBuf, skillPath);
    if (!content) {
      throw new Error(`invalid zip: could not read entry "${skillPath}"`);
    }
    mdText = content.toString("utf8");
  } else {
    mdText = Buffer.isBuffer(input) ? input.toString("utf8") : input;
  }

  // Split frontmatter from body
  if (!mdText.startsWith("---")) {
    throw new Error("invalid SKILL.md: missing frontmatter delimiter (---) at start of file");
  }

  const afterOpen = mdText.slice(3);
  const closeIdx = afterOpen.indexOf("\n---");
  if (closeIdx === -1) {
    throw new Error("invalid SKILL.md: frontmatter block not closed (missing closing ---)");
  }

  const frontmatterBlock = afterOpen.slice(0, closeIdx).trim();
  const bodyRaw = afterOpen.slice(closeIdx + 4).trimStart(); // skip closing ---\n

  const fm = parseFrontmatter(frontmatterBlock);

  if (!fm.name || fm.name.trim() === "") {
    throw new Error("invalid SKILL.md: frontmatter missing required field 'name'");
  }

  // Build extras: every non-directory entry strictly under root except
  // SKILL.md itself, keyed by path relative to root. No prefix allowlist — an
  // entry outside root (a sibling top-level dir in a multi-folder zip) is
  // skipped silently, not part of this skill. When root is "" (SKILL.md at
  // the zip's true top level), a first-level directory that is itself a
  // skill (has its own "<dir>/SKILL.md") is excluded wholesale — it's a
  // sibling skill's tree, not an extra of this one.
  const extras: Record<string, Buffer> = {};
  if (opts.zip && zipBuf !== null && zipPaths !== null && root !== null) {
    const buf = zipBuf;
    const anchoredRoot = root;

    const siblingSkillDirs = new Set<string>();
    if (anchoredRoot === "") {
      for (const p of zipPaths) {
        const m = p.match(/^([^/]+)\/SKILL\.md$/);
        if (m) siblingSkillDirs.add(m[1]);
      }
    }

    const candidates: { fullPath: string; relPath: string }[] = [];

    for (const p of zipPaths) {
      if (p.endsWith("/")) continue; // directory entry, not a file
      if (!p.startsWith(anchoredRoot)) continue; // outside root — not part of this skill
      const relPath = p.slice(anchoredRoot.length);
      if (relPath === "SKILL.md") continue; // the skill body itself, not an extra
      if (anchoredRoot === "" && siblingSkillDirs.has(relPath.split("/")[0])) continue; // sibling skill's own tree

      if (!isValidSkillFilePath(relPath)) {
        throw new Error(`invalid zip: unsafe file path "${relPath}"`);
      }
      candidates.push({ fullPath: p, relPath });
    }

    if (candidates.length > MAX_SKILL_FILES) {
      throw new Error(`zip contains ${candidates.length} files, exceeding the ${MAX_SKILL_FILES}-file cap`);
    }

    let totalBytes = 0;
    for (const { fullPath, relPath } of candidates) {
      const content = readZipEntry(buf, fullPath);
      if (content) {
        totalBytes += content.length;
        if (totalBytes > MAX_SKILL_TOTAL_BYTES) {
          throw new Error(
            `zip extras decompress beyond the aggregate ${MAX_SKILL_TOTAL_BYTES}-byte cap`,
          );
        }
        extras[relPath] = content;
      }
    }
  }

  return {
    name: fm.name.trim(),
    description: (fm.description ?? "").trim(),
    allowedTools: Array.isArray(fm["allowed-tools"]) ? fm["allowed-tools"] : [],
    contextFork: fm.context === "fork",
    disableModelInvocation: fm["disable-model-invocation"] === true,
    body: bodyRaw,
    extras,
    rawSkillMd: mdText,
  };
}
