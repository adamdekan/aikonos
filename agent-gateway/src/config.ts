// Centralised configuration for the agent-gateway. Loads .env (Node 22
// process.loadEnvFile) when present, then reads from the environment.
import { existsSync } from "node:fs";
import { resolve } from "node:path";
import { agentOverridesFromEnv } from "./broker/agent-identity";

// Node >=20.12 / 22 exposes process.loadEnvFile; older runtimes do not.
type NodeWithLoadEnv = typeof process & { loadEnvFile?: (p: string) => void };

function loadDotenv(): void {
  const p = resolve(process.cwd(), ".env");
  if (existsSync(p)) {
    const nodeProc = process as NodeWithLoadEnv;
    if (typeof nodeProc.loadEnvFile === "function") {
      try {
        nodeProc.loadEnvFile(p);
      } catch {
        /* best-effort; env may already be populated by the container */
      }
    }
  }
}
loadDotenv();

function get(env: Record<string, string | undefined>, name: string, fallback?: string): string {
  const v = env[name] ?? fallback;
  if (v === undefined) throw new Error(`missing required env var ${name}`);
  return v;
}

// Rejects any value that is not exactly "true" or "false".
// The current parse (=== "true") silently treats "yes"/"1"/etc. as false —
// an actionable error at startup is better than a wrong-direction bool.
function parseBool(env: Record<string, string | undefined>, name: string, fallback: string): boolean {
  const raw = env[name] ?? fallback;
  if (raw !== "true" && raw !== "false") {
    throw new Error(`invalid ${name}="${raw}": expected true or false`);
  }
  return raw === "true";
}

// Rejects NaN and non-positive values for vars where 0 / negative has no
// defined meaning. All numeric config vars have positive defaults.
function parsePositiveInt(env: Record<string, string | undefined>, name: string, fallback: string): number {
  const raw = env[name] ?? fallback;
  const n = Number(raw);
  if (!Number.isFinite(n) || !Number.isInteger(n) || n <= 0) {
    throw new Error(`invalid ${name}="${raw}": expected positive integer`);
  }
  return n;
}

// Same as parsePositiveInt but admits 0, for vars where 0 means "disabled"
// (maxLlmCallsPerRun, approvalTimeoutMs). Negatives and NaN are still rejected —
// they have no meaning and would silently disable the guard.
function parseNonNegativeInt(env: Record<string, string | undefined>, name: string, fallback: string): number {
  const raw = env[name] ?? fallback;
  const n = Number(raw);
  if (!Number.isFinite(n) || !Number.isInteger(n) || n < 0) {
    throw new Error(`invalid ${name}="${raw}": expected non-negative integer`);
  }
  return n;
}

export interface Config {
  openrouterApiKey: string;
  llmModel: string;
  brokerNorthAddr: string;
  brokerSouthAddr: string;
  brokerServerName: string;
  tlsCert: string;
  tlsKey: string;
  tlsCa: string;
  gatewaySpiffeId: string;
  port: number;
  defaultTenantId: string;
  // OIDC JWT verification (gateway validates incoming bearers via JWKS).
  // oidcJwksUrl is the container-facing JWKS endpoint (e.g. http://keycloak:8080/…).
  // oidcIssuer is the browser-facing issuer written into tokens (e.g. http://localhost:18080/…).
  // oidcAudience is the expected aud claim (aikonos-broker).
  // When oidcIssuer is empty, verification is skipped (unit-test / dev passthrough).
  oidcIssuer: string;
  oidcJwksUrl: string;
  oidcAudience: string;
  // Principal + tenant claim names. Default sub/tenant_id (Keycloak); oid/tid for Entra.
  oidcSubjectClaim: string;
  oidcTenantClaim: string;
  schedulerEnabled: boolean;
  schedulerTickMs: number;
  schedulerClaimLimit: number;
  schedulerRunTimeoutMs: number;
  // Optional user→agentId overrides (JSON object; default localpart-agent).
  agentForUserOverrides: Record<string, string>;
  // External API surface (per-agent keys, `:8090`).
  externalPort: number;
  // Comma-separated list of allowed CORS origins for the external surface.
  // Empty means no CORS is applied (same-origin only in practice).
  externalCorsOrigins: string[];
  // Max requests per minute per IP on the external surface.
  externalRateLimit: number;
  // Idle TTL for persistent thread sessions (ms). A session not used for this
  // long is evicted so its OIDC bearer becomes GC-eligible. Default 30 minutes.
  threadTtlMs: number;
  // ChildSupervisor pool cap and idle-eviction TTL (ms). Consumed by
  // src/ipc/supervisor.ts via the injected SupervisorConfig override — env is
  // no longer read directly at that layer (F26).
  maxChildren: number;
  childTtlMs: number;
  // NATS url for the audit consumer (src/audit/stream.ts). Preserves the
  // legacy NATS_URL fallback; an explicit empty string disables the consumer.
  natsUrl: string;
  // NATS subject the audit consumer subscribes to.
  auditSubject: string;
  // Headers + idle-between-data timeout (ms) for upstream LLM egress requests
  // (src/llm/egress-proxy.ts). Parsed here for F26; consumed starting CP2 (F10).
  egressTimeoutMs: number;
  // Deadline (ms) applied to every unary north/south broker RPC (src/broker/unary.ts).
  // A hung broker call now fails DEADLINE_EXCEEDED instead of hanging the request.
  brokerTimeoutMs: number;
  // Consecutive transport-failure threshold before the rate-limit circuit
  // breaker opens (src/llm/rate-limit-breaker.ts, CP4.1). Not compose-
  // substituted (like egressTimeoutMs/brokerTimeoutMs above) — no env-drift
  // registration needed; see  CP4.1.
  rateLimitBreakerThreshold: number;
  // Max output tokens for a workflow `reason` step's parent-side LLM call
  //. Not compose-
  // substituted (same rationale as rateLimitBreakerThreshold above) — no
  // env-drift registration needed.
  workflowReasonMaxTokens: number;
  // Max LLM calls one run may make (src/llm/egress-proxy.ts). The child's
  // Pi loop is LLM→tool→LLM with no iteration ceiling of its own, so this
  // parent-side counter is the only bound on a model stuck in a tool-retry
  // cycle billing indefinitely. 0 disables the cap. Not compose-substituted
  // (same rationale as workflowReasonMaxTokens above).
  maxLlmCallsPerRun: number;
  // How long a pending HITL approval waits for an answer before it is denied
  // (src/agui/hitl.ts). Without it an unanswered approval holds its child busy
  // for the process's lifetime. 0 disables the timeout. Not compose-substituted
  // (same rationale as maxLlmCallsPerRun above).
  approvalTimeoutMs: number;
  // Master switch for the semantic-recall tier of memory auto-recall
  // — degrading byte-identically to
  // keyword-only recall when false. Not compose-substituted (same rationale
  // as workflowReasonMaxTokens above).
  memorySemanticRecall: boolean;
  // Dedicated timeout (ms) for the memory-recall embeddings call
  // (src/pi/memory-semantic.ts, CP3) — separate from egressTimeoutMs so a slow
  // embedding provider can't stretch every chat turn's egress budget. Not
  // compose-substituted (same rationale as workflowReasonMaxTokens above).
  memoryEmbedTimeoutMs: number;
  // Max branches one `spawn_subagents` fan-out may request
  //. Deliberately well below the compose pool
  // cap of 8 so a fan-out cannot starve the interactive child pool; a request
  // over the cap is rejected for the model to retry serially, never queued.
  // Compose-surfaced at CP10.
  subagentMaxWidth: number;
  // Wall-clock budget (ms) for one subagent branch before its child is aborted
  // and the branch recorded as failed. Mirrors schedulerRunTimeoutMs's default
  // and shape — a branch is the same kind of unattended bounded run. Compose-
  // surfaced at CP10.
  subagentBranchTimeoutMs: number;
}

// Builds and validates a Config from a given env record. Throws with an
// actionable message if any var fails its type constraint.
export function buildConfig(env: Record<string, string | undefined>): Config {
  return {
    openrouterApiKey: get(env, "OPENROUTER_API_KEY", ""),
    llmModel: get(env, "AIKONOS_LLM_MODEL", "anthropic/claude-sonnet-4.6"),
    brokerNorthAddr: get(env, "AIKONOS_BROKER_NORTH_ADDR", "127.0.0.1:9090"),
    brokerSouthAddr: get(env, "AIKONOS_BROKER_SOUTH_ADDR", "127.0.0.1:9091"),
    brokerServerName: get(env, "AIKONOS_BROKER_SERVER_NAME", "broker.aikonos-platform.svc.cluster.local"),
    tlsCert: get(env, "AIKONOS_TLS_CERT", ".svid/svid.pem"),
    tlsKey: get(env, "AIKONOS_TLS_KEY", ".svid/key.pem"),
    tlsCa: get(env, "AIKONOS_TLS_CA", ".svid/ca.pem"),
    gatewaySpiffeId: get(env, "AIKONOS_GATEWAY_SPIFFE_ID", "spiffe://aikonos.com/agent-gateway"),
    port: parsePositiveInt(env, "PORT", "8080"),
    defaultTenantId: get(env, "AIKONOS_TENANT_ID", "11111111-1111-1111-1111-111111111111"),
    oidcIssuer: get(env, "AIKONOS_OIDC_ISSUER", ""),
    oidcJwksUrl: get(env, "AIKONOS_OIDC_JWKS_URL", ""),
    oidcAudience: get(env, "AIKONOS_OIDC_AUDIENCE", "aikonos-broker"),
    oidcSubjectClaim: get(env, "AIKONOS_OIDC_SUBJECT_CLAIM", "sub"),
    oidcTenantClaim: get(env, "AIKONOS_OIDC_TENANT_CLAIM", "tenant_id"),
    schedulerEnabled: parseBool(env, "AIKONOS_SCHEDULER_ENABLED", "false"),
    schedulerTickMs: parsePositiveInt(env, "AIKONOS_SCHEDULER_TICK_MS", "30000"),
    schedulerClaimLimit: parsePositiveInt(env, "AIKONOS_SCHEDULER_CLAIM_LIMIT", "10"),
    schedulerRunTimeoutMs: parsePositiveInt(env, "AIKONOS_SCHEDULER_RUN_TIMEOUT_MS", "180000"),
    agentForUserOverrides: agentOverridesFromEnv(),
    externalPort: parsePositiveInt(env, "AIKONOS_EXTERNAL_PORT", "8090"),
    externalCorsOrigins: get(env, "AIKONOS_EXTERNAL_CORS_ORIGINS", "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
    externalRateLimit: parsePositiveInt(env, "AIKONOS_EXTERNAL_RATE_LIMIT", "60"),
    threadTtlMs: parsePositiveInt(env, "AIKONOS_GATEWAY_THREAD_TTL_MS", "1800000"),
    maxChildren: parsePositiveInt(env, "AIKONOS_GATEWAY_MAX_CHILDREN", "32"),
    childTtlMs: parsePositiveInt(env, "AIKONOS_GATEWAY_CHILD_TTL_MS", "1800000"),
    // NATS_URL is the legacy fallback name; AIKONOS_NATS_URL takes precedence.
    // An explicit empty string at either level is preserved (disables the
    // consumer) rather than falling through to the compose-network default.
    natsUrl: get(env, "AIKONOS_NATS_URL", get(env, "NATS_URL", "nats://nats:4222")),
    auditSubject: get(env, "AIKONOS_AUDIT_SUBJECT", "aikonos.audit.>"),
    egressTimeoutMs: parsePositiveInt(env, "AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS", "120000"),
    brokerTimeoutMs: parsePositiveInt(env, "AIKONOS_GATEWAY_BROKER_TIMEOUT_MS", "30000"),
    rateLimitBreakerThreshold: parsePositiveInt(env, "AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD", "5"),
    // 8192, not 2048: on an OpenAI-dialect reasoning model this budget covers
    // reasoning tokens as well as output, and 2048 was low enough that a reason
    // step could spend the lot thinking and return an empty completion.
    workflowReasonMaxTokens: parsePositiveInt(env, "AIKONOS_WORKFLOW_REASON_MAX_TOKENS", "8192"),
    maxLlmCallsPerRun: parseNonNegativeInt(env, "AIKONOS_GATEWAY_MAX_LLM_CALLS_PER_RUN", "100"),
    approvalTimeoutMs: parseNonNegativeInt(env, "AIKONOS_GATEWAY_APPROVAL_TIMEOUT_MS", "900000"),
    memorySemanticRecall: parseBool(env, "AIKONOS_MEMORY_SEMANTIC_RECALL", "true"),
    memoryEmbedTimeoutMs: parsePositiveInt(env, "AIKONOS_MEMORY_EMBED_TIMEOUT_MS", "10000"),
    subagentMaxWidth: parsePositiveInt(env, "AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH", "3"),
    subagentBranchTimeoutMs: parsePositiveInt(env, "AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS", "180000"),
  };
}

/** Loads and validates the gateway Config from process.env; throws with an actionable message on any missing or malformed value. */
export function loadConfig(): Config {
  return buildConfig(process.env);
}
