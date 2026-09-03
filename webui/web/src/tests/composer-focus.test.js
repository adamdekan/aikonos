// Composer.vue — keyboard focus tests.
// Verifies that the textarea receives focus on mount (unless disabled) and that
// focus is restored after running→false (post-send re-enable).
import { describe, it, expect, afterEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia } from "pinia";
import Composer from "../components/Composer.vue";

// Composer's onMounted unconditionally loads the workspace store (CP6,
// ) — mock it here so this focus-behavior
// suite doesn't leak a real (token-less) client.js call.
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
  // attachTo: document.body is required — jsdom only tracks document.activeElement
  // for elements that are part of the document tree.
  return mount(Composer, {
    props: { modelValue: "", ...props },
    attachTo: document.body,
    global: { plugins: [createPinia()] },
  });
}

describe("Composer.vue — textarea focus", () => {
  let wrapper;

  afterEach(() => {
    wrapper?.unmount();
  });

  it("on mount (not disabled), textarea receives focus", async () => {
    wrapper = mountComposer();
    await flushPromises();
    const ta = wrapper.find("textarea.composer-input").element;
    expect(ta).toBe(document.activeElement);
  });

  it("on mount with disabled:true, textarea is NOT the active element", async () => {
    wrapper = mountComposer({ disabled: true });
    await flushPromises();
    const ta = wrapper.find("textarea.composer-input").element;
    expect(ta).not.toBe(document.activeElement);
  });

  it("when running flips true→false, focus returns to the textarea", async () => {
    wrapper = mountComposer({ running: false });
    await flushPromises();

    // Simulate send: running goes true (textarea becomes disabled)
    await wrapper.setProps({ running: true });

    // Move focus to a real focusable element so we can prove it left the textarea.
    // document.body.focus() is a no-op in jsdom (body has no tabIndex), so we
    // create a temporary input, attach it, and assert it actually received focus
    // before continuing — confirming the textarea is genuinely not active.
    const tempInput = document.createElement("input");
    document.body.appendChild(tempInput);
    tempInput.focus();
    expect(tempInput).toBe(document.activeElement);

    // Send completes: running goes false → textarea re-enables → focus restored
    await wrapper.setProps({ running: false });
    await flushPromises();

    const ta = wrapper.find("textarea.composer-input").element;
    expect(ta).toBe(document.activeElement);

    document.body.removeChild(tempInput);
  });
});
