// CP5: parent-side LLM-egress proxy.
//
// WHY this exists: the untrusted child must never hold the real provider API
// key. The child registers its LLM provider with baseUrl = <proxyBaseUrl> and
// a dummy key. Every completion request therefore arrives here; the proxy
// looks up the child's pinned {upstreamBaseUrl, realApiKey, modelAllowlist}
// by the per-child token in the path, injects the real Authorization header,
// and forwards to the pinned upstream — streaming the response body through
// unbuffered so SSE token-streaming is not delayed.
//
// Security properties (per CP5 spec):
//   1. Loopback-only bind (127.0.0.1) — the key injection point is never
//      exposed to the network.
//   2. Per-child token lookup — unknown/forged token → 403, reaches nothing.
//   3. Path allowlist — only /chat/completions is forwarded; anything else
//      → 404 (SSRF guard).
//   4. Model allowlist — model not in child's list → 400, not forwarded.
//      Fail-closed: unparseable body or missing/non-string model → 400.
//   5. Key lives only in the in-memory Map — never logged, never in a plan.
//   6. Header smuggle guard: cookie, x-api-key, x-forwarded-for, x-real-ip
//      are stripped from inbound requests so a child cannot inject upstream
//      session or billing state.
//   7. HTTPS upstream: forward() selects node:https for https: upstreams —
//      the real production upstream (openrouter.ai) is never contacted over
//      cleartext.
//   8. Runtime failover (slice 3): on a PRE-STREAM failure — a transport
//      error/idle-timeout, or a response whose status shouldFailover accepts
//      (5xx/429/401/403) — the request is retried against the next provider in
//      the child's pinned chain. Every property above is re-established per
//      attempt: the key stays in the Map and only hostnames are ever logged,
//      the rate-limit/spend-cap pre-check re-runs against that target's own
//      hostname, and the dialect/auth header + model rewrite are recomputed
//      from that target. The model allowlist is unaffected — it governs what
//      the CHILD may ask for and is already the union across the chain
//      (session.ts:resolveProviderCredentials), so a parent-side rewrite to a
//      fallback's model needs no re-check.
//      HARD CEILING: once clientRes.headersSent is true there is NO retry.
//      Bytes are already on the wire to the child and an SSE stream cannot be
//      rewound — mid-stream recovery is deliberately out of scope.
//   9. Per-run LLM-call budget: the child's Pi loop (LLM→tool→LLM) has no
//      iteration ceiling of its own, so a model stuck on a flaky tool result
//      would bill for as long as the client stays connected. Each ChildEntry
//      counts logical requests (once per forward(), NOT per failover attempt —
//      the chain is already bounded at 3) and 429s past maxLlmCallsPerRun with
//      no failover: the budget is chain-wide, so every target is equally
//      over-budget. The counter lives in the parent because the child is
//      untrusted; ChildSupervisor.run resets it when a new run starts.
import http from "node:http";
import https from "node:https";
import { randomBytes } from "node:crypto";
import { resolveDialectRequest, isRecord } from "./provider-dialect.js";
import { shouldFailover } from "./provider-fallback.js";
import { log } from "../log.js";

// Spend-caps CP4: userId is optional here (not on RegisterOptions/ChildEntry
// below either) so every pre-existing call site — production and test — that
// predates per-user spend caps keeps compiling unchanged; new call sites
// (egress-proxy's own finishRequest, GovernanceBridge's reason/analyzeImage)
// pass it through so CheckRateLimit can apply the per-user cap.
export type RateLimitChecker = (
  tenantId: string,
  agentId: string,
  provider: string,
  userId?: string,
) => Promise<void>;

// ProviderTarget is one upstream the proxy may send a single attempt to. The
// canonical copy of the shape session.ts's resolveProviderCredentials produces;
// ipc/supervisor.ts re-exports it so the spawn path has one definition.
export interface ProviderTarget {
  upstreamBaseUrl: string;
  apiKey: string;
  modelId: string;
  api?: string;
  apiVersion?: string;
}

export interface RegisterOptions {
  upstreamBaseUrl: string;
  realApiKey: string;
  modelAllowlist: string[];
  tenantId: string;
  agentId: string;
  // Ordered failover chain behind the primary above (tenant selection order,
  // primary excluded). Optional/defaulted like userId below so every call site
  // that predates runtime failover keeps compiling; [] = today's single-target
  // behavior, byte for byte.
  fallbacks?: ProviderTarget[];
  // Spend-caps CP4: the spawn-bound identity's userId, threaded through to the
  // rate-limit pre-gate so a per-user spend cap can be enforced for
  // interactive-chat egress the same way it already is for reason/vision.
  userId?: string;
  // api selects the upstream wire dialect. Defaults to "openai-completions"
  // (Authorization: Bearer). "azure-openai" uses the classic Azure deployment
  // route (api-key header + /openai/deployments/<model>/...?api-version=<ver>).
  api?: string;
  // apiVersion is the Azure ?api-version= value; required when api is azure-openai.
  apiVersion?: string;
}

export interface RegisterResult {
  childToken: string;
  childBaseUrl: string;
}

interface ChildEntry {
  upstreamBaseUrl: string;
  realApiKey: string;
  modelAllowlist: Set<string>;
  tenantId: string;
  agentId: string;
  userId: string;
  api: string;
  apiVersion: string;
  fallbacks: ProviderTarget[];
  // Logical LLM requests made by the current run. Children are pooled and
  // reused across runs, so this is reset per run (resetRunBudget), not per child.
  llmCalls: number;
}

// Azure AI Foundry classic deployment route — api-key header + deployment path.
const API_AZURE_OPENAI = "azure-openai";

// The only path the proxy forwards — completing requests from Pi's openai client.
const ALLOWED_SUFFIX = "/chat/completions";

// TransportLike is the minimal surface of node:http / node:https that forward()
// uses — specifically the single RequestOptions + callback overload. Exposed so
// tests can inject a spy transport to assert HTTPS is selected for https:
// upstreams without needing a real TLS server.
export interface TransportLike {
  request(
    opts: http.RequestOptions,
    callback?: (res: http.IncomingMessage) => void,
  ): http.ClientRequest;
}

// _TestTransports is accepted only in tests via the second constructor arg.
// Production code never passes it — the proxy defaults to the real node
// http/https transports.
export interface _TestTransports {
  http: TransportLike;
  https: TransportLike;
}

/** Loopback HTTP proxy (127.0.0.1 only) that injects the real provider API key before forwarding child LLM requests upstream; the child holds only a per-child token and never sees the key. */
export class EgressProxy {
  private readonly server: http.Server;
  private readonly children = new Map<string, ChildEntry>();
  private port = 0;
  // WHY: test seam only. Production path uses the real node http/https modules.
  private readonly _transports: _TestTransports;
  private rateLimitChecker?: RateLimitChecker;
  private stopped = false;
  // F10: headers timeout + idle-between-data timeout (ms) for the upstream leg.
  // One value serves both — see Config.egressTimeoutMs in config.ts.
  private readonly egressTimeoutMs: number;
  // Property 9: max logical LLM requests per run; 0 disables the cap. See
  // Config.maxLlmCallsPerRun. Default matches config.ts's default so a proxy
  // built without opts (every existing test) is still bounded.
  private readonly maxLlmCallsPerRun: number;

  constructor(
    _transports?: _TestTransports,
    opts?: { egressTimeoutMs?: number; maxLlmCallsPerRun?: number },
  ) {
    this._transports = _transports ?? { http, https };
    this.egressTimeoutMs = opts?.egressTimeoutMs ?? 120_000;
    this.maxLlmCallsPerRun = opts?.maxLlmCallsPerRun ?? 100;
    this.server = http.createServer((req, res) => {
      this.handleRequest(req, res);
    });
  }

  // start() binds the server to 127.0.0.1 on an ephemeral port.
  start(): Promise<void> {
    return new Promise((resolve, reject) => {
      this.server.on("error", reject);
      this.server.listen(0, "127.0.0.1", () => {
        const addr = this.server.address();
        if (!addr || typeof addr === "string") {
          reject(new Error("EgressProxy: unexpected server address after listen"));
          return;
        }
        this.port = addr.port;
        resolve();
      });
    });
  }

  address(): { address: string; port: number } {
    const addr = this.server.address();
    if (!addr || typeof addr === "string") {
      throw new Error("EgressProxy: server not started");
    }
    return { address: addr.address, port: addr.port };
  }

  // register() creates a per-child token and returns the childBaseUrl the child
  // uses as its provider baseUrl. The real apiKey stays in the in-memory Map —
  // it is never returned or logged.
  register(opts: RegisterOptions): RegisterResult {
    // Fail closed if the proxy was never bound. Otherwise port is 0 and the
    // childBaseUrl points at a dead address — the child's LLM POST hangs
    // forever with no error, silently stalling the run. A loud throw at
    // register time surfaces the misconfiguration instead.
    if (this.port === 0) {
      throw new Error("EgressProxy.register called before start(): proxy is not listening");
    }
    const childToken = randomBytes(16).toString("hex");
    this.children.set(childToken, {
      upstreamBaseUrl: opts.upstreamBaseUrl,
      realApiKey: opts.realApiKey,
      modelAllowlist: new Set(opts.modelAllowlist),
      tenantId: opts.tenantId,
      agentId: opts.agentId,
      userId: opts.userId ?? "",
      api: opts.api ?? "openai-completions",
      apiVersion: opts.apiVersion ?? "",
      fallbacks: opts.fallbacks ?? [],
      llmCalls: 0,
    });
    const childBaseUrl = `http://127.0.0.1:${this.port}/${childToken}`;
    return { childToken, childBaseUrl };
  }

  unregister(childToken: string): void {
    this.children.delete(childToken);
  }

  // resetRunBudget zeroes a child's LLM-call counter. Children are pooled and
  // reused across runs, so without this the second run on a reused child would
  // inherit the first's spend. Called by ChildSupervisor.run when a run starts.
  // Unknown token = no-op (the child was already evicted; nothing to reset).
  resetRunBudget(childToken: string): void {
    const entry = this.children.get(childToken);
    if (entry) entry.llmCalls = 0;
  }

  // consumeLlmBudget books one logical LLM call against a child's run budget and
  // reports whether it is allowed. Public so bridge-direct parent-side calls
  // (GovernanceBridge.reason / .analyzeImage) — which bypass this proxy entirely
  // but bill the same run — share one counter instead of each having their own.
  // Unknown token = allowed: there is no run being tracked, so there is no
  // budget to enforce, and inventing a denial would break a call the pre-budget
  // code let through.
  consumeLlmBudget(childToken: string): boolean {
    const entry = this.children.get(childToken);
    if (!entry) return true;
    return this.consumeBudget(entry);
  }

  private consumeBudget(entry: ChildEntry): boolean {
    if (this.maxLlmCallsPerRun === 0) return true;
    entry.llmCalls += 1;
    return entry.llmCalls <= this.maxLlmCallsPerRun;
  }

  // stop() closes the loopback server. Idempotent — a second call (or a call
  // when the server was never started) resolves immediately rather than
  // rejecting, so shutdown orchestration can call it unconditionally. In-flight
  // requests get a bounded grace period (graceMs) to finish before the proxy
  // force-closes remaining sockets, so a stuck child LLM call can't wedge
  // shutdown indefinitely.
  stop(graceMs = 5000): Promise<void> {
    if (this.stopped) return Promise.resolve();
    this.stopped = true;
    return new Promise((resolve) => {
      const forceTimer = setTimeout(() => {
        this.server.closeAllConnections();
      }, graceMs);
      forceTimer.unref();
      this.server.close(() => {
        clearTimeout(forceTimer);
        resolve();
      });
    });
  }

  setRateLimitChecker(fn: RateLimitChecker): void {
    this.rateLimitChecker = fn;
  }

  private handleRequest(req: http.IncomingMessage, res: http.ServerResponse): void {
    const url = req.url ?? "/";

    // Path shape: /<childToken><suffix>
    // e.g. /abc123/chat/completions
    const firstSlash = url.indexOf("/");
    const secondSlash = url.indexOf("/", firstSlash + 1);

    if (secondSlash === -1) {
      // No suffix at all — cannot be a valid path.
      res.writeHead(404).end();
      return;
    }

    const childToken = url.slice(firstSlash + 1, secondSlash);
    const suffix = url.slice(secondSlash); // e.g. "/chat/completions"

    const entry = this.children.get(childToken);
    if (!entry) {
      res.writeHead(403).end();
      return;
    }

    // Path SSRF guard: only forward /chat/completions.
    // Normalise with URL to collapse any ../ attempts.
    let normSuffix: string;
    try {
      normSuffix = new URL(`http://x${suffix}`).pathname;
    } catch {
      res.writeHead(404).end();
      return;
    }
    if (normSuffix !== ALLOWED_SUFFIX) {
      res.writeHead(404).end();
      return;
    }

    // Must read + buffer the body to extract the model field for allowlist check.
    // Completion request bodies are small (text prompt, not file uploads).
    const bodyChunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => bodyChunks.push(chunk));
    req.on("end", () => {
      this.finishRequest(entry, Buffer.concat(bodyChunks), req, res);
    });
    // Guarded exactly like the attempt-chain error/close handlers: this listener
    // outlives the response, so an inbound-stream error arriving after any path
    // has already replied (a 400 from finishRequest, the budget 429, or a piped
    // upstream response) would make writeHead throw ERR_HTTP_HEADERS_SENT
    // synchronously here — uncaught, killing the parent and every user's runs.
    req.on("error", () => {
      if (!res.headersSent) {
        res.writeHead(502).end();
      } else if (!res.writableEnded) {
        res.destroy();
      }
    });
  }

  // Synchronous now that the rate-limit pre-check moved into attempt() — each
  // attempt owns its own check, since each provider is its own limiter subject.
  private finishRequest(
    entry: ChildEntry,
    rawBody: Buffer,
    req: http.IncomingMessage,
    res: http.ServerResponse,
  ): void {
    // Fail-closed model check: reject anything that is not a parseable JSON
    // body with a string model field in the allowlist. Forwarding on parse
    // failure would let a child bypass the model pin entirely.
    let parsedModel: string;
    try {
      const parsed: unknown = JSON.parse(rawBody.toString());
      if (
        parsed === null ||
        typeof parsed !== "object" ||
        !("model" in parsed) ||
        typeof parsed.model !== "string"
      ) {
        res.writeHead(400).end("model field missing or not a string");
        return;
      }
      parsedModel = parsed.model;
    } catch {
      // Unparseable body — fail closed; we cannot verify the model.
      res.writeHead(400).end("unparseable request body");
      return;
    }

    if (!entry.modelAllowlist.has(parsedModel)) {
      res.writeHead(400).end(`model '${parsedModel}' not in allowlist`);
      return;
    }

    this.forward(entry, parsedModel, rawBody, req, res);
  }

  private forward(
    entry: ChildEntry,
    model: string,
    body: Buffer,
    inReq: http.IncomingMessage,
    clientRes: http.ServerResponse,
  ): void {
    // Property 9: one logical request = one budget unit, booked here rather than
    // in attempt() so the ≤3-target failover chain doesn't multiply the count.
    // No failover on denial either — the budget is chain-wide, so every target
    // is equally over-budget.
    if (!this.consumeBudget(entry)) {
      log.warn(
        { tenantId: entry.tenantId, agentId: entry.agentId, budget: this.maxLlmCallsPerRun },
        "llm egress: per-run call budget exceeded — denying",
      );
      clientRes.writeHead(429).end("llm call budget exceeded for this run");
      return;
    }

    // Attempt order: the child's pinned primary, then each tenant fallback in
    // selection order. Bounded by construction — provider-fallback.ts's
    // chatCandidates dedupes to at most three tiers (agent-assigned → tenant
    // default → tenant fallback), so this list can never exceed 3 entries and
    // needs no separate attempt cap.
    const targets: ProviderTarget[] = [
      {
        upstreamBaseUrl: entry.upstreamBaseUrl,
        apiKey: entry.realApiKey,
        modelId: model,
        api: entry.api,
        apiVersion: entry.apiVersion,
      },
      ...entry.fallbacks,
    ];
    // Shared across the whole chain: a client that hangs up must stop the chain,
    // not just the attempt in flight.
    void this.attempt(entry, targets, 0, model, body, inReq, clientRes, { clientGone: false });
  }

  // attempt runs the rate-limit pre-gate for targets[index] and, if allowed,
  // forwards the request to it. Its timer, its timedOut flag, and its inbound
  // socket 'close' listener are all attempt-scoped and torn down before the next
  // attempt begins — the request-scoped versions would leak one timer and one
  // listener per retry (see the keep-alive hazard note below, which now applies
  // per attempt as well as per request).
  private async attempt(
    entry: ChildEntry,
    targets: ProviderTarget[],
    index: number,
    requestedModel: string,
    body: Buffer,
    inReq: http.IncomingMessage,
    clientRes: http.ServerResponse,
    chain: { clientGone: boolean },
  ): Promise<void> {
    const target = targets[index];
    const upstream = new URL(target.upstreamBaseUrl);
    const nextIndex = index + 1;

    // A retry is only invisible while nothing has been written to the client,
    // and only worth doing for a caller that is still connected.
    const canFailOver = (): boolean =>
      nextIndex < targets.length &&
      !clientRes.headersSent &&
      !clientRes.destroyed &&
      !chain.clientGone;

    const failOver = (reason: string): void => {
      // Hostnames only — property 5: the key (and any object carrying it) never
      // reaches a log line.
      log.warn(
        { from: upstream.hostname, to: new URL(targets[nextIndex].upstreamBaseUrl).hostname, reason },
        "llm egress: provider failed pre-stream — failing over to the next provider",
      );
      void this.attempt(entry, targets, nextIndex, requestedModel, body, inReq, clientRes, chain);
    };

    // Rate-limit pre-check, per attempt: a different provider is a different
    // rate-limit and spend-cap subject, so a failover reusing the primary's
    // clearance would let the fallback bypass its own cap entirely.
    if (this.rateLimitChecker) {
      try {
        await this.rateLimitChecker(entry.tenantId, entry.agentId, upstream.hostname, entry.userId);
      } catch (err) {
        // The checker's denial is this provider's own 429 — failover-eligible
        // exactly like an upstream 429; an exhausted chain still ends in 429.
        if (shouldFailover({ status: 429 }) && canFailOver()) {
          failOver("429 (rate-limit pre-check)");
          return;
        }
        clientRes.writeHead(429).end(String(err));
        return;
      }
    }
    // The pre-check is async — the client may have gone away during it.
    if (chain.clientGone || clientRes.destroyed) return;

    const basePath = upstream.pathname.replace(/\/$/, "");
    const isAzure = target.api === API_AZURE_OPENAI;

    // Each target carries its own model id, so a failover must repoint the body
    // at it — the fallback would reject the primary's model. A no-op for the
    // primary, whose modelId IS the child's requested (allowlist-checked) model.
    let outBody = target.modelId === requestedModel ? body : rewriteModel(body, target.modelId);
    // Azure's newer deployments (o-series, GPT-5, and other reasoning models)
    // reject the legacy `max_tokens` field and require `max_completion_tokens`.
    // Pi's openai client always emits `max_tokens`; rewrite it here so those
    // deployments don't 400. `max_completion_tokens` is accepted by all chat
    // models on recent api-versions, so the rename is safe across deployments.
    // Recomputed per attempt from the TARGET's dialect, not the child's — a
    // fallback may be a different dialect than the primary.
    if (isAzure) {
      outBody = rewriteAzureBody(outBody);
    }

    // Azure classic deployment route puts the deployment name (== model) in the
    // path and the api-version in the query; auth is the api-key header, not a
    // Bearer token. Everything else uses the OpenAI-completions shape.
    // URL/header/auth resolution is shared with vision.ts/reason.ts via
    // provider-dialect.ts, which does not URL-encode itself — encode here
    // before calling, since egress-proxy's model/apiVersion come from an
    // untrusted child and must be encoded into the path.
    // egress-proxy has only ever handled azure/openai (never anthropic) —
    // narrow explicitly instead of passing the target's api straight through, so
    // an anthropic-dialect provider still falls back to the openai route exactly
    // as it did before the shared resolver existed. Cross-dialect request/
    // response body translation is out of scope: an anthropic provider through
    // this proxy is a pre-existing limitation that failover neither fixes nor
    // worsens (each target is narrowed the same way the single target was).
    const { path: dialectPath, headers: authHeaders } = resolveDialectRequest({
      api: isAzure ? "azure-openai" : "openai-completions",
      apiKey: target.apiKey,
      apiVersion: encodeURIComponent(target.apiVersion ?? ""),
      modelId: encodeURIComponent(target.modelId),
    });
    const upstreamPath = `${basePath}${dialectPath}`;

    // Build forwarding headers. Replace the incoming Authorization with the
    // real api key (Bearer), or set the Azure api-key header. Pass content-type
    // through.
    const forwardHeaders: Record<string, string> = {
      "content-type": inReq.headers["content-type"] ?? "application/json",
      "content-length": String(outBody.length),
      ...authHeaders,
    };
    // Preserve any extra Pi SDK headers (HTTP-Referer, X-Title) the child sends.
    // Strip headers that could inject upstream session or billing state:
    // cookie and x-api-key could hijack an upstream session; x-forwarded-for
    // and x-real-ip could forge origin identity at the upstream.
    for (const [key, val] of Object.entries(inReq.headers)) {
      if (
        key === "authorization" ||
        key === "api-key" ||
        key === "content-length" ||
        key === "content-type" ||
        key === "host" ||
        key === "transfer-encoding" ||
        key === "connection" ||
        key === "cookie" ||
        key === "x-api-key" ||
        key === "x-forwarded-for" ||
        key === "x-real-ip"
      ) {
        continue;
      }
      if (typeof val === "string") {
        forwardHeaders[key] = val;
      }
    }

    // Select the transport based on the upstream protocol so the real API key
    // is never sent over cleartext to an https upstream.
    const transport = upstream.protocol === "https:" ? this._transports.https : this._transports.http;

    // F10: a single idle timer serves both the headers phase and the
    // streaming phase. It starts on send, is cleared on client disconnect or
    // upstream completion/error, and is refreshed on every response data
    // chunk once streaming begins — so a slow-but-live upstream is never
    // punished, only a genuinely stalled one. `timedOut` lets the single
    // shared error handler below decide 504-vs-502 — one place owns "have we
    // already responded" instead of the timeout branch duplicating it.
    // `settled` marks THIS attempt as having produced its outcome (piped
    // response, handoff to the next target, or terminal error) so a late event
    // from an attempt already handed off cannot touch the client.
    let idleTimer: NodeJS.Timeout | undefined;
    let timedOut = false;
    let settled = false;
    const clearIdleTimer = (): void => {
      if (idleTimer) {
        clearTimeout(idleTimer);
        idleTimer = undefined;
      }
    };
    const armIdleTimer = (): void => {
      clearIdleTimer();
      idleTimer = setTimeout(() => {
        timedOut = true;
        upstreamReq.destroy(new Error("egress: upstream timed out"));
      }, this.egressTimeoutMs);
    };

    const upstreamReq = transport.request(
      {
        hostname: upstream.hostname,
        port: upstream.port || (upstream.protocol === "https:" ? 443 : 80),
        path: upstreamPath,
        method: inReq.method ?? "POST",
        headers: forwardHeaders,
      },
      (upstreamRes) => {
        if (settled) {
          upstreamRes.resume();
          return;
        }
        // Pre-stream failure detection point #2 (the important half): a provider
        // 500/429/401/403 arrives as a *successful* response carrying a bad
        // status. Nothing has been written to the client yet, so classify BEFORE
        // writeHead — an OpenAI 500 or a dead-key 401 is the canonical "this
        // provider is not working".
        if (shouldFailover({ status: upstreamRes.statusCode }) && canFailOver()) {
          settled = true;
          clearIdleTimer();
          detachSocketCloseListener();
          // Drain rather than abandon half-read, so the socket is released.
          upstreamRes.resume();
          failOver(String(upstreamRes.statusCode));
          return;
        }
        // Re-arm with a fresh full budget for the gap between headers
        // arriving and the first body chunk — otherwise that gap only gets
        // whatever was left of the headers-phase timer.
        armIdleTimer();
        // Propagate status + headers; stream the body unbuffered.
        const passHeaders: Record<string, string | string[]> = {};
        for (const [key, val] of Object.entries(upstreamRes.headers)) {
          if (key === "transfer-encoding" || key === "connection") continue;
          if (val !== undefined) passHeaders[key] = val;
        }
        clientRes.writeHead(upstreamRes.statusCode ?? 200, passHeaders);
        // Refresh the idle timer on every chunk — only a stall (no data for
        // egressTimeoutMs) should trip the timeout during streaming.
        upstreamRes.on("data", () => armIdleTimer());
        upstreamRes.on("end", clearIdleTimer);
        // pipe() streams chunks as they arrive — unbuffered SSE passthrough.
        upstreamRes.pipe(clientRes);
      },
    );

    // Client disconnected before completion — stop consuming the upstream
    // stream. IncomingMessage's own 'close' fires as soon as the *request*
    // body finishes reading (well before the response is sent), so it is not
    // a reliable disconnect signal here — listen on the underlying socket's
    // 'close' instead, which only fires on an actual TCP disconnect. The
    // socket is reused across requests on a keep-alive connection, so the
    // listener must be detached once THIS ATTEMPT's cycle ends — otherwise it
    // accumulates one listener per attempt (and per request) on a long-lived
    // socket.
    const onClientSocketClose = (): void => {
      // Abort the whole chain, not just this attempt — there is no caller left
      // to serve a fallback provider's response to.
      chain.clientGone = true;
      if (!clientRes.writableEnded) {
        upstreamReq.destroy();
      }
    };
    inReq.socket.once("close", onClientSocketClose);
    const detachSocketCloseListener = (): void => {
      inReq.socket.removeListener("close", onClientSocketClose);
    };

    upstreamReq.on("error", (err) => {
      clearIdleTimer();
      detachSocketCloseListener();
      if (settled) return;
      settled = true;
      // Pre-stream failure detection point #1: transport error, or this
      // attempt's own idle-timeout destroy. shouldFailover owns the trigger set
      // — never re-derive it here.
      if (shouldFailover({ transportError: true }) && canFailOver()) {
        failOver(timedOut ? "transport (timeout)" : "transport");
        return;
      }
      if (!clientRes.headersSent) {
        clientRes.writeHead(timedOut ? 504 : 502).end(timedOut ? "upstream timed out" : String(err));
      } else if (!clientRes.writableEnded) {
        // Headers were already sent to the client — can't 504/502 anymore
        // (timeout or otherwise), and there is deliberately no retry either:
        // bytes are on the wire and an SSE stream cannot be rewound. End the
        // response to unstick the caller. This is the hard failover ceiling.
        clientRes.destroy();
      }
    });
    upstreamReq.on("close", () => {
      clearIdleTimer();
      detachSocketCloseListener();
      // An upstream socket torn down abruptly (RST) mid-body surfaces here as a
      // bare 'close' with NO preceding 'error' — verified in
      // egress-proxy-failover.test.ts. Cleaning up and returning would leave the
      // client response open forever, and worse: the clearIdleTimer() above just
      // disarmed the only timer that would have rescued it. So this handler owns
      // the outcome for an unsettled attempt, exactly as the error path does.
      //
      // Normal completion reaches here with clientRes.writableEnded already true
      // (upstreamRes 'end' → pipe end()s clientRes → then this 'close'), and a
      // 'close' after an 'error' or a failover hand-off is caught by `settled` —
      // both stay no-ops.
      if (settled || clientRes.writableEnded || clientRes.destroyed || chain.clientGone) return;
      settled = true;
      if (canFailOver()) {
        failOver("upstream closed pre-stream");
        return;
      }
      if (!clientRes.headersSent) {
        clientRes.writeHead(502).end("upstream closed unexpectedly");
        return;
      }
      clientRes.destroy();
    });

    armIdleTimer(); // headers-timeout phase — armed immediately on send.
    upstreamReq.write(outBody);
    upstreamReq.end();
  }
}

// rewriteAzureBody renames the legacy `max_tokens` field to
// `max_completion_tokens` for Azure deployments that reject the former
// (o-series, GPT-5, reasoning models). If the body already uses
// `max_completion_tokens`, or has neither field, it is returned unchanged.
// The body has already been validated as parseable JSON upstream, so a parse
// failure here is treated as a no-op rather than an error.
function rewriteAzureBody(body: Buffer): Buffer {
  const parsed = parseJsonObject(body);
  if (!parsed) return body;
  if (!("max_tokens" in parsed)) return body;
  if (!("max_completion_tokens" in parsed)) {
    parsed.max_completion_tokens = parsed.max_tokens;
  }
  delete parsed.max_tokens;
  return Buffer.from(JSON.stringify(parsed));
}

// rewriteModel repoints the outgoing body at the model of the target actually
// being attempted — each provider in the failover chain has its own model id, so
// a retry that kept the primary's model would be rejected by the fallback.
// Same defensive posture as rewriteAzureBody: the body was already validated as
// parseable JSON upstream, so a parse failure here is a no-op, not an error.
function rewriteModel(body: Buffer, modelId: string): Buffer {
  const parsed = parseJsonObject(body);
  if (!parsed) return body;
  parsed.model = modelId;
  return Buffer.from(JSON.stringify(parsed));
}

function parseJsonObject(body: Buffer): Record<string, unknown> | undefined {
  try {
    const obj: unknown = JSON.parse(body.toString());
    return isRecord(obj) ? obj : undefined;
  } catch {
    return undefined;
  }
}
