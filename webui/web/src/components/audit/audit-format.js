// Shared formatting helpers for Audit.vue and AuditHistory.vue.
// Pure functions — no Vue reactivity, no DOM dependency.

export const DECISION = {
  1: { label: "ALLOW",    cls: "ok",     glyph: "check" },
  2: { label: "DENY",     cls: "deny",   glyph: "close" },
  3: { label: "APPROVAL", cls: "warn",   glyph: "scale" },
  4: { label: "STEP-UP",  cls: "warn",   glyph: "gauge" },
};

export function tsMillis(ev) {
  const s = ev.occurred_at?.seconds;
  if (s != null) return Number(s) * 1000 + Math.floor(Number(ev.occurred_at?.nanos ?? 0) / 1e6);
  return Date.now();
}

export function fmtTimeLive(ev) {
  const d = new Date(tsMillis(ev));
  return d.toLocaleTimeString([], { hour12: false }) + "." + String(d.getMilliseconds()).padStart(3, "0");
}

export function fmtTimeHistory(ev) {
  const s = ev.occurred_at?.seconds;
  if (s == null) return "";
  return new Date(Number(s) * 1000).toLocaleString([], { dateStyle: "short", timeStyle: "medium" });
}

export function actor(ev) {
  return ev.actor_email || ev.actor_user_id || (ev.actor_spiffe_id || "").replace("spiffe://aikonos.com/", "") || "—";
}

export function decision(ev) {
  return DECISION[ev.decision] ?? (ev.decision ? { label: String(ev.decision), cls: "", glyph: null } : null);
}

export function detail(ev) {
  const c = ev.context || {};
  const bits = [];
  if (c.kind)         bits.push(c.kind);   // policy decision record (AG-02 replay)
  if (c.tool_id)      bits.push(c.tool_id);
  if (c.effect_class) bits.push(c.effect_class);
  if (c.cost != null) bits.push(c.cost + "u");
  if (c.outcome && typeof c.outcome === "object") {
    if (c.outcome.decision)   bits.push(c.outcome.decision);
    if (c.outcome.decided_by) bits.push(c.outcome.decided_by);
  } else if (c.outcome) {
    bits.push(c.outcome);
  }
  if (c.reason)       bits.push(c.reason);
  if (c.error)        bits.push("err: " + c.error);
  if (bits.length) return bits.join(" · ");
  // Otherwise the scalar fields as key=value, one level into objects
  // (obo.subject=…), never "[object Object]" or a raw JSON dump.
  const flat = [];
  for (const [k, v] of Object.entries(c)) {
    if (v && typeof v === "object" && !Array.isArray(v)) {
      for (const [k2, v2] of Object.entries(v)) {
        if (v2 !== "" && v2 != null && typeof v2 !== "object") flat.push(`${k}.${k2}=${v2}`);
      }
    } else if (v !== "" && v != null) {
      flat.push(`${k}=${Array.isArray(v) ? v.join(",") : v}`);
    }
  }
  return flat.join(" · ").slice(0, 160);
}

// normalizeEvent: /admin/audit/query answers with the gRPC client's camelCase
// objects (eventId, occurredAt as an ISO string); the live stream and these
// helpers use the proto names (event_id, occurred_at {seconds, nanos}).
// Converts the former to the latter; a stream event passes through.
export function normalizeEvent(ev) {
  if (!ev || ev.event_id !== undefined || ev.eventId === undefined) return ev;
  const out = {};
  for (const [k, v] of Object.entries(ev)) out[k.replace(/[A-Z]/g, (ch) => "_" + ch.toLowerCase())] = v;
  const ms = typeof ev.occurredAt === "string" ? Date.parse(ev.occurredAt) : NaN;
  out.occurred_at = Number.isNaN(ms) ? undefined : { seconds: Math.floor(ms / 1000), nanos: (ms % 1000) * 1e6 };
  return out;
}

// chain is the event's actor chain (AG-12): the user acted for, the agent that
// acted, the envelope sender and the external client. context.obo carries it
// on task-scoped events; actor_agent_id is also set on agent events.
export function chain(ev) {
  const o = ev.context?.obo || {};
  return {
    subject:      o.subject || (o.service_run ? "service" : ""),
    agent:        ev.actor_agent_id || o.actor || "",
    serviceAgent: !!o.service_agent,
    delegatedBy:  o.delegated_by || "",
    client:       o.external_client_id || "",
  };
}
