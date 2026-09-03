// Tests for RunWorkflowModal.vue
// WHY: the modal drives the webui run path — input field rendering, runWorkflow
// call with the correct values, per-step result display, and post-run rating
// are all user-visible contracts. Mocking api/workflows.js avoids any server.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/workflows.js", () => ({
  listWorkflows:     vi.fn(),
  getWorkflow:       vi.fn(),
  runWorkflow:       vi.fn(),
  runWorkflowStream: vi.fn(),
  rateWorkflow:      vi.fn(),
}));

vi.mock("../api/sessions.js", () => ({
  writeSession: vi.fn().mockResolvedValue(undefined),
}));

import RunWorkflowModal from "../components/RunWorkflowModal.vue";
import * as wfApi from "../api/workflows.js";
import * as sessionsApi from "../api/sessions.js";

// A workflow with two declared inputs.
const WF_WITH_INPUTS = {
  lineageId: "lineage-1",
  name:      "my-report",
  version:   2,
  definition: {
    inputs: [
      { name: "since",  default: "2024-01-01" },
      { name: "query",  default: "default" },
    ],
    steps: [],
  },
};

// A workflow with no declared inputs.
const WF_NO_INPUTS = {
  lineageId: "lineage-2",
  name:      "no-inputs",
  version:   1,
  definition: { inputs: [], steps: [] },
};

// A workflow with steps + requires in the definition.
const WF_WITH_PREVIEW = {
  lineageId: "lineage-3",
  name:      "fetch-report",
  version:   1,
  definition: {
    inputs:   [],
    steps:    [
      { skill: "web.fetch",  args: {} },
      { skill: "doc.write",  args: {} },
    ],
    requires: ["skill:web.fetch", "skill:doc.write"],
  },
};

function makeModal(props = {}) {
  return mount(RunWorkflowModal, {
    props: {
      workflow: WF_WITH_INPUTS,
      visible:  true,
      ...props,
    },
    global: { plugins: [createPinia()] },
  });
}

describe("RunWorkflowModal.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("does not render when visible=false", () => {
    const w = makeModal({ visible: false });
    expect(w.find(".run-modal").exists()).toBe(false);
  });

  it("renders a field for each declared input with its default value", () => {
    const w = makeModal();
    const sinceInput = w.find("[data-testid='input-since']");
    const queryInput = w.find("[data-testid='input-query']");
    expect(sinceInput.exists()).toBe(true);
    expect(queryInput.exists()).toBe(true);
    expect(sinceInput.element.value).toBe("2024-01-01");
    expect(queryInput.element.value).toBe("default");
  });

  it("shows 'No inputs required' when definition has no inputs", () => {
    const w = makeModal({ workflow: WF_NO_INPUTS });
    expect(w.text()).toContain("No inputs required");
    expect(w.find("[data-testid='run-inputs']").exists()).toBe(true);
  });

  it("streams the run with the current input values on submit", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal();

    // Change the 'query' field value.
    const queryInput = w.find("[data-testid='input-query']");
    await queryInput.setValue("custom query");

    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(wfApi.runWorkflowStream).toHaveBeenCalledOnce();
    expect(wfApi.runWorkflowStream).toHaveBeenCalledWith(
      "lineage-1",
      { since: "2024-01-01", query: "custom query" },
      expect.objectContaining({ onStep: expect.any(Function) }),
    );
    // The blocking path is only the fallback — not touched when the stream works.
    expect(wfApi.runWorkflow).not.toHaveBeenCalled();
  });

  it("shows result section after a successful run", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({
      ok: true,
      result: { halted: false, steps: [{ stepIndex: 0, allowed: true, skill: "web.fetch" }] },
    });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='run-result']").exists()).toBe(true);
    expect(w.find("[data-testid='result-status']").text()).toContain("Completed");
    expect(w.find("[data-testid='step-item']").exists()).toBe(true);
  });

  it("shows halted message when run halts mid-way", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({
      ok: true,
      result: { halted: true, haltedAtStep: 1, haltReason: "runner lacks web.fetch", steps: [] },
    });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='result-halted']").exists()).toBe(true);
    expect(w.find("[data-testid='result-halted']").text()).toContain("runner lacks web.fetch");
  });

  it("shows an inline error when both the stream and the blocking fallback reject", async () => {
    wfApi.runWorkflowStream.mockRejectedValue(new Error("stream failed"));
    wfApi.runWorkflow.mockRejectedValue(new Error("bridge failed"));
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    // Fallback attempted after the stream failed, then its error surfaces.
    expect(wfApi.runWorkflow).toHaveBeenCalledOnce();
    expect(w.find("[data-testid='run-error']").exists()).toBe(true);
    expect(w.find("[data-testid='run-error']").text()).toContain("bridge failed");
  });

  it("falls back to the blocking run when the stream errors, then renders the result", async () => {
    wfApi.runWorkflowStream.mockRejectedValue(new Error("proxy stripped SSE"));
    wfApi.runWorkflow.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(wfApi.runWorkflow).toHaveBeenCalledWith(
      "lineage-1",
      { since: "2024-01-01", query: "default" },
      expect.any(String),
    );
    expect(w.find("[data-testid='run-result']").exists()).toBe(true);
    expect(w.find("[data-testid='result-status']").text()).toContain("Completed");
  });

  it("marks live steps done/failed as step events arrive during a streamed run", async () => {
    // WHY: the live progress list is the in-flight UX — each `step` event must
    // flip the matching pending row to done (✓) or failed (✗) by index.
    let resolveRun;
    wfApi.runWorkflowStream.mockImplementation((_id, _inputs, handlers) => {
      handlers.onStep({ index: 0, skill: "web.fetch", ok: true });
      handlers.onStep({ index: 1, skill: "doc.write", ok: false, denyReason: "not permitted" });
      return new Promise((res) => { resolveRun = res; });
    });
    const w = makeModal({ workflow: WF_WITH_PREVIEW });
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    // Still running (run promise unresolved): live list is visible.
    const live = w.findAll("[data-testid='live-step']");
    expect(live.length).toBe(2);
    expect(live[0].classes()).toContain("step-ok");
    expect(live[1].classes()).toContain("step-denied");
    expect(live[1].text()).toContain("not permitted");

    // Settle the run — result view replaces the live list.
    resolveRun({ ok: true, result: { halted: false, steps: [] } });
    await flushPromises();
    expect(w.find("[data-testid='run-progress']").exists()).toBe(false);
    expect(w.find("[data-testid='run-result']").exists()).toBe(true);
  });

  it("sends the same session id it files the session record under", async () => {
    // WHY: the id is minted before the run purely so the run's reason-step LLM
    // usage is attributed to the session the user will open. If the two ever
    // diverge, the usage is written against an id no session record uses and the
    // chat view's usage strip silently reads zero.
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const [, , opts] = wfApi.runWorkflowStream.mock.calls[0];
    const [record] = sessionsApi.writeSession.mock.calls[0];
    expect(opts.sessionId).toBeTruthy();
    expect(record.id).toBe(opts.sessionId);
  });

  it("shows rating section after run completes", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='rating-section']").exists()).toBe(true);
  });

  it("calls rateWorkflow with RATING_SUCCESS and note on submit", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    wfApi.rateWorkflow.mockResolvedValue({ ok: true });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    // Select "Good" rating.
    await w.find("[data-testid='btn-rate-success']").trigger("click");
    await flushPromises();

    // Fill in note.
    await w.find("[data-testid='rating-note']").setValue("worked great");

    await w.find("[data-testid='btn-submit-rating']").trigger("click");
    await flushPromises();

    expect(wfApi.rateWorkflow).toHaveBeenCalledOnce();
    expect(wfApi.rateWorkflow).toHaveBeenCalledWith("lineage-1", {
      version:  2,
      rating:   "RATING_SUCCESS",
      note:     "worked great",
    });
  });

  it("calls rateWorkflow with RATING_BAD on bad rating submit", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    wfApi.rateWorkflow.mockResolvedValue({ ok: true });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='btn-rate-bad']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='btn-submit-rating']").trigger("click");
    await flushPromises();

    expect(wfApi.rateWorkflow).toHaveBeenCalledWith("lineage-1", {
      version: 2,
      rating:  "RATING_BAD",
      note:    "",
    });
  });

  it("shows rating-done after successful rating submit", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    wfApi.rateWorkflow.mockResolvedValue({ ok: true });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='btn-rate-success']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='btn-submit-rating']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='rating-done']").exists()).toBe(true);
    expect(w.find("[data-testid='rating-section']").exists()).toBe(false);
  });

  it("submit-rating button is disabled when no rating is selected", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const submitBtn = w.find("[data-testid='btn-submit-rating']");
    expect(submitBtn.element.disabled).toBe(true);
  });

  // ── Preview tests ────────────────────────────────────────────────────────────

  it("preview renders each step's skill before the run", () => {
    // WHY: the spec criterion is "preview shows steps/requires" — the user must
    // see what the workflow will do before committing to Run.
    const w = makeModal({ workflow: WF_WITH_PREVIEW });
    const previewSteps = w.find("[data-testid='preview-steps']");
    expect(previewSteps.exists()).toBe(true);
    expect(previewSteps.text()).toContain("web.fetch");
    expect(previewSteps.text()).toContain("doc.write");
  });

  it("preview renders requires list before the run", () => {
    // WHY: required skills inform the user whether they hold the necessary
    // capabilities before wasting a run attempt.
    const w = makeModal({ workflow: WF_WITH_PREVIEW });
    const previewReqs = w.find("[data-testid='preview-requires']");
    expect(previewReqs.exists()).toBe(true);
    expect(previewReqs.text()).toContain("skill:web.fetch");
    expect(previewReqs.text()).toContain("skill:doc.write");
  });

  it("preview is hidden after the run completes", async () => {
    // WHY: once run, the result view replaces the pre-run panel entirely —
    // preview and result sections must not coexist.
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal({ workflow: WF_WITH_PREVIEW });
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='preview-steps']").exists()).toBe(false);
    expect(w.find("[data-testid='run-result']").exists()).toBe(true);
  });

  // ── Reason step rendering (CP-R6) ───────────────────────────────────────────

  it("maps a mixed tool/reason run result to a session record without [object Object]", async () => {
    // WHY: reason steps carry kind:"reason", skill:"", and resolvedArgs.instruction
    // instead of tool args — the session-record mapping must label them "reason"
    // and never let a parsed reason object stringify as "[object Object]".
    wfApi.runWorkflowStream.mockResolvedValue({
      ok: true,
      result: {
        halted: false,
        steps: [
          {
            kind: "tool",
            allowed: true,
            skill: "doc.read",
            resolvedArgs: { path: "registry.csv" },
            output: "row,data\n1,2",
          },
          {
            kind: "reason",
            allowed: true,
            skill: "",
            resolvedArgs: { instruction: "Find the row matching the IP" },
            output: "the matching row is row 1",
          },
          {
            kind: "reason",
            allowed: true,
            skill: "",
            resolvedArgs: { instruction: "Extract contact fields as JSON" },
            output: { email: "a@b.com", name: "A", company: "B" },
          },
          {
            kind: "tool",
            allowed: false,
            skill: "doc.write",
            resolvedArgs: { path: "alert.txt" },
            denyReason: "not permitted",
          },
        ],
      },
    });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(sessionsApi.writeSession).toHaveBeenCalledOnce();
    const record = sessionsApi.writeSession.mock.calls[0][0];
    const tools = record.messages[1].tools;

    // Existing tool-step assertions unbroken.
    expect(tools[0].name).toBe("doc.read");
    expect(tools[0].argsJson).toBe(JSON.stringify({ path: "registry.csv" }));
    expect(tools[0].result).toBe("row,data\n1,2");
    expect(tools[0].isError).toBe(false);

    // Reason step (text output): labeled "reason", instruction visible in argsJson.
    expect(tools[1].name).toBe("reason");
    expect(tools[1].argsJson).toBe(JSON.stringify({ instruction: "Find the row matching the IP" }));
    expect(tools[1].result).toBe("the matching row is row 1");

    // Reason step (parsed object output): no "[object Object]" anywhere.
    expect(tools[2].name).toBe("reason");
    expect(tools[2].result).not.toContain("[object Object]");
    expect(tools[2].result).toContain("a@b.com");

    // Denied tool step unaffected.
    expect(tools[3].name).toBe("doc.write");
    expect(tools[3].isError).toBe(true);
    expect(tools[3].result).toBe("not permitted");

    // Assistant text is the compact summary, not a dump of every step's output.
    // The run halted on the denied doc.write, so it names that failure.
    const assistantText = record.messages[1].text;
    expect(assistantText).not.toContain("[object Object]");
    expect(assistantText).toContain("my-report");
    expect(assistantText).toContain("4 steps");
    expect(assistantText).toContain("Step 4 (doc.write) was denied: not permitted");
    // Raw step output stays on the tool entries, not in the visible text.
    expect(assistantText).not.toContain("row,data");
    expect(assistantText).not.toContain("a@b.com");
  });

  it("scopes the content-string special case to tool steps only", async () => {
    // WHY: a reason step's output_schema can legitimately carry a top-level
    // string `content` field alongside other fields — the tool-output shortcut
    // (doc.read/web.fetch payloads) must not swallow the rest of that object.
    wfApi.runWorkflowStream.mockResolvedValue({
      ok: true,
      result: {
        halted: false,
        steps: [
          {
            kind: "tool",
            allowed: true,
            skill: "doc.read",
            resolvedArgs: {},
            output: { content: "just the content", other: "should not appear" },
          },
          {
            kind: "reason",
            allowed: true,
            skill: "",
            resolvedArgs: { instruction: "summarize" },
            output: { content: "reason content field", other: "also visible" },
          },
        ],
      },
    });
    const w = makeModal();
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const tools = record.messages[1].tools;

    // Tool step: byte-identical existing behavior — only `.content` renders.
    expect(tools[0].result).toBe("just the content");

    // Reason step: full JSON, both fields visible.
    expect(tools[1].result).toContain("reason content field");
    expect(tools[1].result).toContain("also visible");
    expect(tools[1].result).not.toBe("reason content field");
  });

  it("preview section exists even when definition has no steps or requires", () => {
    // WHY: the section must always render so tests can find [data-testid='run-preview']
    // regardless of workflow contents.
    const w = makeModal({ workflow: WF_NO_INPUTS });
    expect(w.find("[data-testid='run-preview']").exists()).toBe(true);
    expect(w.find("[data-testid='preview-steps']").exists()).toBe(false);
    expect(w.find("[data-testid='preview-requires']").exists()).toBe(false);
  });
});

// ── Schema-aware run inputs (Task 5) ──────────────────────────────────────────
describe("RunWorkflowModal.vue — schema-aware inputs", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  const WF_SCHEMA = {
    lineageId: "lineage-s",
    name:      "typed",
    version:   1,
    definition: {
      steps: [],
      inputs: [
        { name: "flag",   default: false, schema: { type: "boolean" } },
        { name: "count",  default: 3,     schema: { type: "integer" } },
        { name: "mode",   default: "a",   schema: { enum: ["a", "b", "c"] } },
        { name: "plain",  default: "hi" },
      ],
    },
  };

  it("maps schema type/enum to the right widget", () => {
    const w = makeModal({ workflow: WF_SCHEMA });
    expect(w.find("[data-testid='input-flag']").element.type).toBe("checkbox");
    expect(w.find("[data-testid='input-count']").element.type).toBe("number");
    expect(w.find("[data-testid='input-mode']").element.tagName).toBe("SELECT");
    expect(w.find("[data-testid='input-plain']").element.type).toBe("text");
    // enum options rendered
    const opts = w.find("[data-testid='input-mode']").findAll("option");
    expect(opts.map((o) => o.element.value)).toEqual(["a", "b", "c"]);
  });

  it("sends a numeric value for a number input", async () => {
    wfApi.runWorkflowStream.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    const w = makeModal({ workflow: WF_SCHEMA });
    await w.find("[data-testid='input-count']").setValue("7");
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const [, inputs] = wfApi.runWorkflowStream.mock.calls[0];
    expect(inputs.count).toBe(7);
    expect(typeof inputs.count).toBe("number");
    expect(inputs.flag).toBe(false);
    expect(inputs.mode).toBe("a");
  });

  it("marks inputs with no default required and blocks run until filled", async () => {
    const WF_REQ = {
      lineageId: "lineage-r",
      name:      "needs-input",
      version:   1,
      definition: { steps: [], inputs: [{ name: "q" }] }, // no default → required
    };
    const w = makeModal({ workflow: WF_REQ });
    // Required marker present.
    expect(w.find("[data-testid='input-q']").exists()).toBe(true);
    expect(w.find(".req-marker").exists()).toBe(true);
    // Run disabled while empty.
    expect(w.find("[data-testid='btn-run']").element.disabled).toBe(true);
    // Fill it → Run enabled.
    await w.find("[data-testid='input-q']").setValue("something");
    expect(w.find("[data-testid='btn-run']").element.disabled).toBe(false);
  });
});
