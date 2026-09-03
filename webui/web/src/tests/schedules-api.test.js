// Tests for api/schedules.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listSchedules, createSchedule, updateSchedule, pauseSchedule, resumeSchedule, deleteSchedule } from "../api/schedules.js";

describe("api/schedules.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listSchedules calls GET /schedules", async () => {
    clientMod.get.mockResolvedValue({ schedules: [] });
    await listSchedules();
    expect(clientMod.get).toHaveBeenCalledWith("/schedules");
  });

  it("createSchedule posts correct fields to /schedules", async () => {
    clientMod.post.mockResolvedValue({ schedule: null });
    await createSchedule({
      prompt: "run daily report",
      kind: "CRON",
      cronExpr: "0 9 * * *",
      runAt: undefined,
      approvedTools: ["web.fetch"],
    });
    expect(clientMod.post).toHaveBeenCalledWith("/schedules", {
      body: {
        prompt: "run daily report",
        kind: "CRON",
        cronExpr: "0 9 * * *",
        runAt: undefined,
        approvedTools: ["web.fetch"],
      },
    });
  });

  it("updateSchedule patches full fields with no action (broker full-edit branch)", async () => {
    clientMod.patch.mockResolvedValue({ schedule: null });
    await updateSchedule("s1", {
      prompt: "changed",
      kind: "CRON",
      cronExpr: "30 8 * * *",
      runAt: undefined,
      approvedTools: ["web.fetch", "doc.read"],
    });
    expect(clientMod.patch).toHaveBeenCalledWith("/schedules/s1", {
      body: {
        prompt: "changed",
        kind: "CRON",
        cronExpr: "30 8 * * *",
        runAt: undefined,
        approvedTools: ["web.fetch", "doc.read"],
      },
    });
    // No `action` key — that is what selects the broker's full-edit branch.
    expect(clientMod.patch.mock.calls[0][1].body).not.toHaveProperty("action");
  });

  it("pauseSchedule patches with action:pause", async () => {
    clientMod.patch.mockResolvedValue({ schedule: null });
    await pauseSchedule("s1");
    expect(clientMod.patch).toHaveBeenCalledWith("/schedules/s1", { body: { action: "pause" } });
  });

  it("resumeSchedule patches with action:resume", async () => {
    clientMod.patch.mockResolvedValue({ schedule: null });
    await resumeSchedule("s1");
    expect(clientMod.patch).toHaveBeenCalledWith("/schedules/s1", { body: { action: "resume" } });
  });

  it("deleteSchedule calls DELETE /schedules/:id", async () => {
    clientMod.del.mockResolvedValue({ success: true });
    await deleteSchedule("s1");
    expect(clientMod.del).toHaveBeenCalledWith("/schedules/s1");
  });
});
