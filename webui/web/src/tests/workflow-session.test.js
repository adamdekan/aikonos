// CP19: workflow run → persisted, inspectable session.
// WHY: after a workflow run the modal must write a source:"workflow" session
// record whose messages mirror the Chat.vue tool-entry shape, navigate to
// /chat?session=<id>, and still post ratings.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";

// ── module mocks ─────────────────────────────────────────────────────────────

vi.mock("../api/workflows.js", () => ({
  listWorkflows: vi.fn(),
  getWorkflow:   vi.fn(),
  runWorkflow:   vi.fn(),
  rateWorkflow:  vi.fn(),
}));

vi.mock("../api/sessions.js", () => ({
  listSessionFiles:      vi.fn(),
  readSession:           vi.fn(),
  writeSession:          vi.fn(),
  deleteSession:         vi.fn(),
  readManifest:          vi.fn().mockResolvedValue([]),
  writeManifest:         vi.fn().mockResolvedValue({}),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

// uuid is used to generate the session id — stub it so assertions are stable.
vi.mock("../lib/uuid.js", () => ({
  uuid: vi.fn(() => "test-session-id"),
}));

import RunWorkflowModal from "../components/RunWorkflowModal.vue";
import * as wfApi      from "../api/workflows.js";
import * as sessionsApi from "../api/sessions.js";
import { useSessionsStore } from "../store/sessions.js";

// ── helpers ───────────────────────────────────────────────────────────────────

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/",    component: { template: "<div/>" } },
      { path: "/chat", component: { template: "<div/>" } },
    ],
  });
}

const WF = {
  lineageId: "lineage-wf1",
  name: "fetch-report",
  version: 3,
  definition: {
    inputs: [{ name: "since", default: "2024-01-01" }],
    steps:  [
      { skill: "web.fetch", args: { url: "${inputs.since}" } },
      { skill: "doc.write",  args: { content: "result" } },
    ],
  },
};

const RUN_RESULT_TWO_STEPS = {
  ok: true,
  result: {
    halted: false,
    steps: [
      {
        stepIndex:    0,
        skill:        "web.fetch",
        resolvedArgs: { url: "2024-01-01" },
        allowed:      true,
        output:       "page content",
      },
      {
        stepIndex:    1,
        skill:        "doc.write",
        resolvedArgs: { content: "result" },
        allowed:      true,
        output:       "written",
      },
    ],
  },
};

const RUN_RESULT_DENIED = {
  ok: true,
  result: {
    halted: true,
    haltedAtStep: 0,
    haltReason: "runner lacks web.fetch",
    steps: [
      {
        stepIndex:    0,
        skill:        "web.fetch",
        resolvedArgs: { url: "2024-01-01" },
        allowed:      false,
        output:       null,
        denyReason:   "runner lacks web.fetch",
      },
    ],
  },
};

// A single-step run whose output is a structured object (doc.read shape) — the
// live "no output in chat" case: String(output) rendered "[object Object]" and
// the assistant text was empty.
const RUN_RESULT_OBJECT_OUTPUT = {
  ok: true,
  result: {
    halted: false,
    steps: [
      {
        stepIndex:    0,
        skill:        "doc.read",
        resolvedArgs: { path: "x.csv" },
        allowed:      true,
        output:       { path: "x.csv", content: "col1,col2\na,b", content_length: 13 },
      },
    ],
  },
};

// A multi-step run reproducing the live bug: a web.fetch scraping ~9 KB of page
// text, a reason step returning a full generated script, and a pdf.create whose
// output is a { path, size } object. None of the raw output may land in the
// visible assistant text — only the compact summary.
const SCRAPE = "SCRAPED_PAGE_TEXT ".repeat(600); // ~10 KB
const SCRIPT = "import sys\n# generated python script\nprint('hello')\n".repeat(20);
const RUN_RESULT_REASON = {
  ok: true,
  result: {
    halted: false,
    steps: [
      {
        stepIndex: 0, kind: "tool", skill: "web.fetch",
        resolvedArgs: { url: "https://example.com" }, allowed: true, output: SCRAPE,
      },
      {
        stepIndex: 1, kind: "reason", skill: "reason",
        resolvedArgs: { instruction: "compute the thing" }, allowed: true, output: SCRIPT,
      },
      {
        stepIndex: 2, kind: "tool", skill: "pdf.create",
        resolvedArgs: { output_path: "out.pdf" }, allowed: true,
        output: { path: "out.pdf", size: 20480 },
      },
    ],
  },
};

function makeModal(router, pinia, props = {}) {
  return mount(RunWorkflowModal, {
    props: {
      workflow: WF,
      visible:  true,
      ...props,
    },
    global: { plugins: [pinia, router] },
  });
}

// ── tests ─────────────────────────────────────────────────────────────────────

describe("RunWorkflowModal CP19 — session persistence", () => {
  let router;
  let pinia;

  beforeEach(() => {
    pinia = createPinia();
    setActivePinia(pinia);
    vi.clearAllMocks();
    sessionsApi.writeSession.mockResolvedValue({});
    sessionsApi.writeManifest.mockResolvedValue({});
    sessionsApi.readManifest.mockResolvedValue([]);
    router = makeRouter();
  });

  it("writes a session record with source:'workflow' after a successful run", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(sessionsApi.writeSession).toHaveBeenCalledOnce();
    const record = sessionsApi.writeSession.mock.calls[0][0];
    expect(record.source).toBe("workflow");
    expect(record.id).toBeTruthy();
  });

  it("session record has a user message containing the workflow name and inputs", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const userMsg = record.messages.find((m) => m.role === "user");
    expect(userMsg).toBeDefined();
    expect(userMsg.text).toContain("fetch-report");
  });

  it("session record has one tool entry per step in the assistant message", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const assistantMsg = record.messages.find((m) => m.role === "assistant");
    expect(assistantMsg).toBeDefined();
    expect(assistantMsg.tools).toHaveLength(2);
  });

  it("tool entries use skill as name, JSON-stringified resolvedArgs as argsJson, output as result", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const tools = record.messages.find((m) => m.role === "assistant").tools;

    expect(tools[0].name).toBe("web.fetch");
    expect(tools[0].argsJson).toBe(JSON.stringify({ url: "2024-01-01" }));
    expect(tools[0].result).toBe("page content");
    expect(tools[0].isError).toBe(false);
    expect(tools[0].done).toBe(true);

    expect(tools[1].name).toBe("doc.write");
    expect(tools[1].result).toBe("written");
    expect(tools[1].isError).toBe(false);
  });

  it("denied step is marked isError:true with denyReason as result", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_DENIED);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const tools = record.messages.find((m) => m.role === "assistant").tools;

    expect(tools).toHaveLength(1);
    expect(tools[0].name).toBe("web.fetch");
    expect(tools[0].isError).toBe(true);
    expect(tools[0].result).toBe("runner lacks web.fetch");
    expect(tools[0].done).toBe(true);
  });

  it("object step output renders as its content, never [object Object]", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_OBJECT_OUTPUT);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const tools = record.messages.find((m) => m.role === "assistant").tools;
    expect(tools[0].result).not.toContain("[object Object]");
    expect(tools[0].result).toContain("col1,col2");
  });

  it("assistant text carries the final step's text answer in full", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_OBJECT_OUTPUT);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const assistant = record.messages.find((m) => m.role === "assistant");
    expect(assistant.text).toContain("fetch-report");
    expect(assistant.text).toContain("1 step");
    // A final step whose output carries text content (doc.read) IS a text
    // response — shown in full, not collapsed to "Created <path>".
    expect(assistant.text).toContain("col1,col2\na,b");
    expect(assistant.text).not.toContain("Created x.csv");
  });

  it("a long final text answer is never truncated", async () => {
    const prose = "word ".repeat(400).trim(); // ~2000 chars, well past the JSON compaction cap
    wfApi.runWorkflow.mockResolvedValue({
      ok: true,
      result: {
        halted: false,
        steps: [
          {
            stepIndex: 0,
            kind: "reason",
            skill: "",
            resolvedArgs: { instruction: "summarize" },
            allowed: true,
            output: prose,
          },
        ],
      },
    });
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const assistant = record.messages.find((m) => m.role === "assistant");
    expect(assistant.text).toContain(prose);
    expect(assistant.text).not.toContain("…");
  });

  it("compacts a multi-step run: summary in text, raw scrape/script only on tool entries", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_REASON);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    const record = sessionsApi.writeSession.mock.calls[0][0];
    const assistant = record.messages.find((m) => m.role === "assistant");

    // Compact summary: name, count, final "Created <path> (<size>)".
    expect(assistant.text).toContain("fetch-report");
    expect(assistant.text).toContain("3 steps");
    expect(assistant.text).toContain("Created out.pdf");
    expect(assistant.text).toContain("20.0 KB");
    // Raw step output never lands in the visible text.
    expect(assistant.text).not.toContain("SCRAPED_PAGE_TEXT");
    expect(assistant.text).not.toContain("import sys");
    expect(assistant.text.length).toBeLessThan(500);

    // tools[] carries one entry per step with the right name/isError; raw output
    // stays on the entries (bounded to the store cap).
    const tools = assistant.tools;
    expect(tools).toHaveLength(3);
    expect(tools.map((t) => t.name)).toEqual(["web.fetch", "reason", "pdf.create"]);
    expect(tools.every((t) => t.isError === false && t.done === true)).toBe(true);
    expect(tools[1].result).toContain("import sys");
    expect(tools[0].result.length).toBeLessThanOrEqual(4096 + 1); // +1 for the ellipsis
  });

  it("navigates to /chat?session=<id> after run completes", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(router.currentRoute.value.path).toBe("/chat");
    expect(router.currentRoute.value.query.session).toBe("test-session-id");
  });

  it("writes the manifest with the new entry (source:'workflow')", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(sessionsApi.writeManifest).toHaveBeenCalled();
    const manifest = sessionsApi.writeManifest.mock.calls[0][0];
    const entry = manifest.find((e) => e.id === "test-session-id");
    expect(entry).toBeDefined();
    expect(entry.source).toBe("workflow");
  });

  it("rating still posts after session is written and nav fires", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    wfApi.rateWorkflow.mockResolvedValue({ ok: true });
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    // Modal must still render rating UI even after nav (modal not destroyed here).
    await w.find("[data-testid='btn-rate-success']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='btn-submit-rating']").trigger("click");
    await flushPromises();

    expect(wfApi.rateWorkflow).toHaveBeenCalledWith("lineage-wf1", {
      version: 3,
      rating:  "RATING_SUCCESS",
      note:    "",
    });
    expect(w.find("[data-testid='rating-done']").exists()).toBe(true);
  });

  it("does not write a session when run returns an error (api throws)", async () => {
    wfApi.runWorkflow.mockRejectedValue(new Error("network error"));
    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(sessionsApi.writeSession).not.toHaveBeenCalled();
    // stays on current route — no navigation
    expect(router.currentRoute.value.path).toBe("/");
  });

  it("updates the sessions store in-memory list with a source:'workflow' entry after a successful run", async () => {
    wfApi.runWorkflow.mockResolvedValue(RUN_RESULT_TWO_STEPS);
    // Obtain the store from the same pinia instance the component will use.
    const store = useSessionsStore();
    const spy = vi.spyOn(store, "upsertFromRecord");

    const w = makeModal(router, pinia);
    await w.find("[data-testid='btn-run']").trigger("click");
    await flushPromises();

    expect(spy).toHaveBeenCalledOnce();
    const passedRecord = spy.mock.calls[0][0];
    expect(passedRecord.source).toBe("workflow");
    // upsertFromRecord runs manifestEntry internally and appends to sessions.value.
    expect(store.sessions.some((s) => s.id === passedRecord.id && s.source === "workflow")).toBe(true);
  });
});
