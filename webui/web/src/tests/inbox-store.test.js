import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn(),
}));

import { useInboxStore } from "../store/inbox.js";
import * as inboxApi from "../api/inbox.js";

describe("inbox store", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("refresh sets count to envelopes.length", async () => {
    inboxApi.listInbox.mockResolvedValue({ envelopes: [{}, {}, {}] });
    const store = useInboxStore();
    await store.refresh();
    expect(store.count).toBe(3);
  });

  it("refresh with empty envelopes sets count to 0", async () => {
    inboxApi.listInbox.mockResolvedValue({ envelopes: [] });
    const store = useInboxStore();
    store.setCount(5);
    await store.refresh();
    expect(store.count).toBe(0);
  });

  it("refresh sets count to 0 on rejected listInbox and does not throw", async () => {
    inboxApi.listInbox.mockRejectedValue(new Error("network error"));
    const store = useInboxStore();
    store.setCount(7);
    await expect(store.refresh()).resolves.toBeUndefined();
    expect(store.count).toBe(0);
  });

  it("refresh handles missing envelopes key (uses ?? [])", async () => {
    inboxApi.listInbox.mockResolvedValue({});
    const store = useInboxStore();
    await store.refresh();
    expect(store.count).toBe(0);
  });

  it("setCount sets count directly", () => {
    const store = useInboxStore();
    store.setCount(42);
    expect(store.count).toBe(42);
  });

  it("initial count is 0", () => {
    const store = useInboxStore();
    expect(store.count).toBe(0);
  });
});
