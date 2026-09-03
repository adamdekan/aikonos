import { describe, it, expect, vi } from "vitest";
import { mount } from "@vue/test-utils";
import { createPinia } from "pinia";
import Composer from "../components/Composer.vue";

// Composer's onMounted unconditionally loads the workspace store (CP6,
// ) — mock it here so this suite doesn't leak
// a real (token-less) client.js call.
vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn().mockResolvedValue({
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  }),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));

describe("Composer.vue — stop button", () => {
  it("renders Send button (aria-label=Send) when running=false", () => {
    const w = mount(Composer, { props: { modelValue: "", running: false }, global: { plugins: [createPinia()] } });
    const btn = w.find("[aria-label='Send']");
    expect(btn.exists()).toBe(true);
    const stop = w.find("[aria-label='Stop']");
    expect(stop.exists()).toBe(false);
  });

  it("renders Stop button (aria-label=Stop) when running=true", () => {
    const w = mount(Composer, { props: { modelValue: "", running: true }, global: { plugins: [createPinia()] } });
    const stop = w.find("[aria-label='Stop']");
    expect(stop.exists()).toBe(true);
    const send = w.find("[aria-label='Send']");
    expect(send.exists()).toBe(false);
  });

  it("clicking Stop emits 'stop'", async () => {
    const w = mount(Composer, { props: { modelValue: "", running: true }, global: { plugins: [createPinia()] } });
    await w.find("[aria-label='Stop']").trigger("click");
    expect(w.emitted("stop")).toBeTruthy();
    expect(w.emitted("stop").length).toBe(1);
  });

  it("submit emits 'submit' when running=false", async () => {
    const w = mount(Composer, { props: { modelValue: "hello", running: false }, global: { plugins: [createPinia()] } });
    await w.find("form").trigger("submit");
    expect(w.emitted("submit")).toBeTruthy();
  });

  it("textarea is disabled when running=true", async () => {
    const w = mount(Composer, { props: { modelValue: "", running: true }, global: { plugins: [createPinia()] } });
    const ta = w.find("textarea");
    expect(ta.attributes("disabled")).toBeDefined();
  });

  it("textarea is not disabled when running=false and disabled=false", async () => {
    const w = mount(Composer, { props: { modelValue: "", running: false, disabled: false }, global: { plugins: [createPinia()] } });
    const ta = w.find("textarea");
    expect(ta.attributes("disabled")).toBeUndefined();
  });

  it("stop button has class composer-stop", () => {
    const w = mount(Composer, { props: { modelValue: "", running: true }, global: { plugins: [createPinia()] } });
    const stop = w.find("[aria-label='Stop']");
    expect(stop.classes()).toContain("composer-stop");
  });
});
