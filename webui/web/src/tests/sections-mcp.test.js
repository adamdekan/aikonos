// Tests for the MCP access section in sections.js (CP8).
import { describe, it, expect } from "vitest";
import { SECTIONS, subjectRef, principalKind } from "../sections.js";

describe("sections.js — MCP access section", () => {
  const mcpSection = SECTIONS.find((s) => s.key === "mcp");

  it("MCP access section exists", () => {
    expect(mcpSection).toBeDefined();
  });

  it("has objectType mcp_connector", () => {
    expect(mcpSection.objectType).toBe("mcp_connector");
  });

  it("has label 'MCP access'", () => {
    expect(mcpSection.label).toBe("MCP access");
  });

  it("has permitted_agent relation with agent subject", () => {
    const rel = mcpSection.relations.find((r) => r.name === "permitted_agent");
    expect(rel).toBeDefined();
    expect(rel.subjects).toContain("agent");
  });

  it("subjectRef handles agent: prefix correctly (no #member append)", () => {
    expect(subjectRef("agent:bot-123")).toBe("agent:bot-123");
  });

  it("principalKind recognises agent: prefix", () => {
    expect(principalKind("agent:bot-123")).toBe("agent");
  });
});
