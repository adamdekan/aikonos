import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount } from "@vue/test-utils";
import { createRouter, createMemoryHistory } from "vue-router";
import { createPinia, setActivePinia } from "pinia";
import { defineComponent, onMounted, onUnmounted, h } from "vue";
import { useUserStore } from "../store/user.js";
import App from "../App.vue";
import { routes } from "../router.js";
import { autoScrollbar } from "../lib/autoScrollbar.js";

// App.vue uses the globally-registered v-autoscroll directive; provide it here.
const globalDirectives = { autoscroll: autoScrollbar };

// Mock oidc.js so the router guard doesn't try to redirect in tests.
vi.mock("../auth/oidc.js", () => ({
  getUser:        vi.fn().mockResolvedValue({ expired: false, profile: { email: "alice@example.com", sub: "sub-alice" } }),
  login:          vi.fn(),
  logout:         vi.fn(),
  getAccessToken: vi.fn().mockResolvedValue("test-token"),
  handleCallback: vi.fn(),
}));

// Stub route component that records mount/unmount calls
function makeSpyComponent(mountedFn, unmountedFn) {
  return defineComponent({
    setup() {
      onMounted(mountedFn);
      onUnmounted(unmountedFn);
      return () => h("div", "stub");
    },
  });
}

async function buildApp() {
  const pinia = createPinia();
  setActivePinia(pinia);

  // Seed a starting user so the `:key="userStore.user"` binding has a baseline.
  const store = useUserStore(pinia);
  store.setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });

  // Build a real memory router but replace the "/" route component with our spy
  const mounted = vi.fn();
  const unmounted = vi.fn();
  const StubView = makeSpyComponent(mounted, unmounted);

  const testRoutes = [
    { path: "/", component: StubView, meta: { public: true } },
    ...routes.filter((r) => r.path !== "/").map(r => ({ ...r, meta: { ...r.meta, public: true } })),
  ];
  const router = createRouter({ history: createMemoryHistory(), routes: testRoutes });
  await router.push("/");
  await router.isReady();

  const wrapper = mount(App, {
    global: { plugins: [router, pinia], directives: globalDirectives },
  });
  await wrapper.vm.$nextTick();

  return { wrapper, pinia, router, mounted, unmounted };
}

describe("App.vue — router-view keyed on user", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("renders a router-view", async () => {
    const { wrapper } = await buildApp();
    expect(wrapper.find(".main-pane").exists()).toBe(true);
  });

  it("remounts the route component when user switches to a DIFFERENT user", async () => {
    const { wrapper, pinia, mounted, unmounted } = await buildApp();
    const store = useUserStore(pinia);

    // Initial mount fires once
    expect(mounted).toHaveBeenCalledTimes(1);

    // Switch to a different user
    store.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    await wrapper.vm.$nextTick();
    await wrapper.vm.$nextTick();

    // Should have unmounted the old instance and mounted a new one
    expect(unmounted).toHaveBeenCalledTimes(1);
    expect(mounted).toHaveBeenCalledTimes(2);
  });

  it("does NOT remount the route component when setFromProfile called with the same user", async () => {
    const { wrapper, pinia, mounted, unmounted } = await buildApp();
    const store = useUserStore(pinia);

    expect(mounted).toHaveBeenCalledTimes(1);

    // Set to the same user that was seeded in buildApp
    store.setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    await wrapper.vm.$nextTick();
    await wrapper.vm.$nextTick();

    // key unchanged → no remount
    expect(unmounted).toHaveBeenCalledTimes(0);
    expect(mounted).toHaveBeenCalledTimes(1);
  });
});

describe("App.vue — shell gated on authentication", () => {
  beforeEach(() => localStorage.clear());

  // Build App on a given route WITHOUT seeding a user, so userStore.user is empty.
  async function buildUnauthed({ path, isPublic }) {
    const pinia = createPinia();
    setActivePinia(pinia);
    useUserStore(pinia); // instantiate, leave user empty
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path, component: { template: "<div class='stub-view'>view</div>" }, meta: { public: isPublic } }],
    });
    await router.push(path);
    await router.isReady();
    const wrapper = mount(App, { global: { plugins: [router, pinia], directives: globalDirectives } });
    await wrapper.vm.$nextTick();
    return wrapper;
  }

  it("renders NO shell (no Sidebar, no content) for an unauthenticated non-public route", async () => {
    const wrapper = await buildUnauthed({ path: "/", isPublic: false });
    expect(wrapper.find(".app-shell").exists()).toBe(false);
    expect(wrapper.find(".main-pane").exists()).toBe(false);
    expect(wrapper.find(".stub-view").exists()).toBe(false);
    expect(wrapper.find("[data-testid='auth-splash']").exists()).toBe(true);
  });

  it("renders a bare public route (e.g. OIDC callback) with no shell when unauthenticated", async () => {
    const wrapper = await buildUnauthed({ path: "/auth/callback", isPublic: true });
    expect(wrapper.find(".app-shell").exists()).toBe(false);
    expect(wrapper.find("[data-testid='auth-splash']").exists()).toBe(false);
    expect(wrapper.find(".stub-view").exists()).toBe(true);
  });
});
