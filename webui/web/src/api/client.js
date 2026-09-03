// Single fetch gateway. All calls carry Authorization: Bearer <token>; 403
// surfaces as { forbidden: true } so callers can render an empty-state without
// throwing.

import { getAccessToken } from "../auth/oidc.js";

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

  if (res.status === 403) return { forbidden: true, error: data.error };
  if (!res.ok) {
    const err = new Error(data.error || `request failed (${res.status})`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}
