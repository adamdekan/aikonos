import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  checkOutputSizes,
  OutputTooLargeError,
  DEFAULT_MAX_OUTPUT_BYTES,
  MAX_TOTAL_OUTPUT_BYTES,
} from '../src/size-check.js';

test('checkOutputSizes passes when every output is under the cap', () => {
  assert.doesNotThrow(() =>
    checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(10) }], 100)
  );
});

// F82 (M-32): an absent cap used to mean "no cap"; the server default now
// applies so no output is ever unbounded.
test('checkOutputSizes applies the server default when no cap is supplied', () => {
  assert.doesNotThrow(() =>
    checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(1_000_000) }], undefined)
  );
  assert.throws(
    () => checkOutputSizes([{ filename: 'big.docx', buffer: Buffer.alloc(DEFAULT_MAX_OUTPUT_BYTES + 1) }], undefined),
    OutputTooLargeError
  );
});

test('checkOutputSizes throws OutputTooLargeError naming file, size, and cap', () => {
  assert.throws(
    () => checkOutputSizes([{ filename: 'report.docx', buffer: Buffer.alloc(200) }], 100),
    (err) => {
      assert.ok(err instanceof OutputTooLargeError);
      assert.equal(err.filename, 'report.docx');
      assert.equal(err.size, 200);
      assert.equal(err.cap, 100);
      assert.match(err.message, /report\.docx/);
      assert.match(err.message, /200/);
      assert.match(err.message, /100/);
      return true;
    }
  );
});

test('checkOutputSizes throws when a supplied max_output_bytes is non-numeric (does not silently uncap)', () => {
  assert.throws(
    () => checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(1_000_000) }], 'not-a-number'),
    /max_output_bytes/
  );
});

test('checkOutputSizes throws when a supplied max_output_bytes is non-finite or non-positive', () => {
  for (const bad of [NaN, Infinity, -Infinity, 0, -5]) {
    assert.throws(
      () => checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(10) }], bad),
      /max_output_bytes/,
      `expected throw for ${bad}`
    );
  }
});

test('checkOutputSizes fails the whole job on the first oversize file (no partial result)', () => {
  assert.throws(
    () =>
      checkOutputSizes(
        [
          { filename: 'ok.docx', buffer: Buffer.alloc(5) },
          { filename: 'too-big.docx', buffer: Buffer.alloc(200) },
        ],
        100
      ),
    OutputTooLargeError
  );
});

// The broker cuts a response off past 88 MiB in total; 50 outputs each under
// the per-file cap used to pass here and then fail there with a raw read error.
test('checkOutputSizes refuses outputs whose total is past the total cap with 413', () => {
  const outputs = Array.from({ length: 10 }, (_, i) => ({ filename: `p${i}.pdf`, buffer: { length: 9 << 20 } }));
  assert.throws(
    () => checkOutputSizes(outputs, 10 << 20),
    (err) => {
      assert.ok(err instanceof OutputTooLargeError);
      assert.equal(err.statusCode, 413);
      assert.equal(err.cap, MAX_TOTAL_OUTPUT_BYTES);
      assert.match(err.message, /total cap/);
      return true;
    }
  );
  assert.doesNotThrow(() => checkOutputSizes(outputs.slice(0, 8), 10 << 20));
});
