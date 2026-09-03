// Tests for the per-agent API-key functions in api/admin.js and the svc- filter helper.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:  vi.fn(),
  post: vi.fn(),
  del:  vi.fn(),
}));

import * as clientMod from "../api/client.js";
import {
  mintAgentApiKey,
  listAgentApiKeys,
  revokeAgentApiKey,
} from "../api/admin.js";

import { isSvcPrincipal, filterTuples, filterPrincipals } from "../utils/svc-filter.js";

describe("api/admin.js — agent API key functions", () => {
  beforeEach(() => vi.clearAllMocks());

  it("mintAgentApiKey POSTs to /admin/agents/:id/keys with label body", async () => {
    clientMod.post.mockResolvedValue({ rawKey: "tk_abc", key: { id: "k1", keyPrefix: "tk_abc123", label: "ci" } });
    await mintAgentApiKey("agent-1", "ci");
    expect(clientMod.post).toHaveBeenCalledWith("/admin/agents/agent-1/keys", { body: { label: "ci" } });
  });

  it("mintAgentApiKey sends empty string label when omitted", async () => {
    clientMod.post.mockResolvedValue({ rawKey: "tk_xyz", key: { id: "k2", keyPrefix: "tk_xyz999", label: "" } });
    await mintAgentApiKey("agent-2");
    expect(clientMod.post).toHaveBeenCalledWith("/admin/agents/agent-2/keys", { body: { label: "" } });
  });

  it("listAgentApiKeys GETs /admin/agents/:id/keys", async () => {
    clientMod.get.mockResolvedValue({ keys: [] });
    await listAgentApiKeys("agent-1");
    expect(clientMod.get).toHaveBeenCalledWith("/admin/agents/agent-1/keys");
  });

  it("revokeAgentApiKey DELETEs /admin/agents/:id/keys/:keyId", async () => {
    clientMod.del.mockResolvedValue({ success: true });
    await revokeAgentApiKey("agent-1", "key-99");
    expect(clientMod.del).toHaveBeenCalledWith("/admin/agents/agent-1/keys/key-99");
  });
});

describe("svc-filter helper — isSvcPrincipal", () => {
  it("returns true for user:svc- subject", () => {
    expect(isSvcPrincipal("user:svc-abc123")).toBe(true);
  });

  it("returns true for group:svc- object", () => {
    expect(isSvcPrincipal("group:svc-abc123")).toBe(true);
  });

  it("returns false for a normal user", () => {
    expect(isSvcPrincipal("user:alice@example.com")).toBe(false);
  });

  it("returns false for a normal group", () => {
    expect(isSvcPrincipal("group:ops")).toBe(false);
  });

  it("returns false for an agent object", () => {
    expect(isSvcPrincipal("agent:abc123")).toBe(false);
  });

  it("returns false for empty string", () => {
    expect(isSvcPrincipal("")).toBe(false);
  });

  it("returns true for user:svc- with UUID-style id", () => {
    expect(isSvcPrincipal("user:svc-550e8400-e29b-41d4-a716-446655440000")).toBe(true);
  });
});

describe("svc-filter helper — filterTuples", () => {
  const normal   = { user: "user:alice@example.com", relation: "member", object: "group:ops" };
  const svcUser  = { user: "user:svc-agent-x",      relation: "member", object: "group:ops" };
  const svcObj   = { user: "user:alice@example.com",  relation: "member", object: "group:svc-agent-x" };

  it("keeps tuples whose subject and object are both non-svc", () => {
    expect(filterTuples([normal])).toEqual([normal]);
  });

  it("drops a tuple whose subject is user:svc-…", () => {
    expect(filterTuples([svcUser])).toEqual([]);
  });

  it("drops a tuple whose object is group:svc-…", () => {
    expect(filterTuples([svcObj])).toEqual([]);
  });

  it("keeps normal and drops both svc rows from a mixed list", () => {
    expect(filterTuples([normal, svcUser, svcObj])).toEqual([normal]);
  });

  it("returns empty list unchanged", () => {
    expect(filterTuples([])).toEqual([]);
  });
});

describe("svc-filter helper — filterPrincipals", () => {
  const alice  = { id: "user:alice@example.com", kind: "user",  displayName: "Alice" };
  const svcP   = { id: "user:svc-agent-x",      kind: "user",  displayName: "svc-agent-x" };
  const group  = { id: "group:ops",              kind: "group", displayName: "Ops" };

  it("keeps a normal user principal", () => {
    expect(filterPrincipals([alice])).toEqual([alice]);
  });

  it("drops a svc- principal", () => {
    expect(filterPrincipals([svcP])).toEqual([]);
  });

  it("keeps non-svc group", () => {
    expect(filterPrincipals([group])).toEqual([group]);
  });

  it("keeps normal and drops svc from a mixed list", () => {
    expect(filterPrincipals([alice, svcP, group])).toEqual([alice, group]);
  });

  it("returns empty list unchanged", () => {
    expect(filterPrincipals([])).toEqual([]);
  });
});
