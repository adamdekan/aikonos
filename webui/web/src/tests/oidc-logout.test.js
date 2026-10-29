import { describe, it, expect, vi, beforeEach } from "vitest";

// A UserManager double that records what sign-out does, in order.
const h = vi.hoisted(() => ({ calls: [], revocationEndpoint: undefined, unloaded: null }));
const calls = h.calls;
vi.mock("oidc-client-ts", () => ({
  UserManager: class {
    events = {
      addAccessTokenExpired() {},
      addSilentRenewError() {},
      addUserUnloaded(fn) { h.unloaded = fn; },
    };
    metadataService = { getRevocationEndpoint: async () => h.revocationEndpoint };
    async getUser() { return { id_token: "IT", access_token: "AT" }; }
    async revokeTokens() { calls.push("revoke"); }
    async signoutRedirect(args) { calls.push(["signout", args, sessionStorage.length]); }
    async signinRedirect() { calls.push("signin"); }
  },
  WebStorageStateStore: class {},
}));

import { logout, endSession, onSessionEnd } from "../auth/oidc.js";

describe("sign-out (M-27) and session end (L-26)", () => {
  beforeEach(() => {
    calls.length = 0;
    sessionStorage.setItem("oidc.user:x:y", "{\"access_token\":\"AT\"}");
  });

  it("revokes the tokens when the IdP has a revocation endpoint, wipes sessionStorage, then signs out", async () => {
    h.revocationEndpoint = "https://idp.example/revoke";
    await logout();
    expect(calls[0]).toBe("revoke");
    expect(calls[1][0]).toBe("signout");
    expect(calls[1][1].id_token_hint).toBe("IT");
    expect(calls[1][2]).toBe(0);
  });

  it("still signs out when the IdP has no revocation endpoint (Entra)", async () => {
    h.revocationEndpoint = undefined;
    await logout();
    expect(calls.map((c) => (Array.isArray(c) ? c[0] : c))).toEqual(["signout"]);
  });

  it("an ended or unloaded session runs the session-end handlers", async () => {
    const ended = vi.fn();
    onSessionEnd(ended);
    await endSession("/chat");
    expect(ended).toHaveBeenCalledTimes(1);
    expect(calls).toContain("signin");
    h.unloaded();
    expect(ended).toHaveBeenCalledTimes(2);
  });
});
