// Tests for the sidebar-gear UserSettingsModal.
// Modal.vue teleports its content to document.body, so DOM assertions query
// document directly (mirroring ui-modal.test.js's convention) rather than
// wrapper.find, which does not traverse Teleport in this vue-test-utils version.
import { mount } from "@vue/test-utils";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import UserSettingsModal from "../components/UserSettingsModal.vue";
import { useUserStore } from "../store/user.js";
import { useThemeStore } from "../store/theme.js";
import { usePrefsStore } from "../store/prefs.js";

vi.mock("../auth/oidc.js", () => ({
  logout:         vi.fn().mockResolvedValue(undefined),
  login:          vi.fn(),
  getUser:        vi.fn().mockResolvedValue(null),
  getAccessToken: vi.fn().mockResolvedValue(null),
  handleCallback: vi.fn(),
}));

function click(el) {
  el.dispatchEvent(new MouseEvent("click", { bubbles: true }));
}

function setInputValue(el, value) {
  el.value = value;
  el.dispatchEvent(new Event("change", { bubbles: true }));
}

function setChecked(el, checked) {
  el.checked = checked;
  el.dispatchEvent(new Event("change", { bubbles: true }));
}

let wrapper;

describe("UserSettingsModal.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  afterEach(() => {
    if (wrapper) wrapper.unmount();
    wrapper = null;
  });

  it("renders the four categories and defaults to Account", () => {
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    const railButtons = document.querySelectorAll("[data-testid^='settings-cat-']");
    const labels = Array.from(railButtons).map((b) => b.textContent.trim());
    expect(labels).toEqual(["Account", "Appearance", "Chat", "Memory"]);
    const active = document.querySelector("[data-testid='settings-cat-account']");
    expect(active.getAttribute("aria-current")).toBe("true");
  });

  it("switches panes when a rail button is clicked", async () => {
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    click(document.querySelector("[data-testid='settings-cat-appearance']"));
    await wrapper.vm.$nextTick();
    expect(document.querySelector("[data-testid='settings-cat-appearance']").getAttribute("aria-current")).toBe("true");
    expect(document.querySelector("[data-testid='settings-cat-account']").getAttribute("aria-current")).toBeNull();
    expect(document.querySelector("[data-testid='theme-radio-system']")).not.toBeNull();
  });

  it("shows the display name and email on the Account pane", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    expect(document.body.textContent).toContain("Alice");
    expect(document.body.textContent).toContain("alice@example.com");
  });

  it("clicking the theme radio calls setMode", async () => {
    const themeStore = useThemeStore();
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    click(document.querySelector("[data-testid='settings-cat-appearance']"));
    await wrapper.vm.$nextTick();
    setChecked(document.querySelector("[data-testid='theme-radio-light']"), true);
    await wrapper.vm.$nextTick();
    expect(themeStore.mode).toBe("light");
  });

  it("chat controls mutate the prefs store", async () => {
    const prefs = usePrefsStore();
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    click(document.querySelector("[data-testid='settings-cat-chat']"));
    await wrapper.vm.$nextTick();
    // chat-persist is a ToggleSwitch (role=switch button) — each click flips it.
    click(document.querySelector("[data-testid='chat-persist-checkbox']"));
    await wrapper.vm.$nextTick();
    expect(prefs.chatPersistEnabled).toBe(false);
    expect(document.querySelector("[data-testid='chat-persist-turns']").hasAttribute("disabled")).toBe(true);

    click(document.querySelector("[data-testid='chat-persist-checkbox']"));
    await wrapper.vm.$nextTick();
    setInputValue(document.querySelector("[data-testid='chat-persist-turns']"), 50);
    await wrapper.vm.$nextTick();
    expect(prefs.chatPersistTurns).toBe(50);
  });

  it("agent instructions textarea mutates the prefs store and shows the counter", async () => {
    const prefs = usePrefsStore();
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    click(document.querySelector("[data-testid='settings-cat-chat']"));
    await wrapper.vm.$nextTick();

    const textarea = document.querySelector("[data-testid='chat-instructions']");
    expect(textarea).not.toBeNull();
    expect(textarea.getAttribute("maxlength")).toBe("2000");

    textarea.value = "answer in German";
    textarea.dispatchEvent(new Event("input", { bubbles: true }));
    await wrapper.vm.$nextTick();
    expect(prefs.chatInstructions).toBe("answer in German");
    expect(document.querySelector("[data-testid='chat-instructions-counter']").textContent)
      .toContain("16 / 2000");
  });

  it("Sign out calls store.clear() then logout()", async () => {
    const { logout } = await import("../auth/oidc.js");
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    const callOrder = [];
    const clearSpy = vi.spyOn(store, "clear").mockImplementation(() => callOrder.push("clear"));
    logout.mockImplementation(() => {
      callOrder.push("logout");
      return Promise.resolve();
    });
    click(document.querySelector("[data-testid='settings-signout-btn']"));
    await wrapper.vm.$nextTick();
    await Promise.resolve();
    expect(callOrder).toEqual(["clear", "logout"]);
    clearSpy.mockRestore();
  });

  it("emits close when Close is clicked", async () => {
    wrapper = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    click(document.querySelector("[data-testid='settings-close-btn']"));
    await wrapper.vm.$nextTick();
    expect(wrapper.emitted("close")).toBeTruthy();
  });
});
