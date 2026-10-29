import { describe, it, expect } from "vitest";
import { mount } from "@vue/test-utils";
import EventDetail from "../components/audit/EventDetail.vue";
import { normalizeEvent } from "../components/audit/audit-format.js";

describe("EventDetail.vue", () => {
  it("shows occurred_at as an ISO time and decision by name", () => {
    const ev = normalizeEvent({ eventId: "evt-1", occurredAt: "2023-11-14T22:13:20.123Z", decision: 2 });
    const text = mount(EventDetail, { props: { ev } }).text();
    expect(text).toContain("2023-11-14T22:13:20.123Z");
    expect(text).toContain("DENY");
    expect(text).not.toContain("seconds");
  });
});
