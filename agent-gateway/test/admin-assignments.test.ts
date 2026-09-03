// Regression: AccessControl.vue (/admin/roles) calls assignRole with no
// `section` field, so the request body's tuple.section arrives undefined. section
// is a curated UI hint the broker ignores, but it is an enum (int32) on the gRPC
// wire — forwarding it undefined crashed the north client with
// "Request message serialization failure: invalid int32: undefined".
// normalizeRoleTuple must default section so any caller (with or without it)
// produces a wire-safe tuple.
import { test } from "node:test";
import assert from "node:assert/strict";

import { normalizeRoleTuple } from "../src/routes/admin.js";
import { AssignmentSection } from "../gen/ts/proto/broker.js";

test("normalizeRoleTuple defaults an omitted section to UNSPECIFIED", () => {
  const t = normalizeRoleTuple({
    user: "user:alice@example.com",
    relation: "owner_user",
    object: "agent:abc-123",
  });
  assert.equal(t.section, AssignmentSection.ASSIGNMENT_SECTION_UNSPECIFIED);
  assert.equal(typeof t.section, "number");
  assert.equal(t.user, "user:alice@example.com");
  assert.equal(t.relation, "owner_user");
  assert.equal(t.object, "agent:abc-123");
});

test("normalizeRoleTuple preserves an explicit section", () => {
  const t = normalizeRoleTuple({
    user: "agent:echo-agent",
    relation: "permitted_agent",
    object: "mcp_connector:conn-1",
    section: AssignmentSection.MCP_ACCESS,
  });
  assert.equal(t.section, AssignmentSection.MCP_ACCESS);
});

test("normalizeRoleTuple section is never undefined for the usable_by case", () => {
  const t = normalizeRoleTuple({
    user: "group:security-team",
    relation: "usable_by",
    object: "agent:echo-agent2",
  });
  assert.notEqual(t.section, undefined);
  assert.equal(typeof t.section, "number");
});
