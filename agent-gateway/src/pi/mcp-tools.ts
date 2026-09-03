// Build dynamic Pi tool definitions for MCP servers the agent can access.
// Called at session build; each tool routes through the GovernanceBridge exactly
// like static tools (gate → submitPlan → invokeTool with agentId).
import { Type } from "typebox";
import { defineTool, type ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { Config } from "../config";
import type { SouthClient } from "../broker/south";
import type { GovernanceBridge } from "../broker/governance";
import { piMcpToolName, fitsPiToolName, MAX_PI_TOOL_NAME_LEN } from "./mcp-alias.js";

// Minimal logging interface used by buildMcpTools; avoids the full pino type.
interface McpLog {
  warn(obj: Record<string, unknown>, msg: string): void;
  info(obj: Record<string, unknown>, msg: string): void;
}

function textResult(text: string) {
  return { content: [{ type: "text" as const, text }], details: {} };
}

export async function buildMcpTools(
  cfg: Pick<Config, "defaultTenantId">,
  south: Pick<SouthClient, "listAccessibleMcpServersForAgent" | "listMcpServerToolsSouth">,
  bridge: Pick<GovernanceBridge, "gate" | "execute">,
  agentId: string,
  log: McpLog,
): Promise<ToolDefinition[]> {
  let servers;
  try {
    const resp = await south.listAccessibleMcpServersForAgent({
      tenantId: cfg.defaultTenantId,
      agentId,
    });
    servers = resp.connections ?? [];
  } catch (err) {
    log.warn({ err: String(err), agentId }, "listAccessibleMcpServersForAgent failed — no MCP tools registered");
    return [];
  }

  const tools: ToolDefinition[] = [];

  for (const server of servers) {
    try {
      const resp = await south.listMcpServerToolsSouth({
        tenantId: cfg.defaultTenantId,
        connectorId: server.id,
        userId: agentId,
      });

      for (const mcpTool of resp.tools ?? []) {
        const name = piMcpToolName(server.id, mcpTool.name);
        if (!fitsPiToolName(name)) {
          log.warn(
            { tool: mcpTool.name, server: server.id, len: name.length, max: MAX_PI_TOOL_NAME_LEN },
            "mcp tool name exceeds the provider function-name limit — tool omitted",
          );
          continue;
        }
        // Append the MCP server's argument JSON Schema to the description so
        // the LLM knows what fields the tool accepts.  We can't safely
        // instantiate an arbitrary runtime JSON Schema as a TypeBox TObject,
        // so the Pi framework sees an open object (additionalProperties) and
        // the broker validates the actual args.
        let description = `[${server.name}] ${mcpTool.description}`;
        if (mcpTool.inputSchemaJson) {
          try {
            JSON.parse(mcpTool.inputSchemaJson); // validate parseable before appending raw string
            description += `\n\nArguments JSON Schema:\n${mcpTool.inputSchemaJson}`;
          } catch (schemaErr) {
            log.warn({ err: String(schemaErr), tool: mcpTool.name, server: server.id }, "mcp tool has unparseable inputSchemaJson — schema omitted from description");
          }
        }

        const parameters = Type.Object({}, { additionalProperties: true });

        // Capture loop variables for the closure.
        const capturedName = name;
        const readOnlyHint = mcpTool.readOnlyHint ?? false;
        tools.push(
          defineTool({
            name,
            label: `MCP: ${mcpTool.name}`,
            description,
            parameters,
            execute: async (toolCallId, _params) => {
              const decision = await bridge.gate(
                toolCallId,
                capturedName,
                (_params as Record<string, unknown>) ?? {},
                { readOnlyHint },
              );
              if (!decision.allow) {
                return textResult(`aikonos: ${decision.reason ?? "denied"}`);
              }
              const r = await bridge.execute(toolCallId);
              const body = r.ok
                ? typeof r.output === "string"
                  ? r.output
                  : JSON.stringify(r.output, null, 2)
                : `ERROR: ${r.error ?? "tool failed"}`;
              return { ...textResult(body), details: r.output };
            },
          }),
        );
      }

      log.info({ server: server.id, toolCount: (resp.tools ?? []).length }, "registered MCP tools");
    } catch (err) {
      log.warn({ err: String(err), server: server.id }, "listMcpServerToolsSouth failed — skipping server");
    }
  }

  return tools;
}
