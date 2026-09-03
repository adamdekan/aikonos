// Tests for the dot-segment filter used by GET /files.
//
// hasDotSegment(path) returns true when any segment of path (split on "/")
// starts with ".". A mid-segment dot ("foo.bar") is NOT a dot-prefix.
//
// filterFiles(files, includeHidden) wraps hasDotSegment to drive the handler:
//   includeHidden=false → drop entries with dot-prefixed segments
//   includeHidden=true  → return everything
import { test } from "node:test";
import assert from "node:assert/strict";

import { hasDotSegment, filterFiles, fileJson } from "../src/files-filter.js";

// ── hasDotSegment ─────────────────────────────────────────────────────────────

test("hasDotSegment: top-level dot-prefixed segment", () => {
  assert.equal(hasDotSegment(".agent"), true);
});

test("hasDotSegment: nested dot-prefixed first segment", () => {
  assert.equal(hasDotSegment(".agent/Sessions/x.json"), true);
});

test("hasDotSegment: dot-prefixed non-first segment", () => {
  assert.equal(hasDotSegment("a/.b/c"), true);
});

test("hasDotSegment: dot-prefixed last segment", () => {
  assert.equal(hasDotSegment("a/b/.hidden"), true);
});

test("hasDotSegment: mid-segment dot is NOT a dot-prefix", () => {
  assert.equal(hasDotSegment("foo.bar"), false);
});

test("hasDotSegment: nested file with dots in name", () => {
  assert.equal(hasDotSegment("a/b.json"), false);
});

test("hasDotSegment: nested file with multiple dots in name", () => {
  assert.equal(hasDotSegment("docs/report.final.md"), false);
});

test("hasDotSegment: plain path, no dots", () => {
  assert.equal(hasDotSegment("documents/notes"), false);
});

test("hasDotSegment: empty string is not dot-prefixed", () => {
  assert.equal(hasDotSegment(""), false);
});

test("hasDotSegment: bare dot segment is dot-prefixed", () => {
  assert.equal(hasDotSegment("."), true);
});

// ── filterFiles: default mode (includeHidden=false) ───────────────────────────

interface FileEntry {
  path: string;
  size: number;
  modified: string | null;
}

function makeFile(path: string): FileEntry {
  return { path, size: 0, modified: null };
}

test("filterFiles default: drops .agent/ entry", () => {
  const files: FileEntry[] = [
    makeFile(".agent/Sessions/abc.json"),
    makeFile("documents/notes.txt"),
  ];
  const result = filterFiles(files, false);
  assert.equal(result.length, 1);
  assert.equal(result[0].path, "documents/notes.txt");
});

test("filterFiles default: retains ordinary foo.bar entry", () => {
  const files: FileEntry[] = [
    makeFile("foo.bar"),
    makeFile("report.txt"),
  ];
  const result = filterFiles(files, false);
  assert.equal(result.length, 2);
});

test("filterFiles default: retains nested a/b.json entry", () => {
  const files: FileEntry[] = [
    makeFile("a/b.json"),
    makeFile(".hidden"),
  ];
  const result = filterFiles(files, false);
  assert.equal(result.length, 1);
  assert.equal(result[0].path, "a/b.json");
});

test("filterFiles default: drops all dot-prefixed variants", () => {
  const files: FileEntry[] = [
    makeFile(".foo"),
    makeFile("a/.b/c"),
    makeFile("x/y/.z"),
    makeFile("normal/file.txt"),
  ];
  const result = filterFiles(files, false);
  assert.equal(result.length, 1);
  assert.equal(result[0].path, "normal/file.txt");
});

// ── filterFiles: includeHidden=true ───────────────────────────────────────────

test("filterFiles includeHidden=true: returns everything including dot-prefixed", () => {
  const files: FileEntry[] = [
    makeFile(".agent/Sessions/x.json"),
    makeFile("documents/notes.txt"),
    makeFile(".hidden"),
  ];
  const result = filterFiles(files, true);
  assert.equal(result.length, 3);
});

test("filterFiles includeHidden=true: empty input returns empty", () => {
  const result = filterFiles([], true);
  assert.equal(result.length, 0);
});

test("filterFiles includeHidden=false: empty input returns empty", () => {
  const result = filterFiles([], false);
  assert.equal(result.length, 0);
});

// ── fileJson ───────────────────────────────────────────────────────────────

test("fileJson: dir entry maps isDir true and size 0", () => {
  const result = fileJson({ path: "folder", sizeBytes: 0, isDir: true });
  assert.deepEqual(result, { path: "folder", size: 0, modified: null, isDir: true });
});

test("fileJson: file entry maps isDir false", () => {
  const result = fileJson({ path: "report.txt", sizeBytes: 42, isDir: false });
  assert.equal(result.isDir, false);
  assert.equal(result.size, 42);
});

test("fileJson: missing modifiedAt maps to modified null", () => {
  const result = fileJson({ path: "report.txt", sizeBytes: 42 });
  assert.equal(result.modified, null);
});

test("fileJson: modifiedAt maps to ISO string", () => {
  const result = fileJson({
    path: "report.txt",
    sizeBytes: 42,
    modifiedAt: new Date("2026-01-01T00:00:00Z"),
  });
  assert.equal(result.modified, "2026-01-01T00:00:00.000Z");
});
