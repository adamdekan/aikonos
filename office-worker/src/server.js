import http from 'node:http';
import {
  parseMultipart,
  buildMultipartResponse,
  DEFAULT_MAX_UPLOAD_BYTES,
  DEFAULT_MAX_UPLOAD_FILES,
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

// createServer wires the route table + per-job lifecycle. Config is read at
// call time (not module load) so tests can override env-derived defaults.
export function createServer({
  jobTimeoutMs = Number(process.env.OFFICE_JOB_TIMEOUT_MS) || 120000,
  sofficeTimeoutMs = SOFFICE_TIMEOUT_MS,
  maxConcurrency = Number(process.env.OFFICE_MAX_CONCURRENCY) || 4,
  maxUploadBytes = Number(process.env.OFFICE_MAX_UPLOAD_BYTES) || DEFAULT_MAX_UPLOAD_BYTES,
  maxUploadFiles = Number(process.env.OFFICE_MAX_UPLOAD_FILES) || DEFAULT_MAX_UPLOAD_FILES,
} = {}) {
  const semaphore = createSemaphore(maxConcurrency);
  const uploadLimits = { maxUploadBytes, maxUploadFiles };

  return http.createServer((req, res) => {
    handleRequest(req, res, { jobTimeoutMs, sofficeTimeoutMs, semaphore, uploadLimits }).catch((err) => {
      if (!res.headersSent) sendJson(res, 500, { error: err.message });
    });
  });
}

async function handleRequest(req, res, { jobTimeoutMs, sofficeTimeoutMs, semaphore, uploadLimits }) {
  const url = new URL(req.url, 'http://office-worker');

  if (req.method === 'GET' && url.pathname === '/healthz') {
    sendJson(res, 200, { ok: true });
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

  await semaphore.acquire();
  try {
    const { files, params } = await parseMultipart(req, uploadLimits);
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
  }
}
