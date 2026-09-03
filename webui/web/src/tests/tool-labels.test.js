// Unit tests for the chat tool-trace label helper. toolLabel maps a tool call
// to an end-user phrase + one salient argument so repeat calls to the same tool
// read differently.
import { describe, it, expect } from "vitest";
import { toolLabel, salientArg } from "../lib/toolLabels.js";

describe("toolLabel", () => {
  it("labels web_search with the quoted query", () => {
    const label = toolLabel({
      name: "web_search",
      argsJson: JSON.stringify({ query: "Reuters world news latest" }),
    });
    expect(label).toBe('Searching the web — "Reuters world news latest"');
  });

  it("labels web_fetch with the url hostname (unquoted)", () => {
    const label = toolLabel({
      name: "web_fetch",
      argsJson: JSON.stringify({ url: "https://reuters.com/world/some-article" }),
    });
    expect(label).toBe("Reading a web page — reuters.com");
  });

  it("labels doc_write with the file basename", () => {
    const label = toolLabel({
      name: "doc_write",
      argsJson: JSON.stringify({ path: "reports/world_news_2026-07-23.csv", content: "..." }),
    });
    expect(label).toBe("Writing a file — world_news_2026-07-23.csv");
  });

  it("falls back to the first sentence of the description for an unknown MCP tool", () => {
    const label = toolLabel({
      name: "mcp__jira__create_issue",
      description: "Create a Jira issue in the given project. Requires a linked Jira connection.",
      argsJson: "{}",
    });
    expect(label).toBe("Create a Jira issue in the given project");
  });

  it("does not throw on malformed argsJson and returns the bare phrase", () => {
    expect(() => toolLabel({ name: "web_search", argsJson: "{not json" })).not.toThrow();
    expect(toolLabel({ name: "web_search", argsJson: "{not json" })).toBe("Searching the web");
  });

  it("labels load_skill with 'Loading skill: ' + first 5 words of the description", () => {
    const label = toolLabel({
      name: "load_skill",
      description: "Analyze quarterly sales report for the finance team",
    });
    expect(label).toBe("Loading skill: Analyze quarterly sales report for");
  });

  it("labels load_skill from the skill name when no description is present", () => {
    const label = toolLabel({
      name: "load_skill",
      argsJson: JSON.stringify({ name: "invoice-processor" }),
    });
    expect(label).toBe("Loading skill: invoice-processor");
  });

  it("resolves a broker skill id (dotted) to the same label as the Pi tool name", () => {
    const label = toolLabel({
      name: "web.fetch",
      argsJson: JSON.stringify({ url: "https://reuters.com/world/some-article" }),
    });
    expect(label).toBe("Reading a web page — reuters.com");
  });

  it("labels a workflow reason step 'Thinking'", () => {
    expect(toolLabel({ name: "reason", argsJson: "{}" })).toBe("Thinking");
  });

  it("produces distinct labels for two same-name calls with different args", () => {
    const a = toolLabel({ name: "web_search", argsJson: JSON.stringify({ query: "weather" }) });
    const b = toolLabel({ name: "web_search", argsJson: JSON.stringify({ query: "stock prices" }) });
    expect(a).not.toBe(b);
  });
});

describe("salientArg", () => {
  it("returns '' on malformed JSON", () => {
    expect(salientArg("web_search", "{bad")).toBe("");
  });

  it("truncates a long detail to 60 chars with an ellipsis", () => {
    const long = "x".repeat(100);
    const out = salientArg("web_search", JSON.stringify({ query: long }));
    expect(out.length).toBe(60);
    expect(out.endsWith("…")).toBe(true);
  });
});
