// memory-match.ts — pure keyword matcher for memory auto-recall.
//
// Given the user's chat message and the concept metas the broker returned for
// this user (already gated on skill:memory.read and, for the agent bundle, on
// the agent's own Skills — , Auto-recall), decide
// which concepts get injected as a per-turn prompt preamble.
//
// WHY pure: no I/O, no broker calls, no side effects. The /agui route supplies
// the message and the meta list; this module only ranks and caps, so the recall
// rules can be exhaustively unit-tested without a gateway harness.
//
// Matching rule is deliberately identical to auto skill-loading's: reuse
// buildKeywordPattern rather than reimplement the boundary logic. Keywords for
// a concept are its tags plus its title (the title matched as one phrase).
// Frontmatter only — bodies are never matched and never injected.
import { buildKeywordPattern } from "./skill-match.js";

export const MEMORY_RECALL_CAP = 3;

// MemoryConceptLike is the structural subset of the proto MemoryConceptMeta
// that the recall path reads — matching (tags/title/status/stale/id) plus the
// fields the preamble and SSE payload render. Keeps the matcher and the route
// free of proto coupling; MemoryConceptMeta satisfies it as-is.
export interface MemoryConceptLike {
  id: string;
  scope: string;
  groupId: string;
  title: string;
  description: string;
  tags: string[];
  status: string;
  trustTier: string;
  stale: boolean;
}

function countKeywordMatches(message: string, keywords: string[]): number {
  let count = 0;
  for (const keyword of keywords) {
    const pattern = buildKeywordPattern(keyword);
    if (pattern && pattern.test(message)) count++;
  }
  return count;
}

// recallableConcepts drops deprecated concepts — a retired concept must never
// occupy a cap slot that a live one could have used, in either recall tier.
// Shared by matchMemoryConcepts and memory-semantic.ts's semanticRecall so the
// two tiers apply the exact same eligibility rule.
export function recallableConcepts<T extends MemoryConceptLike>(concepts: T[]): T[] {
  return concepts.filter((concept) => concept.status !== "deprecated");
}

// matchMemoryConcepts computes the recall set for one turn.
//
// Deprecated concepts are filtered BEFORE matching — a retired concept must
// never occupy a cap slot that a live one could have used.
//
// Ordering: match count desc (more distinct keywords hit = more relevant),
// then fresh before stale (a stale concept is still useful but outranked by
// current knowledge), then id asc for determinism.
export function matchMemoryConcepts<T extends MemoryConceptLike>(
  message: string,
  concepts: T[],
): { recalled: T[] } {
  const recalled = recallableConcepts(concepts)
    .map((concept) => ({
      concept,
      count: countKeywordMatches(message, [...concept.tags, concept.title]),
    }))
    .filter((candidate) => candidate.count > 0)
    .sort(
      (a, b) =>
        b.count - a.count ||
        Number(a.concept.stale) - Number(b.concept.stale) ||
        a.concept.id.localeCompare(b.concept.id),
    )
    .slice(0, MEMORY_RECALL_CAP)
    .map((candidate) => candidate.concept);

  return { recalled };
}

// RecallVia tags how each entry in a merged recall set was found — surfaced
// on the SSE payload so the
// webui can label a recall chip "keyword" vs "semantic".
export type RecallVia = "keyword" | "semantic";

// mergeRecall combines the keyword tier (always tried first, cheap, no
// network) with the semantic tier (best-effort, embeddings-backed) into one
// capped recall set: keyword entries keep their existing order and win ties,
// then semantic entries not already present (by id) fill any remaining slots,
// total capped at MEMORY_RECALL_CAP. Pure — semantic-tier ranking/filtering
// (cosine floor, dedup-by-cache) is memory-semantic.ts's job; this function
// only merges two already-ranked lists.
export function mergeRecall<T extends MemoryConceptLike>(
  keyword: T[],
  semantic: T[],
): { recalled: T[]; via: Map<string, RecallVia> } {
  const via = new Map<string, RecallVia>();
  const recalled: T[] = [];

  for (const concept of keyword) {
    if (recalled.length >= MEMORY_RECALL_CAP) break;
    recalled.push(concept);
    via.set(concept.id, "keyword");
  }
  for (const concept of semantic) {
    if (recalled.length >= MEMORY_RECALL_CAP) break;
    if (via.has(concept.id)) continue;
    recalled.push(concept);
    via.set(concept.id, "semantic");
  }

  return { recalled, via };
}
