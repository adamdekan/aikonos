import { test } from "node:test";
import assert from "node:assert/strict";
import { preAuthApprover } from "../src/scheduler/ticker";
import type { ApprovalInfo } from "../src/broker/governance";

function info(toolId: string): ApprovalInfo {
  return {
    toolCallId: "c1",
    toolName: toolId,
    toolId,
    effectClass: 0,
    reason: "human approval required",
    args: {},
    stepUp: false,
  };
}

test("pre-auth approver grants a tool in the allowlist", async () => {
  const approve = preAuthApprover(["email.draft", "gdrive.write"]);
  assert.equal(await approve(info("email.draft")), true);
  assert.equal(await approve(info("gdrive.write")), true);
});

test("pre-auth approver denies a tool not in the allowlist", async () => {
  const approve = preAuthApprover(["email.draft"]);
  assert.equal(await approve(info("gdrive.write")), false);
  assert.equal(await approve(info("web.fetch")), false);
});

test("empty allowlist denies everything (auto-deny unattended)", async () => {
  const approve = preAuthApprover([]);
  assert.equal(await approve(info("email.draft")), false);
});
