// Tests for store/user.js — principal derived from OIDC profile; logout clears it.
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";

// We mock auth/oidc.js so the store can be tested without a real UserManager.
// The mock is declared at the module level so vi.mock hoisting works.
vi.mock("../auth/oidc.js", () => ({
  getUser: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  getAccessToken: vi.fn(),
  handleCallback: vi.fn(),
}));

describe("store/user.js", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.resetAllMocks();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  async function importFresh() {
    vi.resetModules();
    vi.mock("../auth/oidc.js", () => ({
      getUser: vi.fn(),
      login: vi.fn(),
      logout: vi.fn(),
      getAccessToken: vi.fn(),
      handleCallback: vi.fn(),
    }));
    const { useUserStore } = await import("../store/user.js");
    return { useUserStore };
  }

  it("user returns email from oidc profile when set", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    expect(store.user).toBe("alice@example.com");
  });

  it("displayName is capitalised first name from email local part", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    store.setFromProfile({ email: "bob@example.com", sub: "sub-bob" });
    expect(store.displayName).toBe("Bob");
  });

  it("sub is exposed on the store", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    store.setFromProfile({ email: "admin@example.com", sub: "sub-admin" });
    expect(store.sub).toBe("sub-admin");
  });

  it("logout clears user and sub", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    store.clear();
    expect(store.user).toBe("");
    expect(store.sub).toBe("");
  });

  it("user is empty string when no profile is set", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    expect(store.user).toBe("");
  });

  it("does NOT write anything to localStorage", async () => {
    setActivePinia(createPinia());
    const { useUserStore } = await importFresh();
    const store = useUserStore();
    const setSpy = vi.spyOn(Storage.prototype, "setItem");
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    expect(setSpy).not.toHaveBeenCalled();
  });
});
