// CP1 (F38): discovery Pinia store — load-once guard, refresh re-fetches,
// per-dataset error state on failure (empty-result degradation preserved).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/admin.js", () => ({
  listUserSkillBundles: vi.fn(),
}));
vi.mock("../api/delegation.js", () => ({
  listDelegatableUsers: vi.fn(),
}));
vi.mock("../api/files.js", () => ({
  listFiles: vi.fn(),
}));
vi.mock("../api/skills.js", () => ({
  listSkills: vi.fn(),
}));

import { listUserSkillBundles } from "../api/admin.js";
import { listDelegatableUsers } from "../api/delegation.js";
import { listFiles } from "../api/files.js";
import { listSkills } from "../api/skills.js";
import { useDiscoveryStore } from "../store/discovery.js";

function mockHealthy() {
  listUserSkillBundles.mockResolvedValue({ bundles: [{ id: "1", name: "search" }] });
  listDelegatableUsers.mockResolvedValue({
    users: [{ userId: "a@example.com", displayName: "A" }],
    groups: [{ groupId: "g1", displayName: "Team" }],
  });
  listFiles.mockResolvedValue({ files: [{ path: "notes.txt" }] });
  listSkills.mockResolvedValue({ skills: [] });
}

describe("discovery store", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("load() populates all four datasets", async () => {
    mockHealthy();
    const store = useDiscoveryStore();
    await store.load();

    expect(store.grantedBundles).toEqual([{ id: "1", name: "search" }]);
    expect(store.delegatableUsers).toEqual([{ userId: "a@example.com", displayName: "A" }]);
    expect(store.delegatableGroups).toEqual([{ groupId: "g1", displayName: "Team" }]);
    expect(store.mentionFiles).toEqual([{ path: "notes.txt" }]);
    expect(store.errors).toEqual({
      grantedBundles: null,
      delegatableUsers: null,
      delegatableGroups: null,
      mentionFiles: null,
    });
  });

  it("a second load() is a no-op — datasets fetched exactly once", async () => {
    mockHealthy();
    const store = useDiscoveryStore();
    await store.load();
    await store.load();

    expect(listUserSkillBundles).toHaveBeenCalledTimes(1);
    expect(listDelegatableUsers).toHaveBeenCalledTimes(1);
    expect(listFiles).toHaveBeenCalledTimes(1);
  });

  it("refresh() re-fetches all datasets even after load()", async () => {
    mockHealthy();
    const store = useDiscoveryStore();
    await store.load();
    await store.refresh();

    expect(listUserSkillBundles).toHaveBeenCalledTimes(2);
    expect(listDelegatableUsers).toHaveBeenCalledTimes(2);
    expect(listFiles).toHaveBeenCalledTimes(2);
  });

  it("a failed loader empties its dataset and records the error, other datasets unaffected", async () => {
    mockHealthy();
    listDelegatableUsers.mockRejectedValue(new Error("network down"));
    const store = useDiscoveryStore();
    await store.load();

    expect(store.delegatableUsers).toEqual([]);
    expect(store.delegatableGroups).toEqual([]);
    expect(store.errors.delegatableUsers).toBe("network down");
    expect(store.errors.delegatableGroups).toBe("network down");
    // Unrelated dataset stays healthy.
    expect(store.grantedBundles).toEqual([{ id: "1", name: "search" }]);
    expect(store.errors.grantedBundles).toBeNull();
  });

  it("a bare refresh() with no prior load() still leaves loaded true", async () => {
    mockHealthy();
    const store = useDiscoveryStore();
    expect(store.loaded).toBe(false);

    await store.refresh();

    expect(store.loaded).toBe(true);
    expect(listUserSkillBundles).toHaveBeenCalledTimes(1);
  });

  it("refresh() after a failure that has since recovered clears the error", async () => {
    listUserSkillBundles.mockRejectedValueOnce(new Error("boom"));
    listDelegatableUsers.mockResolvedValue({ users: [], groups: [] });
    listFiles.mockResolvedValue({ files: [] });
    listSkills.mockResolvedValue({ skills: [] });
    const store = useDiscoveryStore();
    await store.load();
    expect(store.errors.grantedBundles).toBe("boom");

    listUserSkillBundles.mockResolvedValueOnce({ bundles: [] });
    await store.refresh();
    expect(store.errors.grantedBundles).toBeNull();
  });

  it("personal skills are unioned into grantedBundles under the qualified name", async () => {
    mockHealthy();
    listSkills.mockResolvedValue({ skills: [{ name: "mine", description: "my skill" }] });
    const store = useDiscoveryStore();
    await store.load();

    expect(store.grantedBundles).toEqual([
      { id: "1", name: "search" },
      { id: "personal:mine", name: "mine", description: "my skill", personal: true, skillName: "personal:mine" },
    ]);
  });

  it("a broken personal-skills fetch leaves admin bundles intact and grantedBundles error untouched", async () => {
    mockHealthy();
    listSkills.mockRejectedValue(new Error("nope"));
    const store = useDiscoveryStore();
    await store.load();

    expect(store.grantedBundles).toEqual([{ id: "1", name: "search" }]);
    expect(store.errors.grantedBundles).toBeNull();
  });
});
