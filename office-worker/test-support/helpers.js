import { execFileSync } from 'node:child_process';
import http from 'node:http';
import { createServer } from '../src/server.js';

// Checkpoint 3's LibreOffice-backed ops (xlsx.recalc/pptx.thumbnail/
// office.convert) need soffice, which isn't installed on every dev host —
// their tests call this to skip cleanly instead of failing (see API.md /
// the office-doc-tools brief's host-constraint note).
let sofficeAvailable;
export function hasSoffice() {
  if (sofficeAvailable === undefined) {
    try {
      execFileSync(process.env.SOFFICE_BIN || 'soffice', ['--version'], { stdio: 'ignore' });
      sofficeAvailable = true;
    } catch {
      sofficeAvailable = false;
    }
  }
  return sofficeAvailable;
}

// Starts a server on an ephemeral port and returns { baseUrl, close }.
export async function startServer(opts) {
  const server = createServer(opts);
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return {
    baseUrl: `http://127.0.0.1:${port}`,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

// Builds a multipart/form-data request body from { files: [{name, filename,
// contentType, buffer}], params }. Mirrors src/multipart.js's response
// builder so tests exercise the same shape the wire contract documents.
// extraFields adds plain form fields beyond "params" — only the field-count
// cap has a reason to send any.
export function buildMultipartRequest(files, params, extraFields = {}) {
  const boundary = 'test-boundary-1234';
  const parts = [];
  for (const file of files) {
    parts.push(
      Buffer.from(
        `--${boundary}\r\n` +
          `Content-Disposition: form-data; name="${file.name}"; filename="${file.filename}"\r\n` +
          `Content-Type: ${file.contentType || 'application/octet-stream'}\r\n\r\n`
      )
    );
    parts.push(file.buffer);
    parts.push(Buffer.from('\r\n'));
  }
  parts.push(
    Buffer.from(
      `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="params"\r\n\r\n' +
        JSON.stringify(params ?? {}) +
        '\r\n'
    )
  );
  for (const [name, value] of Object.entries(extraFields)) {
    parts.push(
      Buffer.from(
        `--${boundary}\r\n` +
          `Content-Disposition: form-data; name="${name}"\r\n\r\n` +
          value +
          '\r\n'
      )
    );
  }
  parts.push(Buffer.from(`--${boundary}--\r\n`));
  return { contentType: `multipart/form-data; boundary=${boundary}`, body: Buffer.concat(parts) };
}

// Parses a multipart/form-data response body the way a client would, well
// enough for round-trip assertions (not a general-purpose parser).
export function parseMultipartResponse(contentType, body) {
  const boundaryMatch = contentType.match(/boundary=([^;]+)/);
  const boundary = `--${boundaryMatch[1]}`;
  const raw = body.toString('latin1');
  const chunks = raw.split(boundary).slice(1, -1); // drop preamble + trailing "--"
  const parts = [];
  for (const chunk of chunks) {
    const trimmed = chunk.replace(/^\r\n/, '').replace(/\r\n$/, '');
    const headerEnd = trimmed.indexOf('\r\n\r\n');
    const headerBlock = trimmed.slice(0, headerEnd);
    const dataStr = trimmed.slice(headerEnd + 4);
    const nameMatch = headerBlock.match(/name="([^"]+)"/);
    const filenameMatch = headerBlock.match(/filename="([^"]+)"/);
    parts.push({
      name: nameMatch ? nameMatch[1] : undefined,
      filename: filenameMatch ? filenameMatch[1] : undefined,
      buffer: Buffer.from(dataStr, 'latin1'),
    });
  }
  return parts;
}

export function post(baseUrl, path, contentType, body) {
  return new Promise((resolve, reject) => {
    const req = http.request(
      new URL(path, baseUrl),
      { method: 'POST', headers: { 'Content-Type': contentType, 'Content-Length': body.length } },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () =>
          resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) })
        );
      }
    );
    req.on('error', reject);
    req.end(body);
  });
}

export function get(baseUrl, path) {
  return new Promise((resolve, reject) => {
    http
      .get(new URL(path, baseUrl), (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () =>
          resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) })
        );
      })
      .on('error', reject);
  });
}
