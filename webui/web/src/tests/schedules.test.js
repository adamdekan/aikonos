// View tests for Schedules.vue.
// api/schedules.js is fully mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/schedules.js", () => ({
  listSchedules:  vi.fn(),
  createSchedule: vi.fn(),
  updateSchedule: vi.fn(),
  pauseSchedule:  vi.fn(),
  resumeSchedule: vi.fn(),
  deleteSchedule: vi.fn(),
}));

import Schedules from "../views/Schedules.vue";
import RecurrenceSelector from "../components/RecurrenceSelector.vue";
import * as schedApiView from "../api/schedules.js";
import { localTz } from "../lib/cron.js";

// The RecurrenceSelector emits guided cron with a CRON_TZ=<viewer zone> prefix
// (so the broker evaluates fields in the creator's zone). Pin the exact full
// output by prefixing with the same localTz() the component uses.
const TZ_PREFIX = localTz() ? `CRON_TZ=${localTz()} ` : "";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/schedules", component: Schedules },
      { path: "/",          component: { template: "<div/>" } },
    ],
  });
}

const SAMPLE_SCHED = {
  id: "s1",
  owner: "alice@example.com",
  prompt: "daily report",
  kind: "CRON",
  cronExpr: "0 9 * * *",
  nextFireAt: "2024-01-02T09:00:00Z",
  approvedTools: [],
  workflowLineageId: "",
  workflowDisplayName: "",
  state: "ACTIVE",
  lastFireAt: null,
  lastStatus: "",
  lastSummary: "",
  runCount: 0,
  createdBy: "alice@example.com",
  createdAt: "2024-01-01T00:00:00Z",
};

// Stored workflow-mode rows always have empty prompt + empty approvedTools
// — the workflow binding/inputs carry the
// authority instead.
const WORKFLOW_SCHED = {
  ...SAMPLE_SCHED,
  id: "s2",
  prompt: "",
  approvedTools: [],
  workflowLineageId: "wf-lineage-1",
  workflowDisplayName: "Weekly digest",
};

describe("Schedules.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders schedule list from API response", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [SAMPLE_SCHED] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='schedule-row']").length).toBe(1);
    expect(w.text()).toContain("daily report");
  });

  it("createSchedule is called with correct fields from form submit", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [] });
    schedApiView.createSchedule.mockResolvedValue({ schedule: SAMPLE_SCHED });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='new-prompt']").setValue("daily report");
    await w.find("[data-testid='new-kind']").setValue("CRON");
    // Submitting without touching the recurrence selector relies on its default (daily 09:00).
    await w.find("[data-testid='create-schedule-form']").trigger("submit");
    await flushPromises();

    expect(schedApiView.createSchedule).toHaveBeenCalledWith(
      expect.objectContaining({ prompt: "daily report", kind: "CRON", cronExpr: `${TZ_PREFIX}0 9 * * *`, approvedTools: ["web.fetch", "doc.read", "doc.write"] }),
    );
  });

  it("changing the recurrence selector's frequency/time updates the emitted cronExpr", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [] });
    schedApiView.createSchedule.mockResolvedValue({ schedule: SAMPLE_SCHED });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='new-prompt']").setValue("weekly report");
    await w.find("[data-testid='new-kind']").setValue("CRON");
    await w.find("[data-testid='recurrence-freq']").setValue("weekly");
    // default weekly preset is Mon-Fri ([1,2,3,4,5]); toggle Sat on to prove the
    // control drives the emitted cronExpr.
    await w.find("[data-testid='weekday-6']").trigger("click");
    await w.find("[data-testid='cron-time']").setValue("14:30");
    await w.find("[data-testid='create-schedule-form']").trigger("submit");
    await flushPromises();

    expect(schedApiView.createSchedule).toHaveBeenCalledWith(
      expect.objectContaining({ cronExpr: `${TZ_PREFIX}30 14 * * 1,2,3,4,5,6` }),
    );
  });

  it("re-seeds guided controls when modelValue resets externally after mount (post-submit reset)", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [] });
    schedApiView.createSchedule.mockResolvedValue({ schedule: SAMPLE_SCHED });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    // Drive the selector to a different recurrence than the default.
    await w.find("[data-testid='new-prompt']").setValue("weekly report");
    await w.find("[data-testid='new-kind']").setValue("CRON");
    await w.find("[data-testid='recurrence-freq']").setValue("weekly");
    await w.find("[data-testid='cron-time']").setValue("14:30");
    expect(w.find("[data-testid='cron-description']").text()).toBe("Every weekday at 14:30");

    const selector = w.findComponent(RecurrenceSelector);
    const emitCountBeforeReset = selector.emitted("update:modelValue").length;

    // Submit triggers Schedules.vue's post-submit reset of newCron back to the
    // daily-09:00 default, without unmounting RecurrenceSelector (no v-if churn).
    await w.find("[data-testid='create-schedule-form']").trigger("submit");
    await flushPromises();

    expect(w.find("[data-testid='cron-description']").text()).toBe("Every day at 09:00");

    // The re-seed watcher must not create a v-model feedback loop: the prop
    // reset should produce at most a couple more emits, never an unbounded chain.
    const emitCountAfterReset = selector.emitted("update:modelValue").length;
    expect(emitCountAfterReset - emitCountBeforeReset).toBeLessThanOrEqual(2);
  });

  it("shows a human description for a CRON schedule in the list", async () => {
    const weeklySched = { ...SAMPLE_SCHED, id: "s2", cronExpr: "0 9 * * 1,2,3,4,5" };
    schedApiView.listSchedules.mockResolvedValue({ schedules: [weeklySched] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    const row = w.find("[data-testid='schedule-row']");
    expect(row.text()).toContain("09:00");
    expect(row.text().toLowerCase()).toContain("weekday");
  });

  it("pauseSchedule is called with id on pause click", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [SAMPLE_SCHED] });
    schedApiView.pauseSchedule.mockResolvedValue({ schedule: { ...SAMPLE_SCHED, state: "PAUSED" } });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='pause-s1']").trigger("click");
    await flushPromises();
    expect(schedApiView.pauseSchedule).toHaveBeenCalledWith("s1");
  });

  it("resumeSchedule is called with id on resume click", async () => {
    const paused = { ...SAMPLE_SCHED, state: "PAUSED" };
    schedApiView.listSchedules.mockResolvedValue({ schedules: [paused] });
    schedApiView.resumeSchedule.mockResolvedValue({ schedule: SAMPLE_SCHED });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='resume-s1']").trigger("click");
    await flushPromises();
    expect(schedApiView.resumeSchedule).toHaveBeenCalledWith("s1");
  });

  it("deleteSchedule is called with id on delete click", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [SAMPLE_SCHED] });
    schedApiView.deleteSchedule.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-s1']").trigger("click");
    await flushPromises();
    expect(schedApiView.deleteSchedule).toHaveBeenCalledWith("s1");
  });

  it("edit button opens an inline form prefilled from the row and saves changes", async () => {
    const withTools = { ...SAMPLE_SCHED, approvedTools: ["web.fetch", "doc.read"] };
    schedApiView.listSchedules.mockResolvedValue({ schedules: [withTools] });
    schedApiView.updateSchedule.mockResolvedValue({ schedule: { ...withTools, prompt: "edited report" } });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    // No form until Edit clicked; the read-only prompt shows.
    expect(w.find("[data-testid='edit-form-s1']").exists()).toBe(false);
    await w.find("[data-testid='edit-s1']").trigger("click");
    await flushPromises();

    const promptField = w.find("[data-testid='edit-prompt-s1']");
    expect(promptField.exists()).toBe(true);
    expect(promptField.element.value).toBe("daily report"); // prefilled

    await promptField.setValue("edited report");
    await w.find("[data-testid='save-s1']").trigger("click");
    await flushPromises();

    // approvedTools are resent verbatim so the broker doesn't clear them.
    expect(schedApiView.updateSchedule).toHaveBeenCalledWith(
      "s1",
      expect.objectContaining({
        prompt: "edited report",
        kind: "CRON",
        cronExpr: `${TZ_PREFIX}0 9 * * *`,
        approvedTools: ["web.fetch", "doc.read"],
      }),
    );
    // Form closes and the row reflects the update.
    expect(w.find("[data-testid='edit-form-s1']").exists()).toBe(false);
    expect(w.text()).toContain("edited report");
  });

  it("cancel closes the edit form without calling updateSchedule", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [SAMPLE_SCHED] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-s1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='edit-form-s1']").exists()).toBe(true);

    await w.find("[data-testid='cancel-edit-s1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='edit-form-s1']").exists()).toBe(false);
    expect(schedApiView.updateSchedule).not.toHaveBeenCalled();
  });

  it("renders a workflow-mode row with a Workflow badge and the display name instead of prompt text", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [WORKFLOW_SCHED] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    const row = w.find("[data-testid='schedule-row']");
    expect(row.find("[data-testid='workflow-badge']").exists()).toBe(true);
    expect(row.text()).toContain("Weekly digest");
  });

  it("falls back to '(deleted workflow)' when a workflow row's display name is empty", async () => {
    const deletedWorkflowSched = { ...WORKFLOW_SCHED, workflowDisplayName: "" };
    schedApiView.listSchedules.mockResolvedValue({ schedules: [deletedWorkflowSched] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='schedule-row']").text()).toContain("(deleted workflow)");
  });

  it("edit on a workflow row shows timing controls only — no prompt textarea", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [WORKFLOW_SCHED] });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-s2']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='edit-form-s2']").exists()).toBe(true);
    expect(w.find("[data-testid='edit-prompt-s2']").exists()).toBe(false);
    expect(w.find("[data-testid='edit-kind-s2']").exists()).toBe(true);
  });

  it("saving a workflow row's timing edit sends prompt/approvedTools empty, no workflow binding change", async () => {
    schedApiView.listSchedules.mockResolvedValue({ schedules: [WORKFLOW_SCHED] });
    schedApiView.updateSchedule.mockResolvedValue({ schedule: WORKFLOW_SCHED });
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-s2']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='save-s2']").trigger("click");
    await flushPromises();

    expect(schedApiView.updateSchedule).toHaveBeenCalledWith(
      "s2",
      expect.objectContaining({
        prompt: "",
        approvedTools: [],
        kind: "CRON",
        cronExpr: `${TZ_PREFIX}0 9 * * *`,
      }),
    );
  });

  it("shows error banner on API error", async () => {
    schedApiView.listSchedules.mockRejectedValue(new Error("server error"));
    const router = makeRouter();
    await router.push("/schedules");
    const w = mount(Schedules, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
  });
});
