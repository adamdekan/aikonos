// OIDC client wrapper around oidc-client-ts UserManager.
// Authority and client config are deployment settings, never kept in
// localStorage. They resolve in this order (see resolveOidcConfig):
//   1. runtime: window.__AIKONOS_CONFIG__.oidc, set by /runtime-config.js, which
//      server.mjs builds from the container's AIKONOS_WEBUI_OIDC_* env vars;
//   2. build time: the VITE_OIDC_* values Vite baked into the bundle;
//   3. the local-Keycloak defaults below.
// Runtime first means one published image serves any identity provider.
//
// Token storage: both the PKCE state (stateStore) and the user object
// (userStore) are explicitly bound to sessionStorage — tokens do not survive
// a tab close. oidc-client-ts v3 defaults stateStore to localStorage, which
// would make PKCE state durable; we override that here.
// automaticSilentRenew keeps the session alive within the tab.

import { UserManager, WebStorageStateStore } from "oidc-client-ts";

/**
 * Resolves the OIDC settings from the runtime config, the build-time env, and
 * the defaults, in that order. Pure and exported for unit tests. An empty or
 * whitespace-only value counts as unset at every level, so a blank variable
 * falls through instead of yielding an unusable empty authority or scope.
 *
 * scope: Keycloak stamps aud=aikonos-broker via the client-level "webui-audience"
 * protocol mapper (scope-independent), so the default requests only the standard
 * OIDC scopes — requesting "aikonos-broker" as a scope fails ("Invalid scopes")
 * since no such client scope exists in the realm. Entra needs the broker's
 * exposed API scope in the access token's aud, so it sets the scope explicitly
 * (e.g. "openid profile api://<broker-app-id>/access_as_user").
 *
 * tokenKind: which token the broker/gateway accept as the bearer. "access"
 * (default) suits Keycloak and any Entra app that exposes an API; "id" suits
 * Entra deployments that do NOT expose an API (login-only, e.g. Graph User.Read
 * only). See selectBrokerToken for the full rationale.
 */
export function resolveOidcConfig(runtime, buildEnv = {}, origin = "") {
  const rt = runtime && typeof runtime === "object" ? runtime : {};
  const pick = (...values) =>
    values.find((v) => typeof v === "string" && v.trim() !== "")?.trim();
  return {
    authority: pick(rt.authority, buildEnv.VITE_OIDC_AUTHORITY) ?? "http://localhost:18080/realms/aikonos",
    clientId: pick(rt.clientId, buildEnv.VITE_OIDC_CLIENT_ID) ?? "aikonos-webui",
    redirectUri: pick(rt.redirectUri, buildEnv.VITE_OIDC_REDIRECT_URI) ?? `${origin}/auth/callback`,
    scope: pick(rt.scope, buildEnv.VITE_OIDC_SCOPE) ?? "openid profile",
    tokenKind: pick(rt.token, buildEnv.VITE_OIDC_TOKEN) ?? "access",
  };
}

const {
  authority,
  clientId,
  redirectUri,
  scope,
  tokenKind: brokerTokenKind,
} = resolveOidcConfig(window.__AIKONOS_CONFIG__?.oidc, import.meta.env, window.location.origin);

const mgr = new UserManager({
  authority,
  client_id: clientId,
  redirect_uri: redirectUri,
  response_type: "code",
  scope,
  automaticSilentRenew: true,
  // oidc-client-ts v3 defaults stateStore to localStorage; override to sessionStorage
  // so PKCE state is not durable across browser sessions.
  stateStore: new WebStorageStateStore({ store: window.sessionStorage }),
  // userStore defaults to sessionStorage already, but set it explicitly for clarity.
  userStore: new WebStorageStateStore({ store: window.sessionStorage }),
});

// When the access token fully expires (no refresh token, or silent renew failed)
// the cached token is dead and every /api call 401s with no recovery until a
// manual reload. Bounce through the IdP to re-establish the session. With a live
// IdP cookie this is a fast, promptless redirect. addAccessTokenExpiring (~60s
// before exp) still drives automaticSilentRenew first; these only fire once that
// has given up.
mgr.events.addAccessTokenExpired(() => login());
mgr.events.addSilentRenewError(() => login());

/** Redirect the browser to the Keycloak login page. */
export async function login() {
  await mgr.signinRedirect();
}

/**
 * Complete the auth-code exchange on the /auth/callback page.
 * Returns the oidc User on success.
 */
export async function handleCallback() {
  return mgr.signinRedirectCallback();
}

/**
 * End the OIDC session (clears local state; optionally triggers Keycloak logout).
 * post_logout_redirect_uri defaults to the app origin.
 */
export async function logout() {
  await mgr.signoutRedirect({ post_logout_redirect_uri: window.location.origin });
}

/** Returns the current oidc User (with profile + access_token) or null. */
export async function getUser() {
  return mgr.getUser();
}

/**
 * Returns the ID token string (aud=client_id) directly. The broker bearer is
 * normally chosen by selectBrokerToken/tokenKind; this getter is kept for
 * callers that specifically need the ID token.
 */
export async function getIdToken() {
  const u = await mgr.getUser();
  if (!u || u.expired) return null;
  return u.id_token;
}

/**
 * Selects the bearer the broker/gateway will accept, per the tokenKind setting
 * (AIKONOS_WEBUI_OIDC_TOKEN at runtime, VITE_OIDC_TOKEN at build time). Pure
 * (no I/O) and exported so it can be unit-tested directly.
 *
 *   - "access" (default): the access token. Correct for Keycloak (aud=aikonos-broker
 *     stamped by the webui-audience mapper) and for Entra apps that expose an API
 *     (aud=<client-id>, obtained via the api://<client-id>/.default scope). This
 *     is the only model that supports tenant-OneDrive OBO, whose assertion must be
 *     an access token with aud=<client-id>.
 *   - "id": the ID token (aud=<client-id> by construction). For Entra deployments
 *     that do NOT expose an API — the access token would target Microsoft Graph,
 *     so the broker/gateway (aud=<client-id>) only accept the ID token. Such a
 *     deployment must also point AIKONOS_OIDC_JWKS_URL at ?appid=<client-id> (the
 *     app-specific ID-token signing key). OBO is unavailable in this model.
 */
export function selectBrokerToken(user, kind = brokerTokenKind) {
  if (!user || user.expired) return null;
  return kind === "id" ? user.id_token : user.access_token;
}

/**
 * Returns the bearer to send to the broker/gateway — the access token by default,
 * or the ID token when tokenKind is "id" (see selectBrokerToken). Null when the
 * session has expired and silent renew has not yet run.
 */
export async function getAccessToken() {
  return selectBrokerToken(await mgr.getUser());
}
