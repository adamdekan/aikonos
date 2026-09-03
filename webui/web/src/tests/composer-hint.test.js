// Composer.vue — rotating placeholder hint overlay.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
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

function mountComposer(props = {}) {
  return mount(Composer, {
    props: { modelValue: "", placeholder: "Message Aikonos…", ...props },
    global: { plugins: [createPinia()] },
  });
}

describe("Composer.vue — rotating hint", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("shows the base placeholder first, then cycles the # / @ / / examples", async () => {
    const w = mountComposer();
    const hint = () => w.find("[data-testid='composer-hint']");

    expect(hint().exists()).toBe(true);
    expect(hint().text()).toBe("Message Aikonos…");

    // 5s tick → fade out, 300ms → advance to the first example.
    vi.advanceTimersByTime(5000);
    vi.advanceTimersByTime(300);
    await w.vm.$nextTick();
    expect(hint().text()).toContain("#");

    vi.advanceTimersByTime(5300);
    await w.vm.$nextTick();
    expect(hint().text()).toContain("@");

    vi.advanceTimersByTime(5300);
    await w.vm.$nextTick();
    expect(hint().text()).toContain("/");
  });

  it("hides the hint overlay once the input is non-empty", async () => {
    const w = mountComposer({ modelValue: "typing" });
    expect(w.find("[data-testid='composer-hint']").exists()).toBe(false);
  });
});
