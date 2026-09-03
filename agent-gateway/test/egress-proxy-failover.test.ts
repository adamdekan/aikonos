// Runtime failover (slice 3): the egress proxy retries the next provider in the
// child's pinned chain when an attempt fails BEFORE anything reaches the client.
//
// WHY these tests exist: failover moves the proxy from one upstream attempt to
// several, and each of the proxy's security properties has to survive per attempt
// — the fallback's own key/dialect/model, the fallback's own rate-limit subject,
// and no leaked timer or socket listener per retry. The hard ceiling (no retry
// once bytes are on the wire) is pinned too, because an SSE stream cannot be
// rewound and a "helpful" mid-stream retry would corrupt the child's stream.
import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import {
  EgressProxy,
  type ProviderTarget,
  type TransportLike,
  type _TestTransports,
} from "../src/llm/egress-proxy.js";
import { isRecord } from "../src/llm/provider-dialect.js";

// ── Fakes ─────────────────────────────────────────────────────────────────────

interface CapturedRequest {
  authorization: string | undefined;
  apiKey: string | undefined;
  path: string;
  body: string;
}

interface FakeProvider {
  baseUrl: string;
  port: number;
  captured: CapturedRequest[];
  // Mutable so a test can flip a provider's verdict mid-suite.
  status: number;
  body: string;
  close(): Promise<void>;
}

// startFakeProvider answers every request with one non-streaming response, whose
// status/body the test controls. Deliberately simpler than egress-proxy.test.ts's
// streaming upstream — failover is decided from the status line, before any body.
async function startFakeProvider(opts: { status?: number; body?: string } = {}): Promise<FakeProvider> {
  const captured: CapturedRequest[] = [];
  let status = opts.status ?? 200;
  let body = opts.body ?? '{"ok":true}';

  const server = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (chunk: Buffer) => { raw += chunk.toString(); });
    req.on("end", () => {
      const apiKey = req.headers["api-key"];
      captured.push({
        authorization: req.headers["authorization"],
        apiKey: typeof apiKey === "string" ? apiKey : undefined,
        path: req.url ?? "/",
        body: raw,
      });
      res.writeHead(status, { "content-type": "application/json" });
      res.end(body);
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "fake provider must bind");

  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    port: addr.port,
    captured,
    get status() { return status; },
    set status(v) { status = v; },
    get body() { return body; },
    set body(v) { body = v; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

interface CountingProvider {
  baseUrl: string;
  hits: number;
  close(): Promise<void>;
}

// startHungProvider accepts the request and never answers — the only way out is a
// real headers timeout.
async function startHungProvider(): Promise<CountingProvider> {
  let hits = 0;
  const server = http.createServer((req) => {
    req.on("data", () => {});
    req.on("end", () => { hits++; });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "hung provider must bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    get hits() { return hits; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

// startMidStreamStallProvider sends headers + one body chunk and then goes silent
// forever — the failure shape that must NOT be retried, and the one that actually
// reaches the proxy's upstream 'error' handler (via the per-attempt idle timeout)
// with headersSent already true.
//
// Deliberately a stall rather than a socket kill: Node raises NO error on the
// upstream ClientRequest when a provider destroys the socket mid-chunked-body —
// it emits 'close' instead, so the proxy's error branch is never entered at all.
// (Verified identical on the pre-failover code, so it is a pre-existing gap, not
// one this slice introduces.) A stall keeps the upstream request open, so the idle
// timer fires and the ceiling branch is genuinely exercised.
async function startMidStreamStallProvider(): Promise<CountingProvider> {
  let hits = 0;
  const server = http.createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      hits++;
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.write('data: {"chunk":1}\n\n');
      // Never write again, never end() — the real stall the idle timeout catches.
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "mid-stream-stall provider must bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    get hits() { return hits; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

// reserveClosedPort returns a port nothing listens on, so a connection to it is a
// genuine transport error (ECONNREFUSED) rather than a simulated one.
async function reserveClosedPort(): Promise<number> {
  const server = http.createServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "port reservation must bind");
  const { port } = addr;
  await new Promise<void>((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
  return port;
}

async function proxyPost(
  childBaseUrl: string,
  body: Record<string, unknown>,
): Promise<{ status: number; body: string }> {
  const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
  const res = await fetch(url.toString(), {
    method: "POST",
    headers: { "content-type": "application/json", authorization: "Bearer dummy-child-key" },
    body: JSON.stringify(body),
  });
  return { status: res.status, body: await res.text() };
}

// ── 1. A retryable status is transparently retried, with the target's model ────

test("failover: primary 500 → retried on the fallback; client sees the fallback's 200 and its own model/key", async () => {
  // WHY: a provider 500 arrives as a *successful* upstreamRes carrying a bad
  // status — before this slice it was piped straight through to the child. This
  // is the canonical "provider not working" case, and the retry must be totally
  // invisible: the client sees only the fallback's success.
  const primary = await startFakeProvider({ status: 500, body: "primary exploded" });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const { status, body } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 200, "the client must see the fallback's success, never the primary's 500");
    assert.equal(body, '{"from":"fallback"}', "the client must receive the FALLBACK's body");

    assert.equal(primary.captured.length, 1, "the primary must have been attempted once");
    assert.equal(fallback.captured.length, 1, "the fallback must have been attempted once");

    const fbReq = fallback.captured[0];
    assert.ok(fbReq, "the fallback must have captured its request");
    assert.equal(fbReq.authorization, "Bearer sk-fallback", "the fallback must be called with ITS OWN key");
    const forwarded: unknown = JSON.parse(fbReq.body);
    assert.ok(isRecord(forwarded), "the forwarded body must be a JSON object");
    assert.equal(
      forwarded.model,
      "fallback-model",
      "the body sent to the fallback must be rewritten to the fallback's own modelId",
    );

    const primaryReq = primary.captured[0];
    assert.ok(primaryReq, "the primary must have captured its request");
    const primarySent: unknown = JSON.parse(primaryReq.body);
    assert.ok(isRecord(primarySent));
    assert.equal(primarySent.model, "primary-model", "the primary attempt must keep the child's requested model");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── 2. The trigger set is respected — a request error is never retried ─────────

test("failover: primary 400 → NO failover; the 400 is passed through and the fallback is never billed", async () => {
  // WHY: a 400 means the request itself is bad — the fallback would reject the
  // identical body identically, so retrying only burns the fallback's quota.
  // shouldFailover excludes 400/404/422 for exactly this reason.
  const primary = await startFakeProvider({ status: 400, body: "bad request" });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const { status, body } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 400, "a 400 must reach the client unchanged");
    assert.equal(body, "bad request");
    assert.equal(fallback.captured.length, 0, "a 400 must NOT burn the fallback's quota");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── 3. Dead key (401) and throttle (429) ──────────────────────────────────────

for (const badStatus of [401, 429, 403, 503]) {
  test(`failover: primary ${badStatus} → retried on the fallback`, async () => {
    // WHY: 401/403 is this provider's key, 429 is this provider's quota, 5xx is
    // this provider's outage — all conditions a different provider can genuinely
    // satisfy. 401 (a dead/rotated key) and 429 (throttling) are the two most
    // common in production.
    const primary = await startFakeProvider({ status: badStatus, body: "nope" });
    const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
    const proxy = new EgressProxy();
    await proxy.start();
    try {
      const fallbacks: ProviderTarget[] = [
        { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
      ];
      const { childBaseUrl } = proxy.register({
        upstreamBaseUrl: primary.baseUrl,
        realApiKey: "sk-primary",
        modelAllowlist: ["primary-model", "fallback-model"],
        tenantId: "t1",
        agentId: "a1",
        fallbacks,
      });

      const { status, body } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
      assert.equal(status, 200, `a ${badStatus} must fail over, not reach the client`);
      assert.equal(body, '{"from":"fallback"}');
      assert.equal(fallback.captured.length, 1);
    } finally {
      await proxy.stop();
      await primary.close();
      await fallback.close();
    }
  });
}

// ── 4. The hard ceiling: no retry once bytes are on the wire ──────────────────

test("failover: upstream fails AFTER headers + body reached the client → NO retry, connection destroyed", async () => {
  // WHY: this is the hard boundary. Once headersSent is true the child has
  // already received bytes; an SSE stream cannot be rewound, so replaying the
  // request against a fallback would splice two responses into one stream. The
  // only correct action is to destroy the connection (today's behavior) — the
  // fallback must never be attempted. This drives the upstream 'error' handler
  // with headersSent already true, i.e. exactly the branch a naive failover
  // implementation would have retried from.
  const primary = await startMidStreamStallProvider();
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 80 });
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const { statusCode, chunks, complete } = await new Promise<{
      statusCode: number | undefined;
      chunks: string[];
      complete: boolean;
    }>((resolve, reject) => {
      const req = http.request(
        {
          hostname: "127.0.0.1",
          port: proxy.address().port,
          path: `${new URL(childBaseUrl).pathname}/chat/completions`,
          method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" },
        },
        (res) => {
          const seen: string[] = [];
          res.on("data", (chunk: Buffer) => { seen.push(chunk.toString()); });
          res.on("error", () => {});
          res.on("close", () => resolve({ statusCode: res.statusCode, chunks: seen, complete: res.complete }));
        },
      );
      req.on("error", reject);
      req.write(JSON.stringify({ model: "primary-model", messages: [] }));
      req.end();
      setTimeout(() => reject(new Error("mid-stream failure never settled")), 3000).unref();
    });

    assert.equal(statusCode, 200, "the primary's 200 headers must have reached the client");
    assert.deepEqual(chunks, ['data: {"chunk":1}\n\n'], "the primary's first chunk must have reached the client");
    assert.equal(complete, false, "the connection must have been destroyed, not cleanly ended");
    assert.equal(primary.hits, 1);
    assert.equal(
      fallback.captured.length,
      0,
      "a mid-stream failure must NOT be retried — the client already holds bytes from the primary",
    );
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── 5. Exhausted chain keeps today's terminal responses ───────────────────────

test("failover: every provider unreachable → 502 (terminal behavior unchanged)", async () => {
  // WHY: exhausting the chain must land on exactly the response a single failing
  // provider produced before — 502 for a transport error, not a new status.
  const deadPortA = await reserveClosedPort();
  const deadPortB = await reserveClosedPort();
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: `http://127.0.0.1:${deadPortB}`, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: `http://127.0.0.1:${deadPortA}`,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const { status } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 502, "an exhausted chain of transport errors must still be a 502");
  } finally {
    await proxy.stop();
  }
});

test("failover: every provider hangs → 504, and each provider was actually attempted", async () => {
  // WHY: the timeout variant of an exhausted chain. Also proves the idle timer is
  // per-attempt: a request-scoped timer would fire once and never give the
  // fallback its own budget, so the second provider would never see a request.
  const primary = await startHungProvider();
  const fallback = await startHungProvider();
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 60 });
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const { status } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 504, "an exhausted chain of timeouts must still be a 504");
    assert.equal(primary.hits, 1, "the primary must have been attempted");
    assert.equal(fallback.hits, 1, "the fallback must have gotten its own timeout budget");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── 6. No fallbacks configured → unchanged behavior ───────────────────────────

test("failover: no fallbacks configured → a retryable status is passed through unchanged (no chain, no swallow)", async () => {
  // WHY: the overwhelming majority of registrations (and every pre-slice-3 test)
  // has no fallbacks. Failover must not convert a provider's 500 into a proxy
  // 502 — the child must keep seeing the provider's own status and body.
  const primary = await startFakeProvider({ status: 500, body: "primary exploded" });
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model"],
      tenantId: "t1",
      agentId: "a1",
    });

    const { status, body } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 500, "with no chain, the provider's own 500 must reach the client");
    assert.equal(body, "primary exploded");

    primary.status = 200;
    primary.body = '{"ok":true}';
    const ok = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(ok.status, 200);
    assert.equal(ok.body, '{"ok":true}');
    const req = primary.captured[1];
    assert.ok(req);
    const sent: unknown = JSON.parse(req.body);
    assert.ok(isRecord(sent));
    assert.equal(sent.model, "primary-model", "a single-target request must never have its model rewritten");
  } finally {
    await proxy.stop();
    await primary.close();
  }
});

// ── 7. The rate-limit pre-check is per attempt, per hostname ───────────────────

test("failover: each attempt runs the rate-limit pre-check against ITS OWN provider hostname", async () => {
  // WHY: a different provider is a different rate-limit and spend-cap subject. A
  // failover that inherited the primary's clearance would let the fallback bypass
  // its own cap entirely — a real enforcement hole, not a cosmetic one.
  //
  // Distinct hostnames are needed to tell the two checks apart, so the targets
  // carry synthetic hostnames and an injected transport reroutes them to the two
  // loopback fakes (no DNS involved, same seam as the HTTPS-selection test).
  const primary = await startFakeProvider({ status: 500, body: "primary exploded" });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });

  const routes: Record<string, number> = {
    "primary.example": primary.port,
    "fallback.example": fallback.port,
  };
  const reroute: TransportLike = {
    request(opts: http.RequestOptions, cb?: (res: http.IncomingMessage) => void): http.ClientRequest {
      const port = routes[String(opts.hostname)];
      assert.ok(port, `unexpected upstream hostname ${String(opts.hostname)}`);
      return http.request({ ...opts, hostname: "127.0.0.1", port }, cb);
    },
  };
  const transports: _TestTransports = { http: reroute, https: reroute };

  const proxy = new EgressProxy(transports);
  await proxy.start();
  try {
    const seen: Array<[string, string, string, string | undefined]> = [];
    proxy.setRateLimitChecker(async (tenantId, agentId, provider, userId) => {
      seen.push([tenantId, agentId, provider, userId]);
    });

    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: "http://fallback.example", apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: "http://primary.example",
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      userId: "alice@example.com",
      fallbacks,
    });

    const { status } = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(status, 200);
    assert.deepEqual(
      seen,
      [
        ["t1", "a1", "primary.example", "alice@example.com"],
        ["t1", "a1", "fallback.example", "alice@example.com"],
      ],
      "each attempt must pre-check its own provider hostname — the fallback must not inherit the primary's clearance",
    );
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

test("failover: a rate-limit denial on the primary fails over; a denial with the chain exhausted still 429s", async () => {
  // WHY: the checker's denial is this provider's own 429, so it is failover-
  // eligible exactly like an upstream 429 — but only while a target remains. An
  // exhausted chain must keep the pre-existing 429 response.
  const primary = await startFakeProvider({ status: 200, body: '{"from":"primary"}' });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const denied = new Set<string>();
    proxy.setRateLimitChecker(async (_tenantId, _agentId, _provider, _userId) => {
      // Keyed by attempt order rather than hostname: both fakes are on 127.0.0.1.
      const attempt = String(denied.size);
      denied.add(attempt);
      if (attempt === "0") throw new Error("rate limit exceeded: spend_user");
    });

    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const allowed = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(allowed.status, 200, "a denied primary must fail over to the fallback");
    assert.equal(allowed.body, '{"from":"fallback"}');
    assert.equal(primary.captured.length, 0, "a denied provider must never be contacted");

    // Now deny everything: the exhausted chain must land on the pre-existing 429.
    proxy.setRateLimitChecker(async () => { throw new Error("rate limit exceeded: spend_org"); });
    const refused = await proxyPost(childBaseUrl, { model: "primary-model", messages: [] });
    assert.equal(refused.status, 429, "an exhausted chain of denials must still be a 429");
    assert.match(refused.body, /spend_org/);
    assert.equal(fallback.captured.length, 1, "no provider may be contacted once every attempt is denied");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── 8. No timer or socket listener survives a failover ────────────────────────

test("failover: repeated failovers over one keep-alive socket leak no 'close' listener and no idle timer", async () => {
  // WHY: the idle timer, the timedOut flag, and the inbound socket's 'close'
  // listener used to be request-scoped singletons. Making them per-attempt means
  // each attempt must fully tear its own down before the next begins — otherwise
  // a chain leaks one listener and one live timer per retry. The keep-alive socket
  // is the amplifier: 15 requests × 2 attempts on ONE socket is 30 attaches, well
  // past EventEmitter's default cap of 10, so a missed detach surfaces as a
  // MaxListenersExceededWarning. A long egress timeout makes a leaked timer
  // equally visible: it would still be pending after the chain settled.
  const primary = await startFakeProvider({ status: 500, body: "primary exploded" });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 30_000 });
  await proxy.start();
  const agent = new http.Agent({ keepAlive: true, maxSockets: 1 });
  const warnings: string[] = [];
  const onWarning = (w: Error): void => {
    if (w.name === "MaxListenersExceededWarning") warnings.push(w.message);
  };
  process.on("warning", onWarning);
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
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
        req.write(JSON.stringify({ model: "primary-model", messages: [] }));
        req.end();
      });

    await postOnce();
    const timersAfterFirst = process.getActiveResourcesInfo().filter((r) => r === "Timeout").length;

    const requestCount = 15; // 30 attempts on one socket — 3x EventEmitter's cap
    for (let i = 0; i < requestCount; i++) {
      await postOnce();
    }

    // process.emitWarning is async — let it surface before asserting its absence.
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.equal(
      warnings.length,
      0,
      `MaxListenersExceededWarning must not fire after ${requestCount} failing-over keep-alive requests; got: ${warnings.join("; ")}`,
    );

    const timersAfterAll = process.getActiveResourcesInfo().filter((r) => r === "Timeout").length;
    assert.ok(
      timersAfterAll <= timersAfterFirst,
      `pending timers grew from ${timersAfterFirst} to ${timersAfterAll} across ${requestCount} failovers — ` +
        `an attempt's idle timer is not being cleared`,
    );

    assert.equal(primary.captured.length, requestCount + 1);
    assert.equal(fallback.captured.length, requestCount + 1);
  } finally {
    process.removeListener("warning", onWarning);
    agent.destroy();
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── Client disconnect aborts the chain ────────────────────────────────────────

test("failover: client hangs up while the chain is in flight → no further provider is attempted", async () => {
  // WHY: there is no one left to serve a fallback's response to. Continuing the
  // chain would bill the tenant for a response nobody reads.
  const primary = await startHungProvider();
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 500 });
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    await new Promise<void>((resolve) => {
      const req = http.request(
        {
          hostname: "127.0.0.1",
          port: proxy.address().port,
          path: `${new URL(childBaseUrl).pathname}/chat/completions`,
          method: "POST",
          headers: { "content-type": "application/json", authorization: "Bearer dummy" },
        },
        () => {},
      );
      req.on("error", () => {}); // destroy() surfaces a socket hang-up — expected.
      req.write(JSON.stringify({ model: "primary-model", messages: [] }));
      req.end();
      // Disconnect while the primary is still hanging, before its timeout fires.
      setTimeout(() => req.destroy(), 100);
      // Then wait past the primary's timeout: a chain that ignored the hang-up
      // would have moved on to the fallback by now.
      setTimeout(resolve, 800);
    });

    assert.equal(primary.hits, 1, "the primary must have been attempted");
    assert.equal(
      fallback.captured.length,
      0,
      "the chain must stop at a client disconnect — no provider may be attempted for a caller that is gone",
    );
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

// ── Bare upstream 'close' (no 'error') must not hang the client ────────────────
//
// WHY: an upstream socket torn down abruptly (RST) surfaces on the proxy's
// upstream ClientRequest as a bare 'close' with NO preceding 'error' — verified
// empirically, and the reason startMidStreamStallProvider above had to use a
// stall instead of a socket kill. The 'close' handler used to only clear the idle
// timer and detach, which left the client response open FOREVER: the very
// clearIdleTimer() call that ran there disarmed the one timer that would
// otherwise have rescued it. So the handler must own the outcome of an unsettled
// attempt, exactly as the transport-error path does.

// startHeadersThenKillProvider sends headers + one body chunk, then destroys the
// TCP connection without ending the response — the auditor's exact scenario.
async function startHeadersThenKillProvider(): Promise<CountingProvider> {
  let hits = 0;
  const server = http.createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      hits++;
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.write('data: {"chunk":1}\n\n');
      setTimeout(() => res.socket?.destroy(), 20);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "headers-then-kill provider must bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    get hits() { return hits; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

// startKillBeforeHeadersProvider destroys the connection before writing any
// response at all — the same abrupt teardown, but pre-headers, where a retry is
// still invisible to the client.
async function startKillBeforeHeadersProvider(): Promise<CountingProvider> {
  let hits = 0;
  const server = http.createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      hits++;
      res.socket?.destroy();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "kill-before-headers provider must bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    get hits() { return hits; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

// rawProxyPost drives the proxy with node:http (not fetch) so the test can
// observe HOW the client response terminated and how long it took — a hang is
// exactly what fetch would hide behind an opaque pending promise.
interface RawOutcome {
  status?: number;
  body: string;
  terminated: "end" | "aborted" | "req-error" | "timeout";
  elapsedMs: number;
}

function rawProxyPost(
  proxyPort: number,
  childBaseUrl: string,
  body: Record<string, unknown>,
  waitMs: number,
): Promise<RawOutcome> {
  return new Promise<RawOutcome>((resolve) => {
    const started = Date.now();
    let settled = false;
    let status: number | undefined;
    let received = "";
    const done = (terminated: RawOutcome["terminated"]) => {
      if (settled) return;
      settled = true;
      resolve({ status, body: received, terminated, elapsedMs: Date.now() - started });
    };
    const req = http.request(
      {
        hostname: "127.0.0.1",
        port: proxyPort,
        path: `${new URL(childBaseUrl).pathname}/chat/completions`,
        method: "POST",
        headers: { "content-type": "application/json", authorization: "Bearer dummy-child-key" },
      },
      (res) => {
        status = res.statusCode;
        res.on("data", (chunk: Buffer) => { received += chunk.toString(); });
        res.on("end", () => done("end"));
        res.on("aborted", () => done("aborted"));
        res.on("error", () => done("aborted"));
      },
    );
    req.on("error", () => done("req-error"));
    req.write(JSON.stringify(body));
    req.end();
    // The hang guard: if nothing terminated the response by now, it is hung.
    setTimeout(() => done("timeout"), waitMs).unref();
  });
}

test("bare upstream 'close' mid-body: the client response is terminated promptly, never left hanging", async () => {
  // egressTimeoutMs is set far above the observation window on purpose: if the
  // client only survives because the idle timer rescued it, this test fails.
  const primary = await startHeadersThenKillProvider();
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 10_000 });
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const outcome = await rawProxyPost(proxy.address().port, childBaseUrl, { model: "primary-model", messages: [] }, 1500);

    assert.notEqual(
      outcome.terminated,
      "timeout",
      "the client response must be terminated by the proxy, not left hanging for the idle timer that the 'close' handler already disarmed",
    );
    assert.equal(outcome.status, 200, "headers were already forwarded — the status cannot be rewritten");
    assert.ok(
      outcome.elapsedMs < 1000,
      `termination must be prompt, took ${outcome.elapsedMs}ms`,
    );
    assert.equal(
      fallback.captured.length,
      0,
      "headersSent is the hard failover ceiling — a mid-stream teardown must NOT retry (and must not bill the fallback)",
    );
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

test("bare upstream 'close' pre-headers: fails over to the next target", async () => {
  // Node happens to raise ECONNRESET on the upstream ClientRequest for a
  // pre-headers teardown, so this already worked via the 'error' path — the point
  // of pinning it is that the 'close' handler's new outcome branch must reach the
  // same verdict if that ordering ever shifts, and must not double-handle it now.
  const primary = await startKillBeforeHeadersProvider();
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy(undefined, { egressTimeoutMs: 10_000 });
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const outcome = await rawProxyPost(proxy.address().port, childBaseUrl, { model: "primary-model", messages: [] }, 1500);

    assert.equal(outcome.status, 200, "nothing had reached the client, so the retry must be invisible");
    assert.equal(outcome.body, '{"from":"fallback"}', "the client must receive the FALLBACK's body");
    assert.equal(primary.hits, 1, "the primary must have been attempted once");
    assert.equal(fallback.captured.length, 1, "the fallback must have been attempted once");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

test("normal completion is unaffected: the 'close' handler stays a no-op once the client response ended", async () => {
  // WHY: 'close' fires on EVERY upstream request, including the happy path
  // (upstreamRes 'end' → pipe end()s clientRes → then 'close'). If the handler's
  // new outcome branch misread that as an unsettled attempt it would 502 or
  // destroy a perfectly good response, or double-end it.
  const primary = await startFakeProvider({ status: 200, body: '{"from":"primary","complete":true}' });
  const fallback = await startFakeProvider({ status: 200, body: '{"from":"fallback"}' });
  const proxy = new EgressProxy();
  await proxy.start();
  try {
    const fallbacks: ProviderTarget[] = [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ];
    const { childBaseUrl } = proxy.register({
      upstreamBaseUrl: primary.baseUrl,
      realApiKey: "sk-primary",
      modelAllowlist: ["primary-model", "fallback-model"],
      tenantId: "t1",
      agentId: "a1",
      fallbacks,
    });

    const outcome = await rawProxyPost(proxy.address().port, childBaseUrl, { model: "primary-model", messages: [] }, 1500);

    assert.equal(outcome.terminated, "end", "a normal completion must end cleanly, not abort");
    assert.equal(outcome.status, 200);
    assert.equal(outcome.body, '{"from":"primary","complete":true}', "the body must arrive complete and untruncated");
    assert.equal(fallback.captured.length, 0, "a successful primary must never trigger a retry");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});
