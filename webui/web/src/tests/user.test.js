import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useUserStore } from "../store/user.js";

// USERS, STORAGE_KEY, localStorage persistence, and the setUser shim are removed —
// identity comes from the OIDC token via setFromProfile.

describe("store/user.js", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("displayName derives first name with capital from email", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    expect(store.displayName).toBe("Alice");
  });

  it("displayName is empty string when no user is set", () => {
    const store = useUserStore();
    expect(store.displayName).toBe("");
  });

  it("displayName capitalises any local part", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "bob@example.com", sub: "bob@example.com" });
    expect(store.displayName).toBe("Bob");
  });

  it("setFromProfile sets the user email", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    expect(store.user).toBe("admin@example.com");
  });

  it("setFromProfile does NOT write to localStorage", () => {
    const setSpy = vi.spyOn(Storage.prototype, "setItem");
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    expect(setSpy).not.toHaveBeenCalled();
  });

  it("initial user is empty string (no static USERS default)", () => {
    const store = useUserStore();
    expect(store.user).toBe("");
  });

  it("clear resets user, sub, and isAdmin", () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "sub-alice" });
    store.isAdmin = true;
    store.clear();
    expect(store.user).toBe("");
    expect(store.sub).toBe("");
    expect(store.isAdmin).toBe(false);
  });
});
