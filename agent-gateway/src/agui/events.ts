// Canonical source for the gateway's CUSTOM AG-UI SSE event names (C9,
// ). Every gateway producer and the gateway's
// own agui tests import these instead of repeating the literal.
//
// The webui consumer (webui/web/src/api/agui.js) cannot import this module —
// webui/web is plain JS with no npm-workspace/build link to agent-gateway —
// so it keeps its own literals, pinned instead by the repo-root
// contracts.json grep-contract test (agent-gateway/test/contracts.test.ts).
export const AIKONOS_APPROVAL_REQUEST = "aikonos.approval.request";
export const AIKONOS_TOOL_ERROR = "aikonos.tool.error";
export const AIKONOS_USER = "aikonos.user";
export const AIKONOS_SKILLS_LOADED = "aikonos.skills.loaded";
export const AIKONOS_MEMORY_RECALLED = "aikonos.memory.recalled";
export const AIKONOS_SUBAGENT_SPAWNED = "aikonos.subagent.spawned";
export const AIKONOS_SUBAGENT_COMPLETED = "aikonos.subagent.completed";
