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

import { resolveOidcConfig } from "../auth/oidc.js";

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
