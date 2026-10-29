// Single fetch gateway. All calls carry Authorization: Bearer <token>; 403
// surfaces as { forbidden: true } so callers can render an empty-state without
// throwing.

import { ref } from "vue";
import { getAccessToken, endSession } from "../auth/oidc.js";

// Set once the broker refuses this user as blocked (AG-06); App.vue shows a
// global banner instead of each view rendering an empty or misleading state.
export const accountBlocked = ref(false);

const REAUTH_KEY = "aikonos.reauthAt";

// noteAuthFailure handles the account-level refusals every surface shares:
// 403 "user is blocked" raises the banner; 401 (session revoked, token
// rejected) sends the user back to sign-in, returning to this page. A
// step_up_required 401 belongs to the approval UI and is left alone.
export function noteAuthFailure(status, error) {
  if (status === 403 && error === "user is blocked") accountBlocked.value = true;
  if (status !== 401 || error === "step_up_required") return;
  // ponytail: at most one redirect a minute, so a gateway that rejects every
  // token cannot bounce the browser through the IdP in a loop.
  try {
    if (Date.now() - Number(sessionStorage.getItem(REAUTH_KEY) || 0) < 60_000) return;
    sessionStorage.setItem(REAUTH_KEY, String(Date.now()));
  } catch {
    /* storage unavailable: redirect anyway */
  }
  endSession(window.location.pathname + window.location.search);
}

// Passthrough paths that must not be prefixed with /api.
const PASSTHROUGH = ["/agui", "/audit/stream"];

function resolveUrl(path) {
  for (const prefix of PASSTHROUGH) {
    if (path === prefix || path.startsWith(prefix + "?") || path.startsWith(prefix + "/")) {
      return path;
    }
  }
  return `/api${path}`;
}

export async function request(path, { method = "GET", body, headers = {} } = {}) {
  const token = await getAccessToken();
  if (!token) throw new Error("no token — user is not authenticated");

  const baseHeaders = {
    "Authorization": `Bearer ${token}`,
    ...headers,
  };
  if (body !== undefined) baseHeaders["content-type"] = "application/json";

  const res = await fetch(resolveUrl(path), {
    method,
    headers: baseHeaders,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  let data = {};
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }

  noteAuthFailure(res.status, data.error);
  if (res.status === 403) return { forbidden: true, error: data.error };
  if (!res.ok) {
    const err = new Error(data.error || `request failed (${res.status})`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

export const get   = (path, opts = {}) => request(path, { ...opts, method: "GET" });
export const post  = (path, opts = {}) => request(path, { ...opts, method: "POST" });
export const put   = (path, opts = {}) => request(path, { ...opts, method: "PUT" });
export const del   = (path, opts = {}) => request(path, { ...opts, method: "DELETE" });
export const patch = (path, opts = {}) => request(path, { ...opts, method: "PATCH" });

// download fetches a file the gateway serves as an attachment and returns its
// exact text, unparsed, so a document checked byte for byte (decision
// evidence) is saved unchanged. 403 → { forbidden: true }, like request.
export async function download(path) {
  const token = await getAccessToken();
  if (!token) throw new Error("no token — user is not authenticated");

  const res = await fetch(resolveUrl(path), { headers: { "Authorization": `Bearer ${token}` } });
  const text = await res.text();
  let message = `request failed (${res.status})`;
  if (!res.ok) {
    try {
      message = JSON.parse(text).error || message;
    } catch {
      /* not JSON */
    }
  }
  noteAuthFailure(res.status, message);
  if (res.status === 403) return { forbidden: true };
  if (!res.ok) {
    const err = new Error(message);
    err.status = res.status;
    throw err;
  }
  return { text };
}

// upload sends a raw (non-JSON) body with an explicit content-type.
// Used for SKILL.md text/markdown upload where JSON serialization would corrupt the body.
// method defaults to POST; pass method: "PUT" for update-in-place.
export async function upload(path, { body, contentType, method = "POST" }) {
  const token = await getAccessToken();
  if (!token) throw new Error("no token — user is not authenticated");

  const res = await fetch(resolveUrl(path), {
    method,
    headers: { "Authorization": `Bearer ${token}`, "content-type": contentType },
    body,
  });

  let data = {};
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }

  noteAuthFailure(res.status, data.error);
  if (res.status === 403) return { forbidden: true, error: data.error };
  if (!res.ok) {
    const err = new Error(data.error || `request failed (${res.status})`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}
