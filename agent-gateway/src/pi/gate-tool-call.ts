// gateToolCall — the shared tool_call governance hook used by both
// session.ts (buildSession) and session-plan.ts (createSessionFromPlan).
//
// WHY extracted: the hook body was duplicated verbatim across both Pi session
// factories. Extraction makes it a testable pure function and removes the
// duplication. Both call sites register this as their extensionFactories
// pi.on("tool_call", ...) handler.
//
// The load_skill exemption: load_skill is a gateway-internal read-tool —
// it returns tenant-owned bundle text the caller already holds can_use on.
// It is NOT a governed tool. Bypassing bridge.gate here is intentional
// (spec A2). The tools a skill activates are still governed individually on
// their own tool_call.
import type { BridgeClientLike } from "../ipc/bridge-client.js";
import type { ToolCallEvent, ToolCallEventResult } from "@earendil-works/pi-coding-agent";
import type { Logger } from "../log.js";
import { ungatedBuiltins } from "./gating-manifest.js";

/** Evaluate governance for a single Pi tool_call event.
 *  Returns undefined (allow) or { block: true, reason } (deny).
 *  load_skill is unconditionally allowed without calling bridge.gate. */
export async function gateToolCall(
  bridge: Pick<BridgeClientLike, "gate">,
  event: Pick<ToolCallEvent, "toolCallId" | "toolName" | "input">,
  log: Pick<Logger, "info">,
): Promise<ToolCallEventResult | undefined> {
  // load_skill is a gateway-internal read-tool: bypass the broker can_invoke gate.
  if (ungatedBuiltins().has(event.toolName)) {
    log.info({ tool: event.toolName }, "tool_call ALLOWED (load_skill: gateway-internal, ungoverned)");
    return undefined;
  }

  const decision = await bridge.gate(
    event.toolCallId,
    event.toolName,
    (event.input ?? {}) as Record<string, unknown>,
  );

  if (!decision.allow) {
    log.info({ tool: event.toolName, reason: decision.reason }, "tool_call BLOCKED");
    return { block: true, reason: `aikonos: ${decision.reason ?? "denied"}` };
  }

  log.info({ tool: event.toolName }, "tool_call ALLOWED");
  return undefined;
}
