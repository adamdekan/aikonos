import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkOutputSizes, OutputTooLargeError } from '../src/size-check.js';

test('checkOutputSizes passes when every output is under the cap', () => {
  assert.doesNotThrow(() =>
    checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(10) }], 100)
  );
});

test('checkOutputSizes is a no-op when no cap is supplied', () => {
  assert.doesNotThrow(() =>
    checkOutputSizes([{ filename: 'a.docx', buffer: Buffer.alloc(1_000_000) }], undefined)
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
