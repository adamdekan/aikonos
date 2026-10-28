import { get, post } from "./client.js";

// Broker-held approvals the caller may decide (AG-07): every approver of an
// n-of-m gate sees them, from any session.
export function listPendingApprovals() {
  return get("/pending-approvals");
}

// argsDigest is the one the list returned: the broker refuses an approval
// whose arguments changed since.
export function decideApproval(taskId, approved, argsDigest) {
  return post(`/pending-approvals/${encodeURIComponent(taskId)}`, { body: { approved, argsDigest } });
}
