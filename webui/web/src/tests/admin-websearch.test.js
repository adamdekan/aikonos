// Tests for views/admin/WebSearchConfig.vue — org-wide web.search engine admin
// panel.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  getWebSearchConfig:    vi.fn(),
  upsertWebSearchConfig: vi.fn(),
  deleteWebSearchConfig: vi.fn(),
  testWebSearchConfig:   vi.fn(),
}));

import WebSearchConfig from "../views/admin/WebSearchConfig.vue";
import * as adminApi from "../api/admin.js";
import { useToast } from "../components/ui/useToast.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/settings", component: WebSearchConfig },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountView() {
  const router = makeRouter();
  await router.push("/admin/settings");
  const w = mount(WebSearchConfig, { global: { plugins: [router] }, attachTo: document.body });
  await flushPromises();
  return w;
}

function q(testid) {
  return document.querySelector(`[data-testid='${testid}']`);
}

function setField(testid, value, evt = "input") {
  const el = q(testid);
  el.value = value;
  el.dispatchEvent(new Event(evt));
}

const CONFIGURED = {
  engine: "brave",
  maxResults: 5,
  hasKey: true,
  updatedBy: "admin@example.com",
  updatedAt: "2026-07-10T00:00:00Z",
};

describe("WebSearchConfig.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    const { toasts } = useToast();
    toasts.splice(0);
  });
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("loads and renders the stored config", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    await mountView();

    expect(adminApi.getWebSearchConfig).toHaveBeenCalledTimes(1);
    expect(q("websearch-engine").value).toBe("brave");
    expect(q("websearch-max-results").value).toBe("5");
    // The key is never pre-filled — hasKey only drives the placeholder.
    expect(q("websearch-api-key").value).toBe("");
    expect(q("websearch-api-key").placeholder).toContain("unchanged");
  });

  it("engine select offers exactly brave, exa, tavily", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    await mountView();

    const values = Array.from(q("websearch-engine").options).map((o) => o.value);
    expect(values).toEqual(["brave", "exa", "tavily"]);
  });

  it("renders the forbidden empty-state for a non-admin caller", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ forbidden: true });
    await mountView();

    expect(q("forbidden")).not.toBeNull();
    expect(q("websearch-save-btn")).toBeNull();
  });

  it("blank-key-preserve: submitting without touching the key sends apiKey: \"\"", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.upsertWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    await mountView();

    q("websearch-save-btn").click();
    await flushPromises();

    expect(adminApi.upsertWebSearchConfig).toHaveBeenCalledTimes(1);
    const [payload] = adminApi.upsertWebSearchConfig.mock.calls[0];
    expect(payload).toEqual({
      engine: "brave",
      maxResults: 5,
      apiKey: "",
    });
  });

  it("sends the new key when the admin types one", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.upsertWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    await mountView();

    setField("websearch-api-key", "new-key-value");
    q("websearch-save-btn").click();
    await flushPromises();

    const [payload] = adminApi.upsertWebSearchConfig.mock.calls[0];
    expect(payload.apiKey).toBe("new-key-value");
  });

  it("never renders the API key value anywhere after load or save", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.upsertWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    const w = await mountView();

    q("websearch-save-btn").click();
    await flushPromises();

    expect(w.html()).not.toContain("new-key-value");
    expect(q("websearch-api-key").value).toBe("");
  });

  it("Test connection ok renders the success detail", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.testWebSearchConfig.mockResolvedValue({ ok: true, detail: "Brave probe succeeded" });
    await mountView();

    q("websearch-test-btn").click();
    await flushPromises();

    const result = q("websearch-test-result");
    expect(result).not.toBeNull();
    expect(result.textContent).toContain("Brave probe succeeded");
  });

  it("Test connection failure renders the detail", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.testWebSearchConfig.mockResolvedValue({
      ok: false,
      detail: "invalid API key",
    });
    await mountView();

    q("websearch-test-btn").click();
    await flushPromises();

    const result = q("websearch-test-result");
    expect(result).not.toBeNull();
    expect(result.textContent).toContain("invalid API key");
  });

  it("Delete asks for confirmation, then calls deleteWebSearchConfig and resets the form", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    adminApi.deleteWebSearchConfig.mockResolvedValue({});
    vi.stubGlobal("confirm", () => true);
    await mountView();

    q("websearch-delete-btn").click();
    await flushPromises();

    expect(adminApi.deleteWebSearchConfig).toHaveBeenCalledTimes(1);
    expect(q("websearch-max-results").value).toBe("10");
    expect(q("websearch-api-key").placeholder).not.toContain("unchanged");
  });

  it("Delete does nothing when the confirmation is declined", async () => {
    adminApi.getWebSearchConfig.mockResolvedValue({ config: CONFIGURED });
    vi.stubGlobal("confirm", () => false);
    await mountView();

    q("websearch-delete-btn").click();
    await flushPromises();

    expect(adminApi.deleteWebSearchConfig).not.toHaveBeenCalled();
    expect(q("websearch-max-results").value).toBe("5");
  });
});
