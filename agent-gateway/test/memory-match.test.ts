// Tests for the auto-recall keyword matcher (memory-match.ts).
//
// WHY these tests exist: matchMemoryConcepts decides what prior memory gets
// injected into a chat turn's prompt. A false match injects irrelevant (and
// possibly misleading) context the model then treats as fact; a deprecated
// concept surfacing resurrects knowledge the owner deliberately retired
//.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  matchMemoryConcepts,
  mergeRecall,
  MEMORY_RECALL_CAP,
  type MemoryConceptLike,
} from "../src/pi/memory-match.js";

function concept(overrides: Partial<MemoryConceptLike> = {}): MemoryConceptLike {
  return {
    id: overrides.id ?? "c1",
    scope: overrides.scope ?? "user",
    groupId: overrides.groupId ?? "",
    title: overrides.title ?? "",
    description: overrides.description ?? "",
    tags: overrides.tags ?? [],
    status: overrides.status ?? "stable",
    trustTier: overrides.trustTier ?? "unverified",
    stale: overrides.stale ?? false,
  };
}

test("matchMemoryConcepts: tag matches on a word boundary, not a substring", () => {
  // WHY: "cat" must not match inside "category" — a substring match would
  // inject unrelated memory on nearly every turn.
  const c = concept({ tags: ["cat"] });
  assert.equal(matchMemoryConcepts("this category of problems", [c]).recalled.length, 0);
  assert.equal(matchMemoryConcepts("I have a cat", [c]).recalled.length, 1);
});

test("matchMemoryConcepts: title matches as a contiguous phrase", () => {
  const c = concept({ id: "billing", title: "invoice approval flow" });
  assert.equal(
    matchMemoryConcepts("walk me through the invoice approval flow", [c]).recalled.length,
    1,
  );
  assert.equal(
    matchMemoryConcepts("approval of an invoice needs a flow", [c]).recalled.length,
    0,
    "scattered title words must not match",
  );
});

test("matchMemoryConcepts: a concept with no tags and no title never matches", () => {
  const c = concept({ tags: [], title: "" });
  assert.equal(matchMemoryConcepts("anything at all", [c]).recalled.length, 0);
});

test("matchMemoryConcepts: deprecated concepts never surface even on a match", () => {
  const live = concept({ id: "live", tags: ["deploy"] });
  const dead = concept({ id: "dead", tags: ["deploy"], status: "deprecated" });
  const result = matchMemoryConcepts("please deploy", [dead, live]);
  assert.deepEqual(result.recalled.map((c) => c.id), ["live"]);
});

test("matchMemoryConcepts: ranks by match count, then fresh before stale, then id", () => {
  const two = concept({ id: "two-hits", tags: ["deploy", "release"] });
  const staleOne = concept({ id: "a-stale", tags: ["deploy"], stale: true });
  const freshOne = concept({ id: "z-fresh", tags: ["deploy"] });
  const result = matchMemoryConcepts("deploy the release", [staleOne, freshOne, two]);
  assert.deepEqual(
    result.recalled.map((c) => c.id),
    ["two-hits", "z-fresh", "a-stale"],
    "count desc, then fresh before stale, then id asc",
  );
});

test("matchMemoryConcepts: id breaks a tie between equal count and equal staleness", () => {
  const b = concept({ id: "b", tags: ["deploy"] });
  const a = concept({ id: "a", tags: ["deploy"] });
  assert.deepEqual(matchMemoryConcepts("deploy", [b, a]).recalled.map((c) => c.id), ["a", "b"]);
});

test("matchMemoryConcepts: caps the recall set at MEMORY_RECALL_CAP", () => {
  const many = ["a", "b", "c", "d", "e"].map((id) => concept({ id, tags: ["deploy"] }));
  const result = matchMemoryConcepts("deploy", many);
  assert.equal(result.recalled.length, MEMORY_RECALL_CAP);
  assert.deepEqual(result.recalled.map((c) => c.id), ["a", "b", "c"]);
});

test("matchMemoryConcepts: a non-word-edge tag still matches (shared pattern builder)", () => {
  // WHY: proves the matcher reuses skill-match's buildKeywordPattern rather
  // than a plain \b on both edges, which silently drops ".env"/"c++" keywords.
  const c = concept({ tags: [".env"] });
  assert.equal(matchMemoryConcepts("check the .env file", [c]).recalled.length, 1);
  assert.equal(matchMemoryConcepts("check the environment", [c]).recalled.length, 0);
});

// ── mergeRecall ──────────────────────────

test("mergeRecall: keyword entries lead, in their existing order", () => {
  const kw = [concept({ id: "kw-2" }), concept({ id: "kw-1" })];
  const result = mergeRecall(kw, []);
  assert.deepEqual(result.recalled.map((c) => c.id), ["kw-2", "kw-1"]);
  assert.equal(result.via.get("kw-2"), "keyword");
  assert.equal(result.via.get("kw-1"), "keyword");
});

test("mergeRecall: semantic entries fill remaining slots after keyword entries", () => {
  const kw = [concept({ id: "a" })];
  const sem = [concept({ id: "b" }), concept({ id: "c" })];
  const result = mergeRecall(kw, sem);
  assert.deepEqual(result.recalled.map((c) => c.id), ["a", "b", "c"]);
  assert.equal(result.via.get("a"), "keyword");
  assert.equal(result.via.get("b"), "semantic");
  assert.equal(result.via.get("c"), "semantic");
});

test("mergeRecall: a semantic entry already present by id is not duplicated", () => {
  const kw = [concept({ id: "a" })];
  const sem = [concept({ id: "a" }), concept({ id: "b" })];
  const result = mergeRecall(kw, sem);
  assert.deepEqual(result.recalled.map((c) => c.id), ["a", "b"]);
  assert.equal(result.via.get("a"), "keyword", "the keyword tier wins the tag for a shared id");
});

test("mergeRecall: total is capped at MEMORY_RECALL_CAP even when both tiers together exceed it", () => {
  const kw = [concept({ id: "kw-1" }), concept({ id: "kw-2" })];
  const sem = [concept({ id: "sem-1" }), concept({ id: "sem-2" }), concept({ id: "sem-3" })];
  const result = mergeRecall(kw, sem);
  assert.equal(result.recalled.length, MEMORY_RECALL_CAP);
  assert.deepEqual(result.recalled.map((c) => c.id), ["kw-1", "kw-2", "sem-1"]);
});

test("mergeRecall: an already-full keyword tier leaves no room for semantic entries", () => {
  const kw = ["a", "b", "c"].map((id) => concept({ id }));
  const sem = [concept({ id: "d" })];
  const result = mergeRecall(kw, sem);
  assert.deepEqual(result.recalled.map((c) => c.id), ["a", "b", "c"]);
  assert.equal(result.via.has("d"), false);
});

test("mergeRecall: empty keyword tier surfaces semantic entries alone", () => {
  const sem = [concept({ id: "only-semantic" })];
  const result = mergeRecall([], sem);
  assert.deepEqual(result.recalled.map((c) => c.id), ["only-semantic"]);
  assert.equal(result.via.get("only-semantic"), "semantic");
});

// cpuMs returns fn's best CPU time over up to three runs after a warm-up, so
// a busy test runner (time spent descheduled, a cold JIT) does not fail a
// bound the code itself meets.
function cpuMs(fn: () => void): number {
  fn();
  let best = Infinity;
  for (let i = 0; i < 3 && best >= 50; i++) {
    const start = process.cpuUsage();
    fn();
    const { user, system } = process.cpuUsage(start);
    best = Math.min(best, (user + system) / 1000);
  }
  return best;
}

test("F202 matchMemoryConcepts: adversarial phrase tags inside every cap stay under 50 ms", () => {
  // WHY: matching runs synchronously on the parent's shared event loop. 512
  // concepts x 32 63-char phrase tags ("a a … a z") plus a 199-char title of
  // the same shape against an 8000-char "a a …" prompt stalled every user for
  // ~32 s, all within the per-piece caps; only a bound on total work holds.
  // The short tag shape is the one the word cap lets through to the budget.
  for (const tag of ["a ".repeat(31) + "z", "a a a a a z"]) {
    const bundle = Array.from({ length: 512 }, (_, i) =>
      concept({ id: `c${i}`, tags: Array.from({ length: 32 }, () => tag), title: "a ".repeat(99) + "z" }),
    );
    for (const prompt of ["a ".repeat(4000), "a ".repeat(3999) + "z"]) {
      const ms = cpuMs(() => matchMemoryConcepts(prompt, bundle));
      assert.ok(ms < 50, `recall took ${ms} ms (tag ${tag.length} chars, prompt ends ${JSON.stringify(prompt.slice(-2))})`);
    }
  }
});

test("F202 matchMemoryConcepts: only the first 32 tags of at most 64 chars count", () => {
  const late = concept({ id: "late", tags: [...Array.from({ length: 32 }, (_, i) => `t${i}`), "deploy"] });
  const long = concept({ id: "long", tags: ["deploy " + "x".repeat(64)] });
  assert.equal(matchMemoryConcepts("deploy " + "x".repeat(64), [late, long]).recalled.length, 0);
  assert.equal(matchMemoryConcepts("about t3", [late]).recalled.length, 1);
});

test("F202 matchMemoryConcepts: keywords past the prompt clip are not matched", () => {
  const c = concept({ tags: ["needle"] });
  assert.equal(matchMemoryConcepts("x ".repeat(5000) + "needle", [c]).recalled.length, 0);
  assert.equal(matchMemoryConcepts("needle " + "x ".repeat(5000), [c]).recalled.length, 1);
});
