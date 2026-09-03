// skill-match.ts — pure keyword matcher for auto skill-bundle loading.
//
// Given the user's chat message and the FGA-granted bundle list (already
// filtered by listUserAgentSkills), decide which bundles should activate for
// the turn before the LLM sees the prompt.
//
// WHY pure: no I/O, no broker/south calls, no side effects. The caller (the
// /agui route) supplies the message and bundle list; this module only computes
// the match/suppress sets so it can be exhaustively unit-tested without a
// gateway harness.
//
// Matching rule: case-insensitive, word-boundary. A multi-word keyword (e.g.
// "pull request") only matches a contiguous word sequence in the message —
// not each word independently. Bundles with an empty keywords list never match
// (an admin must opt a bundle into auto-loading).
export const AUTO_LOAD_CAP = 3;

export type SuppressReason = "flag_blocked" | "cap_overflow";

// MatchableBundle is the minimal shape the matcher needs — a structural subset
// of SkillBundleEntry so callers/tests don't need to build a full bundle.
export interface MatchableBundle {
  name: string;
  description: string;
  keywords: string[];
  disableModelInvocation: boolean;
}

export interface SuppressedMatch<T> {
  entry: T;
  reason: SuppressReason;
}

export interface SkillMatchResult<T> {
  loaded: T[];
  suppressed: SuppressedMatch<T>[];
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// buildKeywordPattern turns a (possibly multi-word) keyword into a boundary-
// anchored regex that requires the words to appear contiguously (whitespace-
// separated), not as independent substrings scattered across the message.
//
// WHY not a plain \b on both edges: \b only matches at a transition between a
// \w and a non-\w character. A keyword like "c++" or "C#" ends in a non-word
// character, so a trailing \b never fires when that edge is followed by
// another non-word character (e.g. a space) — both sides are non-word, so
// there's no transition, and the match is silently dropped. Symmetric bug on
// a leading edge for keywords like ".env" or "#hashtag". The fix applies \b
// only on an edge whose keyword character is a word character; a non-word
// edge instead uses a lookaround that forbids a word character immediately
// outside the match (so it still won't match mid-word), without requiring an
// actual \w/non-\w transition.
// Exported for memory-match.ts: the
// memory recall matcher must use the exact same boundary rule, so it reuses
// this builder instead of growing a second copy that can drift.
export function buildKeywordPattern(keyword: string): RegExp | null {
  const trimmed = keyword.trim();
  if (!trimmed) return null;
  const escaped = escapeRegExp(trimmed).replace(/\s+/g, "\\s+");
  const startBoundary = /^\w/.test(trimmed) ? "\\b" : "(?<!\\w)";
  const endBoundary = /\w$/.test(trimmed) ? "\\b" : "(?!\\w)";
  return new RegExp(`${startBoundary}${escaped}${endBoundary}`, "gi");
}

// countKeywordMatches returns how many of the bundle's keywords occur (as
// whole-word/whole-phrase, case-insensitive matches) in the message. Each
// keyword counted once regardless of repeat occurrences — the count is used
// only to rank distinct-keyword relevance, not term frequency.
function countKeywordMatches(message: string, keywords: string[]): number {
  let count = 0;
  for (const keyword of keywords) {
    const pattern = buildKeywordPattern(keyword);
    if (pattern && pattern.test(message)) count++;
  }
  return count;
}

// matchSkillBundles computes the auto-load match set for one turn.
//
// Ordering: candidates are ranked by match-count desc, then name asc, before
// the cap and flag checks are applied — so the cap always keeps the most
// relevant matches and ties break deterministically.
//
// disable_model_invocation bundles ARE matched but never activate — they are
// reported as suppressed with reason "flag_blocked" and do not consume a cap
// slot (a bundle that could never activate should not push an eligible one
// out of the top 3).
export function matchSkillBundles<T extends MatchableBundle>(message: string, bundles: T[]): SkillMatchResult<T> {
  const candidates = bundles
    .map((bundle) => ({ bundle, count: countKeywordMatches(message, bundle.keywords) }))
    .filter((c) => c.count > 0)
    .sort((a, b) => b.count - a.count || a.bundle.name.localeCompare(b.bundle.name));

  const loaded: T[] = [];
  const suppressed: SuppressedMatch<T>[] = [];

  for (const { bundle } of candidates) {
    if (bundle.disableModelInvocation) {
      suppressed.push({ entry: bundle, reason: "flag_blocked" });
      continue;
    }
    if (loaded.length >= AUTO_LOAD_CAP) {
      suppressed.push({ entry: bundle, reason: "cap_overflow" });
      continue;
    }
    loaded.push(bundle);
  }

  return { loaded, suppressed };
}
