// EventSource cannot send an Authorization header, and the gateway's audit
// stream requires a bearer (H-07: it scopes the stream to the caller). This is
// a minimal EventSource-shaped reader over fetch: onopen/onmessage/onerror and
// close(), which is all views/admin/Audit.vue uses.
import { getAccessToken } from "../auth/oidc.js";

export function authedEventSource(url) {
  const ctl = new AbortController();
  const es = { onopen: null, onmessage: null, onerror: null, close: () => ctl.abort() };
  (async () => {
    const token = await getAccessToken();
    const resp = await fetch(url, {
      headers: { accept: "text/event-stream", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      signal: ctl.signal,
    });
    if (!resp.ok || !resp.body) throw new Error(`audit stream ${resp.status}`);
    es.onopen?.({});
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error("audit stream ended");
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const line = buf.slice(0, i).split("\n").find((l) => l.startsWith("data:"));
        buf = buf.slice(i + 2);
        if (line) es.onmessage?.({ data: line.slice(5).trim() });
      }
    }
  })().catch(() => { if (!ctl.signal.aborted) es.onerror?.({}); });
  return es;
}
