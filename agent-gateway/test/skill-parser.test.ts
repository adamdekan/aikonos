// Tests for the zip-bomb guard in agent-gateway/src/pi/skill-parser.ts.
//
// A malicious skill bundle can craft a zip entry whose DEFLATE-compressed body
// is tiny but inflates to gigabytes (a "zip bomb"). The 5 MiB upload cap
// (routes/admin.ts) only bounds the *compressed* body, so `readZipEntry` must
// independently bound the *decompressed* output of both the DEFLATE and
// STORED paths.

import { test } from "node:test";
import assert from "node:assert/strict";
import { deflateRawSync } from "node:zlib";

import { parseSkillMd, MAX_SKILL_TOTAL_BYTES } from "../src/pi/skill-parser.js";

// ── ZIP builder helpers (STORED + DEFLATE), mirroring test/skill-upload.test.ts ──

function writeUInt16LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt16LE(val, offset);
}
function writeUInt32LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt32LE(val, offset);
}

interface ZipEntry {
  name: string;
  data: Buffer;
  /** Compression method: 0 = STORED, 8 = DEFLATE. */
  compression: 0 | 8;
  /**
   * Header-declared uncompressed size. Defaults to `data.length` for STORED
   * entries (the real size) but can be overridden to craft a lying header.
   */
  declaredUncompressedSize?: number;
  /**
   * When set, used verbatim as the entry's compressed body instead of
   * deflating `data` — lets a test craft a DEFLATE entry with a corrupt
   * (non-deflate) compressed body.
   */
  rawStoredOverride?: Buffer;
}

function buildZip(entries: ZipEntry[]): Buffer {
  const localHeaders: Buffer[] = [];

  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const stored =
      entry.compression === 8 && !entry.rawStoredOverride
        ? deflateRawSync(entry.data)
        : (entry.rawStoredOverride ?? entry.data);
    const uncompressedSize = entry.declaredUncompressedSize ?? entry.data.length;

    const local = Buffer.alloc(30 + nameBuf.length + stored.length);
    writeUInt32LE(local, 0, 0x04034b50); // signature PK\x03\x04
    writeUInt16LE(local, 4, 20); // version needed
    writeUInt16LE(local, 6, 0); // flags
    writeUInt16LE(local, 8, entry.compression);
    writeUInt16LE(local, 10, 0); // mod time
    writeUInt16LE(local, 12, 0); // mod date
    writeUInt32LE(local, 14, 0); // crc-32 (not validated by parser)
    writeUInt32LE(local, 18, stored.length); // compressed size
    writeUInt32LE(local, 22, uncompressedSize); // uncompressed size (attacker-controlled)
    writeUInt16LE(local, 26, nameBuf.length);
    writeUInt16LE(local, 28, 0); // extra field length
    nameBuf.copy(local, 30);
    stored.copy(local, 30 + nameBuf.length);
    localHeaders.push(local);
  }

  // No central directory needed — listZipEntries/readZipEntry only walk local headers.
  return Buffer.concat(localHeaders);
}

const VALID_SKILL_MD = `---
name: my-skill
description: A test skill
allowed-tools:
  - web.fetch
---

Body.
`;

// ── DEFLATE entry exceeding the cap ───────────────────────────────────────────

test("parseSkillMd — DEFLATE entry that inflates past the cap throws, names the entry", () => {
  // A run of one repeated byte compresses to a handful of bytes yet inflates
  // to whatever size we ask for — the classic zip-bomb shape. 6 MiB > the
  // 5 MiB cap while the compressed body stays tiny. Padded onto valid
  // frontmatter so the cap error — not a frontmatter-parse error — is what
  // would fire absent the size guard.
  const bombPayload = Buffer.concat([
    Buffer.from(VALID_SKILL_MD),
    Buffer.alloc(6 * 1024 * 1024, 0x41),
  ]);
  const zipBuf = buildZip([
    { name: "SKILL.md", data: bombPayload, compression: 8 },
  ]);

  assert.throws(
    () => parseSkillMd(zipBuf, { zip: true }),
    (err: unknown) =>
      err instanceof Error &&
      err.message.includes('"SKILL.md"') &&
      err.message.includes("5242880") &&
      !err.message.includes("frontmatter"),
  );
});

// ── normal small DEFLATE entry still parses ───────────────────────────────────

test("parseSkillMd — normal small DEFLATE entry parses fine", () => {
  const zipBuf = buildZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD), compression: 8 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });
  assert.equal(parsed.name, "my-skill");
});

// ── corrupt DEFLATE entry throws a decompression-failure error, not the cap message ──

test("parseSkillMd — corrupt DEFLATE entry throws a decompression-failure error, not the byte-cap message", () => {
  // Garbage bytes are not a valid deflate stream — inflateRawSync throws
  // Z_DATA_ERROR ("invalid block type"), a RangeError-distinct failure that
  // must not be mislabeled as a size-cap breach.
  const corrupt = Buffer.from([0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff]);
  const zipBuf = buildZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD), compression: 8, rawStoredOverride: corrupt },
  ]);

  assert.throws(
    () => parseSkillMd(zipBuf, { zip: true }),
    (err: unknown) =>
      err instanceof Error &&
      err.message.includes('"SKILL.md"') &&
      err.message.includes("failed to decompress") &&
      !err.message.includes("beyond the") &&
      !err.message.includes("5242880"),
  );
});

// ── STORED entry with a lying oversize uncompressedSize header ──────────────

test("parseSkillMd — STORED entry declaring an oversize uncompressedSize throws", () => {
  // The actual data is tiny, but the header claims 6 MiB uncompressed — the
  // header field is attacker-controlled and must be bounded up front.
  const zipBuf = buildZip([
    {
      name: "SKILL.md",
      data: Buffer.from(VALID_SKILL_MD),
      compression: 0,
      declaredUncompressedSize: 6 * 1024 * 1024,
    },
  ]);

  assert.throws(
    () => parseSkillMd(zipBuf, { zip: true }),
    (err: unknown) =>
      err instanceof Error &&
      err.message.includes('"SKILL.md"') &&
      err.message.includes("5242880"),
  );
});

// ── normal small STORED entry still parses ────────────────────────────────────

test("parseSkillMd — normal small STORED entry parses fine", () => {
  const zipBuf = buildZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD), compression: 0 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });
  assert.equal(parsed.name, "my-skill");
});

// ── full-tree extraction ──────────────────────

const NAME = "my-skill";
const NAMED_SKILL_MD = `---
name: ${NAME}
description: A test skill
---

Body.
`;

test("parseSkillMd — wrapped zip (<name>/SKILL.md) extracts every file, root-stripped, byte-identical", () => {
  // Real-world shape: skill-creator's package_skill.py wraps the whole tree in
  // a top-level "<name>/" directory before zipping.
  const pngBytes = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03]);
  const zipBuf = buildZip([
    { name: `${NAME}/SKILL.md`, data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
    { name: `${NAME}/scripts/x.py`, data: Buffer.from("print('hi')\n"), compression: 0 },
    { name: `${NAME}/references/r.md`, data: Buffer.from("# Reference\n"), compression: 0 },
    { name: `${NAME}/LICENSE.txt`, data: Buffer.from("MIT License\n"), compression: 0 },
    { name: `${NAME}/assets/img/logo.png`, data: pngBytes, compression: 0 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });

  assert.equal(parsed.name, NAME);
  assert.deepEqual(
    Object.keys(parsed.extras).sort(),
    ["LICENSE.txt", "assets/img/logo.png", "references/r.md", "scripts/x.py"],
  );
  assert.ok(Buffer.isBuffer(parsed.extras["assets/img/logo.png"]));
  assert.ok(
    parsed.extras["assets/img/logo.png"].equals(pngBytes),
    "binary content must be byte-identical, never UTF-8 coerced",
  );
  assert.equal(parsed.extras["scripts/x.py"].toString("utf8"), "print('hi')\n");
  assert.equal(parsed.extras["LICENSE.txt"].toString("utf8"), "MIT License\n");
});

test("parseSkillMd — unwrapped zip (SKILL.md at top level) extracts the same tree shape", () => {
  const zipBuf = buildZip([
    { name: "SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
    { name: "scripts/x.py", data: Buffer.from("print('hi')\n"), compression: 0 },
    { name: "references/r.md", data: Buffer.from("# Reference\n"), compression: 0 },
    { name: "LICENSE.txt", data: Buffer.from("MIT License\n"), compression: 0 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });

  assert.deepEqual(Object.keys(parsed.extras).sort(), ["LICENSE.txt", "references/r.md", "scripts/x.py"]);
});

test("parseSkillMd — a sibling top-level dir outside the wrapped root is skipped, not thrown", () => {
  const zipBuf = buildZip([
    { name: `${NAME}/SKILL.md`, data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
    { name: `${NAME}/scripts/x.py`, data: Buffer.from("print('hi')\n"), compression: 0 },
    { name: "other-dir/leak.txt", data: Buffer.from("not part of this skill\n"), compression: 0 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });

  assert.deepEqual(Object.keys(parsed.extras).sort(), ["scripts/x.py"]);
});

test("parseSkillMd — top-level SKILL.md excludes a sibling directory that is itself a skill, but keeps a plain data dir", () => {
  // root === "" here (SKILL.md at the zip's true top level). other-dir has
  // its own SKILL.md, so its whole tree belongs to that sibling skill, not
  // this one — unlike data/, which has no SKILL.md and is an ordinary extra.
  const zipBuf = buildZip([
    { name: "SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
    { name: "references/r.md", data: Buffer.from("# Reference\n"), compression: 0 },
    { name: "other-dir/SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
    { name: "other-dir/scripts/run.py", data: Buffer.from("print('hi')\n"), compression: 0 },
    { name: "data/x.csv", data: Buffer.from("a,b\n1,2\n"), compression: 0 },
  ]);

  const parsed = parseSkillMd(zipBuf, { zip: true });

  assert.deepEqual(Object.keys(parsed.extras).sort(), ["data/x.csv", "references/r.md"]);
});

// ── zip-slip guards (mirrors broker's validSkillFilePath) ─────────────────────

const ZIP_SLIP_CASES: readonly (readonly [string, string])[] = [
  ["parent traversal", "../evil"],
  ["absolute path", "/etc/passwd"],
  ["backslash path", "windows\\evil.txt"],
  ["drive prefix", "C:/evil.txt"],
  ["NUL byte", "bad\0name"],
  ["non-canonical dot segment", "a/./b"],
  ["non-canonical double slash", "a//b"],
];

for (const [label, badName] of ZIP_SLIP_CASES) {
  test(`parseSkillMd — zip-slip via ${label} ("${badName}") throws`, () => {
    const zipBuf = buildZip([
      { name: "SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
      { name: badName, data: Buffer.from("pwned"), compression: 0 },
    ]);

    assert.throws(() => parseSkillMd(zipBuf, { zip: true }), /unsafe file path/);
  });
}

// ── file-count cap ────────────────────────────────────────────────────────────

test("parseSkillMd — more than MAX_SKILL_FILES extras throws, names the cap", () => {
  const entries: { name: string; data: Buffer; compression: 0 | 8 }[] = [
    { name: "SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
  ];
  for (let i = 0; i < 101; i++) {
    entries.push({ name: `references/f${i}.md`, data: Buffer.from(`file ${i}`), compression: 0 });
  }
  const zipBuf = buildZip(entries);

  assert.throws(
    () => parseSkillMd(zipBuf, { zip: true }),
    (err: unknown) => err instanceof Error && err.message.includes("101") && err.message.includes("100"),
  );
});

// ── aggregate decompressed-bytes cap ─────────────────────────────────────────

test("parseSkillMd — extras whose decompressed total exceeds MAX_SKILL_TOTAL_BYTES throws, names the cap", () => {
  // Each entry sits under the per-entry 5 MiB cap (MAX_ENTRY_DECOMPRESSED_BYTES)
  // so it's the aggregate 20 MiB cap that must fire. A repeated byte keeps
  // every entry's compressed body tiny — same zip-bomb shape as the DEFLATE
  // test above, just spread across several entries instead of one.
  const perEntryBytes = 4.5 * 1024 * 1024; // < MAX_ENTRY_DECOMPRESSED_BYTES, 5 of them > MAX_SKILL_TOTAL_BYTES
  const entries: ZipEntry[] = [
    { name: "SKILL.md", data: Buffer.from(NAMED_SKILL_MD), compression: 0 },
  ];
  for (let i = 0; i < 5; i++) {
    entries.push({
      name: `references/f${i}.md`,
      data: Buffer.alloc(perEntryBytes, 0x41),
      compression: 8,
    });
  }
  const zipBuf = buildZip(entries);

  assert.throws(
    () => parseSkillMd(zipBuf, { zip: true }),
    (err: unknown) =>
      err instanceof Error &&
      err.message.includes(String(MAX_SKILL_TOTAL_BYTES)) &&
      !err.message.includes("frontmatter"),
  );
});
