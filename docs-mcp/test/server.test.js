/**
 * server.test.js — CP2 integration tests for the MCP HTTP server.
 *
 * Mounts createApp on :0 (ephemeral port) and drives it with fetch,
 * mirroring Aikonos's broker client wire format:
 *   POST /mcp, Accept: application/json, text/event-stream, body = JSON-RPC 2.0.
 *
 * The SDK in stateless mode may return either a direct JSON body or an SSE
 * text/event-stream with a `data:` line. parseMcpResponse handles both.
 *
 * Stateless mode: each POST is independent — no initialize handshake required.
 * If the SDK rejects a bare tools/call without prior initialize, we document
 * that and use a minimal two-POST pattern instead (see NOTE below).
 */

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { createApp } from '../src/server.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const FIXTURE_ROOT = join(__dirname, 'fixtures', 'corpus');

// ---------------------------------------------------------------------------
// Test server lifecycle
// ---------------------------------------------------------------------------

let baseUrl;
let httpServer;

before(async () => {
  const app = createApp({ corpusRoot: FIXTURE_ROOT });
  httpServer = createServer(app);
  await new Promise((resolve) => httpServer.listen(0, '127.0.0.1', resolve));
  const { port } = httpServer.address();
  baseUrl = `http://127.0.0.1:${port}`;
});

after(async () => {
  await new Promise((resolve, reject) =>
    httpServer.close((err) => (err ? reject(err) : resolve())),
  );
});

// ---------------------------------------------------------------------------
// Wire helper — handles both direct JSON and SSE data: responses.
// ---------------------------------------------------------------------------

async function mcpPost(body) {
  const res = await fetch(`${baseUrl}/mcp`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
    },
    body: JSON.stringify(body),
  });

  const contentType = res.headers.get('content-type') ?? '';

  if (contentType.includes('text/event-stream')) {
    // SSE: read all lines, extract the last non-empty `data:` line.
    const text = await res.text();
    const dataLines = text
      .split('\n')
      .filter((l) => l.startsWith('data:'))
      .map((l) => l.slice('data:'.length).trim())
      .filter(Boolean);
    if (dataLines.length === 0) throw new Error('SSE response had no data: lines');
    return JSON.parse(dataLines[dataLines.length - 1]);
  }

  // Direct JSON body.
  return res.json();
}

// Build a JSON-RPC 2.0 request object.
function rpc(method, params, id = 1) {
  return { jsonrpc: '2.0', id, method, params };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('GET /healthz returns 200 {"status":"ok"}', async () => {
  const res = await fetch(`${baseUrl}/healthz`);
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.equal(body.status, 'ok');
});

test('initialize returns protocolVersion and serverInfo', async () => {
  const resp = await mcpPost(
    rpc('initialize', {
      protocolVersion: '2024-11-05',
      capabilities: {},
      clientInfo: { name: 'test', version: '0.0.1' },
    }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.ok(resp.result.protocolVersion, 'missing protocolVersion');
  assert.ok(resp.result.serverInfo, 'missing serverInfo');
  assert.equal(resp.result.serverInfo.name, 'aikonos-docs');
});

test('tools/list returns exactly 3 tools with descriptions and readOnlyHint', async () => {
  const resp = await mcpPost(rpc('tools/list', {}));
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  const tools = resp.result.tools;
  assert.equal(tools.length, 3, `expected 3 tools, got ${tools.length}`);

  const names = new Set(tools.map((t) => t.name));
  assert.ok(names.has('search_docs'), 'missing search_docs');
  assert.ok(names.has('list_docs'), 'missing list_docs');
  assert.ok(names.has('read_doc'), 'missing read_doc');

  for (const tool of tools) {
    assert.ok(tool.description && tool.description.length > 0, `tool ${tool.name} has no description`);
    assert.ok(
      tool.annotations?.readOnlyHint === true,
      `tool ${tool.name} missing readOnlyHint`,
    );
  }
});

test('tools/call search_docs finds fixture content', async () => {
  // "Aikonos" appears many times in intro.md — should appear in results.
  const resp = await mcpPost(
    rpc('tools/call', { name: 'search_docs', arguments: { query: 'Aikonos' } }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.ok(!resp.result.isError, 'unexpected isError');
  const text = resp.result.content[0].text;
  assert.ok(text.includes('intro.md'), `expected intro.md in results, got: ${text}`);
});

test('tools/call search_docs returns non-error "no results" for unmatched query', async () => {
  const resp = await mcpPost(
    rpc('tools/call', {
      name: 'search_docs',
      arguments: { query: 'xyzzy_no_match_at_all_9999' },
    }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.ok(!resp.result.isError, 'no-match should not be isError');
  const text = resp.result.content[0].text.toLowerCase();
  assert.ok(
    text.includes('no result') || text.includes('no match') || text.includes('not found'),
    `expected "no results" message, got: ${text}`,
  );
});

test('tools/call read_doc returns content for a valid fixture path', async () => {
  const resp = await mcpPost(
    rpc('tools/call', { name: 'read_doc', arguments: { path: 'intro.md' } }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.ok(!resp.result.isError, 'unexpected isError');
  const text = resp.result.content[0].text;
  assert.ok(text.includes('Aikonos'), 'expected corpus content in response');
});

test('tools/call read_doc returns content for a nested fixture path', async () => {
  const resp = await mcpPost(
    rpc('tools/call', { name: 'read_doc', arguments: { path: 'subdir/nested.md' } }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.ok(!resp.result.isError, 'unexpected isError');
  const text = resp.result.content[0].text;
  assert.ok(text.includes('Nested'), 'expected nested doc content');
});

test('tools/call read_doc with ../escape returns isError with no host path leak', async () => {
  const resp = await mcpPost(
    rpc('tools/call', { name: 'read_doc', arguments: { path: '../escape.md' } }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.equal(resp.result.isError, true, 'expected isError for path escape');
  // Safe message must not expose the real absolute fixture path.
  const text = resp.result.content[0].text;
  assert.ok(
    !text.includes(FIXTURE_ROOT),
    `host path leaked in error message: ${text}`,
  );
});

test('tools/call read_doc with absolute path returns isError with no host path leak', async () => {
  const resp = await mcpPost(
    rpc('tools/call', { name: 'read_doc', arguments: { path: '/etc/passwd' } }),
  );
  assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
  assert.equal(resp.result.isError, true, 'expected isError for absolute path');
  const text = resp.result.content[0].text;
  assert.ok(!text.includes(FIXTURE_ROOT), `host path leaked: ${text}`);
});

// ---------------------------------------------------------------------------
// Non-CorpusError path: corpusRoot that does not exist causes realRoot() to
// throw a plain Error (not CorpusError), exercising the generic catch branch.
// ---------------------------------------------------------------------------

test('tools/call read_doc with non-existent corpusRoot returns isError with generic message', async () => {
  // Mount a fresh app against a root that cannot be resolved.
  const badRoot = join(__dirname, 'fixtures', '__does_not_exist__');
  const badApp = createApp({ corpusRoot: badRoot });
  const badServer = createServer(badApp);
  await new Promise((resolve) => badServer.listen(0, '127.0.0.1', resolve));
  const { port } = badServer.address();
  const badBase = `http://127.0.0.1:${port}`;

  try {
    const res = await fetch(`${badBase}/mcp`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
      body: JSON.stringify(rpc('tools/call', { name: 'read_doc', arguments: { path: 'intro.md' } })),
    });
    const contentType = res.headers.get('content-type') ?? '';
    let resp;
    if (contentType.includes('text/event-stream')) {
      const text = await res.text();
      const dataLines = text.split('\n').filter(l => l.startsWith('data:')).map(l => l.slice('data:'.length).trim()).filter(Boolean);
      resp = JSON.parse(dataLines[dataLines.length - 1]);
    } else {
      resp = await res.json();
    }
    assert.ok(resp.result, `expected result, got: ${JSON.stringify(resp)}`);
    assert.equal(resp.result.isError, true, 'expected isError for non-existent root');
    // Generic branch: message must be the fallback, not a CorpusError message.
    assert.equal(resp.result.content[0].text, 'Document could not be read.');
    // Must not leak the bad root path.
    assert.ok(!resp.result.content[0].text.includes(badRoot), 'host path leaked in generic error');
  } finally {
    await new Promise((resolve, reject) => badServer.close((err) => (err ? reject(err) : resolve())));
  }
});

// ---------------------------------------------------------------------------
// Wire-compat invariant 3: bare tools/call with no prior initialize and no
// session id must return a successful result (stateless mode contract).
// ---------------------------------------------------------------------------

test('wire-compat: bare tools/call list_docs without initialize returns successful result', async () => {
  // No initialize. No Mcp-Session-Id header. This must succeed — the SDK in
  // stateless mode (sessionIdGenerator: undefined) must not require a handshake.
  const res = await fetch(`${baseUrl}/mcp`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      // Deliberately omit Mcp-Session-Id.
    },
    body: JSON.stringify(rpc('tools/call', { name: 'list_docs', arguments: {} })),
  });
  const contentType = res.headers.get('content-type') ?? '';
  let resp;
  if (contentType.includes('text/event-stream')) {
    const text = await res.text();
    const dataLines = text.split('\n').filter(l => l.startsWith('data:')).map(l => l.slice('data:'.length).trim()).filter(Boolean);
    if (dataLines.length === 0) throw new Error('SSE response had no data: lines');
    resp = JSON.parse(dataLines[dataLines.length - 1]);
  } else {
    resp = await res.json();
  }
  // Must be a successful result, not a protocol error or session-required error.
  assert.ok(resp.result, `stateless tools/call failed — SDK may now require initialize: ${JSON.stringify(resp)}`);
  assert.ok(!resp.result.isError, `unexpected isError: ${JSON.stringify(resp.result)}`);
});
