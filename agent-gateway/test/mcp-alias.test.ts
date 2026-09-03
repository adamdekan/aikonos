// MCP Pi tool names must survive the LLM provider's 64-char function-name
// limit AND round-trip back to the broker toolId that carries authority.
//
// The bug this pins: the name used to embed the connection's full 36-char UUID,
// leaving 21 chars for the tool name. Any longer name made the provider reject
// the entire request ("string too long. Expected a string with maximum length
// 64") — which killed the whole conversation, not just that one tool call.
import test from "node:test";
import assert from "node:assert/strict";
import {
  piMcpToolName,
  resolveMcpAlias,
  aliasFor,
  fitsPiToolName,
  MAX_PI_TOOL_NAME_LEN,
  __resetMcpAliasesForTest,
} from "../src/pi/mcp-alias.js";
import { mapTool } from "../src/broker/mapping.js";

const CONN = "d875805d-0fbe-42f7-8384-ef9dba4c07e1";

// The 20 Grafana tool names that were unusable under the old scheme, longest
// first. Real names, read off the deployed server's tools/list.
const REAL_LONG_TOOLS = [
  "suggest_loki_alloy_label_config", // 31
  "list_prometheus_metric_metadata", // 31
  "list_provisioning_repositories", // 30
  "list_pyroscope_profile_types", // 28
  "list_prometheus_metric_names", // 28
  "list_prometheus_label_values", // 28
  "list_pyroscope_label_values", // 27
  "list_prometheus_label_names", // 27
  "get_dashboard_panel_queries", // 27
  "validate_provisioning_file", // 26
  "query_prometheus_histogram", // 26
  "list_pyroscope_label_names", // 26
];

test("every real Grafana tool name now fits the provider limit", () => {
  __resetMcpAliasesForTest();
  for (const tool of REAL_LONG_TOOLS) {
    const name = piMcpToolName(CONN, tool);
    assert.ok(
      fitsPiToolName(name),
      `${tool} → ${name} is ${name.length} chars, over the ${MAX_PI_TOOL_NAME_LEN} limit`,
    );
  }
});

test("the previously-failing name is well under the limit", () => {
  __resetMcpAliasesForTest();
  // This exact name was 70 chars and 400'd the conversation on the on-prem host.
  const name = piMcpToolName(CONN, "get_dashboard_panel_queries");
  assert.equal(name, "mcp__d875805d__get_dashboard_panel_queries");
  assert.equal(name.length, 42); // was 70

});

test("round-trip: Pi name maps back to the full connector id, not the alias", () => {
  __resetMcpAliasesForTest();
  const name = piMcpToolName(CONN, "list_prometheus_metric_names");
  const mapping = mapTool(name);
  assert.ok(mapping, "name must resolve");
  // The broker needs the *full* id — an alias here would fail the connection lookup.
  assert.equal(mapping.toolId, `mcp:${CONN}:list_prometheus_metric_names`);
});

test("round-trip holds for every real long tool name", () => {
  __resetMcpAliasesForTest();
  for (const tool of REAL_LONG_TOOLS) {
    const mapping = mapTool(piMcpToolName(CONN, tool));
    assert.ok(mapping, `${tool} must resolve`);
    assert.equal(mapping.toolId, `mcp:${CONN}:${tool}`);
  }
});

test("an unregistered alias fails closed — never passed through as a connector id", () => {
  __resetMcpAliasesForTest();
  // A name the parent never minted must not reach InvokeTool: this toolId is
  // what gets FGA-checked and Biscuit-scoped.
  assert.equal(mapTool("mcp__deadbeef__get_thing"), undefined);
  // Nor may a full connector id be smuggled in as the first segment.
  assert.equal(mapTool(`mcp__${CONN}__get_thing`), undefined);
});

test("alias is order-independent: same id always yields the same alias", () => {
  __resetMcpAliasesForTest();
  const other = "aaaaaaaa-1111-2222-3333-444444444444";
  piMcpToolName(other, "get_x");
  const aliasA = aliasFor(CONN);
  __resetMcpAliasesForTest();
  piMcpToolName(CONN, "get_x"); // registered first this time
  assert.equal(aliasFor(CONN), aliasA, "alias must not depend on registration order");
});

test("prefix collision between two connections throws rather than remapping", () => {
  __resetMcpAliasesForTest();
  const a = "d875805d-0fbe-42f7-8384-ef9dba4c07e1";
  const b = "d875805d-9999-4444-8888-aaaaaaaaaaaa"; // same first 8 chars
  piMcpToolName(a, "get_x");
  // Silently remapping would authorize the wrong server's tool, so this must
  // fail loud. Both call sites build inside a per-server try/catch, so the
  // colliding server is dropped and the session still starts.
  assert.throws(() => piMcpToolName(b, "get_x"), /alias collision/);
  // The first registration is still intact.
  assert.equal(resolveMcpAlias(aliasFor(a)), a);
});

test("a tool name long enough to breach the limit even with the alias is rejected", () => {
  __resetMcpAliasesForTest();
  // 5 + 8 + 2 = 15 chars of prefix, so 50+ chars of tool name still overflows.
  const absurd = "x".repeat(60);
  assert.equal(fitsPiToolName(piMcpToolName(CONN, absurd)), false);
});

test("workflow-stored broker ids keep the full id and are unaffected", () => {
  __resetMcpAliasesForTest();
  // Workflows persist mcp:<fullId>:<tool>, parsed by mapMcpBrokerId — that path
  // never involved an alias and must keep working with no registration at all.
  const mapping = mapTool(`mcp:${CONN}:get_dashboard_panel_queries`);
  assert.ok(mapping, "broker-id form must resolve without a registered alias");
  assert.equal(mapping.toolId, `mcp:${CONN}:get_dashboard_panel_queries`);
});
