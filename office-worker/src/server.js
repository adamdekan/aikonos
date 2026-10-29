import http from 'node:http';
import { createHash, timingSafeEqual } from 'node:crypto';
import {
  parseMultipart,
  buildMultipartResponse,
  DEFAULT_MAX_UPLOAD_BYTES,
  DEFAULT_MAX_UPLOAD_FILES,
  MAX_FIELDS,
  MAX_FIELD_BYTES,
} from './multipart.js';
import { KNOWN_OPS, HANDLERS, SOFFICE_OPS } from './ops.js';
import { createSemaphore } from './semaphore.js';
import { withJobDir, createJobRunner } from './job.js';
import { checkOutputSizes } from './size-check.js';

const ROUTE_RE = /^\/v1\/([^/]+)\/([^/]+)$/;

function sendJson(res, status, body) {
  const buf = Buffer.from(JSON.stringify(body));
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': buf.length });
  res.end(buf);
}

// soffice ops (checkpoint 3: xlsx.recalc, pptx.thumbnail, office.convert) get
// a longer default timeout than the OFFICE_JOB_TIMEOUT_MS knob controls.
// Not its own env var — the design's knob list names only OFFICE_JOB_TIMEOUT_MS/OFFICE_MAX_CONCURRENCY/the broker's
// OFFICE_WORKER_URL, so a 4th env var here would drift from check-env-drift.sh.
const SOFFICE_TIMEOUT_MS = 180000;

// The request body is read in full before a job slot is taken, so a client
// that drips (or never sends) its body cannot hold a slot; it gets this long
// to deliver it. Same not-an-env-var reasoning as SOFFICE_TIMEOUT_MS.
const BODY_TIMEOUT_MS = 30000;

// Request bodies are held in memory from parsing until their job ends, and
// they are parsed before a job slot is taken, so their total is bounded on its
// own: 16 files x 11 MiB per request times every open connection is past the
// container's 2g mem_limit. Same not-an-env-var reasoning as SOFFICE_TIMEOUT_MS.
const MAX_BUFFERED_BYTES = 512 << 20;

// Shared secret the broker sends (X-Aikonos-Office-Token). The worker runs
// model-authored scripts, so the internal `office` network is not the only
// thing standing between a co-located container and code execution here.
export const TOKEN_HEADER = 'x-aikonos-office-token';
export const TOKEN_ENV = 'AIKONOS_OFFICE_WORKER_TOKEN';

// Fails closed: a worker without a token would accept every caller on its
// network. AIKONOS_DEV_MODE=true is the explicit escape hatch (local dev,
// tests), matching the broker's own devModeGuard.
export function assertTokenConfigured(env = process.env) {
  if (!env[TOKEN_ENV] && env.AIKONOS_DEV_MODE !== 'true') {
    throw new Error(
      `refusing to start: ${TOKEN_ENV} is unset and AIKONOS_DEV_MODE!=true; ` +
        `set the shared secret (the broker sends the same value) or AIKONOS_DEV_MODE=true (dev only)`
    );
  }
}

// Hashing both sides first gives timingSafeEqual equal-length inputs, so the
// comparison leaks neither the token's bytes nor its length.
function tokenMatches(presented, expected) {
  if (typeof presented !== 'string') return false;
  const a = createHash('sha256').update(presented).digest();
  const b = createHash('sha256').update(expected).digest();
  return timingSafeEqual(a, b);
}

class BodyTimeoutError extends Error {
  constructor(ms) {
    super(`request body not received within ${ms}ms`);
    this.name = 'BodyTimeoutError';
    this.statusCode = 408;
  }
}

function parseWithTimeout(req, uploadLimits, ms) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => {
      req.destroy();
      reject(new BodyTimeoutError(ms));
    }, ms);
  });
  return Promise.race([parseMultipart(req, uploadLimits), timeout]).finally(() => clearTimeout(timer));
}

// createServer wires the route table + per-job lifecycle. Config is read at
// call time (not module load) so tests can override env-derived defaults.
export function createServer({
  jobTimeoutMs = Number(process.env.OFFICE_JOB_TIMEOUT_MS) || 120000,
  sofficeTimeoutMs = SOFFICE_TIMEOUT_MS,
  maxConcurrency = Number(process.env.OFFICE_MAX_CONCURRENCY) || 4,
  maxUploadBytes = Number(process.env.OFFICE_MAX_UPLOAD_BYTES) || DEFAULT_MAX_UPLOAD_BYTES,
  maxUploadFiles = Number(process.env.OFFICE_MAX_UPLOAD_FILES) || DEFAULT_MAX_UPLOAD_FILES,
  // ponytail: 2x concurrency waiting is already past the broker's own
  // client timeout at the default job timeout; a deeper queue only holds
  // memory for requests nobody is still waiting on.
  maxQueue = maxConcurrency * 2,
  bodyTimeoutMs = BODY_TIMEOUT_MS,
  maxBufferedBytes = MAX_BUFFERED_BYTES,
  token = process.env[TOKEN_ENV] || '',
} = {}) {
  const semaphore = createSemaphore(maxConcurrency, maxQueue);
  const uploadLimits = { maxUploadBytes, maxUploadFiles };
  // ponytail: charged by declared size, not bytes read — node never reads a
  // body past its Content-Length, and the caps reject anything over the most
  // one request can carry, which is what a chunked body is charged.
  const perRequest = maxUploadFiles * maxUploadBytes + MAX_FIELDS * MAX_FIELD_BYTES;
  const buffered = { bytes: 0, max: maxBufferedBytes, perRequest };
  const ctx = { jobTimeoutMs, sofficeTimeoutMs, semaphore, buffered, uploadLimits, bodyTimeoutMs, token };

  const server = http.createServer((req, res) => {
    handleRequest(req, res, ctx).catch((err) => {
      if (!res.headersSent) sendJson(res, 500, { error: err.message });
    });
  });
  // Bodies are now buffered before a slot is taken, so the connection count is
  // what bounds that buffering: running + queued jobs, plus room for
  // /healthz.
  server.maxConnections = maxConcurrency + maxQueue + 2;
  // requestTimeout covers receiving the request only (a long job's response
  // is not cut off); headersTimeout must not exceed it.
  server.requestTimeout = bodyTimeoutMs * 2;
  server.headersTimeout = Math.min(10000, server.requestTimeout);
  return server;
}

async function handleRequest(req, res, ctx) {
  const { jobTimeoutMs, sofficeTimeoutMs, semaphore, buffered, uploadLimits, bodyTimeoutMs, token } = ctx;
  const url = new URL(req.url, 'http://office-worker');

  // Unauthenticated: the compose healthcheck probes it without the secret.
  if (req.method === 'GET' && url.pathname === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  // Before routing, parsing or a slot: an unauthenticated caller learns
  // nothing and costs nothing.
  if (token && !tokenMatches(req.headers[TOKEN_HEADER], token)) {
    console.warn(
      `office-worker: denied ${req.method} ${url.pathname} from ${req.socket.remoteAddress}: missing or wrong ${TOKEN_HEADER}`
    );
    sendJson(res, 401, { error: 'unauthorized' });
    return;
  }

  const match = url.pathname.match(ROUTE_RE);
  if (req.method !== 'POST' || !match) {
    sendJson(res, 404, { error: `no route for ${req.method} ${url.pathname}` });
    return;
  }

  const [, format, op] = match;
  const opKey = `${format}/${op}`;
  if (!KNOWN_OPS.has(opKey)) {
    sendJson(res, 404, { error: `unknown operation "${opKey}"` });
    return;
  }

  const handler = HANDLERS[opKey];
  if (!handler) {
    sendJson(res, 501, { error: `"${opKey}" is not implemented yet` });
    return;
  }

  const declared = req.headers['content-length'];
  const charge = Math.min(declared === undefined ? Infinity : Number(declared), buffered.perRequest);
  if (buffered.bytes + charge > buffered.max) {
    console.warn(`office-worker: refused ${opKey}: ${buffered.bytes} request bytes already buffered`);
    sendJson(res, 503, { error: 'office-worker is busy (request memory budget in use); retry later' });
    return;
  }
  buffered.bytes += charge;

  let parsed;
  try {
    parsed = await parseWithTimeout(req, uploadLimits, bodyTimeoutMs);
    await semaphore.acquire();
  } catch (err) {
    buffered.bytes -= charge;
    sendJson(res, err.statusCode || 500, { error: err.message });
    return;
  }
  try {
    const { files, params } = parsed;
    const timeoutMs = SOFFICE_OPS.has(opKey) ? sofficeTimeoutMs : jobTimeoutMs;
    const runner = createJobRunner({ timeoutMs });

    const result = await withJobDir((jobDir) =>
      runner.run(({ spawn }) => handler({ jobDir, files, params, spawn }))
    );

    const outputs = result.outputs || [];
    checkOutputSizes(outputs, params.max_output_bytes);

    const { contentType, body } = buildMultipartResponse(outputs, result.result ?? {});
    res.writeHead(200, { 'Content-Type': contentType, 'Content-Length': body.length });
    res.end(body);
  } catch (err) {
    const status = err.statusCode || 500;
    sendJson(res, status, { error: err.message });
  } finally {
    semaphore.release();
    buffered.bytes -= charge;
  }
}
