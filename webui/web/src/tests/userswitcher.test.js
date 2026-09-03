// UserSwitcher.vue: compact sidebar-footer row (display name + gear button)
// that opens UserSettingsModal. Inline theme toggle and footer Sign out were
// removed (both now live in the modal) —  CP5.
import { mount } from "@vue/test-utils";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import UserSwitcher from "../components/UserSwitcher.vue";
import UserSettingsModal from "../components/UserSettingsModal.vue";
import { useUserStore } from "../store/user.js";

// Mock oidc.js so the component (and the modal it mounts) can mount without a
// real UserManager.
vi.mock("../auth/oidc.js", () => ({
  logout:         vi.fn().mockResolvedValue(undefined),
  login:          vi.fn(),
  getUser:        vi.fn().mockResolvedValue(null),
  getAccessToken: vi.fn().mockResolvedValue(null),
  handleCallback: vi.fn(),
}));

describe("UserSwitcher.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("shows the authenticated user's display name", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    const wrapper = mount(UserSwitcher);
    expect(wrapper.text()).toContain("Alice");
  });

  it("shows the email as the title attribute on the name", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    const wrapper = mount(UserSwitcher);
    expect(wrapper.find(".user-name").attributes("title")).toBe("alice@example.com");
  });

  it("renders a gear button labeled 'User settings'", () => {
    const wrapper = mount(UserSwitcher);
    const gear = wrapper.find("[aria-label='User settings']");
    expect(gear.exists()).toBe(true);
  });

  it("does NOT render the inline theme toggle or a footer Sign out button", () => {
    const wrapper = mount(UserSwitcher);
    expect(wrapper.find(".theme-toggle").exists()).toBe(false);
    expect(wrapper.find(".logout-btn").exists()).toBe(false);
  });

  it("clicking the gear opens UserSettingsModal", async () => {
    const wrapper = mount(UserSwitcher);
    expect(wrapper.findComponent(UserSettingsModal).props("open")).toBe(false);
    await wrapper.find("[aria-label='User settings']").trigger("click");
    expect(wrapper.findComponent(UserSettingsModal).props("open")).toBe(true);
  });

  it("closing the modal (close emit) hides it again", async () => {
    const wrapper = mount(UserSwitcher);
    await wrapper.find("[aria-label='User settings']").trigger("click");
    expect(wrapper.findComponent(UserSettingsModal).props("open")).toBe(true);
    wrapper.findComponent(UserSettingsModal).vm.$emit("close");
    await wrapper.vm.$nextTick();
    expect(wrapper.findComponent(UserSettingsModal).props("open")).toBe(false);
  });

  it("collapsed=true renders only the centered gear button", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    const wrapper = mount(UserSwitcher, { props: { collapsed: true } });
    expect(wrapper.find(".user-name").exists()).toBe(false);
    expect(wrapper.find("[aria-label='User settings']").exists()).toBe(true);
  });

  it("collapsed=false (default) renders the name and the gear", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    const wrapper = mount(UserSwitcher);
    expect(wrapper.find(".user-name").exists()).toBe(true);
    expect(wrapper.find("[aria-label='User settings']").exists()).toBe(true);
  });
});
