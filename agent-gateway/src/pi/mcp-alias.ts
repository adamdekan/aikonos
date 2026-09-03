// The single source of truth for MCP Pi tool names.
//
// A Pi tool name is an LLM function name, and both OpenAI and Anthropic cap
// those at 64 characters. The name has to carry the connection the tool belongs
// to, because the parent resolves it back to the broker toolId
// (mcp:<connectorId>:<toolName>) that gets FGA-checked and Biscuit-scoped.
//
// Embedding the connection's full 36-char UUID left only 21 characters for the
// tool name itself:
//
//   mcp__ + <36-char uuid> + __ + <tool name>   <= 64
//   \________ 43 fixed _________/   \_ 21 left _/
//
// Any MCP server whose tools are named more descriptively than that made the
// provider reject the *entire* request with a 400 — 20 of the Grafana MCP
// server's 52 tools, for instance. So the name carries a short alias of the
// connection id instead, and this module maps it back.
//
// The alias is a pure prefix of the id, which matters: it does not depend on
// registration order, so the same connection always yields the same alias no
// matter which session built it first.

// Characters an LLM function name permits.
function sanitizeSegment(s: string): string {
  return s.replace(/[^a-zA-Z0-9_-]/g, "_").replace(/^_+|_+$/g, "") || "x";
}

// 8 hex characters of a UUID is 32 bits — ample for the handful of MCP
// connections a tenant registers, and a genuine collision fails loud below
// rather than silently resolving to the wrong connection.
const ALIAS_LEN = 8;

// The provider-imposed ceiling on an LLM function name.
export const MAX_PI_TOOL_NAME_LEN = 64;

// alias → full connector id. Populated whenever a name is built (session build),
// which always precedes the child's first tool call through gate().
const byAlias = new Map<string, string>();

// mcpAlias mints the alias for a connection id and registers it so
// resolveMcpAlias can invert it. Throws on a prefix collision between two
// different connections: the alias resolves to a broker toolId that carries
// authority, so remapping one of them silently would authorize the wrong
// server's tool. Both call sites build names inside a per-server try/catch, so
// a throw drops that one server rather than the whole session.
export function mcpAlias(connectorId: string): string {
  const alias = sanitizeSegment(connectorId).slice(0, ALIAS_LEN);
  const existing = byAlias.get(alias);
  if (existing !== undefined && existing !== connectorId) {
    throw new Error(
      `mcp alias collision: "${alias}" maps to both ${existing} and ${connectorId}`,
    );
  }
  byAlias.set(alias, connectorId);
  return alias;
}

// resolveMcpAlias inverts mcpAlias. Returns undefined for an alias that was
// never registered — callers must treat that as "unknown tool", never as a
// connector id to pass through.
export function resolveMcpAlias(alias: string): string | undefined {
  return byAlias.get(alias);
}

// aliasFor computes the alias without registering it — for call sites that only
// need to match a name that already exists rather than mint a new one. Never
// throws.
export function aliasFor(connectorId: string): string {
  return sanitizeSegment(connectorId).slice(0, ALIAS_LEN);
}

// piMcpToolName builds the Pi tool name for one MCP tool. The only builder —
// mapMcpTool in broker/mapping.ts is the only parser.
export function piMcpToolName(connectorId: string, toolName: string): string {
  return `mcp__${mcpAlias(connectorId)}__${sanitizeSegment(toolName)}`;
}

// fitsPiToolName guards against a tool whose name is long enough to breach the
// limit even with the alias. One such tool would otherwise 400 every request in
// the conversation, not just its own call — so the callers drop the tool and
// log, trading one missing tool for a working chat.
export function fitsPiToolName(name: string): boolean {
  return name.length <= MAX_PI_TOOL_NAME_LEN;
}

// Test-only: clear the registry between cases.
export function __resetMcpAliasesForTest(): void {
  byAlias.clear();
}
