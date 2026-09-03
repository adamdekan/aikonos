// memory-semantic.ts — semantic-recall IO module: a second, best-effort recall tier layered on top of
// matchMemoryConcepts's keyword result. Embeds the prompt plus every
// recallable concept's text (cache permitting) in one batched call, then
// ranks by cosine similarity.
//
// WHY fail-open (unlike vision's fail-closed): recall is best-effort UX, not
// a security surface. ANY failure — knob off, no candidate, rate-limit
// denial, provider timeout/error, malformed response — degrades to nothing
// added, so the turn always runs with (at minimum) the keyword-only result
// callers already had. This module never throws.
import { createHash } from "node:crypto";
import type { Logger } from "pino";
import { callEmbeddingProvider, type EmbeddingCallResult } from "../llm/embed.js";
import { embeddingCandidates, type EmbeddingProviderLike } from "../llm/provider-fallback.js";
import type { RateLimitChecker } from "../llm/egress-proxy.js";
import { MEMORY_RECALL_CAP, recallableConcepts, type MemoryConceptLike } from "./memory-match.js";

// A concept's embed text is deterministic from its own fields (title,
// description, tags) — a fresh embedding is only worth the round trip when
// one of those fields changed since the last turn that saw this id.
const PROMPT_CLIP_CHARS = 8000;
const CACHE_MAX_ENTRIES = 2048;
const SIMILARITY_FLOOR = 0.30;

// VectorCache is a bounded in-process LRU keyed by a concept's content hash.
// A plain Map already preserves insertion order, so "delete then re-set" on a
// read is enough to bump an entry to most-recently-used without a dedicated
// linked-list structure.
export class VectorCache {
  private readonly map = new Map<string, number[]>();

  constructor(private readonly maxEntries: number = CACHE_MAX_ENTRIES) {}

  get(key: string): number[] | undefined {
    const hit = this.map.get(key);
    if (hit === undefined) return undefined;
    this.map.delete(key);
    this.map.set(key, hit);
    return hit;
  }

  set(key: string, vector: number[]): void {
    if (!this.map.has(key) && this.map.size >= this.maxEntries) {
      const oldest = this.map.keys().next().value;
      if (oldest !== undefined) this.map.delete(oldest);
    }
    this.map.delete(key);
    this.map.set(key, vector);
  }

  get size(): number {
    return this.map.size;
  }
}

// Shared across turns/users within this process — safe because the cache key
// is a hash of the concept's OWN content, never of who asked, so no cross-
// tenant information leaks through a cache hit.
const defaultCache = new VectorCache();

function cacheKey(concept: MemoryConceptLike): string {
  const raw = `${concept.id}|${concept.title}|${concept.description}|${concept.tags.join(",")}`;
  return createHash("sha256").update(raw).digest("hex");
}

function conceptText(concept: MemoryConceptLike): string {
  return `${concept.title}\n${concept.description}\n${concept.tags.join(" ")}`;
}

// cosine is computed explicitly (dot product over the norms) rather than
// assuming the provider returns unit-normalized vectors — providers differ on
// this and a wrong assumption silently corrupts every ranking.
function cosine(a: number[], b: number[]): number {
  const len = Math.min(a.length, b.length);
  let dot = 0;
  let normA = 0;
  let normB = 0;
  for (let i = 0; i < len; i++) {
    dot += a[i] * b[i];
    normA += a[i] * a[i];
    normB += b[i] * b[i];
  }
  if (normA === 0 || normB === 0) return 0;
  return dot / (Math.sqrt(normA) * Math.sqrt(normB));
}

// SemanticEmbeddingProvider is the structural subset of LlmProvider (proto)
// this module needs: enough for embeddingCandidates' selection (id/enabled/
// api/models) plus the connection details callEmbeddingProvider requires
// (endpoint/apiKey/apiVersion). No proto import — same posture as
// provider-fallback.ts/embed.ts.
export interface SemanticEmbeddingProvider extends EmbeddingProviderLike {
  endpoint: string;
  apiKey: string;
  apiVersion?: string;
}

// EmitEmbeddingUsageRequest mirrors the fields of the proto EmitLlmUsageRequest
// this call site sends — structural, so this module stays decoupled from the
// generated proto types (matching GovernanceBridge's emitParentLlmUsage).
export interface EmitEmbeddingUsageRequest {
  tenantId: string;
  userId: string;
  agentId: string;
  provider: string;
  model: string;
  tokensIn: number;
  tokensOut: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
  runId: string;
  sessionId: string;
  source: string;
  quantity: number;
  unit: string;
}

export interface SemanticRecallSouth {
  getLlmProviders(req: {
    tenantId: string;
  }): Promise<{ providers: SemanticEmbeddingProvider[]; defaults: Record<string, string> }>;
  emitLlmUsage(req: EmitEmbeddingUsageRequest): Promise<void>;
}

export interface SemanticRecallParams<T extends MemoryConceptLike> {
  // Master switch (Config.memorySemanticRecall) — false skips before any I/O.
  enabled: boolean;
  prompt: string;
  // The full recallable-candidate pool for this turn (the same list
  // matchMemoryConcepts was given) — NOT pre-filtered to unmatched-by-keyword;
  // mergeRecall handles dedup-by-id when combining the two tiers.
  concepts: T[];
  // How many concepts the keyword tier already recalled — a full cap means
  // there is no slot semantic could fill, so skip before any I/O.
  keywordMatchCount: number;
  tenantId: string;
  userId: string;
  // Bare agent id ("" when the turn is not agent-bound) — usage attribution.
  agentId: string;
  runId: string;
  timeoutMs: number;
  south: SemanticRecallSouth;
  rateLimitChecker: RateLimitChecker;
  log: Pick<Logger, "warn">;
  // Injection seams for tests — production defaults to the real call/cache.
  embed?: (opts: {
    provider: SemanticEmbeddingProvider;
    modelId: string;
    inputs: string[];
    timeoutMs?: number;
  }) => Promise<EmbeddingCallResult>;
  cache?: VectorCache;
}

// semanticRecall computes the semantic tier for one turn. Never throws — any
// failure (including a rate-limit denial, which the checker signals by
// rejecting) is caught, logged once, and degrades to an empty result so the
// caller's keyword-only recall is unaffected.
export async function semanticRecall<T extends MemoryConceptLike>(
  params: SemanticRecallParams<T>,
): Promise<T[]> {
  const {
    enabled, prompt, concepts, keywordMatchCount, tenantId, userId, agentId, runId,
    timeoutMs, south, rateLimitChecker, log,
    embed = callEmbeddingProvider,
    cache = defaultCache,
  } = params;

  if (!enabled) return [];
  if (keywordMatchCount >= MEMORY_RECALL_CAP) return [];
  const candidates = recallableConcepts(concepts);
  if (candidates.length === 0) return [];

  try {
    const { providers, defaults } = await south.getLlmProviders({ tenantId });
    const chain = embeddingCandidates(providers, defaults ?? {});
    if (chain.length === 0) return [];
    const picked = chain[0];

    // Pre-gate before any embed call — a denial (the checker rejects) is
    // caught below and degrades exactly like any other failure.
    await rateLimitChecker(tenantId, agentId, new URL(picked.provider.endpoint).hostname, userId);

    const keyed = candidates.map((concept) => ({ concept, key: cacheKey(concept) }));
    const vectors = new Map<string, number[]>();
    const uncached: { concept: T; key: string }[] = [];
    for (const entry of keyed) {
      const hit = cache.get(entry.key);
      if (hit) vectors.set(entry.key, hit);
      else uncached.push(entry);
    }

    // One batched call per turn: the prompt (never cached — it changes every
    // turn) plus every concept text not already cached.
    const inputs = [prompt.slice(0, PROMPT_CLIP_CHARS), ...uncached.map((u) => conceptText(u.concept))];
    const result = await embed({ provider: picked.provider, modelId: picked.modelId, inputs, timeoutMs });
    const promptVector = result.embeddings[0];

    uncached.forEach((entry, i) => {
      const vector = result.embeddings[i + 1];
      if (vector) {
        cache.set(entry.key, vector);
        vectors.set(entry.key, vector);
      }
    });

    const ranked = keyed
      .map(({ concept, key }) => ({ concept, score: cosine(promptVector, vectors.get(key) ?? []) }))
      .filter((r) => r.score >= SIMILARITY_FLOOR)
      .sort((a, b) => b.score - a.score || a.concept.id.localeCompare(b.concept.id))
      .map((r) => r.concept);

    // Fire-and-forget usage emit — an emit failure must never fail a recall
    // that otherwise succeeded. quantity/unit are 0/"" (token-billed, not a
    // per-unit-priced call); the broker computes cost from tokens × rate.
    void south
      .emitLlmUsage({
        tenantId, userId, agentId,
        provider: picked.provider.id,
        model: picked.modelId,
        tokensIn: result.tokensIn,
        tokensOut: 0,
        cacheRead: 0,
        cacheWrite: 0,
        cost: 0,
        runId,
        sessionId: "",
        source: "embedding",
        quantity: 0,
        unit: "",
      })
      .catch((err: unknown) => {
        log.warn({ err: String(err) }, "memory semantic recall: emitLlmUsage failed");
      });

    return ranked;
  } catch (err) {
    log.warn({ err: String(err) }, "memory semantic recall failed — degrading to keyword-only");
    return [];
  }
}
