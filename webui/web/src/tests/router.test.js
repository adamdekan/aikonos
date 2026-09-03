import { describe, it, expect, vi, beforeEach } from "vitest";
import { createRouter, createWebHashHistory } from "vue-router";
import { createPinia, setActivePinia } from "pinia";
import { routes, authGuard } from "../router.js";
import PolicyTabs from "../views/admin/PolicyTabs.vue";
import Settings from "../views/admin/Settings.vue";

// Mock oidc.js so the guard never triggers a real signinRedirect in tests.
vi.mock("../auth/oidc.js", () => ({
  getUser: vi.fn(),
  login:   vi.fn(),
}));

import { getUser, login } from "../auth/oidc.js";

describe("router", () => {
  const EXPECTED_PATHS = [
    "/",
    "/auth/callback",
    "/chat",
    "/files",
    "/connections",
    "/connect/callback",
    "/schedules",
    "/workflows",
    "/inbox",
    "/admin/roles",
    "/admin/network",
    "/admin/mcp",
    "/admin/runs",
    "/admin/policy",
    // old paths redirect to /admin/policy — matched[0] resolves to PolicyTabs
    "/admin/audit",
    "/admin/audit/history",
    "/admin/decisions",
    "/admin/policy/simulate",
    "/admin/alerts",
    "/admin/agents",
    "/admin/skill-bundles",
  ];

  it.each(EXPECTED_PATHS)("path %s resolves to a component", async (path) => {
    setActivePinia(createPinia());
    // Provide a valid session so the guard doesn't block navigation.
    getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });
    const router = createRouter({ history: createWebHashHistory(), routes });
    router.beforeEach(authGuard);
    await router.push(path);
    await router.isReady();
    expect(router.currentRoute.value.matched.length).toBeGreaterThan(0);
    const component = router.currentRoute.value.matched[0]?.components?.default;
    expect(component).toBeTruthy();
  });

  // Regression: /skills (personal) and /admin/skills (admin bundles) must
  // resolve to DISTINCT components. A duplicate `Skills` import binding once
  // collapsed both onto the admin view — vitest's transform tolerated the
  // duplicate declaration, but the production Rollup build rejected it. Pinning
  // the two routes to different component modules catches that class of bug
  // in the test suite instead of only at `vite build`.
  it("/skills and /admin/skills resolve to different components", async () => {
    setActivePinia(createPinia());
    getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });
    const router = createRouter({ history: createWebHashHistory(), routes });
    router.beforeEach(authGuard);
    await router.push("/skills");
    await router.isReady();
    const personal = router.currentRoute.value.matched[0]?.components?.default;
    await router.push("/admin/skills");
    await router.isReady();
    const admin = router.currentRoute.value.matched[0]?.components?.default;
    expect(personal).toBeTruthy();
    expect(admin).toBeTruthy();
    expect(personal).not.toBe(admin);
  });

  it.each(["/asdfgh", "/admin/nope", "/chat/deeper/none", "/x/y/z"])(
    "unknown path %s redirects to /chat",
    async (path) => {
      setActivePinia(createPinia());
      getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });
      const router = createRouter({ history: createWebHashHistory(), routes });
      router.beforeEach(authGuard);
      await router.push(path);
      await router.isReady();
      expect(router.currentRoute.value.path).toBe("/chat");
    },
  );

  it("/admin/audit redirects and resolves to the PolicyTabs component", async () => {
    setActivePinia(createPinia());
    getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });
    const router = createRouter({ history: createWebHashHistory(), routes });
    router.beforeEach(authGuard);
    await router.push("/admin/audit");
    await router.isReady();
    // Redirect lands on /admin/policy?tab=audit
    expect(router.currentRoute.value.path).toBe("/admin/policy");
    const component = router.currentRoute.value.matched[0]?.components?.default;
    expect(component).toBe(PolicyTabs);
  });

  // Network Access / Rate Limits / Spend Caps folded into Settings categories —
  // the old top-level paths must stay deep-linkable via ?tab=.
  it.each([
    ["/admin/network", "network"],
    ["/admin/rate-limits", "rate-limits"],
    ["/admin/spend-caps", "spend-caps"],
  ])("%s redirects to Settings with tab=%s", async (path, tab) => {
    setActivePinia(createPinia());
    getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });
    const router = createRouter({ history: createWebHashHistory(), routes });
    router.beforeEach(authGuard);
    await router.push(path);
    await router.isReady();
    expect(router.currentRoute.value.path).toBe("/admin/settings");
    expect(router.currentRoute.value.query.tab).toBe(tab);
    expect(router.currentRoute.value.matched[0]?.components?.default).toBe(Settings);
  });
});

describe("router auth guard", () => {
  let router;

  beforeEach(() => {
    vi.clearAllMocks();
    setActivePinia(createPinia());
    router = createRouter({ history: createWebHashHistory(), routes });
    router.beforeEach(authGuard);
  });

  it("calls login() when navigating to a protected route without a session", async () => {
    getUser.mockResolvedValue(null);
    login.mockResolvedValue(undefined);

    // push resolves once the navigation settles (aborted by guard returning false).
    // Do not await isReady() — the initial hash-history navigation fires the guard
    // first and also aborts, which is expected.
    await router.push("/chat");

    expect(login).toHaveBeenCalled();
  });

  it("calls login() when the session is expired", async () => {
    getUser.mockResolvedValue({ expired: true });
    login.mockResolvedValue(undefined);

    await router.push("/files");

    expect(login).toHaveBeenCalled();
  });

  it("does NOT call login() when navigating to /auth/callback (public)", async () => {
    getUser.mockResolvedValue(null);

    await router.push("/auth/callback");
    await router.isReady();

    expect(login).not.toHaveBeenCalled();
  });

  it("does NOT call login() when navigating to /connect/callback (public)", async () => {
    getUser.mockResolvedValue(null);

    await router.push("/connect/callback");
    await router.isReady();

    expect(login).not.toHaveBeenCalled();
  });

  it("allows navigation to a protected route with a valid session", async () => {
    getUser.mockResolvedValue({ expired: false, profile: { email: "u@test.dev", sub: "sub1" } });

    await router.push("/files");
    await router.isReady();

    expect(login).not.toHaveBeenCalled();
    expect(router.currentRoute.value.path).toBe("/files");
  });
});
