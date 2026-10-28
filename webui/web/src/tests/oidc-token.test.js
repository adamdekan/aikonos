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

import { selectBrokerToken, stepUpArgs } from "../auth/oidc.js";

describe("selectBrokerToken", () => {
  const user = { access_token: "AT", id_token: "IT", expired: false };

  it("defaults to the access token (VITE_OIDC_TOKEN unset → 'access')", () => {
    // No kind argument → falls back to the module default, which is "access"
    // when VITE_OIDC_TOKEN is not set in the test env.
    expect(selectBrokerToken(user)).toBe("AT");
  });

  it("returns the access token when kind is 'access'", () => {
    expect(selectBrokerToken(user, "access")).toBe("AT");
  });

  it("returns the ID token when kind is 'id' (login-only Entra apps)", () => {
    expect(selectBrokerToken(user, "id")).toBe("IT");
  });

  it("returns null for a missing user", () => {
    expect(selectBrokerToken(null, "access")).toBeNull();
    expect(selectBrokerToken(undefined, "id")).toBeNull();
  });

  it("returns null for an expired user regardless of kind", () => {
    const expired = { ...user, expired: true };
    expect(selectBrokerToken(expired, "access")).toBeNull();
    expect(selectBrokerToken(expired, "id")).toBeNull();
  });
});

// AG-13: the step-up re-authentication asks for a fresh login and the
// tenant's MFA acr; an Entra authentication context goes as a claims challenge.
describe("stepUpArgs", () => {
  it("forces a fresh login with acr_values for Keycloak-style acrs", () => {
    expect(stepUpArgs("mfa", "access")).toEqual({ max_age: 0, acr_values: "mfa" });
  });

  it("forces a fresh login only when no acr is configured", () => {
    expect(stepUpArgs("", "access")).toEqual({ max_age: 0 });
  });

  it("sends an Entra auth context as a claims challenge on the broker's token", () => {
    const claims = (kind) => JSON.parse(stepUpArgs("c1", kind).extraQueryParams.claims);
    expect(claims("access")).toEqual({ access_token: { acrs: { essential: true, value: "c1" } } });
    expect(claims("id")).toEqual({ id_token: { acrs: { essential: true, value: "c1" } } });
    expect(stepUpArgs("c1", "access").acr_values).toBeUndefined();
  });
});
