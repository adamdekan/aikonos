// Per-run LLM-call budget (EgressProxy property 9).
//
// WHY these tests exist: nothing else in aikonos caps how many LLM calls one run
// may make. The child's Pi loop is LLM→tool→LLM with no iteration ceiling, so a
// model stuck re-trying a flaky tool result bills for as long as the client stays
// connected. The counter has to live in the parent (the child is untrusted), be
// charged once per LOGICAL request (not once per failover attempt), reset when a
// pooled child starts a new run, and be shared with the bridge-direct parent-side
// LLM calls that bypass the proxy entirely.
import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { EgressProxy, type ProviderTarget } from "../src/llm/egress-proxy.js";

interface FakeProvider {
  baseUrl: string;
  hits: number;
  status: number;
  close(): Promise<void>;
}

async function startFakeProvider(opts: { status?: number } = {}): Promise<FakeProvider> {
  let hits = 0;
  let status = opts.status ?? 200;
  const server = http.createServer((req, res) => {
    req.on("data", () => {});
    req.on("end", () => {
      hits++;
      res.writeHead(status, { "content-type": "application/json" });
      res.end('{"ok":true}');
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object", "fake provider must bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    get hits() { return hits; },
    get status() { return status; },
    set status(v) { status = v; },
    close: () => new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    ),
  };
}

async function proxyPost(childBaseUrl: string): Promise<{ status: number; body: string }> {
  const url = new URL("chat/completions", childBaseUrl.endsWith("/") ? childBaseUrl : childBaseUrl + "/");
  const res = await fetch(url.toString(), {
    method: "POST",
    headers: { "content-type": "application/json", authorization: "Bearer dummy-child-key" },
    body: JSON.stringify({ model: "m", messages: [] }),
  });
  return { status: res.status, body: await res.text() };
}

function registerChild(proxy: EgressProxy, upstreamBaseUrl: string, fallbacks: ProviderTarget[] = []) {
  return proxy.register({
    upstreamBaseUrl,
    realApiKey: "sk-primary",
    modelAllowlist: ["m", "fallback-model"],
    tenantId: "t1",
    agentId: "a1",
    fallbacks,
  });
}

test("budget: calls past maxLlmCallsPerRun are 429'd and never reach the upstream", async () => {
  const provider = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 3 });
  await proxy.start();
  try {
    const { childBaseUrl } = registerChild(proxy, provider.baseUrl);

    for (let i = 1; i <= 3; i++) {
      const { status } = await proxyPost(childBaseUrl);
      assert.equal(status, 200, `call ${i} is within budget and must be forwarded`);
    }

    const denied = await proxyPost(childBaseUrl);
    assert.equal(denied.status, 429, "the call past the budget must be denied");
    assert.match(denied.body, /llm call budget exceeded for this run/);
    assert.equal(provider.hits, 3, "an over-budget call must not be billed upstream at all");
  } finally {
    await proxy.stop();
    await provider.close();
  }
});

test("budget: the denial is persistent — a looping child cannot wear the counter down", async () => {
  // WHY: this is the actual cost-leak property. A one-shot denial that then let
  // calls through again would leave the unbounded loop unbounded.
  const provider = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 1 });
  await proxy.start();
  try {
    const { childBaseUrl } = registerChild(proxy, provider.baseUrl);
    assert.equal((await proxyPost(childBaseUrl)).status, 200);

    for (let i = 0; i < 5; i++) {
      assert.equal((await proxyPost(childBaseUrl)).status, 429, "every subsequent call stays denied");
    }
    assert.equal(provider.hits, 1, "no upstream request may follow the first once the budget is spent");
  } finally {
    await proxy.stop();
    await provider.close();
  }
});

test("budget: an over-budget call does NOT fail over — every target is equally over-budget", async () => {
  // WHY: 429 is normally a failover trigger. A budget 429 must not become a
  // reason to bill the fallback, since the budget is chain-wide, not per-provider.
  const primary = await startFakeProvider();
  const fallback = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 1 });
  await proxy.start();
  try {
    const { childBaseUrl } = registerChild(proxy, primary.baseUrl, [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ]);
    assert.equal((await proxyPost(childBaseUrl)).status, 200);

    const denied = await proxyPost(childBaseUrl);
    assert.equal(denied.status, 429);
    assert.equal(fallback.hits, 0, "a budget denial must never spill onto the fallback");
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

test("budget: a failover chain costs ONE unit, not one per attempt", async () => {
  // WHY: failover multiplication is already bounded (≤3 targets), so charging per
  // attempt would silently divide the configured budget by up to three.
  const primary = await startFakeProvider({ status: 500 });
  const fallback = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 2 });
  await proxy.start();
  try {
    const { childBaseUrl } = registerChild(proxy, primary.baseUrl, [
      { upstreamBaseUrl: fallback.baseUrl, apiKey: "sk-fallback", modelId: "fallback-model" },
    ]);

    // Each logical request burns two upstream attempts (primary 500 → fallback).
    assert.equal((await proxyPost(childBaseUrl)).status, 200);
    assert.equal((await proxyPost(childBaseUrl)).status, 200);
    assert.equal(primary.hits + fallback.hits, 4, "four upstream attempts for two logical requests");

    assert.equal(
      (await proxyPost(childBaseUrl)).status,
      429,
      "the third LOGICAL request is over a budget of 2, regardless of attempt count",
    );
  } finally {
    await proxy.stop();
    await primary.close();
    await fallback.close();
  }
});

test("budget: resetRunBudget lets a pooled child serve a fresh run with a full budget", async () => {
  // WHY: children are pooled and reused across runs (ChildSupervisor.run calls
  // this), so without the reset the second run would start already spent.
  const provider = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 1 });
  await proxy.start();
  try {
    const { childToken, childBaseUrl } = registerChild(proxy, provider.baseUrl);
    assert.equal((await proxyPost(childBaseUrl)).status, 200);
    assert.equal((await proxyPost(childBaseUrl)).status, 429, "budget spent within this run");

    proxy.resetRunBudget(childToken);

    assert.equal((await proxyPost(childBaseUrl)).status, 200, "a new run starts with a full budget");
    assert.equal((await proxyPost(childBaseUrl)).status, 429, "and is bounded again in its own right");
  } finally {
    await proxy.stop();
    await provider.close();
  }
});

test("budget: bridge-direct calls (consumeLlmBudget) share the child's counter", async () => {
  // WHY: GovernanceBridge.reason/analyzeImage never touch this proxy but bill the
  // same run. A separate counter per call site would be an unbounded side channel
  // around the cap.
  const provider = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 3 });
  await proxy.start();
  try {
    const { childToken, childBaseUrl } = registerChild(proxy, provider.baseUrl);

    assert.equal(proxy.consumeLlmBudget(childToken), true, "unit 1 — bridge-direct");
    assert.equal(proxy.consumeLlmBudget(childToken), true, "unit 2 — bridge-direct");
    assert.equal((await proxyPost(childBaseUrl)).status, 200, "unit 3 — proxied child call");

    assert.equal(
      proxy.consumeLlmBudget(childToken),
      false,
      "a bridge-direct call must be denied once the proxied calls have spent the shared budget",
    );
    assert.equal((await proxyPost(childBaseUrl)).status, 429, "and so must the next proxied call");
    assert.equal(provider.hits, 1, "only the single in-budget proxied call reached upstream");
  } finally {
    await proxy.stop();
    await provider.close();
  }
});

test("budget: maxLlmCallsPerRun=0 disables the cap", async () => {
  const provider = await startFakeProvider();
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 0 });
  await proxy.start();
  try {
    const { childToken, childBaseUrl } = registerChild(proxy, provider.baseUrl);
    for (let i = 0; i < 5; i++) {
      assert.equal((await proxyPost(childBaseUrl)).status, 200);
    }
    assert.equal(proxy.consumeLlmBudget(childToken), true, "the bridge seam is disabled too, not just the proxy path");
    assert.equal(provider.hits, 5);
  } finally {
    await proxy.stop();
    await provider.close();
  }
});

test("budget: an unknown child token is not tracked and is never spuriously denied", async () => {
  // A token whose child was already evicted has no run to bill against; inventing
  // a denial there would break a call the pre-budget code let through.
  const proxy = new EgressProxy(undefined, { maxLlmCallsPerRun: 1 });
  await proxy.start();
  try {
    assert.equal(proxy.consumeLlmBudget("no-such-token"), true);
    assert.equal(proxy.consumeLlmBudget("no-such-token"), true);
    proxy.resetRunBudget("no-such-token"); // must not throw
  } finally {
    await proxy.stop();
  }
});
