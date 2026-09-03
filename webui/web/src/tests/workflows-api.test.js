// Tests for api/workflows.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import {
  listWorkflows,
  getWorkflow,
  saveWorkflow,
  runWorkflow,
  rateWorkflow,
  publishWorkflow,
  forkWorkflow,
  deleteWorkflow,
  pinVersion,
  clearPin,
  listVersions,
  proposeVersion,
  decideVersion,
} from "../api/workflows.js";

describe("api/workflows.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listWorkflows calls GET /workflows", async () => {
    clientMod.get.mockResolvedValue({ workflows: [] });
    await listWorkflows();
    expect(clientMod.get).toHaveBeenCalledWith("/workflows");
  });

  it("listWorkflows passes limit + cursor as query params", async () => {
    clientMod.get.mockResolvedValue({ workflows: [], nextCursor: "c2" });
    await listWorkflows({ limit: 50, cursor: "c1" });
    expect(clientMod.get).toHaveBeenCalledWith("/workflows?limit=50&cursor=c1");
  });

  it("listWorkflows omits cursor when not given and returns the full body", async () => {
    clientMod.get.mockResolvedValue({ workflows: [{ lineageId: "l1" }], nextCursor: "c2", sharedUnavailable: true });
    const body = await listWorkflows({ limit: 25 });
    expect(clientMod.get).toHaveBeenCalledWith("/workflows?limit=25");
    expect(body.nextCursor).toBe("c2");
    expect(body.sharedUnavailable).toBe(true);
  });

  it("getWorkflow calls GET /workflows/:id", async () => {
    clientMod.get.mockResolvedValue({ definitionJson: "{}", version: 1 });
    await getWorkflow("lineage-1");
    expect(clientMod.get).toHaveBeenCalledWith("/workflows/lineage-1");
  });

  it("saveWorkflow posts body to /workflows", async () => {
    clientMod.post.mockResolvedValue({ lineageId: "l1", version: 1 });
    await saveWorkflow({ definitionJson: "{}", name: "my-wf" });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows", {
      body: { definitionJson: "{}", name: "my-wf" },
    });
  });

  it("runWorkflow posts inputs to /workflows/:id/run", async () => {
    clientMod.post.mockResolvedValue({ ok: true, result: { halted: false, steps: [] } });
    await runWorkflow("l1", { query: "hello" });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/run", {
      body: { inputs: { query: "hello" } },
    });
  });

  it("rateWorkflow posts to /workflows/:id/rate", async () => {
    clientMod.post.mockResolvedValue({ ok: true });
    await rateWorkflow("l1", { version: 2, rating: "RATING_SUCCESS" });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/rate", {
      body: { version: 2, rating: "RATING_SUCCESS" },
    });
  });

  it("publishWorkflow posts to /workflows/:id/publish", async () => {
    clientMod.post.mockResolvedValue({ visibilityKind: "shared", groups: ["g1"] });
    await publishWorkflow("l1", { version: 1, groupIds: ["g1"] });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/publish", {
      body: { version: 1, groupIds: ["g1"] },
    });
  });

  it("forkWorkflow posts to /workflows/:id/fork", async () => {
    clientMod.post.mockResolvedValue({ lineageId: "l2" });
    await forkWorkflow("l1", { newName: "my-fork" });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/fork", {
      body: { newName: "my-fork" },
    });
  });

  it("deleteWorkflow deletes /workflows/:id", async () => {
    clientMod.del.mockResolvedValue({ ok: true, versionsDeleted: 2 });
    await deleteWorkflow("l1");
    expect(clientMod.del).toHaveBeenCalledWith("/workflows/l1");
  });

  it("pinVersion posts to /workflows/:id/pin", async () => {
    clientMod.post.mockResolvedValue({ ok: true });
    await pinVersion("l1", { version: 3 });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/pin", {
      body: { version: 3 },
    });
  });

  it("clearPin deletes /workflows/:id/pin", async () => {
    clientMod.del.mockResolvedValue({ ok: true });
    await clearPin("l1");
    expect(clientMod.del).toHaveBeenCalledWith("/workflows/l1/pin");
  });

  it("listVersions calls GET /workflows/:id/versions", async () => {
    clientMod.get.mockResolvedValue({ versions: [] });
    await listVersions("l1");
    expect(clientMod.get).toHaveBeenCalledWith("/workflows/l1/versions");
  });

  it("proposeVersion posts to /workflows/:id/propose", async () => {
    clientMod.post.mockResolvedValue({ version: 2 });
    await proposeVersion("l1", { definitionJson: "{}" });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/propose", {
      body: { definitionJson: "{}" },
    });
  });

  it("decideVersion posts to /workflows/:id/decide", async () => {
    clientMod.post.mockResolvedValue({ approvalState: "approved" });
    await decideVersion("l1", { version: 2, approved: true });
    expect(clientMod.post).toHaveBeenCalledWith("/workflows/l1/decide", {
      body: { version: 2, approved: true },
    });
  });
});
