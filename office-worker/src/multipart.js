// Wire contract (see API.md): requests are multipart/form-data with file
// parts carrying document bytes and a "params" field carrying JSON op params.
// Responses are multipart/form-data with output file parts plus a "result"
// JSON part. Node has no core multipart writer, so the response side is
// hand-rolled — it's a handful of Buffer.concat calls, not worth a dependency.
import Busboy from 'busboy';
import { randomBytes } from 'node:crypto';

export class InvalidParamsError extends Error {
  constructor(message) {
    super(message);
    this.name = 'InvalidParamsError';
    this.statusCode = 400;
  }
}

// An ingress cap was exceeded. Mirrors size-check.js's OutputTooLargeError on
// the response side: 413, and the whole job fails — never a partial result.
export class RequestTooLargeError extends Error {
  constructor(message) {
    super(message);
    this.name = 'RequestTooLargeError';
    this.statusCode = 413;
  }
}

// The broker refuses to send an input file larger than workspacefs.MaxFileBytes
// (10 MiB, broker/internal/workspacefs/store.go), so this must sit at or above
// that or we'd reject a request the broker already accepted. The +1 MiB of
// slack mirrors office_client.go's officeMaxResponseBytes on the response side.
export const DEFAULT_MAX_UPLOAD_BYTES = (10 << 20) + (1 << 20);

// Every pdf.transform op (merge/rotate/decrypt/fill-forms/split) sends one
// file part per caller-supplied `paths` entry with no bound of its own
// (office_pdf.go) — merge is merely the one with a reason to send several.
// 16 covers any real call while keeping worst-case buffered ingress (files are
// read whole into memory) far under the container's 2g mem_limit even at full
// concurrency.
export const DEFAULT_MAX_UPLOAD_FILES = 16;

// Only "params" is ever read, but it carries a model-authored script for the
// *.create/*.edit ops. 2 MiB is well above any real script and above busboy's
// own 1 MiB default, which until now truncated a larger one into invalid JSON.
export const MAX_FIELD_BYTES = 2 << 20;
export const MAX_FIELDS = 8;

// Parses an incoming multipart request. Resolves { files, params } where
// files is [{ fieldname, filename, contentType, buffer }] and params is the
// JSON-parsed "params" form field (or {} if absent).
//
// Every cap rejects rather than truncates. Busboy's default on hitting a limit
// is to silently drop the excess, and a truncated document that parses into a
// plausible-looking wrong result is worse than an error.
export function parseMultipart(req, limits = {}) {
  const maxUploadBytes = limits.maxUploadBytes ?? DEFAULT_MAX_UPLOAD_BYTES;
  const maxUploadFiles = limits.maxUploadFiles ?? DEFAULT_MAX_UPLOAD_FILES;

  return new Promise((resolve, reject) => {
    let bb;
    try {
      bb = Busboy({
        headers: req.headers,
        limits: {
          // busboy flags a size limit at === the limit (fileSize fires
          // 'limit', fieldSize sets valueTruncated) with zero bytes actually
          // dropped, so both options are the first *rejected* size, not the
          // last accepted one. +1 makes each cap mean what API.md documents —
          // load-bearing if an operator ever sets OFFICE_MAX_UPLOAD_BYTES to
          // exactly the broker's cap. (The two count limits need no such
          // adjustment: busboy tests those before incrementing, so files: N /
          // fields: N accept exactly N.)
          fileSize: maxUploadBytes + 1,
          files: maxUploadFiles,
          fields: MAX_FIELDS,
          fieldSize: MAX_FIELD_BYTES + 1,
        },
      });
    } catch (err) {
      reject(new InvalidParamsError(`invalid multipart request: ${err.message}`));
      return;
    }

    const files = [];
    let paramsRaw;
    let settled = false;

    function fail(err) {
      if (settled) return;
      settled = true;
      req.unpipe(bb);
      reject(err);
    }

    bb.on('file', (fieldname, stream, info) => {
      const chunks = [];
      stream.on('limit', () =>
        fail(
          new RequestTooLargeError(
            `file part "${info.filename}" exceeds the ${maxUploadBytes}-byte upload cap`
          )
        )
      );
      stream.on('data', (chunk) => chunks.push(chunk));
      stream.on('end', () => {
        files.push({
          fieldname,
          filename: info.filename,
          contentType: info.mimeType,
          buffer: Buffer.concat(chunks),
        });
      });
    });

    bb.on('field', (fieldname, value, info) => {
      // valueTruncated only — busboy's multipart parser hardcodes
      // nameTruncated: false (only its urlencoded parser ever sets it).
      if (info?.valueTruncated) {
        fail(
          new RequestTooLargeError(
            `form field "${fieldname}" exceeds the ${MAX_FIELD_BYTES}-byte field cap`
          )
        );
        return;
      }
      if (fieldname === 'params') paramsRaw = value;
    });

    bb.on('filesLimit', () =>
      fail(new RequestTooLargeError(`request exceeds the ${maxUploadFiles}-file part cap`))
    );
    bb.on('fieldsLimit', () =>
      fail(new RequestTooLargeError(`request exceeds the ${MAX_FIELDS}-form field cap`))
    );

    bb.on('close', () => {
      if (settled) return;
      settled = true;
      let params = {};
      if (paramsRaw) {
        try {
          params = JSON.parse(paramsRaw);
        } catch (err) {
          reject(new InvalidParamsError(`invalid params JSON: ${err.message}`));
          return;
        }
      }
      resolve({ files, params });
    });

    bb.on('error', fail);
    req.on('error', fail);
    req.pipe(bb);
  });
}

// Header field-values ride inside a quoted-string on one header line; a
// handler-supplied name/filename is untrusted (echoed/derived from request
// data) and must not be allowed to close the quote early or inject a CRLF
// (which would start a new header line / smuggle a part). Percent-encode the
// quote, strip CR/LF outright — cheaper than round-tripping through a real
// RFC 2231 encoder for values that never need to display verbatim anyway.
function sanitizeDispositionValue(value) {
  return String(value).replace(/[\r\n"]/g, (ch) => (ch === '"' ? '%22' : ''));
}

// Builds a multipart/form-data response body. outputs: [{ name, filename,
// contentType, buffer }]. result: JSON-serializable object, sent as the final
// "result" part.
export function buildMultipartResponse(outputs, result) {
  const boundary = `office-${randomBytes(16).toString('hex')}`;
  const parts = [];

  for (const output of outputs) {
    const name = sanitizeDispositionValue(output.name);
    const filename = sanitizeDispositionValue(output.filename);
    parts.push(
      Buffer.from(
        `--${boundary}\r\n` +
          `Content-Disposition: form-data; name="${name}"; filename="${filename}"\r\n` +
          `Content-Type: ${output.contentType || 'application/octet-stream'}\r\n\r\n`
      )
    );
    parts.push(output.buffer);
    parts.push(Buffer.from('\r\n'));
  }

  parts.push(
    Buffer.from(
      `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="result"\r\n' +
        'Content-Type: application/json\r\n\r\n' +
        JSON.stringify(result ?? {}) +
        '\r\n'
    )
  );
  parts.push(Buffer.from(`--${boundary}--\r\n`));

  return { contentType: `multipart/form-data; boundary=${boundary}`, body: Buffer.concat(parts) };
}
