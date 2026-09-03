import type { ResolveSouth } from "../pi/session-plan.js";

// Structural, not Pick<SouthClient, …>: the real SouthClient already satisfies
// ResolveSouth (that is how resolveSessionPlan is wired in production), so this
// is strictly looser for every existing caller while letting a narrow injected
// south — e.g. src/subagent/run.ts's SubagentSouth — reuse this resolver instead
// of growing a second copy of the same walk (llm/provider-fallback.ts convention).
export type AllowlistSouth = Pick<
  ResolveSouth,
  "listAccessibleMcpServersForAgent" | "listMcpServerToolsSouth"
>;

export async function resolveAutoApproveAllowlist(
  south: AllowlistSouth,
  tenantId: string,
  agentId: string,
  skills: string[],
): Promise<string[]> {
  const ids = new Set<string>(skills);

  let connections: { id: string }[];
  try {
    const resp = await south.listAccessibleMcpServersForAgent({ tenantId, agentId });
    connections = resp.connections ?? [];
  } catch {
    // listAccessibleMcpServersForAgent failed — degrade to skills only
    return [...ids];
  }

  for (const server of connections) {
    try {
      const resp = await south.listMcpServerToolsSouth({
        tenantId,
        connectorId: server.id,
        userId: agentId,
      });
      for (const tool of resp.tools ?? []) {
        ids.add(`mcp:${server.id}:${tool.name}`);
      }
    } catch {
      // skip unreachable server; other servers already collected
    }
  }

  return [...ids];
}
