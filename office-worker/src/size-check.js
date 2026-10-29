// Every output artifact is checked against the request's max_output_bytes cap
// before the response is built — a single failing file fails the whole job,
// never a partial multipart response.
export class OutputTooLargeError extends Error {
  constructor(filename, size, cap) {
    super(`output file "${filename}" is ${size} bytes, exceeding the ${cap}-byte cap`);
    this.name = 'OutputTooLargeError';
    this.statusCode = 413;
    this.filename = filename;
    this.size = size;
    this.cap = cap;
  }
}

export class InvalidCapError extends Error {
  constructor(maxBytes) {
    super(`max_output_bytes must be a finite positive number, got ${JSON.stringify(maxBytes)}`);
    this.name = 'InvalidCapError';
    this.statusCode = 400;
  }
}

// Applied when a caller sends no max_output_bytes (the broker always sends
// its 10 MiB workspace cap): no output is ever unbounded.
export const DEFAULT_MAX_OUTPUT_BYTES = 50 << 20;

// outputs: [{ filename, buffer, ... }]. maxBytes undefined/null means no cap
// was supplied — DEFAULT_MAX_OUTPUT_BYTES applies. Any other value must be a
// finite positive number: a non-numeric or degenerate cap (e.g. a string,
// NaN, 0, negative) must fail loud rather than silently comparing false
// forever and letting every output through uncapped.
export function checkOutputSizes(outputs, maxBytes) {
  if (maxBytes === undefined || maxBytes === null) maxBytes = DEFAULT_MAX_OUTPUT_BYTES;
  if (typeof maxBytes !== 'number' || !Number.isFinite(maxBytes) || maxBytes <= 0) {
    throw new InvalidCapError(maxBytes);
  }
  for (const output of outputs) {
    const size = output.buffer.length;
    if (size > maxBytes) {
      throw new OutputTooLargeError(output.filename, size, maxBytes);
    }
  }
}
