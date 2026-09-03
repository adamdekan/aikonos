// Unit test for the shared scheduleJson mapper (src/schedule-json.ts), which
// routes/schedules.ts and routes/admin.ts both import — mirrors the
// files-filter.ts/fileJson precedent for a pure wire-shape mapper shared
// across route files. Pins workflowLineageId/workflowDisplayName so a future
// duplication (or a dropped field) fails here, not silently in one of the two
// consuming views (Schedules.vue vs admin Runs.vue).
import { test } from "node:test";
import assert from "node:assert/strict";

import { scheduleJson } from "../src/schedule-json.js";
import { ScheduleKind, ScheduledRunState, type ScheduledRun } from "../gen/ts/proto/broker.js";

function baseRun(overrides: Partial<ScheduledRun> = {}): ScheduledRun {
  return {
    id: "s1",
    ownerUserId: "alice@example.com",
    prompt: "daily report",
    kind: ScheduleKind.SCHEDULE_KIND_CRON,
    cronExpr: "0 9 * * *",
    nextFireAt: undefined,
    approvedTools: [],
    state: ScheduledRunState.SCHEDULED_RUN_ACTIVE,
    lastFireAt: undefined,
    lastStatus: "",
    lastSummary: "",
    runCount: 0,
    createdBy: "alice@example.com",
    createdAt: undefined,
    workflowLineageId: "",
    workflowInputs: {},
    workflowDisplayName: "",
    ...overrides,
  };
}

test("scheduleJson surfaces workflowLineageId + workflowDisplayName for a workflow-mode row", () => {
  const json = scheduleJson(
    baseRun({ prompt: "", workflowLineageId: "wf-lineage-1", workflowDisplayName: "Weekly digest" }),
  );
  assert.equal(json.workflowLineageId, "wf-lineage-1");
  assert.equal(json.workflowDisplayName, "Weekly digest");
});

test("scheduleJson leaves workflow fields empty for a prompt-mode row", () => {
  const json = scheduleJson(baseRun());
  assert.equal(json.workflowLineageId, "");
  assert.equal(json.workflowDisplayName, "");
  assert.equal(json.prompt, "daily report");
});
