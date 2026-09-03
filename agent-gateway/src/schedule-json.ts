// Shared wire-shape mapper for ScheduledRun, used by both routes/schedules.ts
// (per-user schedule management) and routes/admin.ts (tenant-admin oversight
// view) — mirrors the files-filter.ts/fileJson precedent for a pure mapper
// shared across route files, so a field added to the proto only needs to be
// added here once.
import { ScheduleKind, ScheduledRunState, type ScheduledRun } from "../gen/ts/proto/broker.js";

export function kindToStr(k: ScheduleKind): "CRON" | "ONCE" {
  return k === ScheduleKind.SCHEDULE_KIND_ONCE ? "ONCE" : "CRON";
}

export function stateToStr(s: ScheduledRunState): string {
  switch (s) {
    case ScheduledRunState.SCHEDULED_RUN_ACTIVE: return "ACTIVE";
    case ScheduledRunState.SCHEDULED_RUN_PAUSED: return "PAUSED";
    case ScheduledRunState.SCHEDULED_RUN_COMPLETED: return "COMPLETED";
    case ScheduledRunState.SCHEDULED_RUN_FAILED: return "FAILED";
    default: return "UNKNOWN";
  }
}

export function scheduleJson(r: ScheduledRun) {
  return {
    id: r.id,
    owner: r.ownerUserId,
    prompt: r.prompt,
    kind: kindToStr(r.kind),
    cronExpr: r.cronExpr,
    nextFireAt: r.nextFireAt?.toISOString() ?? null,
    approvedTools: r.approvedTools ?? [],
    // Workflow-mode display fields (empty on a prompt-mode row); joined
    // server-side by ListScheduledRuns, never denormalized onto the row.
    workflowLineageId: r.workflowLineageId ?? "",
    workflowDisplayName: r.workflowDisplayName ?? "",
    state: stateToStr(r.state),
    lastFireAt: r.lastFireAt?.toISOString() ?? null,
    lastStatus: r.lastStatus,
    lastSummary: r.lastSummary,
    runCount: r.runCount,
    createdBy: r.createdBy,
    createdAt: r.createdAt?.toISOString() ?? null,
  };
}
