// End-user-facing labels for chat tool-trace lines. The model-facing tool
// descriptions (agent-gateway/src/pi/tools.ts) are too technical for the chat
// UI, and two calls to the same tool render identically. toolLabel() maps a
// tool call to a plain phrase plus one salient argument, so "web_search" for
// "Reuters news" reads differently from "web_search" for "weather".

// Tool name → short, plain, gerund-form phrase. Covers every built-in Pi tool
// (agent-gateway/src/pi/tools.ts TOOL_NAMES) plus load_skill (handled below).
export const FRIENDLY = {
  web_fetch: "Reading a web page",
  web_search: "Searching the web",
  workspace_read: "Listing workspace files",
  doc_read: "Reading a file",
  doc_write: "Writing a file",
  email_draft: "Drafting an email",
  gdrive_read: "Reading from Google Drive",
  gdrive_write: "Writing to Google Drive",
  onedrive_read: "Reading from OneDrive",
  onedrive_write: "Writing to OneDrive",
  delegate: "Delegating a task",
  workflow_save: "Saving a workflow",
  workflow_run: "Running a workflow",
  workflow_list: "Listing workflows",
  workflow_publish: "Publishing a workflow",
  workflow_propose: "Proposing a workflow change",
  analyze_image: "Analyzing an image",
  docx_create: "Creating a Word document",
  docx_edit: "Editing a Word document",
  docx_extract: "Reading a Word document",
  xlsx_create: "Creating a spreadsheet",
  xlsx_edit: "Editing a spreadsheet",
  xlsx_extract: "Reading a spreadsheet",
  xlsx_recalc: "Recalculating a spreadsheet",
  pptx_create: "Creating a presentation",
  pptx_edit: "Editing a presentation",
  pptx_extract: "Reading a presentation",
  pptx_thumbnail: "Previewing a presentation",
  pdf_create: "Creating a PDF",
  pdf_transform: "Transforming a PDF",
  pdf_extract: "Reading a PDF",
  office_convert: "Converting a document",
  reason: "Thinking",
};

// Workflow steps carry broker skill ids ("web.fetch") rather than Pi tool names
// ("web_fetch"); normalize the separator so both resolve to the same label.
function normalizeName(name) {
  return String(name || "").replace(/\./g, "_");
}

const ARG_MAX = 60;

function clamp(text, max) {
  const t = String(text || "").trim();
  return t.length > max ? t.slice(0, max - 1) + "…" : t;
}

function basename(p) {
  const parts = String(p).split("/").filter(Boolean);
  return parts.length ? parts[parts.length - 1] : String(p);
}

function hostname(url) {
  try {
    return new URL(url).hostname;
  } catch {
    return String(url);
  }
}

function firstWords(text, n) {
  return String(text || "").trim().split(/\s+/).slice(0, n).join(" ");
}

function firstSentence(text) {
  return clamp(String(text || "").split(". ")[0], 80);
}

// One short human detail from the call's arguments, or "" when there's nothing
// useful (or the JSON is malformed). Truncated to 60 chars.
export function salientArg(name, argsJson) {
  name = normalizeName(name);
  let args;
  try {
    args = JSON.parse(argsJson || "{}");
  } catch {
    return "";
  }
  if (!args || typeof args !== "object") return "";

  let val = "";
  if (name === "web_search") val = args.query || "";
  else if (name === "web_fetch") val = args.url ? hostname(args.url) : "";
  else if (name === "load_skill") val = args.name || "";
  else if (name.startsWith("workflow_")) val = args.name || "";
  else if (args.path) val = basename(args.path);
  else if (args.output_path) val = basename(args.output_path);
  else if (args.url) val = hostname(args.url);
  else if (args.query) val = args.query;
  else if (args.name) val = args.name;
  else if (args.title) val = args.title;

  return clamp(val, ARG_MAX);
}

// The chat-trace line for one tool call. load_skill keeps its first-5-words
// behavior; other tools use the FRIENDLY phrase (or the description's first
// sentence for unknown/MCP tools), with a salient argument appended.
export function toolLabel(tool) {
  const name = normalizeName(tool.name);

  if (name === "load_skill") {
    let desc = tool.description;
    if (!desc) {
      try {
        desc = JSON.parse(tool.argsJson || "{}").name || name;
      } catch {
        desc = name;
      }
    }
    return `Loading skill: ${firstWords(desc, 5)}`;
  }

  const base = FRIENDLY[name] || firstSentence(tool.description) || name;
  const arg = salientArg(name, tool.argsJson);
  if (!arg) return base;
  const detail = name === "web_search" ? `"${arg}"` : arg;
  return `${base} — ${detail}`;
}
