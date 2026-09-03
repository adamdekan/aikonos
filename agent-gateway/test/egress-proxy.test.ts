// CP5 tests: LLM-egress proxy — per-child upstream+model pinning + SSRF guard.
//
// WHY these tests exist:
//   The egress proxy is the only barrier between the untrusted child's LLM calls
//   and the real provider API key. A child that can bypass it (wrong token, path
//   SSRF, model swap, sibling-token reuse) can exfiltrate the key or bill another
//   child's upstream. Every test encodes one of those threat vectors.
import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import net from "node:net";
import { EgressProxy, type TransportLike, type _TestTransports } from "../src/llm/egress-proxy.js";
import {
  resolveSessionPlan,
  type ResolveIdentity,
  type ResolveSessionDeps,
  type ProviderLike,
} from "../src/pi/session-plan.js";

// ── Fake upstream HTTP server ──────────────────────────────────────────────────
//
// Starts a real HTTP server on loopback. Tests drive it by writing to
// `upstream.nextHandler` before each call. Captures received requests.

interface CapturedRequest {
  authorization: string | undefined;
  apiKey: string | undefined;
  path: string;
  body: string;
}

// Gap the fake upstream leaves between SSE chunks. Every timing assertion below
// is expressed relative to it, so the whole file scales from one number. Sized
// well above loopback + test-runner scheduling jitter: node --test runs test
// FILES in parallel, so a gap in the tens of milliseconds can stretch past a
// tens-of-milliseconds idle timeout purely from CPU contention with sibling
// files, which used to make three of these tests flake as the suite grew.
const UPSTREAM_CHUNK_GAP_MS = 60;

interface FakeUpstream {
  baseUrl: string;
  captured: CapturedRequest[];
  // Set before a call to control what the upstream replies with.
  nextChunks: string[];
  // Resolves once the proxy's connection to this fake upstream is torn down
  // before the response finished normally (res.end() never called) — the
  // observable signature of the proxy destroying the upstream leg.
  responseClosedEarly: Promise<void>;
  close(): Promise<void>;
}

async function startFakeUpstream(): Promise<FakeUpstream> {
  const captured: CapturedRequest[] = [];
  let nextChunks: string[] = ['data: {"id":"1"}\n\n'];
  let resolveClosedEarly: () => void;
  const responseClosedEarly = new Promise<void>((resolve) => { resolveClosedEarly = resolve; });

  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (chunk: Buffer) => { body += chunk.toString(); });
    req.on("end", () => {
      captured.push({
        authorization: req.headers["authorization"],
        apiKey: req.headers["api-key"] as string | undefined,
        path: req.url ?? "/",
        body,
      });

      res.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        "transfer-encoding": "chunked",
      });
      res.on("close", () => {
        if (!res.writableEnded) resolveClosedEarly();
      });

      // Write chunks with a small async gap so the streaming test can
      // detect the first chunk before the second is sent.
      const chunks = nextChunks.slice();
      let idx = 0;

      function writeNext(): void {
        if (idx >= chunks.length) {
          res.end();
          return;
        }
        const chunk = chunks[idx++];
        res.write(chunk);
        // UPSTREAM_CHUNK_GAP_MS between chunks so the streaming assertion can
        // distinguish "forwarded first before second arrived" from
        // "collected-then-forwarded".
        setTimeout(writeNext, UPSTREAM_CHUNK_GAP_MS);
      }
      writeNext();
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "fake upstream must bind");
  const baseUrl = `http://127.0.0.1:${addr.port}`;

  return {
    baseUrl,
    captured,
    get nextChunks() { return nextChunks; },
    set nextChunks(v) { nextChunks = v; },
    responseClosedEarly,
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

// ── Helper: send a POST to the proxy via the child's baseUrl ──────────────────

async function proxyPost(
  childBaseUrl: string,
  body: Record<string, unknown>,
  opts: { expectedStatus?: number } = {},
): Promise<{ status: number; body: string }> {
  const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
  const res = await fetch(url.toString(), {
    method: "POST",
    headers: { "content-type": "application/json", authorization: "Bearer dummy-child-key" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  if (opts.expectedStatus !== undefined) {
    assert.equal(res.status, opts.expectedStatus, `expected HTTP ${opts.expectedStatus}, got ${res.status}: ${text}`);
  }
  return { status: res.status, body: text };
}

// ── Tests ──────────────────────────────────────────────────────────────────────

test("CP5 egress-proxy: binds ONLY on loopback — never 0.0.0.0 (would expose real key to the network)", async () => {
  // WHY: the proxy holds the real provider API key. Binding on 0.0.0.0 would
  // expose the injection point to any process on the network — defeating the
  // entire key-isolation model.
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    assert.equal(
      proxy.address().address,
      "127.0.0.1",
      "proxy must bind on 127.0.0.1, not 0.0.0.0 — real key is injected here",
    );
  } finally {
    await proxy.stop();
  }
});

test("CP5 egress-proxy: register() before start() throws — fail closed, never hand out a port-0 baseUrl", () => {
  // WHY: until start() binds the server, port is 0. A childBaseUrl of
  // http://127.0.0.1:0/<token> points at a dead address, so the child's LLM
  // POST hangs forever with no error and the run silently stalls (the live
  // bug: gateway bootstrap forgot to call start()). register() must fail loud.
  const proxy = new EgressProxy();
  assert.throws(
    () =>
      proxy.register({
        upstreamBaseUrl: "https://openrouter.ai/api/v1",
        realApiKey: "sk-real",
        tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["m"],
      }),
    /before start/,
    "register before start must throw, not return a port-0 baseUrl",
  );
});

test("CP5 egress-proxy: child request with dummy key → proxy injects real Authorization, fake upstream sees real key", async () => {
  // WHY: the child registers the provider with a dummy key. The proxy must
  // replace it with the real key before forwarding. If the proxy passes through
  // the dummy key the upstream call fails; if it skips injection the child could
  // be used to probe key-validity without ever having the real key — but that is
  // a weaker concern than "child holds key". The primary assertion is that the
  // UPSTREAM sees the REAL key and the child sent ONLY the dummy.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real-injected-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const dummyKeyUsedByChild = "dummy-child-key";
    await proxyPost(childBaseUrl, { model: "test-model", messages: [] }, { expectedStatus: 200 });

    assert.equal(upstream.captured.length, 1, "upstream must have received exactly one request");
    const req = upstream.captured[0];
    assert.ok(req, "upstream request must exist");
    assert.equal(
      req.authorization,
      "Bearer sk-real-injected-key",
      "upstream must receive the REAL api key injected by the proxy",
    );
    // Child used the dummy key — verify it was NOT passed through.
    assert.notEqual(
      req.authorization,
      `Bearer ${dummyKeyUsedByChild}`,
      "proxy must NOT pass through the child's dummy key",
    );
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: azure-openai → deployment-route URL + api-key header, no Bearer", async () => {
  // WHY: the classic Azure deployment route differs from OpenAI in two ways the
  // proxy must enforce: the deployment name (== model) goes in the path with an
  // ?api-version= query, and auth is the `api-key` header — NEVER Authorization.
  // A regression that sent Bearer or dropped api-version would 401/404 at Azure.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-azure-real",
      tenantId: "test-tenant",
      agentId: "test-agent",
      modelAllowlist: ["gpt-4o-deployment"],
      api: "azure-openai",
      apiVersion: "2024-08-01-preview",
    });

    await proxyPost(childBaseUrl, { model: "gpt-4o-deployment", messages: [] }, { expectedStatus: 200 });

    assert.equal(upstream.captured.length, 1, "upstream must receive exactly one request");
    const req = upstream.captured[0];
    assert.ok(req, "upstream request must exist");
    assert.equal(
      req.path,
      "/openai/deployments/gpt-4o-deployment/chat/completions?api-version=2024-08-01-preview",
      "azure must use the classic deployment-route path with api-version query",
    );
    assert.equal(req.apiKey, "sk-azure-real", "azure must send the real key in the api-key header");
    assert.equal(req.authorization, undefined, "azure must NOT send an Authorization/Bearer header");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: azure-openai → max_tokens renamed to max_completion_tokens (reasoning/GPT-5 deployments)", async () => {
  // WHY: Azure's o-series/GPT-5/reasoning deployments reject the legacy
  // `max_tokens` field with a 400 ("Use 'max_completion_tokens' instead").
  // Pi's openai client always emits `max_tokens`, so the proxy must rewrite it
  // for Azure or those deployments are unusable.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-azure-real",
      tenantId: "test-tenant",
      agentId: "test-agent",
      modelAllowlist: ["o3-deployment"],
      api: "azure-openai",
      apiVersion: "2024-08-01-preview",
    });

    await proxyPost(
      childBaseUrl,
      { model: "o3-deployment", messages: [], max_tokens: 8192 },
      { expectedStatus: 200 },
    );

    assert.equal(upstream.captured.length, 1, "upstream must receive exactly one request");
    const req = upstream.captured[0];
    assert.ok(req, "upstream request must exist");
    const forwarded = JSON.parse(req.body) as Record<string, unknown>;
    assert.equal(forwarded.max_completion_tokens, 8192, "max_tokens must be renamed to max_completion_tokens");
    assert.ok(!("max_tokens" in forwarded), "legacy max_tokens must be removed for azure");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: non-azure (openai) → max_tokens left untouched", async () => {
  // WHY: the max_tokens→max_completion_tokens rename is Azure-specific. OpenAI /
  // OpenRouter still accept max_tokens; rewriting it for them would be wrong.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      tenantId: "test-tenant",
      agentId: "test-agent",
      modelAllowlist: ["gpt-4o"],
      api: "openai-completions",
      apiVersion: "",
    });

    await proxyPost(
      childBaseUrl,
      { model: "gpt-4o", messages: [], max_tokens: 8192 },
      { expectedStatus: 200 },
    );

    assert.equal(upstream.captured.length, 1, "upstream must receive exactly one request");
    const req = upstream.captured[0];
    assert.ok(req, "upstream request must exist");
    const forwarded = JSON.parse(req.body) as Record<string, unknown>;
    assert.equal(forwarded.max_tokens, 8192, "non-azure must keep max_tokens unchanged");
    assert.ok(!("max_completion_tokens" in forwarded), "non-azure must not introduce max_completion_tokens");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: streaming unbuffered — first SSE chunk arrives before second is sent", async () => {
  // WHY: Pi/OpenAI completions use SSE streaming. A proxy that collects the
  // full response before forwarding would cause a "loading spinner" UX and
  // could OOM on large responses. This test proves the proxy pipes the upstream
  // body without buffering.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    // Two SSE chunks, UPSTREAM_CHUNK_GAP_MS apart (set in startFakeUpstream).
    upstream.nextChunks = ['data: {"chunk":1}\n\n', 'data: {"chunk":2}\n\n'];

    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");

    // Use http.get with a streaming response reader to detect the first chunk
    // arriving before the second.
    await new Promise<void>((resolve, reject) => {
      const req = http.request(
        { hostname: "127.0.0.1", port: proxy.address().port, path: `${new URL(childBaseUrl).pathname}/chat/completions`, method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" } },
        (res) => {
          const chunks: string[] = [];
          let firstChunkAt = 0;

          let secondChunkAt = 0;
          res.on("data", (chunk: Buffer) => {
            const text = chunk.toString();
            chunks.push(text);
            if (chunks.length === 1) {
              firstChunkAt = Date.now();
            } else if (chunks.length === 2) {
              secondChunkAt = Date.now();
              // The upstream delays chunk 2 by UPSTREAM_CHUNK_GAP_MS. A
              // collect-then-flush proxy delivers both chunks simultaneously
              // (gap ≈ 0); a streaming proxy delivers the first immediately and
              // the second a full gap later. Assert half the gap: far above
              // scheduling jitter, far below the real gap, so only a buffering
              // regression can fail it.
              const minGapMs = UPSTREAM_CHUNK_GAP_MS / 2;
              assert.ok(firstChunkAt > 0, "first chunk timestamp must be set");
              assert.ok(
                secondChunkAt - firstChunkAt >= minGapMs,
                `proxy appears to buffer: gap between chunk 1 and chunk 2 was ${secondChunkAt - firstChunkAt}ms, expected ≥${minGapMs}ms — a collect-then-flush proxy would deliver both simultaneously`,
              );
              resolve();
            }
          });
          res.on("error", reject);
          res.on("end", () => {
            if (chunks.length < 2) {
              reject(new Error(`expected 2 chunks, got ${chunks.length}: ${chunks.join("")}`));
            }
          });
        },
      );
      req.on("error", reject);
      req.write(JSON.stringify({ model: "test-model", messages: [] }));
      req.end();
    });
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: model not in child allowlist → 400, request not forwarded to upstream", async () => {
  // WHY: the model allowlist pins which model a specific child may call. Without
  // it a compromised child could swap to a more-expensive or less-restricted
  // model — billing spend or policy evasion.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    const { status } = await proxyPost(
      childBaseUrl,
      { model: "forbidden-model", messages: [] },
      { expectedStatus: 400 },
    );
    assert.equal(status, 400);
    assert.equal(upstream.captured.length, 0, "forbidden model must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: path SSRF guard — path other than /chat/completions → 404, not forwarded", async () => {
  // WHY: the child cannot repoint the proxy to an arbitrary upstream path.
  // Without the pinned-path guard a child could POST to /<token>/v1/models,
  // /<token>/admin, or any arbitrary path on the upstream — SSRF via the
  // proxy's injected credential.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl, childToken } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const proxyUrl = new URL(childBaseUrl);
    // Attempt to reach a non-completions path.
    for (const badPath of ["/v1/models", "/evil", "/chat/completions/../admin"]) {
      const res = await fetch(
        `http://127.0.0.1:${proxyUrl.port}/${childToken}${badPath}`,
        {
          method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" },
          body: JSON.stringify({ model: "test-model" }),
        },
      );
      assert.equal(res.status, 404, `SSRF path '${badPath}' must return 404, got ${res.status}`);
    }
    assert.equal(upstream.captured.length, 0, "no SSRF path must reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: unknown/forged child token → 403, reaches nothing (per-child isolation)", async () => {
  // WHY: the child token is the per-child isolation key. A forged or guessed
  // token must return 403 immediately without forwarding to any upstream. This
  // is the load-bearing guard for per-child upstream pinning.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const forgedToken = "00000000-0000-0000-0000-000000000000";
    const res = await fetch(
      `http://127.0.0.1:${proxy.address().port}/${forgedToken}/chat/completions`,
      {
        method: "POST",
        headers: { "content-type": "application/json", authorization: "Bearer dummy" },
        body: JSON.stringify({ model: "test-model", messages: [] }),
      },
    );
    assert.equal(res.status, 403, `forged token must return 403, got ${res.status}`);
    assert.equal(upstream.captured.length, 0, "forged token must not reach any upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: per-child isolation — child A token cannot reach child B upstream", async () => {
  // WHY: when multiple children are registered their upstreams must be strictly
  // isolated. Child A's token may only forward to A's pinned upstream; using it
  // must never cause a request to arrive at B's upstream. This is the Phase-3
  // load-bearing property (per-user keying); the test must exist even though
  // Phase 2 uses single-child.
  const upstreamA = await startFakeUpstream();
  const upstreamB = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const regA = proxy.register({
      upstreamBaseUrl: upstreamA.baseUrl,
      realApiKey: "sk-key-a",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["model-a"],
    });
    const regB = proxy.register({
      upstreamBaseUrl: upstreamB.baseUrl,
      realApiKey: "sk-key-b",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["model-a"],
    });

    // Child A's token + model-a → should reach upstreamA, not upstreamB.
    await proxyPost(regA.childBaseUrl, { model: "model-a", messages: [] }, { expectedStatus: 200 });

    assert.equal(upstreamA.captured.length, 1, "child A request must reach upstreamA");
    assert.equal(upstreamB.captured.length, 0, "child A request must NOT reach upstreamB");

    // Verify upstreamA got key-a, not key-b.
    const req = upstreamA.captured[0];
    assert.ok(req, "upstreamA must have a captured request");
    assert.equal(req.authorization, "Bearer sk-key-a");
  } finally {
    await proxy.stop();
    await upstreamA.close();
    await upstreamB.close();
  }
});

test("CP5 egress-proxy: unregister removes child — subsequent requests with that token → 403", async () => {
  // WHY: when a child session ends its token must be invalidated. A stale token
  // left in the map could be guessed (small window, but defense-in-depth).
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl, childToken } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    // First call must succeed.
    await proxyPost(childBaseUrl, { model: "test-model", messages: [] }, { expectedStatus: 200 });
    assert.equal(upstream.captured.length, 1);

    proxy.unregister(childToken);

    // After unregister, the same token must return 403.
    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
    const res = await fetch(url.toString(), {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer dummy" },
      body: JSON.stringify({ model: "test-model", messages: [] }),
    });
    assert.equal(res.status, 403, "unregistered token must return 403");
    assert.equal(upstream.captured.length, 1, "no further upstream requests after unregister");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

// ── resolveSessionPlan + proxy integration ─────────────────────────────────────

test("CP5 resolveSessionPlan with proxy injected: plan.proxyBaseUrl is the proxy URL (with token), plan carries NO real key", async () => {
  // WHY: resolveSessionPlan is the parent-side resolver that assembles the
  // InitMessage sent to the child. CP5 wires deps.proxy so the plan carries the
  // child's proxy baseUrl. The real api key must never appear in the plan —
  // the plan is the *only* thing the parent sends to the child.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    function makeSouth(overrides: Partial<{ providers: ProviderLike[] }> = {}) {
      return {
        getLlmProviders: async (_req: { tenantId: string }) => ({ providers: overrides.providers ?? [] }),
        getTenantModel: async (_req: { tenantId: string }) => ({ model: "" }),
        listUserSkills: async (_req: { tenantId: string; userId: string }) => ({ skills: ["web.fetch"] }),
        listUserAgentSkills: async (_req: { tenantId: string; userId: string }) => ({ bundles: [] }),
        listAccessibleMcpServersForAgent: async (_req: { tenantId: string; agentId: string }) => ({ connections: [] }),
        listMcpServerToolsSouth: async (_req: { tenantId: string; connectorId: string; userId: string }) => ({ tools: [] }),
        getAgentSpec: async (_req: { tenantId: string; agentId: string }) => ({ found: false }),
      };
    }

    const identity: ResolveIdentity = {
      tenantId: "11111111-1111-1111-1111-111111111111",
      userId: "user-abc",
      agentId: "agent-1",
    };

    const south = makeSouth();
    const modelId = "anthropic/claude-sonnet-4.6";
    const realApiKey = "sk-real-secret";

    // Simulate what the supervisor will do in CP6: register the child in the
    // proxy first, then pass the childBaseUrl into resolveSessionPlan.
    const { childBaseUrl, childToken } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey,
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: [modelId],
    });

    const deps: ResolveSessionDeps = {
      south,
      cfg: {
        llmModel: modelId,
        defaultTenantId: identity.tenantId,
      },
      proxyBaseUrl: childBaseUrl,
    };

    const plan = await resolveSessionPlan(identity, deps);

    // plan.proxyBaseUrl must be the child's proxy URL (contains the child token).
    assert.ok(
      plan.proxyBaseUrl.includes(childToken),
      `plan.proxyBaseUrl must contain the child token; got ${plan.proxyBaseUrl}`,
    );

    // The plan must contain NO real api key.
    const serialised = JSON.stringify(plan);
    assert.ok(
      !serialised.includes(realApiKey),
      `plan must NOT contain the real api key — key leaked into plan: ${serialised}`,
    );

    // Strict secret-free key check (mirrors CP4 test, re-asserted here because
    // CP5 adds the proxy wiring and we must prove the wiring didn't introduce a leak).
    for (const forbidden of ["apiKey", "api_key", "bearer", "token", "ownerGrant", "grant", "secret"]) {
      assert.ok(
        !serialised.includes(forbidden),
        `CP5 re-assert: plan must not contain '${forbidden}'; got: ${serialised}`,
      );
    }
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

// ── Fix-1: HTTPS transport selection ──────────────────────────────────────────

test("CP5 egress-proxy fix-1: selects https transport for https: upstream URL, not http (real-key cleartext prevention)", async () => {
  // WHY: the production upstream is https://openrouter.ai — using node:http
  // would open plain TCP to :443 and transmit the real API key in cleartext.
  // This test injects a spy transport pair and asserts the https transport is
  // called for an https: upstream and the http transport is NOT called.
  let httpsCallCount = 0;
  let httpCallCount = 0;

  const realUpstream = await startFakeUpstream();

  // httpsSpy records the call, then delegates to the real http transport
  // re-routed to the plain fake upstream (loopback-only, test isolation).
  const httpsSpy: TransportLike = {
    request(opts: http.RequestOptions, cb?: (res: http.IncomingMessage) => void): http.ClientRequest {
      httpsCallCount++;
      const rerouted: http.RequestOptions = { ...opts, port: Number(new URL(realUpstream.baseUrl).port) };
      return http.request(rerouted, cb);
    },
  };
  const httpSpy: TransportLike = {
    request(opts: http.RequestOptions, cb?: (res: http.IncomingMessage) => void): http.ClientRequest {
      httpCallCount++;
      return http.request(opts, cb);
    },
  };

  const transports: _TestTransports = { http: httpSpy, https: httpsSpy };
  const proxy = new EgressProxy(transports);
  await proxy.start();
  try {
    // Register with an https: upstream URL (arbitrary port — the spy redirects to the real upstream).
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: `https://127.0.0.1:${new URL(realUpstream.baseUrl).port}`,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    await proxyPost(childBaseUrl, { model: "test-model", messages: [] }, { expectedStatus: 200 });

    assert.equal(httpsCallCount, 1, "https transport must be called once for an https: upstream");
    assert.equal(httpCallCount, 0, "http transport must NOT be called for an https: upstream");
  } finally {
    await proxy.stop();
    await realUpstream.close();
  }
});

test("CP5 egress-proxy fix-1: selects http transport for http: upstream URL (loopback test server path)", async () => {
  // WHY: confirms the http/https branch works in both directions — http: uses
  // http transport (the existing test suite already exercises this path; this
  // test makes the spy-based assertion symmetric).
  let httpsCallCount = 0;
  let httpCallCount = 0;

  const realUpstream = await startFakeUpstream();

  const httpsSpy: TransportLike = {
    request(opts: http.RequestOptions, cb?: (res: http.IncomingMessage) => void): http.ClientRequest {
      httpsCallCount++;
      return http.request(opts, cb);
    },
  };
  const httpSpy: TransportLike = {
    request(opts: http.RequestOptions, cb?: (res: http.IncomingMessage) => void): http.ClientRequest {
      httpCallCount++;
      return http.request(opts, cb);
    },
  };

  const transports: _TestTransports = { http: httpSpy, https: httpsSpy };
  const proxy = new EgressProxy(transports);
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: realUpstream.baseUrl, // http:
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    await proxyPost(childBaseUrl, { model: "test-model", messages: [] }, { expectedStatus: 200 });

    assert.equal(httpCallCount, 1, "http transport must be called once for an http: upstream");
    assert.equal(httpsCallCount, 0, "https transport must NOT be called for an http: upstream");
  } finally {
    await proxy.stop();
    await realUpstream.close();
  }
});

// ── Fix-2: fail-closed model check ────────────────────────────────────────────

test("CP5 egress-proxy fix-2: malformed JSON body → 400, not forwarded (fail-closed model check)", async () => {
  // WHY: the old code skipped the model check on parse failure and forwarded the
  // request to upstream — an unparseable body bypassed the model pin entirely.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
    const res = await fetch(url.toString(), {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer dummy" },
      body: "NOT JSON {{{",
    });
    assert.equal(res.status, 400, `malformed body must return 400, got ${res.status}`);
    assert.equal(upstream.captured.length, 0, "malformed body must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy fix-2: missing model field → 400, not forwarded", async () => {
  // WHY: a body without a model field cannot be checked against the allowlist —
  // forwarding it would let a child bypass model pinning.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    const { status } = await proxyPost(
      childBaseUrl,
      { messages: [] }, // no model field
      { expectedStatus: 400 },
    );
    assert.equal(status, 400);
    assert.equal(upstream.captured.length, 0, "missing model must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy fix-2: non-string model (null) → 400, not forwarded", async () => {
  // WHY: a model:null cannot be matched against the string allowlist — must reject.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
    const res = await fetch(url.toString(), {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer dummy" },
      body: JSON.stringify({ model: null, messages: [] }),
    });
    assert.equal(res.status, 400, `null model must return 400, got ${res.status}`);
    assert.equal(upstream.captured.length, 0, "null model must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy fix-2: non-string model (number) → 400, not forwarded", async () => {
  // WHY: a model:42 cannot be matched against the string allowlist — must reject.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
    const res = await fetch(url.toString(), {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer dummy" },
      body: JSON.stringify({ model: 42, messages: [] }),
    });
    assert.equal(res.status, 400, `number model must return 400, got ${res.status}`);
    assert.equal(upstream.captured.length, 0, "number model must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

// ── Fix-4: header smuggle guard ───────────────────────────────────────────────

// ── Rate-limit checker ────────────────────────────────────────────────────────

test("CP5 egress-proxy: setRateLimitChecker — passes request through when checker allows", async () => {
  // WHY: a checker that returns without throwing must not block the request.
  // Verifies that the allow path does NOT produce a 429.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    proxy.setRateLimitChecker(async (_tid, _aid, _prov) => { /* allow — no throw */ });
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      modelAllowlist: ["gpt-4"],
      tenantId: "t1",
      agentId: "a1",
    });
    const { status } = await proxyPost(childBaseUrl, { model: "gpt-4", messages: [] });
    assert.notEqual(status, 429, `allow path must not return 429, got ${status}`);
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: setRateLimitChecker — returns 429 when checker throws", async () => {
  // WHY: a checker that throws signals a rate-limit denial; the proxy must
  // return 429 and must NOT forward the request to upstream.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    proxy.setRateLimitChecker(async () => { throw new Error("rate limit exceeded: rpm=60/60"); });
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      modelAllowlist: ["gpt-4"],
      tenantId: "t1",
      agentId: "a1",
    });
    const { status } = await proxyPost(childBaseUrl, { model: "gpt-4", messages: [] });
    assert.equal(status, 429, `deny path must return 429, got ${status}`);
    assert.equal(upstream.captured.length, 0, "denied request must not reach upstream");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("Spend-caps CP4 egress-proxy: register()'s userId is threaded to the rate-limit checker's 4th argument", async () => {
  // WHY: CheckRateLimit needs the caller's userId to enforce a per-user spend
  // cap. The proxy stores whatever userId register() was given (the spawn-bound
  // identity, wired by ipc/supervisor.ts) and must pass it through untouched —
  // including the external-invoke/scheduled "svc-<agentId>" shape, which is
  // just an ordinary string as far as the proxy is concerned.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const seenArgs: unknown[][] = [];
    proxy.setRateLimitChecker(async (tenantId, agentId, provider, userId) => {
      seenArgs.push([tenantId, agentId, provider, userId]);
    });
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      modelAllowlist: ["gpt-4"],
      tenantId: "t1",
      agentId: "svc-agent-uuid",
      userId: "svc-agent-uuid",
    });
    await proxyPost(childBaseUrl, { model: "gpt-4", messages: [] }, { expectedStatus: 200 });

    assert.equal(seenArgs.length, 1);
    assert.deepEqual(seenArgs[0], ["t1", "svc-agent-uuid", new URL(upstream.baseUrl).hostname, "svc-agent-uuid"]);
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("Spend-caps CP4 egress-proxy: register() without userId defaults the checker's 4th argument to \"\"", async () => {
  // WHY: existing callers (and every pre-CP4 test) never pass userId — the
  // proxy must not throw or silently drop the request; it defaults to "" so
  // CheckRateLimit still runs (just without a per-user cap match).
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const seenArgs: unknown[][] = [];
    proxy.setRateLimitChecker(async (tenantId, agentId, provider, userId) => {
      seenArgs.push([tenantId, agentId, provider, userId]);
    });
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      modelAllowlist: ["gpt-4"],
      tenantId: "t1",
      agentId: "a1",
    });
    await proxyPost(childBaseUrl, { model: "gpt-4", messages: [] }, { expectedStatus: 200 });

    assert.equal(seenArgs.length, 1);
    assert.equal(seenArgs[0][3], "", "userId must default to empty string when register() omits it");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy: setRateLimitChecker — allows request when checker swallows transport error (fail-open)", async () => {
  // WHY: the production checker (in server.ts) wraps the RPC in try/catch and
  // returns (does not throw) on transport error — fail-open. This test validates
  // that a checker which swallows the error and returns does NOT produce a 429.
  // The proxy's contract: only a throw from the checker triggers 429.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    proxy.setRateLimitChecker(async () => {
      // Simulate fail-open: catch transport error, log it, return without throwing.
      try {
        throw new Error("ECONNREFUSED");
      } catch {
        return; // fail-open
      }
    });
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-real",
      modelAllowlist: ["gpt-4"],
      tenantId: "t1",
      agentId: "a1",
    });
    const { status } = await proxyPost(childBaseUrl, { model: "gpt-4", messages: [] });
    assert.notEqual(status, 429, `fail-open path must not return 429, got ${status}`);
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("CP5 egress-proxy fix-4: cookie/x-api-key/x-forwarded-for/x-real-ip NOT forwarded to upstream", async () => {
  // WHY: a child that sends cookie or x-api-key headers could hijack an upstream
  // session or billing context; x-forwarded-for/x-real-ip could forge the origin
  // identity at the upstream. The proxy must strip all four before forwarding.
  const capturedHeaders: Record<string, string | string[] | undefined>[] = [];
  const headerServer = http.createServer((req, res) => {
    capturedHeaders.push(Object.fromEntries(Object.entries(req.headers)));
    let body = "";
    req.on("data", (c: Buffer) => { body += c.toString(); });
    req.on("end", () => {
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    });
  });
  await new Promise<void>((resolve) => headerServer.listen(0, "127.0.0.1", resolve));
  const hAddr = headerServer.address();
  assert.ok(hAddr && typeof hAddr === "object");
  const headerBaseUrl = `http://127.0.0.1:${hAddr.port}`;
  const headerClose = () => new Promise<void>((resolve, reject) =>
    headerServer.close((err) => (err ? reject(err) : resolve())),
  );

  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: headerBaseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
    const res = await fetch(url.toString(), {
      method: "POST",
      headers: {
        "content-type": "application/json",
        authorization: "Bearer dummy",
        cookie: "session=evil-hijack",
        "x-api-key": "upstream-billing-key",
        "x-forwarded-for": "1.2.3.4",
        "x-real-ip": "5.6.7.8",
      },
      body: JSON.stringify({ model: "test-model", messages: [] }),
    });
    assert.equal(res.status, 200);
    assert.equal(capturedHeaders.length, 1, "upstream must receive exactly one request");
    const h = capturedHeaders[0];
    assert.ok(h);
    assert.ok(!("cookie" in h), `upstream must not receive cookie header`);
    assert.ok(!("x-api-key" in h), `upstream must not receive x-api-key header`);
    assert.ok(!("x-forwarded-for" in h), `upstream must not receive x-forwarded-for header`);
    assert.ok(!("x-real-ip" in h), `upstream must not receive x-real-ip header`);
  } finally {
    await proxy.stop();
    await headerClose();
  }
});

test("CP3 egress-proxy: stop() is idempotent — a second call resolves instead of rejecting", async () => {
  const proxy = new EgressProxy();
  await proxy.start();
  await proxy.stop();
  await proxy.stop(); // must not throw/reject
});

test("CP3 egress-proxy: stop() resolves even if the proxy was never started", async () => {
  const proxy = new EgressProxy();
  await proxy.stop(); // must not throw/reject
});

// ── F10: upstream timeouts + client-disconnect propagation ───────────────────

test("F10 egress-proxy: hung upstream (no response headers) → client gets 504 within timeout, upstream request destroyed", async () => {
  // WHY: a stuck/hostile upstream that never answers must not hang the client
  // forever. The proxy needs a headers timeout on the upstream leg. The fake
  // upstream here accepts the connection but never writes a response — a
  // genuine hang, not a transport error, so only a real timeout can resolve it.
  let socketClosed: Promise<void> = Promise.resolve();
  const hungServer = http.createServer((_req, _res) => {
    // Deliberately never call res.writeHead/res.end — simulates a stuck upstream.
  });
  hungServer.on("connection", (socket) => {
    socketClosed = new Promise((resolve) => socket.on("close", () => resolve()));
  });
  await new Promise<void>((resolve) => hungServer.listen(0, "127.0.0.1", resolve));
  const hAddr = hungServer.address();
  assert.ok(hAddr && typeof hAddr === "object");
  const hungBaseUrl = `http://127.0.0.1:${hAddr.port}`;
  const hungClose = () => new Promise<void>((resolve, reject) =>
    hungServer.close((err) => (err ? reject(err) : resolve())),
  );

  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 50 });
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: hungBaseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const { status } = await proxyPost(childBaseUrl, { model: "test-model", messages: [] });
    assert.equal(status, 504, `hung upstream must yield 504, got ${status}`);
    // The 504 is written to the client before the (async) socket teardown
    // necessarily completes — await it directly instead of racing a counter.
    await Promise.race([
      socketClosed,
      new Promise<void>((_resolve, reject) =>
        setTimeout(() => reject(new Error("upstream socket was never closed after headers timeout")), 1000),
      ),
    ]);
  } finally {
    await proxy.stop();
    await hungClose();
  }
});

test("F10 egress-proxy: streaming stalls mid-body (no data for egressTimeoutMs) → upstream destroyed, client connection ended", async () => {
  // WHY: proves the per-chunk refresh, not just "some timeout eventually
  // fires". Four chunks are spaced 30ms apart (each gap well under the 50ms
  // idle timeout) but their SUM (90ms) exceeds it — a proxy that armed the
  // timer once and never refreshed it would cut the stream off around the
  // 50ms mark, after only 1-2 chunks. All four must arrive; only the
  // subsequent real stall (no fifth chunk, ever) may end the connection.
  //
  // Margins are deliberately wide (150ms headroom per gap), matching the
  // headers-to-first-chunk test below: the assertion is about the per-chunk
  // re-arm, not timer precision, and the original 50ms/30ms pair was tight
  // enough that concurrent test files could stretch a 30ms gap past the timeout
  // and cut the stream off early.
  const egressTimeoutMs = 300;
  const chunkGapMs = 150;
  const totalChunks = 4;
  let served = 0;
  const stallServer = http.createServer((_req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    const writeNext = (): void => {
      served++;
      res.write(`data: {"chunk":${served}}\n\n`);
      if (served < totalChunks) {
        setTimeout(writeNext, chunkGapMs);
      }
      // After the last chunk, deliberately never write again and never end() —
      // this is the real stall the idle timeout must catch.
    };
    writeNext();
  });
  await new Promise<void>((resolve) => stallServer.listen(0, "127.0.0.1", resolve));
  const sAddr = stallServer.address();
  assert.ok(sAddr && typeof sAddr === "object");
  const stallBaseUrl = `http://127.0.0.1:${sAddr.port}`;
  const stallClose = () => new Promise<void>((resolve, reject) =>
    stallServer.close((err) => (err ? reject(err) : resolve())),
  );

  const proxy = new EgressProxy(undefined, { egressTimeoutMs });
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: stallBaseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const { chunkCount, ended } = await new Promise<{ chunkCount: number; ended: boolean }>((resolve, reject) => {
      const req = http.request(
        {
          hostname: "127.0.0.1",
          port: proxy.address().port,
          path: `${new URL(childBaseUrl).pathname}/chat/completions`,
          method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" },
        },
        (res) => {
          let chunkCountLocal = 0;
          res.on("data", () => { chunkCountLocal++; });
          res.on("end", () => resolve({ chunkCount: chunkCountLocal, ended: true }));
          res.on("error", () => resolve({ chunkCount: chunkCountLocal, ended: true }));
        },
      );
      req.on("error", reject);
      req.write(JSON.stringify({ model: "test-model", messages: [] }));
      req.end();
      // Safety net so the test itself cannot hang. Comfortably above the
      // expected ~750ms (4 chunks × chunkGapMs, then the idle timeout).
      setTimeout(() => resolve({ chunkCount: -1, ended: false }), 5000).unref();
    });

    assert.ok(ended, "client response must have been ended by the idle timeout after the real stall");
    assert.equal(
      chunkCount,
      totalChunks,
      `all ${totalChunks} chunks (spaced ${chunkGapMs}ms, summing to ${chunkGapMs * (totalChunks - 1)}ms > ` +
        `${egressTimeoutMs}ms timeout) must have arrived — a proxy that doesn't refresh per chunk would cut off early`,
    );
  } finally {
    await proxy.stop();
    await stallClose();
  }
});

test("F10 egress-proxy: headers-then-first-chunk gap re-arms the idle timer with a fresh budget", async () => {
  // WHY: pins the armIdleTimer() call at the top of the response callback
  // (egress-proxy.ts) — the re-arm for the gap between headers arriving and
  // the first body chunk. Headers arrive at headersDelayMs of egressTimeoutMs,
  // then the first chunk arrives chunkDelayMs after THAT (their sum exceeds
  // egressTimeoutMs, but each individual gap is under it). Without the
  // re-arm, the headers-phase timer (armed at request-send, t=0) is never
  // refreshed and trips before the first chunk arrives.
  //
  // res.flushHeaders() is load-bearing here: Node coalesces writeHead() with
  // the first write()/end() call and does not put headers on the wire until
  // then — without an explicit flush this test would (silently) collapse
  // into "headers and first chunk arrive together", not exercise the gap
  // between them at all. Margins are wide (well beyond loopback/test
  // scheduling jitter of tens of ms) so the assertion is about the re-arm,
  // not timer precision.
  const egressTimeoutMs = 300;
  const headersDelayMs = 200;
  const chunkDelayMs = 200;
  const lateHeadersServer = http.createServer((_req, res) => {
    setTimeout(() => {
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.flushHeaders();
      setTimeout(() => {
        res.write('data: {"chunk":1}\n\n');
        res.end();
      }, chunkDelayMs);
    }, headersDelayMs);
  });
  await new Promise<void>((resolve) => lateHeadersServer.listen(0, "127.0.0.1", resolve));
  const lAddr = lateHeadersServer.address();
  assert.ok(lAddr && typeof lAddr === "object");
  const lateHeadersBaseUrl = `http://127.0.0.1:${lAddr.port}`;
  const lateHeadersClose = () => new Promise<void>((resolve, reject) =>
    lateHeadersServer.close((err) => (err ? reject(err) : resolve())),
  );

  const proxy = new EgressProxy(undefined, { egressTimeoutMs });
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: lateHeadersBaseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    const { status } = await proxyPost(childBaseUrl, { model: "test-model", messages: [] });
    assert.equal(
      status,
      200,
      `headers at ${headersDelayMs}ms + first chunk ${chunkDelayMs}ms later (total ${headersDelayMs + chunkDelayMs}ms > ` +
        `${egressTimeoutMs}ms timeout, each gap under it) must succeed — the re-arm on response-callback entry must give ` +
        `the headers-to-first-chunk gap a fresh full budget`,
    );
  } finally {
    await proxy.stop();
    await lateHeadersClose();
  }
});

test("F10 egress-proxy: client disconnects mid-stream → upstream request destroyed", async () => {
  // WHY: if the client goes away before the upstream finishes, the proxy must
  // stop consuming the provider stream instead of running it to completion
  // for nobody.
  const upstream = await startFakeUpstream();
  upstream.nextChunks = ['data: {"chunk":1}\n\n', 'data: {"chunk":2}\n\n', 'data: {"chunk":3}\n\n'];
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });

    await new Promise<void>((resolve, reject) => {
      const req = http.request(
        {
          hostname: "127.0.0.1",
          port: proxy.address().port,
          path: `${new URL(childBaseUrl).pathname}/chat/completions`,
          method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" },
        },
        (res) => {
          res.once("data", () => {
            // Client disconnects right after the first chunk.
            req.destroy();
          });
        },
      );
      req.on("error", () => {}); // destroy() itself surfaces a socket-hang-up error — expected.
      req.write(JSON.stringify({ model: "test-model", messages: [] }));
      req.end();
      setTimeout(resolve, 200);
    });

    assert.equal(upstream.captured.length, 1, "upstream must have received the request");
    // The load-bearing assertion: the proxy must have actually torn down its
    // connection to the upstream (not just stopped writing to the client) —
    // observed here as the fake upstream's response closing before it ever
    // called res.end().
    await Promise.race([
      upstream.responseClosedEarly,
      new Promise<void>((_resolve, reject) =>
        setTimeout(() => reject(new Error("upstream connection was never torn down after client disconnect")), 1000),
      ),
    ]);
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("F10 egress-proxy: normal fast request unaffected by timeout wiring (parity)", async () => {
  const upstream = await startFakeUpstream();
  // Comfortably above the fake upstream's inter-chunk gap: the point is that a
  // healthy request is untouched by the timer, not how tight the timer can be.
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: UPSTREAM_CHUNK_GAP_MS * 5 });
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });
    // Fake upstream's default nextChunks streams one chunk, then ends one
    // UPSTREAM_CHUNK_GAP_MS later — well within the idle timeout per chunk.
    const { status } = await proxyPost(childBaseUrl, { model: "test-model", messages: [] });
    assert.equal(status, 200, "fast request must complete normally, unaffected by timeout wiring");
  } finally {
    await proxy.stop();
    await upstream.close();
  }
});

test("F10 egress-proxy: N sequential requests over one keep-alive connection do not accumulate socket 'close' listeners", async () => {
  // WHY: the inbound socket is reused across requests on a keep-alive
  // connection. If the per-request disconnect listener is never detached on
  // normal completion, each request leaves one more 'close' listener on the
  // same socket — reviewer reproduced a MaxListenersExceededWarning (default
  // cap 10) at 15 sequential requests. This test drives the same scenario
  // black-box, via Node's own warning mechanism, rather than reaching into
  // the proxy's private http.Server.
  const upstream = await startFakeUpstream();
  const proxy = new EgressProxy();
  await proxy.start();
  const agent = new http.Agent({ keepAlive: true, maxSockets: 1 });
  const warnings: string[] = [];
  const onWarning = (w: Error): void => {
    if (w.name === "MaxListenersExceededWarning") warnings.push(w.message);
  };
  process.on("warning", onWarning);
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: upstream.baseUrl,
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["test-model"],
    });
    const path = `${new URL(childBaseUrl).pathname}/chat/completions`;

    const postOnce = (): Promise<void> =>
      new Promise((resolve, reject) => {
        const req = http.request(
          {
            hostname: "127.0.0.1",
            port: proxy.address().port,
            path,
            method: "POST",
            agent,
            headers: { "content-type": "application/json", authorization: "Bearer dummy" },
          },
          (res) => {
            res.resume();
            res.on("end", () => resolve());
            res.on("error", reject);
          },
        );
        req.on("error", reject);
        req.write(JSON.stringify({ model: "test-model", messages: [] }));
        req.end();
      });

    const requestCount = 15; // exceeds EventEmitter's default max-listeners cap (10)
    for (let i = 0; i < requestCount; i++) {
      await postOnce();
    }

    // The warning is emitted asynchronously (process.emitWarning) — give it a
    // moment to surface before asserting its absence.
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.equal(
      warnings.length,
      0,
      `MaxListenersExceededWarning must not fire after ${requestCount} keep-alive requests; got: ${warnings.join("; ")}`,
    );
  } finally {
    process.removeListener("warning", onWarning);
    agent.destroy();
    await proxy.stop();
    await upstream.close();
  }
});

test("CP3 egress-proxy: stop() force-closes lingering connections after the grace period", async () => {
  const proxy = new EgressProxy();
  await proxy.start();
  const addr = proxy.address();

  // Open a raw socket and never send a request, simulating a stuck in-flight
  // connection that would otherwise keep server.close() pending forever.
  const socket = await new Promise<net.Socket>((resolve) => {
    const s = net.connect(addr.port, addr.address, () => resolve(s));
  });

  const start = Date.now();
  await proxy.stop(20); // tiny grace so the test doesn't hang
  const elapsed = Date.now() - start;

  // Should resolve close to the grace period, not hang indefinitely waiting
  // for the idle socket to close on its own.
  assert.ok(elapsed < 2000, `stop() took ${elapsed}ms — force-close grace period did not fire`);
  socket.destroy();
});

test("egress-proxy: a late inbound-stream error after the response was sent does not crash the parent", async () => {
  // WHY: handleRequest's req.on("error") outlives the response it replies on.
  // Every other path (finishRequest's 400s, forward's budget 429, a piped
  // upstream response) may already have written to clientRes by the time that
  // listener fires, and writeHead after headersSent throws ERR_HTTP_HEADERS_SENT
  // SYNCHRONOUSLY inside the listener — uncaught, killing the gateway parent and
  // with it every user's in-flight run. A clientRes 'error' listener would NOT
  // contain this (a sync throw is not an error event); the headersSent guard is
  // what does. Reachability of a post-'end' IncomingMessage 'error' is
  // deliberately left unverified — this pins the handler, not the trigger, so the
  // error is injected directly onto the request stream.
  const proxy = new EgressProxy();
  await proxy.start();
  let thrownByListener: unknown;
  let injected = false;
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: "http://127.0.0.1:1/unused", // never dialed — the 400 path returns first
      realApiKey: "sk-key",
      tenantId: "test-tenant", agentId: "test-agent", modelAllowlist: ["allowed-model"],
    });

    // A second 'request' listener registers its own 'end' handler AFTER the
    // proxy's, so it runs once the proxy has already replied 400 — exactly the
    // headers-already-sent ordering the guard exists for. emit() dispatches
    // listeners synchronously, so a throw from the guarded handler surfaces here.
    proxy["server"].on("request", (req: http.IncomingMessage) => {
      req.on("end", () => {
        injected = true;
        try {
          req.emit("error", new Error("aborted"));
        } catch (err) {
          thrownByListener = err;
        }
      });
    });

    const { status } = await proxyPost(
      childBaseUrl,
      { model: "forbidden-model", messages: [] },
      { expectedStatus: 400 },
    );
    assert.equal(status, 400, "the client still gets its original reply, unchanged by the late error");
    assert.ok(injected, "the late inbound-stream error must actually have been injected");
    assert.equal(
      thrownByListener,
      undefined,
      `the guarded handler must swallow a post-reply inbound error, threw: ${String(thrownByListener)}`,
    );
  } finally {
    await proxy.stop();
  }
});
