import { describe, it, expect, vi } from "vitest";

// oidc.js constructs a UserManager at module load; stub the library so importing
// the module under test has no side effects (no storage/timer access).
vi.mock("oidc-client-ts", () => ({
  UserManager: class {
    constructor() {}
    events = { addAccessTokenExpired() {}, addSilentRenewError() {} };
  },
  WebStorageStateStore: class {
    constructor() {}
  },
}));

import { resolveOidcConfig, safeReturnTo, isPopupCallback } from "../auth/oidc.js";

const ORIGIN = "https://aikonos.example.com";

const BUILD = {
  VITE_OIDC_AUTHORITY: "https://build.example.com/realm",
  VITE_OIDC_CLIENT_ID: "build-client",
  VITE_OIDC_REDIRECT_URI: "https://build.example.com/auth/callback",
  VITE_OIDC_SCOPE: "openid build",
  VITE_OIDC_TOKEN: "access",
};

describe("resolveOidcConfig", () => {
  it("falls back to the local-Keycloak defaults when nothing is set", () => {
    expect(resolveOidcConfig(undefined, {}, ORIGIN)).toEqual({
      authority: "http://localhost:18080/realms/aikonos",
      clientId: "aikonos-webui",
      redirectUri: `${ORIGIN}/auth/callback`,
      scope: "openid profile",
      tokenKind: "access",
    });
  });

  it("uses the build-time VITE_OIDC_* values when there is no runtime config", () => {
    expect(resolveOidcConfig({}, BUILD, ORIGIN)).toEqual({
      authority: "https://build.example.com/realm",
      clientId: "build-client",
      redirectUri: "https://build.example.com/auth/callback",
      scope: "openid build",
      tokenKind: "access",
    });
  });

  it("prefers runtime values over build-time values, key by key", () => {
    const runtime = {
      authority: "https://login.microsoftonline.com/tenant/v2.0",
      clientId: "runtime-client",
      token: "id",
    };
    expect(resolveOidcConfig(runtime, BUILD, ORIGIN)).toEqual({
      authority: "https://login.microsoftonline.com/tenant/v2.0",
      clientId: "runtime-client",
      redirectUri: "https://build.example.com/auth/callback",
      scope: "openid build",
      tokenKind: "id",
    });
  });

  it("treats empty and whitespace-only values as unset at every level", () => {
    const runtime = { authority: "", scope: "   " };
    const build = { VITE_OIDC_AUTHORITY: "", VITE_OIDC_SCOPE: "openid build" };
    const cfg = resolveOidcConfig(runtime, build, ORIGIN);
    expect(cfg.authority).toBe("http://localhost:18080/realms/aikonos");
    expect(cfg.scope).toBe("openid build");
  });

  it("trims surrounding whitespace from a chosen value", () => {
    expect(resolveOidcConfig({ clientId: "  padded  " }, {}, ORIGIN).clientId).toBe("padded");
  });

  it("ignores a runtime config that is not an object", () => {
    expect(resolveOidcConfig(null, BUILD, ORIGIN).clientId).toBe("build-client");
    expect(resolveOidcConfig("bogus", BUILD, ORIGIN).clientId).toBe("build-client");
  });
});

describe("safeReturnTo", () => {
  it("keeps an in-app path with its query", () => {
    expect(safeReturnTo("/admin/settings?tab=identity")).toBe("/admin/settings?tab=identity");
  });
  it("refuses off-site and protocol-relative targets", () => {
    for (const bad of ["https://evil.example", "//evil.example", "/\\evil.example", "javascript:alert(1)", undefined, 42]) {
      expect(safeReturnTo(bad)).toBe("/");
    }
  });
  it("never returns to the callback itself", () => {
    expect(safeReturnTo("/auth/callback?code=x")).toBe("/");
  });
});

describe("isPopupCallback (AG-13 step-up)", () => {
  const win = (search, stored) => {
    const w = { location: { search } };
    w.opener = stored === undefined ? null : { sessionStorage: { getItem: (k) => stored[k] ?? null } };
    return w;
  };
  it("is true when the opener holds this state as a popup request", () => {
    expect(isPopupCallback(win("?code=c&state=s1", { "oidc.s1": JSON.stringify({ request_type: "si:p" }) }))).toBe(true);
  });
  it("is false for a normal redirect, an unknown state, or no opener", () => {
    expect(isPopupCallback(win("?code=c&state=s1", { "oidc.s1": JSON.stringify({ request_type: "si:r" }) }))).toBe(false);
    expect(isPopupCallback(win("?code=c&state=s2", { "oidc.s1": JSON.stringify({ request_type: "si:p" }) }))).toBe(false);
    expect(isPopupCallback(win("?code=c&state=s1"))).toBe(false);
  });
  it("is false when the opener is cross-origin (storage access throws)", () => {
    const w = { location: { search: "?state=s1" }, opener: { get sessionStorage() { throw new Error("SecurityError"); } } };
    expect(isPopupCallback(w)).toBe(false);
  });
});
