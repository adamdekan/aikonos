// CP3 tests: read_skill_file built-in.
//
// WHY these tests exist:
//   read_skill_file is the second progressive-disclosure stage — the model
//   reads one file's raw content on demand. Resolution runs against the FULL
//   authorized catalog (bundles ∪ personal), INCLUDING disable_model_invocation
//   entries — the /command lesson (commits 26d549b/30c6f11): the parent may
//   have activated a DMI bundle via /command, and the model must still be able
//   to read its files even though it cannot call load_skill on it directly.
//   The path guard and the UTF-8/size content gate must reject before/after the
//   fetch respectively — binary or oversized content must never reach the model.
import { test } from "node:test";
import assert from "node:assert/strict";

import { makeReadSkillFileTool, type SkillFileFetcher } from "../src/pi/read-skill-file.js";
import type { SkillBundleEntry } from "../src/ipc/protocol.js";

// ── helpers ────────────────────────────────────────────────────────────────────

function bundle(overrides: Partial<SkillBundleEntry> = {}): SkillBundleEntry {
  return {
    id: overrides.id ?? "bundle-uuid-1",
    name: overrides.name ?? "my-skill",
    description: overrides.description ?? "Does something useful",
    body: overrides.body ?? "## My Skill",
    allowedTools: overrides.allowedTools ?? ["web.fetch"],
    contextFork: overrides.contextFork ?? false,
    disableModelInvocation: overrides.disableModelInvocation ?? false,
    keywords: overrides.keywords ?? [],
    filePaths: overrides.filePaths ?? ["references/guide.md"],
    ...(overrides.origin ? { origin: overrides.origin } : {}),
  };
}

function personalEntry(bareName: string, overrides: Partial<SkillBundleEntry> = {}): SkillBundleEntry {
  const qualified = "personal:" + bareName;
  return bundle({
    id: qualified,
    name: qualified,
    origin: "personal",
    ...overrides,
  });
}

function fetcherReturning(content: Buffer | Uint8Array): { fetcher: SkillFileFetcher; calls: { ref: string; path: string }[] } {
  const calls: { ref: string; path: string }[] = [];
  const fetcher: SkillFileFetcher = async (ref, path) => {
    calls.push({ ref, path });
    const wire = { ok: true, contentB64: Buffer.from(content).toString("base64") };
    // Round-trip through JSON — mirrors the real child IPC channel
    // (supervisor.ts's defaultSpawnChild forks with serialization:"json",
    // which flattens a raw Uint8Array into a numeric-keyed plain object in
    // transit). contentB64, a plain string, must survive this unchanged —
    // this is what closes the JSON-boundary corruption bug (bug 1, CP3
    // review): without it, every test in this file exercised only the
    // in-memory fake channel, which never serializes, and so never caught
    // the corruption that broke every real production read.
    return JSON.parse(JSON.stringify(wire)) as typeof wire;
  };
  return { fetcher, calls };
}

// ── resolution: bundle ref vs personal ref ─────────────────────────────────────

test("read_skill_file: bundle entry passes the bundle's id as ref", async () => {
  const b = bundle({ id: "bundle-uuid-42", name: "my-skill" });
  const { fetcher, calls } = fetcherReturning(Buffer.from("hello", "utf8"));
  const tool = makeReadSkillFileTool([b], fetcher);

  await tool.execute("call-1", { skill: "my-skill", path: "references/guide.md" });

  assert.deepEqual(calls, [{ ref: "bundle-uuid-42", path: "references/guide.md" }]);
});

test("read_skill_file: personal entry passes the qualified 'personal:<name>' id as ref", async () => {
  const p = personalEntry("my-notes");
  const { fetcher, calls } = fetcherReturning(Buffer.from("hello", "utf8"));
  const tool = makeReadSkillFileTool([p], fetcher);

  await tool.execute("call-2", { skill: "personal:my-notes", path: "assets/x.txt" });

  assert.deepEqual(calls, [{ ref: "personal:my-notes", path: "assets/x.txt" }]);
});

test("read_skill_file: a disable_model_invocation bundle is still readable (unlike load_skill)", async () => {
  // WHY: the parent may have activated a DMI bundle via /command
  // (resolveCommandSkill honors DMI, load_skill's model-facing byName does
  // not) — the model must still be able to read that bundle's files.
  const dmi = bundle({ name: "admin-dmi", disableModelInvocation: true });
  const { fetcher, calls } = fetcherReturning(Buffer.from("secret file", "utf8"));
  const tool = makeReadSkillFileTool([dmi], fetcher);

  const result = await tool.execute("call-3", { skill: "admin-dmi", path: "references/guide.md" });

  assert.equal(calls.length, 1, "a DMI bundle must still resolve and reach the fetcher");
  assert.ok(result.content[0]?.text.includes("secret file"));
});

test("read_skill_file: unknown skill name → tool error naming 'unknown skill', fetcher never called", async () => {
  const { fetcher, calls } = fetcherReturning(Buffer.from("x"));
  const tool = makeReadSkillFileTool([bundle({ name: "my-skill" })], fetcher);

  const result = await tool.execute("call-4", { skill: "does-not-exist", path: "a.txt" });

  assert.match(result.content[0]?.text ?? "", /unknown skill/i);
  assert.equal(calls.length, 0, "an unresolvable skill must never reach the fetcher");
});

// ── path guard (mirrors skill-parser.ts's isValidSkillFilePath) ────────────────

const INVALID_PATHS: [string, string][] = [
  ["/etc/passwd", "absolute"],
  ["../secret", "parent traversal"],
  ["a/../../etc/passwd", "embedded parent traversal"],
  ["a\\b", "backslash"],
  ["a\0b", "NUL byte"],
  ["C:foo", "drive prefix"],
  ["", "empty"],
  ["a/./b", "non-canonical (./ segment)"],
];

for (const [path, why] of INVALID_PATHS) {
  test(`read_skill_file: rejects invalid path — ${why}`, async () => {
    const { fetcher, calls } = fetcherReturning(Buffer.from("x"));
    const tool = makeReadSkillFileTool([bundle({ name: "my-skill" })], fetcher);

    const result = await tool.execute("call-guard", { skill: "my-skill", path });

    assert.match(result.content[0]?.text ?? "", /invalid path/i, `path ${JSON.stringify(path)} (${why}) must be rejected`);
    assert.equal(calls.length, 0, `path ${JSON.stringify(path)} (${why}) must never reach the fetcher`);
  });
}

test("read_skill_file: a valid canonical path reaches the fetcher", async () => {
  const { fetcher, calls } = fetcherReturning(Buffer.from("ok", "utf8"));
  const tool = makeReadSkillFileTool([bundle({ name: "my-skill" })], fetcher);

  await tool.execute("call-5", { skill: "my-skill", path: "scripts/run.py" });

  assert.equal(calls.length, 1);
  assert.equal(calls[0]?.path, "scripts/run.py");
});

// ── JSON IPC boundary (bug 1, CP3 review) ──────────────────────────────────────
//
// WHY this test exists: a raw Uint8Array does not survive the real child IPC
// channel — child_process.fork's serialization:"json" (supervisor.ts's
// defaultSpawnChild) flattens it into a numeric-keyed plain object, at which
// point .byteLength is undefined (so the size-cap check silently no-ops) and
// TextDecoder.decode() throws on the non-typed-array, which the tool's catch
// block reports as "file is not valid UTF-8 text" — for every read, including
// small valid text. Verified directly: JSON.parse(JSON.stringify({content:
// new TextEncoder().encode("hello")})).content is {0:104,1:101,...}, not a
// Uint8Array. contentB64 (a plain string) sidesteps this — this test proves a
// fetcher result carrying the OLD "content" shape (no contentB64 field at
// all) is treated as a failed read rather than silently misdecoded.
test("read_skill_file: a fetcher result predating the contentB64 wire shape (no contentB64 field) is a clean failed read, never a misdecode", async () => {
  const b = bundle({ name: "my-skill" });
  const flattenedLegacyShape = JSON.parse(
    JSON.stringify({ ok: true, content: new TextEncoder().encode("hello") }),
  ) as { ok: boolean; content: unknown };
  const fetcher: SkillFileFetcher = async () => flattenedLegacyShape as unknown as { ok: boolean; contentB64?: string };
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-bug1", { skill: "my-skill", path: "a.txt" });

  assert.match(result.content[0]?.text ?? "", /read failed/i);
});

// ── fetch failure ───────────────────────────────────────────────────────────────

test("read_skill_file: a failed fetch surfaces the fetcher's error", async () => {
  const b = bundle({ name: "my-skill" });
  const fetcher: SkillFileFetcher = async () => ({ ok: false, error: "NotFound" });
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-6", { skill: "my-skill", path: "references/guide.md" });

  assert.match(result.content[0]?.text ?? "", /NotFound/);
});

// ── content gate: UTF-8 validity + size cap (never raw bytes to the model) ─────

test("read_skill_file: UTF-8 happy path returns the decoded text", async () => {
  const b = bundle({ name: "my-skill" });
  const { fetcher } = fetcherReturning(Buffer.from("## Guide\nHello, world.", "utf8"));
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-7", { skill: "my-skill", path: "references/guide.md" });

  assert.equal(result.content[0]?.text, "## Guide\nHello, world.");
});

test("read_skill_file: binary (invalid UTF-8) content → descriptive error, never raw bytes", async () => {
  const b = bundle({ name: "my-skill" });
  // A lone continuation byte (0x80) is never valid at the start of a UTF-8
  // sequence — TextDecoder({fatal:true}) must reject it.
  const { fetcher } = fetcherReturning(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x80, 0x80]));
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-8", { skill: "my-skill", path: "assets/logo.png" });

  assert.match(result.content[0]?.text ?? "", /binary|not valid utf-8/i);
});

test("read_skill_file: content over 256 KiB → descriptive error naming the cap, not decoded", async () => {
  const b = bundle({ name: "my-skill" });
  const oversized = Buffer.alloc(256 * 1024 + 1, "a");
  const { fetcher } = fetcherReturning(oversized);
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-9", { skill: "my-skill", path: "scripts/big.py" });

  assert.match(result.content[0]?.text ?? "", /262144/, "must name the exact byte cap");
  assert.match(result.content[0]?.text ?? "", /exceed|too large|large|cap/i);
});

test("read_skill_file: content exactly at the 256 KiB cap is accepted", async () => {
  const b = bundle({ name: "my-skill" });
  const exact = Buffer.alloc(256 * 1024, "a");
  const { fetcher } = fetcherReturning(exact);
  const tool = makeReadSkillFileTool([b], fetcher);

  const result = await tool.execute("call-10", { skill: "my-skill", path: "scripts/exact.py" });

  assert.equal(result.content[0]?.text.length, 256 * 1024);
});
