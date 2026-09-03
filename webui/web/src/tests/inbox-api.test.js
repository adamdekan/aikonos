// Tests for api/inbox.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listInbox, dismiss, delegate } from "../api/inbox.js";

describe("api/inbox.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listInbox calls GET /inbox (no user query param — derived from bearer)", async () => {
    clientMod.get.mockResolvedValue({ envelopes: [] });
    await listInbox();
    expect(clientMod.get).toHaveBeenCalledWith("/inbox");
  });

  it("dismiss posts to /inbox/:id/dismiss", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    await dismiss("env-3");
    expect(clientMod.post).toHaveBeenCalledWith("/inbox/env-3/dismiss", {});
  });

  it("delegate posts to /delegate without a 'from' field (gateway derives from bearer)", async () => {
    clientMod.post.mockResolvedValue({ ok: true, envelopeId: "e1" });
    await delegate({ to: "bob@example.com", intent: "fetch data", scopes: ["web:read"], maxCost: 30 });
    expect(clientMod.post).toHaveBeenCalledWith("/delegate", {
      body: { to: "bob@example.com", intent: "fetch data", scopes: ["web:read"], maxCost: 30 },
    });
  });

  it("delegate body does NOT include a 'from' field", async () => {
    clientMod.post.mockResolvedValue({ ok: true });
    await delegate({ to: "bob@example.com", intent: "x", scopes: [], maxCost: 0 });
    const [, opts] = clientMod.post.mock.calls[0];
    expect(opts.body.from).toBeUndefined();
  });
});
