import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { listDocs, readDoc, searchDocs, CorpusError } from '../src/corpus.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const FIXTURE = path.resolve(__dirname, 'fixtures/corpus');

// ---------------------------------------------------------------------------
// listDocs
// ---------------------------------------------------------------------------

describe('listDocs', () => {
  it('returns all .md files sorted by path ascending', async () => {
    const docs = await listDocs(FIXTURE);
    const paths = docs.map(d => d.path);
    assert.deepEqual(paths, [...paths].sort());
  });

  it('ignores non-.md files', async () => {
    const docs = await listDocs(FIXTURE);
    assert.ok(docs.every(d => d.path.endsWith('.md')));
    assert.ok(!docs.some(d => d.path.endsWith('.txt')));
  });

  it('returns H1 as title when present', async () => {
    const docs = await listDocs(FIXTURE);
    const intro = docs.find(d => d.path === 'intro.md');
    assert.ok(intro, 'intro.md must appear in listing');
    assert.equal(intro.title, 'Introduction to Aikonos');
  });

  it('falls back to basename (no extension) when no H1', async () => {
    const docs = await listDocs(FIXTURE);
    const noH1 = docs.find(d => d.path === 'no-h1.md');
    assert.ok(noH1, 'no-h1.md must appear in listing');
    assert.equal(noH1.title, 'no-h1');
  });

  it('includes nested .md files with forward-slash paths', async () => {
    const docs = await listDocs(FIXTURE);
    const nested = docs.find(d => d.path === 'subdir/nested.md');
    assert.ok(nested, 'subdir/nested.md must appear in listing');
    assert.equal(nested.title, 'Nested Document');
  });
});

// ---------------------------------------------------------------------------
// readDoc
// ---------------------------------------------------------------------------

describe('readDoc', () => {
  it('returns { path, content } for a valid relative path', async () => {
    const doc = await readDoc(FIXTURE, 'intro.md');
    assert.equal(doc.path, 'intro.md');
    assert.ok(doc.content.includes('Introduction to Aikonos'));
  });

  it('reads a nested file by relative path', async () => {
    const doc = await readDoc(FIXTURE, 'subdir/nested.md');
    assert.equal(doc.path, 'subdir/nested.md');
    assert.ok(doc.content.includes('Nested Document'));
  });

  it('rejects ../ path traversal with CorpusError', async () => {
    await assert.rejects(
      () => readDoc(FIXTURE, '../escape.md'),
      (err) => {
        assert.ok(err instanceof CorpusError, `expected CorpusError, got ${err.constructor.name}`);
        // message must not be or start with an absolute path
        assert.ok(!err.message.startsWith('/'), `message starts with /: ${err.message}`);
        return true;
      }
    );
  });

  it('rejects absolute path input with CorpusError', async () => {
    await assert.rejects(
      () => readDoc(FIXTURE, '/etc/passwd'),
      (err) => {
        assert.ok(err instanceof CorpusError, `expected CorpusError, got ${err.constructor.name}`);
        assert.ok(!err.message.includes('/home/'), `message leaks host path: ${err.message}`);
        return true;
      }
    );
  });

  it('rejects non-existent file with CorpusError', async () => {
    await assert.rejects(
      () => readDoc(FIXTURE, 'does-not-exist.md'),
      (err) => {
        assert.ok(err instanceof CorpusError, `expected CorpusError, got ${err.constructor.name}`);
        assert.ok(!err.message.includes('/home/'), `message leaks host path: ${err.message}`);
        return true;
      }
    );
  });

  it('rejects non-.md path with CorpusError', async () => {
    await assert.rejects(
      () => readDoc(FIXTURE, 'ignored.txt'),
      (err) => {
        assert.ok(err instanceof CorpusError);
        return true;
      }
    );
  });
});

// ---------------------------------------------------------------------------
// searchDocs
// ---------------------------------------------------------------------------

describe('searchDocs', () => {
  it('ranks a file with many term hits above one with fewer', async () => {
    // intro.md has "Aikonos" many times; subdir/nested.md has it once
    const results = await searchDocs(FIXTURE, 'aikonos', 10);
    assert.ok(results.length >= 2, 'expected at least 2 results');
    const intrIdx = results.findIndex(r => r.path === 'intro.md');
    const nestIdx = results.findIndex(r => r.path === 'subdir/nested.md');
    assert.ok(intrIdx !== -1, 'intro.md should appear');
    assert.ok(nestIdx !== -1, 'subdir/nested.md should appear');
    assert.ok(intrIdx < nestIdx, 'intro.md should outrank subdir/nested.md');
  });

  it('excludes files with zero score', async () => {
    // search for a term only in intro.md — no-h1.md and nested.md should not appear
    // "zero-trust" appears only in intro.md
    const results = await searchDocs(FIXTURE, 'zero-trust', 10);
    assert.ok(results.every(r => r.score > 0));
    assert.ok(results.some(r => r.path === 'intro.md'));
  });

  it('respects limit', async () => {
    // "aikonos" hits multiple files; limit to 1
    const results = await searchDocs(FIXTURE, 'aikonos', 1);
    assert.equal(results.length, 1);
  });

  it('clamps limit below 1 to 1', async () => {
    const results = await searchDocs(FIXTURE, 'aikonos', 0);
    assert.ok(results.length >= 1);
    assert.ok(results.length <= 1);
  });

  it('clamps limit above 20 to 20', async () => {
    const results = await searchDocs(FIXTURE, 'aikonos', 99);
    assert.ok(results.length <= 20);
  });

  it('breaks ties by path ascending', async () => {
    // "once" appears exactly once in no-h1.md and once in subdir/nested.md — equal score.
    // Both must appear so we can assert their relative order.
    const results = await searchDocs(FIXTURE, 'once', 10);
    assert.ok(results.length >= 2, `expected at least 2 results for "once", got ${results.length}`);
    // paths should be in ascending order among equal-score entries
    const paths = results.map(r => r.path);
    const sorted = [...paths].sort();
    assert.deepEqual(paths, sorted, 'tie-broken paths must be sorted ascending');
  });

  it('returns empty array for empty query', async () => {
    const results = await searchDocs(FIXTURE, '', 10);
    assert.deepEqual(results, []);
  });

  it('returns empty array when no file matches', async () => {
    const results = await searchDocs(FIXTURE, 'xyzzy_no_such_term_12345', 10);
    assert.deepEqual(results, []);
  });

  it('each result has path, score, snippet fields', async () => {
    const results = await searchDocs(FIXTURE, 'aikonos', 10);
    for (const r of results) {
      assert.ok('path' in r, 'result must have path');
      assert.ok('score' in r, 'result must have score');
      assert.ok('snippet' in r, 'result must have snippet');
      assert.ok(typeof r.score === 'number' && r.score > 0);
    }
  });
});
