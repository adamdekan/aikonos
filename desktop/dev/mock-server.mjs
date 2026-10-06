// Development-only stand-in for an aikonOS server, so the desktop client can
// be run and clicked through without a compose stack. Never deploy it: it
// signs anyone in, signs nothing, and enforces nothing.
//
//   node desktop/dev/mock-server.mjs          # http://localhost:4300
//
// It serves, on one origin, what the client reaches on a real deployment:
//   - the identity provider (OIDC discovery, a sign-in page, token, logout),
//     at /realms/aikonos, issuing unsigned JWT-shaped tokens;
//   - the web server's client settings: /desktop.json, /runtime-config.js;
//   - the gateway's member API under /api, backed by an in-memory workspace;
//   - POST /agui, a scripted AG-UI stream. Words in the prompt pick the
//     script: "approve" pauses a tool call for a decision ("stepup" makes it
//     a high-risk one), "tool" runs one, "skill" announces a skill, "memory"
//     recalls a concept, "fanout" spawns sub-agents, "fail" rejects the
//     request the way the web proxy does (HTTP 200 carrying a JSON error),
//     "error" ends the run with RUN_ERROR.
//
// MOCK_AUTO_SIGNIN=1 skips the sign-in page, for a debug build started with
// AIKONOS_DEV_HEADLESS_SIGNIN=1. MOCK_DESKTOP_VERSION,
// MOCK_DESKTOP_MINIMUM_VERSION and MOCK_DESKTOP_NOTES advertise a release.
import { createServer } from "node:http";
import { randomUUID } from "node:crypto";

const PORT = Number(process.env.PORT ?? 4300);
const ORIGIN = `http://localhost:${PORT}`;
const ISSUER = `${ORIGIN}/realms/aikonos`;
const USER = {
  sub: "alice@example.com",
  email: "alice@example.com",
  name: "Alice Example",
  tenant_id: "11111111-1111-1111-1111-111111111111",
};
// The desktop release to advertise, as AIKONOS_DESKTOP_* would on a real
// server. A minimum alone also advertises it, as the web server does.
const DESKTOP_MINIMUM = process.env.MOCK_DESKTOP_MINIMUM_VERSION ?? "";
const DESKTOP_VERSION = process.env.MOCK_DESKTOP_VERSION || DESKTOP_MINIMUM;
const DESKTOP_NOTES = process.env.MOCK_DESKTOP_NOTES ?? "";

// ── helpers ────────────────────────────────────────────────────────────────
const b64url = (obj) => Buffer.from(JSON.stringify(obj)).toString("base64url");
function token(kind, ttl) {
  const now = Math.floor(Date.now() / 1000);
  return `${b64url({ alg: "none", typ: "JWT" })}.${b64url({
    ...USER, iss: ISSUER, aud: kind === "id" ? "aikonos-desktop" : "aikonos-broker",
    azp: "aikonos-desktop", iat: now, exp: now + ttl, typ: kind,
  })}.mock`;
}
function json(res, code, body) {
  res.writeHead(code, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}
function html(res, code, body) {
  res.writeHead(code, { "content-type": "text/html; charset=utf-8" });
  res.end(body);
}
// A value safe inside a double-quoted HTML attribute.
function attr(value) {
  return String(value).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
}
function bearer(req) {
  const h = req.headers.authorization ?? "";
  return h.startsWith("Bearer ") && h.length > 7;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── in-memory state ────────────────────────────────────────────────────────
const files = new Map(); // path -> { bytes: Buffer, modified: ISO }
function putFile(path, bytes) {
  files.set(path, { bytes: Buffer.from(bytes), modified: new Date().toISOString() });
}
putFile("reports/q3-summary.md", "# Q3 summary\n\nRevenue grew 12%.\n");
putFile("reports/ward-rota.xlsx", Buffer.alloc(18_432, 1));
putFile("notes.txt", "Remember to review the discharge checklist.\n");
putFile("references/xray-sample.png", Buffer.alloc(52_000, 2));
const now = () => new Date().toISOString();
function seedSession(id, title, prompt, reply, extra = {}) {
  const record = {
    id, title, agent_id: null, agent_name: "", pinned: false, pinned_at: null,
    created_at: now(), updated_at: now(), thread_id: randomUUID(), first_message: prompt,
    messages: [
      { role: "user", text: prompt },
      { role: "assistant", text: reply, tools: [], error: null },
    ],
    ...extra,
  };
  putFile(`.agent/Sessions/${id}.json`, JSON.stringify(record));
  return record;
}
const seeded = [
  seedSession(randomUUID(), "Summarise the Q3 report", "Summarise the Q3 report",
    "**Q3 at a glance**\n\n- Revenue grew 12%\n- Two new wards opened\n\n```text\nnet: +12%\n```"),
  seedSession(randomUUID(), "Daily news brief", "Daily news brief",
    "Here is today's brief:\n\n1. A new day clinic opens in the north wing.\n2. New EU guidance on AI in care.",
    { source: "schedule", schedule_id: "sched-1" }),
];
putFile(".agent/Sessions/index.json", JSON.stringify(seeded.map((r) => ({
  id: r.id, title: r.title, agent_id: r.agent_id, agent_name: r.agent_name, pinned: false,
  pinned_at: null, created_at: r.created_at, updated_at: r.updated_at,
  source: r.source ?? null, schedule_id: r.schedule_id ?? null,
}))));

const agents = [{ id: "7b0c1c9e-0d1a-4f3e-9c2a-2f1d5b2e8a11", name: "Ward assistant" }];
let soul = "Be concise. Answer in the language of the question.";
const pending = new Map(); // toolCallId -> { info, resolve }
let inbox = [
  {
    envelopeId: "env-1", fromUserId: "bob@example.com", fromDisplayName: "Bob Example",
    task: { intent: "Check the night rota for ward 4", payloadRef: "", requiredSkills: [], priority: "high", kind: "" },
    receivedAt: now(), status: "pending",
  },
  {
    envelopeId: "env-2", fromUserId: "carol@example.com", fromDisplayName: "Carol Example",
    task: { intent: "", payloadRef: "", requiredSkills: [], priority: "normal", kind: "skill_transfer" },
    receivedAt: now(), status: "pending",
  },
];
let schedules = [
  {
    id: "sched-1", owner: USER.sub, prompt: "Daily news brief", kind: "CRON",
    cronExpr: "CRON_TZ=Europe/Vienna 0 7 * * 1,2,3,4,5", nextFireAt: now(),
    approvedTools: ["web.fetch"], workflowLineageId: "", workflowDisplayName: "",
    state: "ACTIVE", lastFireAt: now(), lastStatus: "ok", lastSummary: "Brief written",
    runCount: 12, createdBy: USER.sub, createdAt: now(),
  },
];
let personalSkills = [
  { name: "discharge-letter", description: "Draft a discharge letter from notes", keywords: ["discharge"],
    allowedTools: ["doc.write"], disableModelInvocation: false, valid: true, warning: "", sizeBytes: 2048 },
];
const bundles = [
  { id: "b-1", name: "pdf", description: "Read and summarise PDFs", body: "", allowedTools: ["pdf_extract"],
    contextFork: false, disableModelInvocation: false, createdBy: "admin", keywords: ["pdf"], filePaths: [] },
];
const workflows = [
  { lineageId: "wf-1", name: "Morning brief", version: 3, visibilityKind: "private", status: "active",
    accessState: "runnable", missingRequirements: [], isOwner: true, boundAgentId: "", boundAgentOk: true },
  { lineageId: "wf-2", name: "Shared intake triage", version: 1, visibilityKind: "shared", status: "active",
    accessState: "greyed_out", missingRequirements: ["skill:triage"], isOwner: false, boundAgentId: "", boundAgentOk: true },
];
const definitions = {
  "wf-1": { apiVersion: "aikonos.com/v1", kind: "Workflow",
    metadata: { name: "Morning brief", description: "Fetch the news and summarise it", visibility: { kind: "private" } },
    inputs: [{ name: "topic", default: "health care" }],
    steps: [{ kind: "tool", skill: "web.fetch", args: {} }, { kind: "reason", instruction: "Summarise" }] },
};
let connectors = [
  { connectorId: "c-1", provider: 2, displayName: "OneDrive (alice)", scopes: ["Files.Read"], status: "active", connectedAt: now(), managed: false },
];
let workspacePref = { backend: "local", onedriveFolderPath: "" };
const concepts = [
  { id: "ward-4-rota", scope: "user", groupId: "", agentId: "", type: "fact", title: "Ward 4 rota rules",
    description: "Night shifts rotate weekly", tags: ["rota"], status: "stable", trustTier: "human-reviewed",
    stale: false, staleAfter: "", generatedBy: "agent", generatedAt: now() },
];

// ── AG-UI scripts ──────────────────────────────────────────────────────────
function sseHeaders(res) {
  res.writeHead(200, {
    "content-type": "text/event-stream", "cache-control": "no-cache",
    "x-accel-buffering": "no", connection: "keep-alive",
  });
  res.write("retry: 3000\n\n");
}
const send = (res, ev) => res.write(`data: ${JSON.stringify(ev)}\n\n`);

async function streamText(res, text) {
  const messageId = randomUUID();
  send(res, { type: "TEXT_MESSAGE_START", messageId, role: "assistant" });
  for (const chunk of text.match(/[\s\S]{1,12}/g) ?? []) {
    send(res, { type: "TEXT_MESSAGE_CONTENT", messageId, delta: chunk });
    await sleep(25);
  }
  send(res, { type: "TEXT_MESSAGE_END", messageId });
}

async function runTool(res, name, toolId, args, { approval = false, stepUp = false } = {}) {
  const toolCallId = `toolu_${randomUUID().replaceAll("-", "").slice(0, 20)}`;
  send(res, { type: "TOOL_CALL_START", toolCallId, toolCallName: name, toolDescription: `Mock ${name}` });
  send(res, { type: "TOOL_CALL_ARGS", toolCallId, delta: JSON.stringify(args) });
  send(res, { type: "TOOL_CALL_END", toolCallId });
  let approved = true;
  if (approval) {
    const info = {
      toolCallId, toolName: name, toolId, effectClass: stepUp ? 4 : 2,
      reason: stepUp ? "elevated (step-up) approval required" : "human approval required",
      args, stepUp,
    };
    send(res, { type: "CUSTOM", name: "aikonos.approval.request", value: info });
    approved = await new Promise((resolve) => {
      pending.set(toolCallId, { info, resolve });
      setTimeout(() => { if (pending.delete(toolCallId)) resolve(false); }, 120_000);
    });
  }
  await sleep(400);
  const content = approved
    ? JSON.stringify({ content: [{ type: "text", text: `${name} ok` }], details: {} })
    : JSON.stringify({ content: [{ type: "text", text: "aikonOS: approval declined" }], details: {} });
  send(res, { type: "TOOL_CALL_RESULT", messageId: randomUUID(), toolCallId, content, role: "tool" });
  if (!approved) send(res, { type: "CUSTOM", name: "aikonos.tool.error", value: { toolCallId, content } });
  return approved;
}

async function agui(req, res, body) {
  if (!bearer(req)) return json(res, 200, { error: "invalid or expired bearer token" });
  const prompt = String(body.prompt ?? "");
  const words = prompt.toLowerCase();
  // The web proxy flushes 200 before reading the gateway's status.
  if (words.includes("fail")) return json(res, 200, { error: "unknown agent: mock" });
  sseHeaders(res);
  const threadId = body.threadId ?? randomUUID();
  const runId = randomUUID();
  let closed = false;
  // The response's close, not the request's: a request emits "close" as
  // soon as its body has been read.
  res.on("close", () => { closed = true; });
  send(res, { type: "RUN_STARTED", threadId, runId });
  send(res, { type: "CUSTOM", name: "aikonos.user", value: { user: USER.email } });
  if (words.includes("skill") || body.skillName) {
    send(res, { type: "CUSTOM", name: "aikonos.skills.loaded", value: { skills: [
      { name: body.skillName ?? "pdf", description: "Read and summarise PDFs", status: "loaded" },
      { name: "spreadsheet", description: "", status: "suppressed", reason: "activation limit reached (3 per turn)" },
    ] } });
  }
  if (words.includes("memory")) {
    send(res, { type: "CUSTOM", name: "aikonos.memory.recalled", value: { concepts: [
      { id: "ward-4-rota", scope: "user", title: "Ward 4 rota rules", status: "stable", trustTier: "human-reviewed", stale: false },
    ] } });
  }
  await sleep(300);
  if (words.includes("error")) {
    await streamText(res, "Starting…");
    send(res, { type: "RUN_ERROR", message: "internal error" });
    return res.end();
  }
  if (words.includes("approve")) {
    await streamText(res, "I need to write a file. ");
    const ok = await runTool(res, "doc_write", "doc.write", { path: "reports/draft.md", content: "Draft text", options: { overwrite: true, tags: ["a", "b"] } },
      { approval: true, stepUp: words.includes("stepup") });
    if (closed) return;
    await streamText(res, ok ? "The file is written to **reports/draft.md**." : "You declined, so nothing was written.");
  } else if (words.includes("tool")) {
    await runTool(res, "web_search", "web.search", { query: "EU AI Act hospitals" });
    await runTool(res, "web_fetch", "web.fetch", { url: "https://example.org/news/ai-act" });
    await streamText(res, "I searched the web and read one page. The **EU AI Act** applies to hospitals as deployers of high-risk systems.");
  } else if (words.includes("fanout")) {
    for (const [index, task] of ["read the rota", "check the budget"].entries()) {
      send(res, { type: "CUSTOM", name: "aikonos.subagent.spawned", value: { index, task } });
    }
    await sleep(800);
    send(res, { type: "CUSTOM", name: "aikonos.subagent.completed", value: { index: 0, task: "read the rota", ok: true, cost: 0.0042 } });
    send(res, { type: "CUSTOM", name: "aikonos.subagent.completed", value: { index: 1, task: "check the budget", ok: false, failure: "timeout", cost: 0.0011 } });
    await streamText(res, "One branch finished; the other timed out.");
  } else {
    await streamText(res, `You said: *${prompt}*\n\nHere is a list:\n\n1. One\n2. Two\n\n| Ward | Beds |\n|---|---|\n| 4 | 22 |\n\n\`inline code\` and a [link](https://example.org).`);
  }
  send(res, { type: "RUN_FINISHED", threadId, runId });
  res.end();
}

async function workflowStream(res, lineageId) {
  sseHeaders(res);
  const steps = definitions[lineageId]?.steps ?? [];
  for (const [index, step] of steps.entries()) {
    await sleep(500);
    res.write(`event: step\ndata: ${JSON.stringify({ index, skill: step.skill, ok: true })}\n\n`);
  }
  const result = { ok: true, result: { halted: false, steps: steps.map((step, stepIndex) => ({
    stepIndex, kind: step.kind, skill: step.skill ?? "", resolvedArgs: {}, allowed: true,
    output: step.kind === "reason" ? "Summary: all quiet." : { status: 200 },
  })) } };
  res.write(`event: result\ndata: ${JSON.stringify(result)}\n\n`);
  res.end();
}

// ── routes ─────────────────────────────────────────────────────────────────
function listFiles(query) {
  const dir = (query.get("dir") ?? "").replace(/\/$/, "");
  const recursive = query.get("recursive") === "1";
  const hidden = query.get("includeHidden") === "1";
  const out = new Map();
  for (const [path, file] of files) {
    if (!hidden && path.split("/").some((seg) => seg.startsWith("."))) continue;
    if (dir === "" ) {
      out.set(path, { path, size: file.bytes.length, modified: file.modified, isDir: false });
      continue;
    }
    const prefix = dir === "." ? "" : `${dir}/`;
    if (!path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    if (recursive || !rest.includes("/")) {
      out.set(path, { path, size: file.bytes.length, modified: file.modified, isDir: false });
    } else {
      const sub = prefix + rest.split("/")[0];
      out.set(sub, { path: sub, size: 0, modified: null, isDir: true });
    }
  }
  return [...out.values()].sort((a, b) => a.path.localeCompare(b.path));
}

async function api(req, res, url, raw) {
  if (!bearer(req)) return json(res, 401, { error: "invalid or expired bearer token" });
  const path = url.pathname.replace(/^\/api/, "");
  const q = url.searchParams;
  const body = (() => { try { return raw.length ? JSON.parse(raw.toString("utf8")) : {}; } catch { return {}; } })();
  const m = (re) => path.match(re);
  const method = req.method;
  let r;

  if (method === "GET" && path === "/files") return json(res, 200, { files: listFiles(q) });
  if (method === "GET" && path === "/files/content") {
    const file = files.get(q.get("path"));
    if (!file) return json(res, 404, { error: "file not found" });
    return json(res, 200, { path: q.get("path"), mime: "application/octet-stream", contentBase64: file.bytes.toString("base64") });
  }
  if (method === "POST" && path === "/files") {
    const bytes = Buffer.from(body.contentBase64 ?? "", "base64");
    if (bytes.length > 10 * 1024 * 1024) return json(res, 400, { error: "file too large" });
    if (body.path?.startsWith(".agent/") && !body.path.startsWith(".agent/Sessions/")) return json(res, 403, { error: "reserved path" });
    putFile(body.path, bytes);
    return json(res, 200, { file: { path: body.path, size: bytes.length, modified: now(), isDir: false } });
  }
  if (method === "DELETE" && path === "/files") {
    const target = q.get("path");
    let deleted = files.delete(target);
    for (const key of [...files.keys()]) if (key.startsWith(`${target}/`)) { files.delete(key); deleted = true; }
    return json(res, 200, { success: deleted });
  }
  if (method === "POST" && path === "/files/move") {
    const file = files.get(body.from);
    if (!file) return json(res, 404, { error: "file not found" });
    files.delete(body.from); files.set(body.to, file);
    return json(res, 200, { file: { path: body.to, size: file.bytes.length, modified: file.modified, isDir: false } });
  }
  if (method === "POST" && path === "/files/dir") { putFile(`${body.path}/.keep`, ""); return json(res, 200, { success: true }); }

  if (method === "GET" && path === "/user/skills") return json(res, 200, { skills: ["web.fetch", "doc.write", "scheduler", "workflows"] });
  if (method === "GET" && path === "/user/skill-bundles") return json(res, 200, { bundles });
  if (method === "GET" && path === "/agents") return json(res, 200, { agents });
  if ((r = m(/^\/agents\/([^/]+)\/soul$/))) {
    if (method === "PUT") { soul = String(body.soul ?? ""); }
    return json(res, 200, { soul });
  }
  if (method === "GET" && path === "/delegatable-users") return json(res, 200, {
    users: [{ userId: "bob@example.com", displayName: "Bob Example" }],
    groups: [{ groupId: "ward-4", displayName: "Ward 4", memberCount: 6 }],
  });
  if (method === "POST" && path === "/delegate") return json(res, 200, { ok: true, envelopeId: randomUUID() });
  if (method === "GET" && path === "/inbox") return json(res, 200, { envelopes: inbox });
  if ((r = m(/^\/inbox\/([^/]+)\/dismiss$/))) { inbox = inbox.filter((e) => e.envelopeId !== decodeURIComponent(r[1])); return json(res, 200, { success: true }); }
  if (method === "GET" && path === "/approvals") return json(res, 200, { approvals: [...pending.values()].map((p) => p.info) });
  if ((r = m(/^\/approve\/([^/]+)$/))) {
    const entry = pending.get(decodeURIComponent(r[1]));
    if (!entry) return json(res, 200, { resolved: false });
    pending.delete(decodeURIComponent(r[1]));
    entry.resolve(body.approved === true);
    return json(res, 200, { resolved: true });
  }
  if ((r = m(/^\/sessions\/([^/]+)\/usage$/))) return json(res, 200, {
    models: ["mock-large", "mock-small"], tokensIn: 1234, tokensOut: 567, cacheRead: 0, cacheWrite: 0, costMicros: 4210, calls: 2,
  });
  if (method === "GET" && path === "/connectors") return json(res, 200, { connectors });
  if (method === "GET" && path === "/connectors/providers") return json(res, 200, { providers: [
    { provider: 1, key: "google_drive", displayName: "Google Drive", connectorId: "" },
    { provider: 2, key: "onedrive", displayName: "OneDrive", connectorId: "c-1" },
  ] });
  if (method === "POST" && path === "/connectors/begin") return json(res, 200, { authorizeUrl: `${ORIGIN}/mock-oauth?provider=${body.provider}`, state: randomUUID() });
  if ((r = m(/^\/connectors\/([^/]+)\/revoke$/))) { connectors = connectors.filter((c) => c.connectorId !== decodeURIComponent(r[1])); return json(res, 200, { success: true }); }
  if (path === "/workspace/backend") {
    if (method === "PUT") workspacePref = { backend: body.backend, onedriveFolderPath: body.onedriveFolderPath ?? "" };
    return json(res, 200, { pref: workspacePref, onedriveAvailable: true, onedriveStatus: "connected" });
  }
  if (path === "/workspace/onedrive/folders") {
    const dir = q.get("dir") ?? "";
    const names = dir === "" ? ["Documents", "Ward 4"] : dir.split("/").length < 3 ? ["Shared", "Archive"] : [];
    return json(res, 200, { folders: names.map((name) => ({ name, path: dir ? `${dir}/${name}` : name })) });
  }
  if (method === "GET" && path === "/memory/groups") return json(res, 200, { groups: [{ groupId: "ward-4", member: true, manager: true }] });
  if (method === "GET" && path === "/memory") return json(res, 200, { concepts: q.get("scope") === "user" ? concepts : [] });
  if (method === "GET" && path === "/memory/concept") return json(res, 200, { meta: concepts[0], body: "Night shifts rotate weekly, starting Mondays." });
  if (method === "POST" && /^\/memory\/(verify|deprecate)$/.test(path)) {
    concepts[0] = { ...concepts[0], ...(path.endsWith("verify") ? { trustTier: "human-reviewed" } : { status: "deprecated" }) };
    return json(res, 200, { meta: concepts[0] });
  }
  if (method === "POST" && path === "/memory/delete") return json(res, 200, { deleted: true });
  if (method === "GET" && path === "/schedules") return json(res, 200, { schedules });
  if (method === "POST" && path === "/schedules") {
    const schedule = { ...schedules[0], id: randomUUID(), prompt: body.prompt, kind: body.kind ?? "CRON",
      cronExpr: body.cronExpr ?? "", nextFireAt: body.runAt ?? now(), approvedTools: body.approvedTools ?? [],
      state: "ACTIVE", runCount: 0, lastFireAt: null, lastStatus: "", lastSummary: "" };
    schedules = [...schedules, schedule];
    return json(res, 200, { schedule });
  }
  if ((r = m(/^\/schedules\/([^/]+)$/))) {
    const id = decodeURIComponent(r[1]);
    if (method === "DELETE") { schedules = schedules.filter((s) => s.id !== id); return json(res, 200, { success: true }); }
    schedules = schedules.map((s) => s.id !== id ? s : body.action
      ? { ...s, state: body.action === "pause" ? "PAUSED" : "ACTIVE" }
      : { ...s, prompt: body.prompt, kind: body.kind, cronExpr: body.cronExpr ?? "", approvedTools: body.approvedTools ?? [] });
    return json(res, 200, { schedule: schedules.find((s) => s.id === id) ?? null });
  }
  if (method === "GET" && path === "/workflows") return json(res, 200, { workflows, nextCursor: "", sharedUnavailable: false });
  if ((r = m(/^\/workflows\/([^/]+)\/run$/))) {
    if (q.get("stream") === "1") return workflowStream(res, decodeURIComponent(r[1]));
    return json(res, 200, { ok: true, result: { halted: false, steps: [] } });
  }
  if ((r = m(/^\/workflows\/([^/]+)\/versions$/))) return json(res, 200, { versions: [
    { version: 3, approvalState: "approved", createdAt: now() }, { version: 2, approvalState: "approved", createdAt: now() },
  ], nextCursor: "" });
  if ((r = m(/^\/workflows\/([^/]+)\/(rate|publish|pin|decide)$/))) return json(res, 200, { ok: true });
  if ((r = m(/^\/workflows\/([^/]+)\/fork$/))) return json(res, 200, { lineageId: randomUUID() });
  if ((r = m(/^\/workflows\/([^/]+)$/))) {
    if (method === "DELETE") return json(res, 200, { ok: true, versionsDeleted: 3 });
    const def = definitions[decodeURIComponent(r[1])];
    if (!def) return json(res, 404, { error: "workflow not found" });
    return json(res, 200, { definitionJson: JSON.stringify(def), version: 3 });
  }
  if (method === "GET" && path === "/skills") return json(res, 200, { skills: personalSkills, granted: bundles, grantedUnavailable: false });
  if (method === "POST" && path === "/skills/import") {
    const text = raw.toString("utf8");
    const name = (text.match(/^name:\s*(.+)$/m)?.[1] ?? "imported-skill").trim();
    if (personalSkills.some((s) => s.name === name)) return json(res, 409, { error: `a skill named "${name}" already exists`, suggested_name: `${name}-2` });
    personalSkills = [...personalSkills, { name, description: "Imported skill", keywords: [], allowedTools: [], disableModelInvocation: false, valid: true, warning: "", sizeBytes: raw.length }];
    return json(res, 201, { name });
  }
  if ((r = m(/^\/skills\/transfers\/([^/]+)\/accept$/))) { inbox = inbox.filter((e) => e.envelopeId !== decodeURIComponent(r[1])); return json(res, 200, { installedName: body.name_override ?? "shared-skill" }); }
  if ((r = m(/^\/skills\/transfers\/([^/]+)$/))) return json(res, 200, {
    skillName: "shared-skill", fromUserId: "carol@example.com", body: "---\nname: shared-skill\n---\nDo the thing.",
    manifest: [{ path: "SKILL.md", size: 42 }], flags: [], contentHash: "sha256:mock", conflict: false,
  });
  if ((r = m(/^\/skills\/([^/]+)\/share$/))) return json(res, 200, { envelopeIds: [randomUUID()], skippedUserIds: [] });
  if ((r = m(/^\/skills\/([^/]+)$/)) && method === "DELETE") {
    personalSkills = personalSkills.filter((s) => s.name !== decodeURIComponent(r[1]));
    return json(res, 200, { success: true });
  }
  if (method === "GET" && path === "/admin/assignments") return json(res, 403, { error: "tenant admin required" });
  return json(res, 404, { message: `Route ${method}:${path} not found`, error: "Not Found", statusCode: 404 });
}

function oidc(req, res, url, raw) {
  const path = url.pathname.slice("/realms/aikonos".length);
  if (path === "/.well-known/openid-configuration") return json(res, 200, {
    issuer: ISSUER,
    authorization_endpoint: `${ISSUER}/protocol/openid-connect/auth`,
    token_endpoint: `${ISSUER}/protocol/openid-connect/token`,
    end_session_endpoint: `${ISSUER}/protocol/openid-connect/logout`,
    revocation_endpoint: `${ISSUER}/protocol/openid-connect/revoke`,
  });
  if (path === "/protocol/openid-connect/auth") {
    const target = new URL(url.searchParams.get("redirect_uri"));
    target.searchParams.set("code", randomUUID());
    target.searchParams.set("state", url.searchParams.get("state") ?? "");
    if (process.env.MOCK_AUTO_SIGNIN === "1") {
      res.writeHead(302, { location: target.toString() });
      return res.end();
    }
    return html(res, 200, `<!doctype html><title>Mock sign-in</title>
<body style="font-family:sans-serif;background:#2b2b2b;color:#f0eee9;display:grid;place-items:center;height:100vh;margin:0">
<form method="get" action="${target.origin}${target.pathname}">
<h1 style="font-size:20px">Mock identity provider</h1><p>Signs in as ${USER.email}.</p>
${[...target.searchParams].map(([k, v]) => `<input type="hidden" name="${attr(k)}" value="${attr(v)}">`).join("")}
<button style="padding:8px 16px">Sign in</button></form></body>`);
  }
  if (path === "/protocol/openid-connect/token") {
    const form = new URLSearchParams(raw.toString("utf8"));
    if (!["authorization_code", "refresh_token"].includes(form.get("grant_type"))) return json(res, 400, { error: "unsupported_grant_type" });
    return json(res, 200, {
      access_token: token("access", 120), id_token: token("id", 120),
      refresh_token: `refresh-${randomUUID()}`, expires_in: 120, token_type: "Bearer",
    });
  }
  if (path === "/protocol/openid-connect/logout" || path === "/protocol/openid-connect/revoke") return json(res, 200, {});
  return json(res, 404, { error: "not found" });
}

const server = createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", async () => {
    const raw = Buffer.concat(chunks);
    const url = new URL(req.url, ORIGIN);
    const started = Date.now();
    res.on("finish", () => console.log(`${req.method} ${url.pathname} ${res.statusCode} ${Date.now() - started}ms`));
    try {
      if (url.pathname.startsWith("/realms/aikonos")) return oidc(req, res, url, raw);
      if (url.pathname === "/desktop.json") return json(res, 200, {
        oidc: { authority: ISSUER, clientId: "aikonos-desktop", scope: "openid profile", token: "access" },
        release: DESKTOP_VERSION
          ? {
              version: DESKTOP_VERSION,
              ...(DESKTOP_MINIMUM ? { minimumVersion: DESKTOP_MINIMUM } : {}),
              url: `${ORIGIN}/downloads/aikonos.exe`,
              ...(DESKTOP_NOTES ? { notes: DESKTOP_NOTES } : {}),
            }
          : null,
      });
      if (url.pathname === "/runtime-config.js") {
        res.writeHead(200, { "content-type": "application/javascript" });
        return res.end(`window.__AIKONOS_CONFIG__ = ${JSON.stringify({ oidc: { authority: ISSUER } })};\n`);
      }
      if (url.pathname === "/healthz") return json(res, 200, { ok: true });
      if (url.pathname === "/mock-oauth") return html(res, 200, "<p>Mock provider consent. Close this tab.</p>");
      if (req.method === "POST" && url.pathname === "/agui") {
        let body = {};
        try { body = JSON.parse(raw.toString("utf8")); } catch { /* empty */ }
        return await agui(req, res, body);
      }
      if (url.pathname.startsWith("/api/")) return await api(req, res, url, raw);
      return html(res, 200, "<p>aikonOS mock server. Point the desktop app at this address.</p>");
    } catch (err) {
      console.error(err);
      if (!res.headersSent) json(res, 500, { error: "internal error" });
      else res.end();
    }
  });
});

server.listen(PORT, () => console.log(`aikonOS mock server on ${ORIGIN} (dev only)`));
