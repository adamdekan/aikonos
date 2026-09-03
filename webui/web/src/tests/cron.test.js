import { describe, it, expect } from "vitest";
import { buildCron, parseCron, describeCron, localTz } from "../lib/cron.js";

describe("buildCron", () => {
  it("minutes: interval=1 emits the every-minute wildcard, not */1", () => {
    expect(buildCron({ freq: "minutes", interval: 1 }, "")).toBe("* * * * *");
  });

  it("minutes: interval=15 emits a step expression", () => {
    expect(buildCron({ freq: "minutes", interval: 15 }, "")).toBe("*/15 * * * *");
  });

  it("hourly: interval=1 pins the minute and runs every hour, not */1", () => {
    expect(buildCron({ freq: "hourly", interval: 1, minute: 5 }, "")).toBe("5 * * * *");
  });

  it("hourly: interval=2 emits a step expression on the hour field", () => {
    expect(buildCron({ freq: "hourly", interval: 2, minute: 0 }, "")).toBe("0 */2 * * *");
  });

  it("daily: pins minute and hour, wildcards the rest", () => {
    expect(buildCron({ freq: "daily", minute: 0, hour: 9 }, "")).toBe("0 9 * * *");
  });

  it("weekly: de-dupes and sorts weekdays ascending regardless of input order", () => {
    expect(buildCron({ freq: "weekly", minute: 30, hour: 14, weekdays: [4, 1, 4] }, "")).toBe(
      "30 14 * * 1,4",
    );
  });

  it("monthly: pins day-of-month, wildcards day-of-week", () => {
    expect(buildCron({ freq: "monthly", minute: 0, hour: 0, dom: 1 }, "")).toBe("0 0 1 * *");
  });

  it("defaults missing minute/hour to 0 so a bare freq still yields a valid string", () => {
    expect(buildCron({ freq: "daily" }, "")).toBe("0 0 * * *");
  });

  it("monthly: defaults missing dom to 1 instead of emitting 'undefined'", () => {
    expect(buildCron({ freq: "monthly" }, "")).toBe("0 0 1 * *");
  });

  it("weekly: missing weekdays wildcards day-of-week instead of emitting a trailing space", () => {
    expect(buildCron({ freq: "weekly" }, "")).toBe("0 0 * * *");
  });

  it("weekly: empty weekdays array wildcards day-of-week instead of emitting a trailing space", () => {
    expect(buildCron({ freq: "weekly", weekdays: [] }, "")).toBe("0 0 * * *");
  });
});

describe("buildCron timezone prefix", () => {
  it("prepends a CRON_TZ token so the broker evaluates fields in the creator's zone", () => {
    expect(buildCron({ freq: "daily", minute: 14, hour: 15 }, "Europe/Vienna")).toBe(
      "CRON_TZ=Europe/Vienna 14 15 * * *",
    );
  });

  it("prefixes even minute-of-hour freqs so an offset zone shifts the minute field too", () => {
    expect(buildCron({ freq: "minutes", interval: 15 }, "Asia/Kolkata")).toBe(
      "CRON_TZ=Asia/Kolkata */15 * * * *",
    );
  });

  it("emits no prefix for an empty tz", () => {
    expect(buildCron({ freq: "daily", minute: 14, hour: 15 }, "")).toBe("14 15 * * *");
  });

  it("default arg emits a CRON_TZ prefix whenever the browser reports a zone", () => {
    const out = buildCron({ freq: "daily", minute: 14, hour: 15 });
    if (localTz()) {
      expect(out.startsWith("CRON_TZ=")).toBe(true);
    } else {
      expect(out).toBe("14 15 * * *");
    }
  });
});

describe("parseCron round-trip", () => {
  it("round-trips minutes", () => {
    const r = { freq: "minutes", interval: 15 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });

  it("round-trips the minutes interval=1 special case", () => {
    const r = { freq: "minutes", interval: 1 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });

  it("round-trips hourly", () => {
    const r = { freq: "hourly", interval: 2, minute: 5 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });

  it("round-trips the hourly interval=1 special case", () => {
    const r = { freq: "hourly", interval: 1, minute: 5 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });

  it("round-trips daily", () => {
    const r = { freq: "daily", minute: 0, hour: 9 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });

  it("round-trips weekly with a normalized (sorted, de-duped) weekday list", () => {
    const r = { freq: "weekly", minute: 30, hour: 14, weekdays: [4, 1, 4] };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual({ freq: "weekly", minute: 30, hour: 14, weekdays: [1, 4] });
  });

  it("round-trips monthly", () => {
    const r = { freq: "monthly", minute: 0, hour: 0, dom: 1 };
    expect(parseCron(buildCron(r, "Europe/Vienna"))).toEqual(r);
  });
});

describe("parseCron timezone token", () => {
  it("strips a CRON_TZ token and discards the zone from the returned object", () => {
    expect(parseCron("CRON_TZ=Europe/Vienna 14 15 * * *")).toEqual({ freq: "daily", minute: 14, hour: 15 });
  });

  it("strips a bare TZ token too", () => {
    expect(parseCron("TZ=UTC 0 9 * * *")).toEqual({ freq: "daily", minute: 0, hour: 9 });
  });

  it("returns null for a TZ token with no fields instead of choking on it", () => {
    expect(parseCron("CRON_TZ=X")).toBeNull();
  });
});

describe("parseCron fallback", () => {
  it("returns null for an empty string instead of throwing", () => {
    expect(parseCron("")).toBeNull();
  });

  it("returns null for an unsupported macro form", () => {
    expect(parseCron("@daily")).toBeNull();
  });

  it("returns null for a syntactically-valid but unmapped 5-field expression", () => {
    // day-of-month and day-of-week both pinned simultaneously: not one of the
    // recognized shapes (daily/weekly/monthly/hourly/minutes), so it must degrade.
    expect(parseCron("1,2 3 4 5 6")).toBeNull();
  });
});

describe("describeCron", () => {
  it("describes the every-minute wildcard", () => {
    expect(describeCron("* * * * *")).toBe("Every minute");
  });

  it("describes a minutes-interval schedule", () => {
    expect(describeCron("*/15 * * * *")).toBe("Every 15 minutes");
  });

  it("describes an hourly-at-minute schedule", () => {
    expect(describeCron("5 * * * *")).toBe("Hourly at :05");
  });

  it("describes an every-N-hours schedule with zero-padded time", () => {
    expect(describeCron("0 */2 * * *")).toBe("Every 2 hours at :00");
  });

  it("describes a daily schedule with zero-padded HH:MM", () => {
    expect(describeCron("0 9 * * *")).toBe("Every day at 09:00");
  });

  it("special-cases the Mon-Fri weekday preset", () => {
    expect(describeCron("0 9 * * 1,2,3,4,5")).toBe("Every weekday at 09:00");
  });

  it("describes a multi-day (non-preset) weekly schedule by day names", () => {
    expect(describeCron("30 14 * * 1,4")).toBe("Every Mon, Thu at 14:30");
  });

  it("describes a monthly schedule", () => {
    expect(describeCron("0 0 1 * *")).toBe("Monthly on day 1 at 00:00");
  });

  it("falls back to a Custom label for an empty string instead of throwing", () => {
    expect(describeCron("")).toBe("Custom ()");
  });

  it("falls back to a Custom label for an unsupported macro", () => {
    expect(describeCron("@daily")).toBe("Custom (@daily)");
  });

  it("falls back to a Custom label for a valid-but-unmapped expression", () => {
    expect(describeCron("1,2 3 4 5 6")).toBe("Custom (1,2 3 4 5 6)");
  });
});

describe("describeCron timezone suffix", () => {
  it("appends a foreign zone so a viewer elsewhere isn't misled by bare digits", () => {
    const foreign =
      localTz() === "Pacific/Kiritimati" ? "Pacific/Chatham" : "Pacific/Kiritimati";
    expect(describeCron(`CRON_TZ=${foreign} 0 9 * * *`)).toBe(
      `Every day at 09:00 (${foreign})`,
    );
  });

  it("omits the suffix when the spec's zone is the viewer's own zone", () => {
    const tz = localTz();
    const expr = tz ? `CRON_TZ=${tz} 0 9 * * *` : "0 9 * * *";
    expect(describeCron(expr)).toBe("Every day at 09:00");
  });

  it("falls back to Custom with the full original expr (prefix included) when unparseable", () => {
    expect(describeCron("CRON_TZ=X")).toBe("Custom (CRON_TZ=X)");
  });
});
