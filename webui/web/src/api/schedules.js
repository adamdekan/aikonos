import { get, post, del, patch } from "./client.js";

export function listSchedules() {
  return get("/schedules");
}

export function createSchedule({ prompt, kind, cronExpr, runAt, approvedTools }) {
  return post("/schedules", { body: { prompt, kind, cronExpr, runAt, approvedTools } });
}

export function pauseSchedule(id) {
  return patch(`/schedules/${id}`, { body: { action: "pause" } });
}

export function resumeSchedule(id) {
  return patch(`/schedules/${id}`, { body: { action: "resume" } });
}

// Revoke ends the schedule's standing delegation for good (AG-17 S4): the run
// is paused and cannot be resumed.
export function revokeSchedule(id) {
  return patch(`/schedules/${id}`, { body: { action: "revoke" } });
}

// No `action` field → the broker takes the full-edit branch (overwrites
// prompt/kind/cronExpr/runAt/approvedTools). approvedTools must be resent or the
// broker treats nil as "clear to empty".
export function updateSchedule(id, { prompt, kind, cronExpr, runAt, approvedTools }) {
  return patch(`/schedules/${id}`, { body: { prompt, kind, cronExpr, runAt, approvedTools } });
}

export function deleteSchedule(id) {
  return del(`/schedules/${id}`);
}
