// Pure serialization helpers for the Audit view export feature.
// Exported for direct unit testing — no DOM dependency.

/**
 * Serialize events to indented JSON string.
 * @param {object[]} events
 * @returns {string}
 */
export function toJson(events) {
  return JSON.stringify(events, null, 2);
}

/**
 * CSV-escape a single field value.
 * Wraps in quotes if the value contains a comma, double-quote, or newline;
 * doubles any interior double-quotes.
 * @param {string} value
 * @returns {string}
 */
function csvEscape(value) {
  const s = String(value ?? "");
  if (s.includes(",") || s.includes('"') || s.includes("\n")) {
    return '"' + s.replaceAll('"', '""') + '"';
  }
  return s;
}

const CSV_HEADER = "time,event_type,actor,resource_ref,decision,tenant_id";

const DECISION_LABELS = { 1: "ALLOW", 2: "DENY", 3: "APPROVAL", 4: "STEP-UP" };

function fmtTime(ev) {
  const s = ev.occurred_at?.seconds;
  const ms = s != null
    ? Number(s) * 1000 + Math.floor(Number(ev.occurred_at?.nanos ?? 0) / 1e6)
    : Date.now();
  const d = new Date(ms);
  return d.toISOString();
}

function actor(ev) {
  return ev.actor_email || ev.actor_user_id || (ev.actor_spiffe_id || "").replace("spiffe://aikonos.com/", "") || "";
}

function decisionLabel(ev) {
  return DECISION_LABELS[ev.decision] ?? (ev.decision ? String(ev.decision) : "");
}

/**
 * Serialize events to CSV string.
 * Header: time,event_type,actor,resource_ref,decision,tenant_id
 * Fields containing commas, double-quotes, or newlines are quoted;
 * interior double-quotes are doubled.
 * @param {object[]} events
 * @returns {string}
 */
export function toCsv(events) {
  const rows = [CSV_HEADER];
  for (const ev of events) {
    rows.push([
      csvEscape(fmtTime(ev)),
      csvEscape(ev.event_type ?? ""),
      csvEscape(actor(ev)),
      csvEscape(ev.resource_ref ?? ""),
      csvEscape(decisionLabel(ev)),
      csvEscape(ev.tenant_id ?? ""),
    ].join(","));
  }
  return rows.join("\n");
}
