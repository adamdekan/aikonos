// Tests for the auto-skill-loading keyword matcher (skill-match.ts).
//
// WHY these tests exist: matchSkillBundles is the sole gate deciding which
// bundles auto-activate for a chat turn before the LLM sees the prompt. It
// must be exhaustively covered — a false match auto-grants tool access for the
// turn; a missed suppression silently drops a bundle that should have been
// reported to the user's timeline.
import { test } from "node:test";
import assert from "node:assert/strict";
import { matchSkillBundles, AUTO_LOAD_CAP, type MatchableBundle } from "../src/pi/skill-match.js";

function bundle(overrides: Partial<MatchableBundle> = {}): MatchableBundle {
  return {
    name: overrides.name ?? "bundle",
    description: overrides.description ?? "does a thing",
    keywords: overrides.keywords ?? [],
    disableModelInvocation: overrides.disableModelInvocation ?? false,
  };
}

test("matchSkillBundles: word-boundary match, not substring", () => {
  // WHY: "cat" must not match inside "category" — a substring match would
  // flood the timeline with irrelevant activations on common short keywords.
  const b = bundle({ name: "cats", keywords: ["cat"] });
  assert.equal(matchSkillBundles("I love this category of problems", [b]).loaded.length, 0);
  assert.equal(matchSkillBundles("I have a cat", [b]).loaded.length, 1);
});

test("matchSkillBundles: case-insensitive", () => {
  const b = bundle({ name: "deploy", keywords: ["deploy"] });
  const result = matchSkillBundles("Please DEPLOY the service", [b]);
  assert.equal(result.loaded.length, 1);
  assert.equal(result.loaded[0].name, "deploy");
});

test("matchSkillBundles: multi-word keyword requires contiguous phrase", () => {
  // WHY: "pull request" must match as a phrase, not as "pull" and "request"
  // appearing independently anywhere in the message.
  const b = bundle({ name: "gh", keywords: ["pull request"] });
  assert.equal(matchSkillBundles("please review this pull request", [b]).loaded.length, 1);
  assert.equal(
    matchSkillBundles("please pull the branch and request a review", [b]).loaded.length,
    0,
    "scattered words must not match a phrase keyword",
  );
});

test("matchSkillBundles: empty keywords never match", () => {
  const b = bundle({ name: "silent", keywords: [] });
  const result = matchSkillBundles("silent should never load", [b]);
  assert.equal(result.loaded.length, 0);
  assert.equal(result.suppressed.length, 0);
});

test("matchSkillBundles: disable_model_invocation bundles are matched but suppressed, never loaded", () => {
  const b = bundle({ name: "blocked", keywords: ["blocked"], disableModelInvocation: true });
  const result = matchSkillBundles("this message says blocked", [b]);
  assert.equal(result.loaded.length, 0);
  assert.equal(result.suppressed.length, 1);
  assert.equal(result.suppressed[0].entry.name, "blocked");
  assert.equal(result.suppressed[0].reason, "flag_blocked");
});

test("matchSkillBundles: flag-blocked bundle does not consume a cap slot", () => {
  // A flag-blocked bundle ranked ahead of eligible ones must not push an
  // eligible bundle out of the top-3 — it never could have activated anyway.
  const blocked = bundle({ name: "aaa-blocked", keywords: ["shared"], disableModelInvocation: true });
  const eligible = [
    bundle({ name: "b1", keywords: ["shared"] }),
    bundle({ name: "b2", keywords: ["shared"] }),
    bundle({ name: "b3", keywords: ["shared"] }),
  ];
  const result = matchSkillBundles("shared shared shared shared", [blocked, ...eligible]);
  assert.equal(result.loaded.length, 3, "all 3 eligible bundles must load despite the blocked one ranking first alphabetically");
  assert.deepEqual(result.loaded.map((b) => b.name).sort(), ["b1", "b2", "b3"]);
  assert.equal(result.suppressed.length, 1);
  assert.equal(result.suppressed[0].reason, "flag_blocked");
});

test("matchSkillBundles: cap of 3 — overflow suppressed with cap_overflow, ordered match-count desc then name", () => {
  const b1 = bundle({ name: "one-match", keywords: ["alpha"] });
  const b2 = bundle({ name: "two-match", keywords: ["alpha", "beta"] });
  const b3 = bundle({ name: "three-match", keywords: ["alpha", "beta", "gamma"] });
  const b4 = bundle({ name: "also-one-match", keywords: ["alpha"] });
  const message = "alpha beta gamma";

  const result = matchSkillBundles(message, [b1, b2, b3, b4]);

  assert.equal(result.loaded.length, AUTO_LOAD_CAP);
  // three-match (3 keywords hit) > two-match (2) > tie between one-match and
  // also-one-match (1 each) broken by name asc: "also-one-match" < "one-match".
  assert.deepEqual(result.loaded.map((b) => b.name), ["three-match", "two-match", "also-one-match"]);
  assert.equal(result.suppressed.length, 1);
  assert.equal(result.suppressed[0].entry.name, "one-match");
  assert.equal(result.suppressed[0].reason, "cap_overflow");
});

test("matchSkillBundles: no candidates at all when nothing matches — both lists empty", () => {
  const b = bundle({ name: "unrelated", keywords: ["xyz-nonexistent-term"] });
  const result = matchSkillBundles("hello world", [b]);
  assert.deepEqual(result, { loaded: [], suppressed: [] });
});

test("matchSkillBundles: a keyword repeated in the message still counts once toward ranking", () => {
  const b = bundle({ name: "repeat", keywords: ["alpha"] });
  const result = matchSkillBundles("alpha alpha alpha", [b]);
  assert.equal(result.loaded.length, 1);
});

// ── keywords with non-word edge characters (reviewer-found bug) ──────────────
//
// \b requires a transition between a \w and non-\w character. A keyword like
// "c++" ends in a non-word character ("+"), so a trailing \b never matches
// when followed by another non-word character (e.g. a space) — both sides of
// that position are non-word, so there's no transition. The matcher must
// still treat these as whole-term matches, not silently drop them.

test("matchSkillBundles: keyword ending in a non-word character (c++) matches at a normal word boundary", () => {
  const b = bundle({ name: "cpp-bundle", keywords: ["c++"] });
  const result = matchSkillBundles("I love c++ language", [b]);
  assert.equal(result.loaded.length, 1, "c++ followed by a space must match");
});

test("matchSkillBundles: keyword ending in a non-word character (C#) matches case-insensitively", () => {
  const b = bundle({ name: "csharp-bundle", keywords: ["C#"] });
  const result = matchSkillBundles("let's write some c# code today", [b]);
  assert.equal(result.loaded.length, 1, "c# followed by a space must match");
});

test("matchSkillBundles: keyword ending in a non-word character (C#) does not match as a substring inside a longer token", () => {
  // "c#x" is a single contiguous token — "C#" must NOT match inside it: a
  // word character ("x") immediately follows the "#", so this is a checkable
  // false-positive the trailing-edge lookaround must reject.
  const b = bundle({ name: "csharp-bundle", keywords: ["C#"] });
  const result = matchSkillBundles("look at c#x for details", [b]);
  assert.equal(result.loaded.length, 0, "C# must not match when immediately followed by a word character");
});

test("matchSkillBundles: keyword starting with a non-word character (.env) matches after a space", () => {
  const b = bundle({ name: "dotenv-bundle", keywords: [".env"] });
  const result = matchSkillBundles("please read the .env file", [b]);
  assert.equal(result.loaded.length, 1, ".env preceded by a space must match");
});

test("matchSkillBundles: keyword starting with a non-word character (.env) does not match as a substring inside a longer word", () => {
  const b = bundle({ name: "dotenv-bundle", keywords: [".env"] });
  const result = matchSkillBundles("myapp.environment.production", [b]);
  assert.equal(result.loaded.length, 0, ".env must not match inside myapp.environment (word char 'i' follows immediately)");
});

test("matchSkillBundles: keyword starting with a non-word character (#hashtag) matches at start of message", () => {
  const b = bundle({ name: "hashtag-bundle", keywords: ["#hashtag"] });
  const result = matchSkillBundles("#hashtag season is here", [b]);
  assert.equal(result.loaded.length, 1, "#hashtag at the very start of the message must match");
});

// ── union input ─────────────────────────────
//
// matchSkillBundles is generic over MatchableBundle and does no origin-aware
// branching — a personal entry (qualified name "personal:<name>", per load-
// skill.ts's PERSONAL_SKILL_PREFIX) is just another candidate. These tests pin
// that the matcher needs no changes to work correctly over a mixed list.

test("matchSkillBundles: matches across a union of admin bundles and personal entries", () => {
  const adminBundle = bundle({ name: "deploy-helper", keywords: ["deploy"] });
  const personal = bundle({ name: "personal:my-notes", keywords: ["notes"] });
  const result = matchSkillBundles("please deploy and check my notes", [adminBundle, personal]);
  assert.deepEqual(result.loaded.map((b) => b.name).sort(), ["deploy-helper", "personal:my-notes"]);
});

test("matchSkillBundles: cap and flag-blocked suppression apply identically to a personal entry in the union", () => {
  const personalBlocked = bundle({ name: "personal:blocked", keywords: ["shared"], disableModelInvocation: true });
  const eligible = [
    bundle({ name: "b1", keywords: ["shared"] }),
    bundle({ name: "b2", keywords: ["shared"] }),
    bundle({ name: "personal:b3", keywords: ["shared"] }),
  ];
  const result = matchSkillBundles("shared shared shared shared", [personalBlocked, ...eligible]);
  assert.equal(result.loaded.length, 3, "all 3 eligible union entries must load");
  assert.deepEqual(result.loaded.map((b) => b.name).sort(), ["b1", "b2", "personal:b3"]);
  assert.equal(result.suppressed.length, 1);
  assert.equal(result.suppressed[0].entry.name, "personal:blocked");
  assert.equal(result.suppressed[0].reason, "flag_blocked");
});
