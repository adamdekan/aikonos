import { describe, it, expect } from "vitest";
import { stripMention } from "../lib/mention.js";

describe("stripMention", () => {
  it("removes @<displayName> token at the start of text", () => {
    expect(stripMention("@Alice do the thing", "Alice")).toBe("do the thing");
  });

  it("removes token at end-of-text", () => {
    expect(stripMention("do this @Alice", "Alice")).toBe("do this");
  });

  it("removes token mid-text without leaving a double-space", () => {
    expect(stripMention("please @Alice do the thing", "Alice")).toBe("please do the thing");
  });

  it("handles displayName containing a space", () => {
    expect(stripMention("@John Smith review this", "John Smith")).toBe("review this");
  });

  it("handles group displayName with spaces mid-text", () => {
    expect(stripMention("urgent @Engineering Team fix the bug", "Engineering Team")).toBe(
      "urgent fix the bug"
    );
  });

  it("preserves a stray @ elsewhere in the content", () => {
    expect(stripMention("@Alice ping me at @admin about this", "Alice")).toBe(
      "ping me at @admin about this"
    );
  });

  it("removes only the first occurrence when displayName appears multiple times", () => {
    expect(stripMention("@Alice and @Alice again", "Alice")).toBe("and @Alice again");
  });

  it("trims leading/trailing whitespace from result", () => {
    expect(stripMention("  @Alice  ", "Alice")).toBe("");
  });

  it("returns the original text trimmed when displayName is not found", () => {
    expect(stripMention("do the thing", "Alice")).toBe("do the thing");
  });

  it("handles displayName with regex metacharacters safely", () => {
    // displayName contains a dot — should match literally, not as a regex wildcard
    expect(stripMention("@Dr.Smith check this", "Dr.Smith")).toBe("check this");
  });

  it("preserves newlines in multi-line content", () => {
    expect(stripMention("@Alice line1\nline2", "Alice")).toBe("line1\nline2");
  });

  it("returns text trimmed when displayName is empty (guard path)", () => {
    expect(stripMention("  do the thing  ", "")).toBe("do the thing");
  });
});
