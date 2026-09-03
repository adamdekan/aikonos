// Build a 1-step aikonos Plan from a single Pi tool call (the JIT single-step
// plan that reconciles Pi's interactive loop with aikonos's plan-then-execute).
import type { Plan } from "../../gen/ts/proto/plan";
import type { ToolMapping } from "./mapping";

export const GATEWAY_EXECUTION_HINT = "aikonos:execution=gateway";

export interface OneStepPlanInput {
  taskId: string;
  tenantId: string;
  toolCallId: string;
  mapping: ToolMapping;
  args: Record<string, unknown>;
}

export function oneStepPlan(in_: OneStepPlanInput): Plan {
  return {
    planId: `plan-${in_.toolCallId}`,
    taskId: in_.taskId,
    tenantId: in_.tenantId,
    effectClassCeiling: in_.mapping.effectClass,
    estimatedTotalCost: 1,
    requiredWorkspacePaths: [],
    hasDlpAttestation: false,
    steps: [
      {
        seq: 1,
        toolId: in_.mapping.toolId,
        args: in_.args,
        justification: `agent tool call ${in_.mapping.toolId}`,
        effectClass: in_.mapping.effectClass,
        estimatedCost: 1,
        // Deprecated: the broker derives
        // reads_sensitive server-side from the routed effect class and no
        // longer reads this field. Required by the wire type, so it can't be
        // omitted outright — hardcoded rather than the vacuous `? false :
        // false` ternary this replaced.
        readsSensitive: false,
        workspacePathsNeeded: [],
      },
    ],
  };
}
