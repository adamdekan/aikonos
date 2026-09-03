/**
 * Pure helpers for the recurring-schedule selector (F6).
 *
 * The broker treats `cronExpr` as an opaque cron string (robfig/cron/v3 — no
 * seconds, no 6-field, no `@macros`) and is the authoritative validator. It
 * honors an optional leading `CRON_TZ=<IANA>` token, evaluating the fields in
 * that zone; guided mode emits it (see `buildCron`) so a schedule's time means
 * the creator's local wall clock rather than the broker's UTC default. A spec
 * with no token is still evaluated in UTC, unchanged. These helpers
 * build/parse/describe the 5-field shape (plus the optional token); anything
 * outside it degrades gracefully rather than throwing, since the raw-cron
 * advanced fallback lets a user type anything.
 */

const DAY_NAMES = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const WEEKDAY_PRESET = [1, 2, 3, 4, 5];

function pad2(n) {
  return String(n).padStart(2, "0");
}

function hhmm(hour, minute) {
  return `${pad2(hour)}:${pad2(minute)}`;
}

/**
 * The viewer's IANA time zone, or "" if the browser can't report one.
 * @returns {string}
 */
export function localTz() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    return "";
  }
}

// Split a leading CRON_TZ=/TZ= token off an expression. A bare "CRON_TZ=X"
// with no fields deliberately does NOT match (no space) and falls through to
// the ordinary 5-field rejection — this also dodges robfig/cron v3.0.1's
// slice-bounds panic on a token with no space.
function splitTz(expr) {
  const m = expr.match(/^(?:CRON_TZ|TZ)=(\S+)\s+(.+)$/);
  return m ? { tz: m[1], rest: m[2] } : { tz: "", rest: expr };
}

function buildFields(recurrence) {
  const { freq } = recurrence;
  const minute = recurrence.minute ?? 0;
  const hour = recurrence.hour ?? 0;

  switch (freq) {
    case "minutes": {
      const interval = recurrence.interval ?? 1;
      return interval === 1 ? "* * * * *" : `*/${interval} * * * *`;
    }
    case "hourly": {
      const interval = recurrence.interval ?? 1;
      return interval === 1 ? `${minute} * * * *` : `${minute} */${interval} * * *`;
    }
    case "daily":
      return `${minute} ${hour} * * *`;
    case "weekly": {
      const days = [...new Set(recurrence.weekdays ?? [])].sort((a, b) => a - b);
      return `${minute} ${hour} * * ${days.length > 0 ? days.join(",") : "*"}`;
    }
    case "monthly":
      return `${minute} ${hour} ${recurrence.dom ?? 1} * *`;
    default:
      return `${minute} ${hour} * * *`;
  }
}

/**
 * Build a cron string from a recurrence object. When `tz` is non-empty the
 * 5-field expression is prefixed with a `CRON_TZ=<tz>` token so the broker
 * evaluates the fields in that zone — uniform across every freq, since an
 * offset zone (e.g. India's :30) shifts even the minute-of-hour field.
 * @param {{freq: string, interval?: number, minute?: number, hour?: number, weekdays?: number[], dom?: number}} recurrence
 * @param {string} [tz] IANA zone; defaults to the viewer's. Pass "" for a bare (UTC-evaluated) string.
 * @returns {string}
 */
export function buildCron(recurrence, tz = localTz()) {
  const fields = buildFields(recurrence);
  return tz ? `CRON_TZ=${tz} ${fields}` : fields;
}

/**
 * Parse a 5-field cron string into a recurrence object, recognizing only the
 * shapes buildCron can emit. Returns null for anything else (never throws) —
 * the advanced raw-cron fallback covers everything not recognized here.
 * @param {string} expr
 * @returns {object|null}
 */
export function parseCron(expr) {
  if (typeof expr !== "string") return null;
  // Strip any CRON_TZ=/TZ= prefix and discard the zone — the returned object
  // carries only recurrence fields (round-trip equality + re-seeding rely on
  // that shape). Editing re-anchors to the editor's zone with the same digits.
  const { rest } = splitTz(expr.trim());
  const fields = rest.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  const [min, hr, dom, mon, dow] = fields;

  if (mon !== "*") return null;

  // Every minute.
  if (min === "*" && hr === "*" && dom === "*" && dow === "*") {
    return { freq: "minutes", interval: 1 };
  }

  // Minutes step: */N * * * *
  const minStep = min.match(/^\*\/(\d+)$/);
  if (minStep && hr === "*" && dom === "*" && dow === "*") {
    return { freq: "minutes", interval: Number(minStep[1]) };
  }

  const minute = Number(min);
  const isMinuteFixed = /^\d+$/.test(min) && minute >= 0 && minute <= 59;

  // Hourly step: M */N * * *
  const hrStep = hr.match(/^\*\/(\d+)$/);
  if (isMinuteFixed && hrStep && dom === "*" && dow === "*") {
    return { freq: "hourly", interval: Number(hrStep[1]), minute };
  }

  // Hourly every hour: M * * * *
  if (isMinuteFixed && hr === "*" && dom === "*" && dow === "*") {
    return { freq: "hourly", interval: 1, minute };
  }

  const hour = Number(hr);
  const isHourFixed = /^\d+$/.test(hr) && hour >= 0 && hour <= 23;

  // Daily: M H * * *
  if (isMinuteFixed && isHourFixed && dom === "*" && dow === "*") {
    return { freq: "daily", minute, hour };
  }

  // Weekly: M H * * d1,d2,...
  if (isMinuteFixed && isHourFixed && dom === "*" && dow !== "*") {
    const parts = dow.split(",");
    const valid = parts.every((p) => /^\d+$/.test(p) && Number(p) >= 0 && Number(p) <= 6);
    const days = parts.map((p) => Number(p));
    if (valid && days.length > 0) {
      const weekdays = [...new Set(days)].sort((a, b) => a - b);
      return { freq: "weekly", minute, hour, weekdays };
    }
    return null;
  }

  // Monthly: M H <dom> * *
  const domNum = Number(dom);
  const isDomFixed = /^\d+$/.test(dom) && domNum >= 1 && domNum <= 31;
  if (isMinuteFixed && isHourFixed && isDomFixed && dow === "*") {
    return { freq: "monthly", minute, hour, dom: domNum };
  }

  return null;
}

/**
 * Plain-language description of a cron expression, preferring parseCron's
 * structured read; falls back to a "Custom (<expr>)" label for anything it
 * can't map (never throws on empty/garbage input).
 * @param {string} expr
 * @returns {string}
 */
export function describeCron(expr) {
  const r = parseCron(expr);
  if (!r) return `Custom (${expr})`;

  let base;
  switch (r.freq) {
    case "minutes":
      base = r.interval === 1 ? "Every minute" : `Every ${r.interval} minutes`;
      break;
    case "hourly":
      base = r.interval === 1
        ? `Hourly at :${pad2(r.minute)}`
        : `Every ${r.interval} hours at :${pad2(r.minute)}`;
      break;
    case "daily":
      base = `Every day at ${hhmm(r.hour, r.minute)}`;
      break;
    case "weekly": {
      if (
        r.weekdays.length === WEEKDAY_PRESET.length &&
        r.weekdays.every((d, i) => d === WEEKDAY_PRESET[i])
      ) {
        base = `Every weekday at ${hhmm(r.hour, r.minute)}`;
      } else {
        const names = r.weekdays.map((d) => DAY_NAMES[d]).join(", ");
        base = `Every ${names} at ${hhmm(r.hour, r.minute)}`;
      }
      break;
    }
    case "monthly":
      base = `Monthly on day ${r.dom} at ${hhmm(r.hour, r.minute)}`;
      break;
    default:
      return `Custom (${expr})`;
  }

  // Surface a foreign zone so a viewer elsewhere isn't misled by bare digits;
  // omit it when the spec's zone is the viewer's own (r non-null ⇒ expr is a
  // string, so the trim is safe).
  const { tz } = splitTz(expr.trim());
  return tz && tz !== localTz() ? `${base} (${tz})` : base;
}
