// Tests for views/admin/Observability.vue — read-only OTLP export state.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/admin.js", () => ({
  getObservability: vi.fn(),
}));

import Observability from "../views/admin/Observability.vue";
import * as adminApi from "../api/admin.js";

async function mountView() {
  const w = mount(Observability, { global: { plugins: [createPinia()] } });
  await flushPromises();
  return w;
}

describe("Observability.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("shows Enabled + endpoint + exported_job when export is configured", async () => {
    adminApi.getObservability.mockResolvedValue({
      otelEndpoint: "otel-collector:4317",
      exportEnabled: true,
      exportedJob: "aikonos-broker",
    });
    const w = await mountView();

    expect(adminApi.getObservability).toHaveBeenCalled();
    expect(w.find("[data-testid='obs-status']").text()).toBe("Enabled");
    expect(w.find("[data-testid='obs-endpoint']").text()).toContain("otel-collector:4317");
    expect(w.text()).toContain('exported_job="aikonos-broker"');
  });

  it("shows Disabled and a config hint when no endpoint is set", async () => {
    adminApi.getObservability.mockResolvedValue({
      otelEndpoint: "",
      exportEnabled: false,
      exportedJob: "aikonos-broker",
    });
    const w = await mountView();

    expect(w.find("[data-testid='obs-status']").text()).toBe("Disabled");
    expect(w.find("[data-testid='obs-endpoint']").text()).toContain("not configured");
    expect(w.text()).toContain("AIKONOS_OTEL_ENDPOINT=otel-collector:4317");
  });

  it("renders the forbidden empty-state on 403 instead of a bogus card", async () => {
    // client.js resolves 403 as { forbidden: true } rather than throwing.
    adminApi.getObservability.mockResolvedValue({ forbidden: true, error: "not admin" });
    const w = await mountView();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='obs-card']").exists()).toBe(false);
    expect(w.find("[data-testid='obs-error']").exists()).toBe(false);
  });

  it("surfaces a load failure via the error banner with retry", async () => {
    adminApi.getObservability.mockRejectedValueOnce(new Error("broker down"));
    const w = await mountView();

    expect(w.find("[data-testid='obs-error']").exists()).toBe(true);
    expect(w.text()).toContain("broker down");

    // Retry re-invokes the loader; second call resolves.
    adminApi.getObservability.mockResolvedValueOnce({
      otelEndpoint: "otel-collector:4317",
      exportEnabled: true,
      exportedJob: "aikonos-broker",
    });
    await w.find("[data-testid='obs-retry']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='obs-card']").exists()).toBe(true);
  });
});
