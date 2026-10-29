import { describe, it, expect } from "vitest";
import { detail, normalizeEvent, fmtTimeHistory } from "../components/audit/audit-format.js";

describe("audit-format detail", () => {
  it("summarizes a policy decision record instead of '[object Object]'", () => {
    const d = detail({ context: { kind: "tool_invocation", outcome: { decision: "deny", decided_by: "opa:aikonos/tool_invocation" } } });
    expect(d).toBe("tool_invocation · deny · opa:aikonos/tool_invocation");
  });

  it("flattens nested context (task.created obo) into key=value, not raw JSON", () => {
    const d = detail({ context: { obo: { kind: "interactive", subject: "alice", actor: "" } } });
    expect(d).toBe("obo.kind=interactive · obo.subject=alice");
    expect(d).not.toMatch(/[{}]/);
  });

  it("keeps the existing tool summary", () => {
    expect(detail({ context: { tool_id: "web.fetch", cost: 2, outcome: "ok" } })).toBe("web.fetch · 2u · ok");
  });
});

describe("audit-format normalizeEvent", () => {
  it("maps the gateway's camelCase query result to the stream's shape", () => {
    const ev = normalizeEvent({ eventId: "e1", eventType: "x.y", resourceRef: "r", actorUserId: "u", occurredAt: "2026-10-01T12:00:00.500Z", context: { a: 1 } });
    expect(ev.event_id).toBe("e1");
    expect(ev.event_type).toBe("x.y");
    expect(ev.resource_ref).toBe("r");
    expect(ev.actor_user_id).toBe("u");
    expect(ev.occurred_at).toEqual({ seconds: Date.parse("2026-10-01T12:00:00Z") / 1000, nanos: 500e6 });
    expect(ev.context).toEqual({ a: 1 });
    expect(fmtTimeHistory(ev)).not.toBe("");
  });

  it("passes a stream-shaped event through unchanged", () => {
    const ev = { event_id: "e1", occurred_at: { seconds: 1 } };
    expect(normalizeEvent(ev)).toBe(ev);
  });
});
