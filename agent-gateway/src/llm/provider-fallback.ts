// Single provider-selection order for every LLM call site in the gateway:
// interactive chat (pi/session.ts, pi/session-plan.ts), workflow reason steps
// (reason.ts), and vision (vision.ts). Before this module the three-tier chat
// order was copy-pasted in two of those four places and reason.ts had only one
// tier — adding the tenant fallback tier would have made it three copies.
//
// The tenant-designated fallback (LlmProvider.is_fallback) REPLACES the former
// "any enabled provider with a model" last tier: an arbitrary first-match
// lottery is worse than a deliberate operator choice, and worse than failing.
//
// Structural types only (no proto import) so the ordering stays unit-testable
// and the module can't drag secrets or gRPC types into a call site.

export interface ProviderModelLike {
  id: string;
  // The model's own max output-token budget, straight off the provider record
  // (LlmProviderModel.max_tokens). 0 or absent means the operator has not set
  // one. Optional so the narrower stub shapes need not restate it.
  maxTokens?: number;
  // The modality this model serves (LlmModel.mode). Absent/empty == "chat" —
  // matches the proto's own documented default. Only embeddingCandidates below
  // reads this; the chat/vision tiers never filter on it.
  mode?: string;
}

export interface ChatProviderLike {
  id: string;
  enabled: boolean;
  isDefault: boolean;
  // Absent = not the tenant fallback. Optional so the narrower stub shapes
  // (session-plan's ResolveSouth, reason.ts) need not restate the whole proto.
  isFallback?: boolean;
  models: ProviderModelLike[];
}

export interface VisionProviderLike {
  id: string;
  enabled: boolean;
  visionCapable: boolean;
  isDefaultVision: boolean;
  isFallback?: boolean;
  models: ProviderModelLike[];
}

// The fields of an agent spec that influence selection. AgentSpec satisfies it.
export interface SpecLike {
  preferredProvider?: string;
  model?: string;
}

export interface Candidate<P> {
  provider: P;
  modelId: string;
  // The selected model's own output-token budget, 0 when the operator has not
  // set one. Carried here because this module is the only place that decides
  // *which* model a call uses, so it is the only place that can say which
  // model's budget applies. What 0 means is the call site's business: reason
  // steps fall back to their configured default, vision has no budget at all.
  maxTokens: number;
}

// A candidate keeps the agent's named model when its provider lists it, else
// the provider's first model. Returns the model entry rather than just its id
// so the caller also gets that model's token budget.
function modelFor(provider: ChatProviderLike, spec?: SpecLike): ProviderModelLike {
  const named = spec?.model ? provider.models.find((m) => m.id === spec.model) : undefined;
  return named ?? provider.models[0];
}

// Drops undefined tiers and a provider already claimed by an earlier tier — the
// tenant default may itself be marked the fallback.
function dedupe<P extends { id: string }>(tiers: (P | undefined)[]): P[] {
  const seen = new Set<string>();
  const ordered: P[] = [];
  for (const provider of tiers) {
    if (!provider || seen.has(provider.id)) continue;
    seen.add(provider.id);
    ordered.push(provider);
  }
  return ordered;
}

// chatCandidates returns the ordered chain for a chat/text completion:
// agent-assigned provider+model → tenant default → tenant fallback.
// Only enabled providers carrying at least one model qualify. Empty result =
// the tenant has nothing usable; the caller decides whether that throws.
export function chatCandidates<P extends ChatProviderLike>(
  providers: P[],
  spec?: SpecLike,
): Candidate<P>[] {
  const usable = providers.filter((p) => p.enabled && p.models.length > 0);
  const assigned =
    spec?.preferredProvider && spec.model
      ? usable.find((p) => p.id === spec.preferredProvider && p.models.some((m) => m.id === spec.model))
      : undefined;
  return dedupe([
    assigned,
    usable.find((p) => p.isDefault),
    usable.find((p) => p.isFallback),
  ]).map((provider) => {
    const model = modelFor(provider, spec);
    return { provider, modelId: model.id, maxTokens: model.maxTokens ?? 0 };
  });
}

// visionCandidates returns the ordered chain for an image call:
// tenant default-vision → tenant fallback. Both must be visionCapable — a
// fallback that can't take an image would fail by construction, so it is
// skipped rather than tried. There is no agent-level vision override.
export function visionCandidates<P extends VisionProviderLike>(providers: P[]): Candidate<P>[] {
  const usable = providers.filter((p) => p.enabled && p.visionCapable && p.models.length > 0);
  return dedupe([
    usable.find((p) => p.isDefaultVision),
    usable.find((p) => p.isFallback),
  ]).map((provider) => ({
    provider,
    modelId: provider.models[0].id,
    maxTokens: provider.models[0].maxTokens ?? 0,
  }));
}

export interface EmbeddingProviderLike {
  id: string;
  enabled: boolean;
  api: string;
  models: ProviderModelLike[];
}

// embeddingCandidates returns the ordered chain for an embeddings call: the
// tenant-designated default embedding provider → the first other usable
// provider. Usable = enabled, not anthropic-messages (no embeddings API at
// all), and carrying at least one embedding-mode model — a provider whose
// only models are chat models is never selected even if it is the tenant's
// chat default. The candidate's model is always that provider's first
// embedding-mode model, never its first model overall.
export function embeddingCandidates<P extends EmbeddingProviderLike>(
  providers: P[],
  defaults: Record<string, string>,
): Candidate<P>[] {
  const usable = providers.filter(
    (p) => p.enabled && p.api !== "anthropic-messages" && p.models.some((m) => m.mode === "embedding"),
  );
  const defaultId = defaults["embedding"];
  return dedupe([usable.find((p) => p.id === defaultId), usable[0]]).map((provider) => {
    const model = provider.models.filter((m) => m.mode === "embedding")[0];
    return { provider, modelId: model.id, maxTokens: model.maxTokens ?? 0 };
  });
}

// shouldFailover decides whether a failed upstream attempt is worth retrying on
// the next candidate in the chain.
//
// 4xx request errors (400 malformed body, 404 unknown model/route, 422
// unprocessable) are excluded deliberately: the fallback would receive the same
// body and reject it identically, so retrying only burns quota and latency.
// 401/403 (this provider's key) and 429 (this provider's quota) are per-provider
// conditions a different provider can genuinely satisfy.
export function shouldFailover(r: { status?: number; transportError?: boolean }): boolean {
  if (r.transportError) return true;
  if (r.status === undefined) return false;
  return r.status >= 500 || r.status === 429 || r.status === 401 || r.status === 403;
}
