import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useUserStore } from "../store/user.js";

// Mock client.js before importing adminStatus so we control `get`.
vi.mock("../api/client.js", () => ({
  get: vi.fn(),
  post: vi.fn(),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { refreshAdminStatus } from "../api/adminStatus.js";

describe("refreshAdminStatus", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
  });

  it("sets isAdmin=true when GET /admin/assignments returns 200 (no forbidden flag)", async () => {
    clientMod.get.mockResolvedValueOnce({ assignments: [] });
    const store = useUserStore();
    expect(store.isAdmin).toBe(false);
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(true);
  });

  it("sets isAdmin=false when GET /admin/assignments returns {forbidden:true}", async () => {
    clientMod.get.mockResolvedValueOnce({ forbidden: true });
    const store = useUserStore();
    store.isAdmin = true; // start as true to confirm it flips
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(false);
  });

  it("200 clears adminStatusUnknown", async () => {
    clientMod.get.mockResolvedValueOnce({ assignments: [] });
    const store = useUserStore();
    store.adminStatusUnknown = true;
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(true);
    expect(store.adminStatusUnknown).toBe(false);
  });

  it("{forbidden:true} clears adminStatusUnknown", async () => {
    clientMod.get.mockResolvedValueOnce({ forbidden: true });
    const store = useUserStore();
    store.isAdmin = true;
    store.adminStatusUnknown = true;
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(false);
    expect(store.adminStatusUnknown).toBe(false);
  });

  it("thrown error while isAdmin=true does NOT downgrade; sets adminStatusUnknown=true", async () => {
    clientMod.get.mockRejectedValueOnce(new Error("network error"));
    const store = useUserStore();
    store.isAdmin = true;
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(true);
    expect(store.adminStatusUnknown).toBe(true);
  });

  it("thrown error while isAdmin=false (first-load failure) keeps isAdmin=false; sets adminStatusUnknown=true", async () => {
    clientMod.get.mockRejectedValueOnce(new Error("502 bad gateway"));
    const store = useUserStore();
    expect(store.isAdmin).toBe(false);
    await refreshAdminStatus();
    expect(store.isAdmin).toBe(false);
    expect(store.adminStatusUnknown).toBe(true);
  });
});
