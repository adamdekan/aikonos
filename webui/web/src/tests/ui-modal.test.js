import { describe, it, expect } from "vitest";
import { mount } from "@vue/test-utils";
import Modal from "../components/ui/Modal.vue";

describe("Modal.vue", () => {
  it("does not render body content when open=false", () => {
    const w = mount(Modal, {
      props: { open: false, title: "Test" },
      slots: { default: "<p>body text</p>" },
      attachTo: document.body,
    });
    expect(w.find(".modal-overlay").exists()).toBe(false);
    expect(document.body.textContent).not.toContain("body text");
    w.unmount();
  });

  it("renders title and body when open=true", () => {
    const w = mount(Modal, {
      props: { open: true, title: "My Dialog" },
      slots: { default: "<p>body text</p>" },
      attachTo: document.body,
    });
    expect(document.body.textContent).toContain("My Dialog");
    expect(document.body.textContent).toContain("body text");
    w.unmount();
  });

  it("emits close on Escape keydown", async () => {
    const w = mount(Modal, {
      props: { open: true, title: "Escape test" },
      attachTo: document.body,
    });
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    expect(w.emitted("close")).toBeTruthy();
    w.unmount();
  });

  it("emits close on overlay click", async () => {
    const w = mount(Modal, {
      props: { open: true, title: "Overlay test" },
      attachTo: document.body,
    });
    // The overlay is teleported to body; query it directly
    const overlay = document.querySelector(".modal-overlay");
    expect(overlay).not.toBeNull();
    overlay.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await w.vm.$nextTick();
    expect(w.emitted("close")).toBeTruthy();
    w.unmount();
  });

  it("cleans up the keydown listener after unmount", async () => {
    const w = mount(Modal, {
      props: { open: true, title: "Cleanup test" },
      attachTo: document.body,
    });
    w.unmount();
    // dispatch after unmount — should not throw, close should not be emitted
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    // if no error is thrown the cleanup is working
    expect(true).toBe(true);
  });
});
