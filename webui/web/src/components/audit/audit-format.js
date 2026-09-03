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
  if (c.tool_id)      bits.push(c.tool_id);
  if (c.effect_class) bits.push(c.effect_class);
  if (c.cost != null) bits.push(c.cost + "u");
  if (c.outcome)      bits.push(c.outcome);
  if (c.error)        bits.push("err: " + c.error);
  if (bits.length) return bits.join(" · ");
  const keys = Object.keys(c);
  return keys.length ? JSON.stringify(c).slice(0, 120) : "";
}
