// Pi custom tools surfaced to the LLM. Each tool's execute() does NOT do the
// work locally — it routes through the GovernanceBridge → broker InvokeTool →
// Tool Proxy. This is what makes every action faithfully broker-governed.
import { Type } from "typebox";
import { defineTool, type ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../ipc/bridge-client";

function textResult(text: string) {
  return { content: [{ type: "text" as const, text }], details: {} };
}

async function run(bridge: BridgeClientLike, toolCallId: string) {
  const r = await bridge.execute(toolCallId);
  const body =
    r.ok
      ? typeof r.output === "string"
        ? r.output
        : JSON.stringify(r.output, null, 2)
      : `ERROR: ${r.error ?? "tool failed"}`;
  return { ...textResult(body), details: r.output as unknown };
}

export function makeTools(bridge: BridgeClientLike): ToolDefinition[] {
  return [
    defineTool({
      name: "web_fetch",
      label: "Fetch URL",
      description:
        "Fetch a public web page over HTTPS and return its text. Read-only; runs through the aikonos policy + egress proxy.",
      parameters: Type.Object({ url: Type.String({ description: "https URL to fetch" }) }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "web_search",
      label: "Search the web",
      description:
        "Search the web via the tenant's configured search engine and return ranked results " +
        "({title, url, snippet}). Read-only; does not load page content — call web_fetch on a " +
        "chosen result's url to read it.",
      parameters: Type.Object({
        query: Type.String({ description: "search query" }),
        count: Type.Optional(Type.Number({ description: "max results to return" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "workspace_read",
      label: "List workspace",
      description: "List the files in the current task's workspace.",
      parameters: Type.Object({}),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "doc_read",
      label: "Read document",
      description: "Read a document previously written to the task workspace.",
      parameters: Type.Object({ path: Type.String({ description: "relative path in the workspace" }) }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "doc_write",
      label: "Write document",
      description:
        "Write a document to the task workspace (write_local). Persists a file the user can read back.",
      parameters: Type.Object({
        path: Type.String({ description: "relative path in the workspace" }),
        content: Type.String({ description: "file contents" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "email_draft",
      label: "Draft email",
      description: "Compose (does not send) an email draft. write_local.",
      parameters: Type.Object({
        to: Type.String(),
        subject: Type.String(),
        body: Type.String(),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "gdrive_read",
      label: "Read Google Drive",
      description:
        "Read the user's Google Drive: with file_id, returns that file's text content; without it, lists files (optionally filtered by a Drive query). Read-only; requires a linked Google Drive connection.",
      parameters: Type.Object({
        file_id: Type.Optional(Type.String({ description: "Drive file id to read; omit to list files" })),
        query: Type.Optional(Type.String({ description: "Drive v3 query, e.g. \"name contains 'report'\"" })),
        max_results: Type.Optional(Type.Number({ description: "max files to list (default 25)" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "gdrive_write",
      label: "Write to Google Drive",
      description:
        "Create a file in the user's Google Drive (write_external — requires human approval). Requires a linked Google Drive connection.",
      parameters: Type.Object({
        name: Type.String({ description: "file name to create" }),
        content: Type.String({ description: "file contents" }),
        mime_type: Type.Optional(Type.String({ description: "MIME type (default text/plain)" })),
        folder_id: Type.Optional(Type.String({ description: "parent folder id (optional)" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "onedrive_read",
      label: "Read OneDrive",
      description:
        "Read the user's OneDrive: with item_id or path, returns that file's text content; with neither, lists root files. Read-only; requires a linked OneDrive connection.",
      parameters: Type.Object({
        item_id: Type.Optional(Type.String({ description: "OneDrive item id to read" })),
        path: Type.Optional(Type.String({ description: "OneDrive path, e.g. 'docs/report.txt'" })),
        max_results: Type.Optional(Type.Number({ description: "max files to list (default 25)" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "onedrive_write",
      label: "Write to OneDrive",
      description:
        "Create or replace a file in the user's OneDrive at the given path (write_external — requires human approval; simple upload, <=4MB). Requires a linked OneDrive connection.",
      parameters: Type.Object({
        path: Type.String({ description: "destination path, e.g. 'docs/out.txt'" }),
        content: Type.String({ description: "file contents" }),
        content_type: Type.Optional(Type.String({ description: "MIME type (default text/plain)" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "delegate",
      label: "Delegate task",
      description:
        "Delegate a task to another user by email. Use when the user asks to hand off / delegate / assign work to someone (e.g. 'delegate the SIEM triage to bob@example.com'). The recipient receives it in their inbox; governed by the broker (you can only grant scopes you hold).",
      parameters: Type.Object({
        to: Type.String({ description: "recipient email, e.g. bob@example.com" }),
        intent: Type.String({ description: "the task to perform, in plain language" }),
        scopes: Type.Optional(
          Type.Array(Type.String(), { description: "capability scopes to grant, e.g. ['siem:read']" }),
        ),
      }),
      execute: async (_toolCallId, params: { to: string; intent: string; scopes?: string[] }) => {
        const r = await bridge.delegate(params.to, params.intent, params.scopes ?? ["siem:read"]);
        const text = r.ok
          ? `Delegated to ${params.to} (envelope ${r.envelopeId?.slice(0, 8)}…). They'll see it in their inbox.`
          : `Delegation denied: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_save",
      label: "Save workflow",
      description:
        "Save a workflow definition so it can be run later. Compose the workflow as the tool input: provide a name, optional description, the ordered steps (each with a skill id and args), and optional parameterised inputs. Returns a lineageId you can use with workflow_run.",
      parameters: Type.Object({
        name: Type.String({ description: "workflow name" }),
        description: Type.Optional(Type.String({ description: "human-readable description" })),
        steps: Type.Array(
          Type.Object({
            kind: Type.Optional(Type.Union([Type.Literal("tool"), Type.Literal("reason")], { description: "step kind — 'tool' (default, invokes a aikonos tool) or 'reason' (a parent-side LLM reasoning/synthesis step, no tool call)" })),
            skill: Type.Optional(Type.String({ description: "aikonos tool id for this step — required for a tool step; MUST be one of your available tools (e.g. web.fetch, doc.read, doc.write). Do not invent skills. Omit for a reason step." })),
            args: Type.Optional(Type.Record(Type.String(), Type.Unknown(), { description: "static or ${inputs.<name>} templated args — tool steps only" })),
            instruction: Type.Optional(Type.String({ description: "required for a reason step: the instruction the parent-side LLM executes, written with ${inputs.*} and ${steps.N.output[.path]} references to earlier steps. Use this for computation or synthesis between tool calls instead of inventing a skill. Omit for a tool step." })),
            output_schema: Type.Optional(Type.Record(Type.String(), Type.Unknown(), { description: "optional JSON Schema object for a reason step's output, used when a later step references structured fields from this reason step's result" })),
          }),
          { description: "ordered list of steps" },
        ),
        inputs: Type.Optional(
          Type.Array(
            Type.Object({
              name: Type.String({ description: "input name" }),
              default: Type.Optional(Type.String({ description: "default value" })),
            }),
            { description: "parameterised inputs for the workflow" },
          ),
        ),
      }),
      execute: async (_toolCallId, params: Record<string, unknown>) => {
        const r = await bridge.saveWorkflow(params);
        const text = r.ok
          ? `Workflow saved (lineageId: ${r.lineageId}, version: ${r.version}).`
          : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_run",
      label: "Run workflow",
      description:
        "Run a previously saved workflow by its lineageId. Optionally supply runtime input values to fill ${inputs.*} placeholders in step args.",
      parameters: Type.Object({
        lineageId: Type.String({ description: "the lineageId returned by workflow_save" }),
        inputs: Type.Optional(
          Type.Record(Type.String(), Type.String(), { description: "runtime values for workflow inputs" }),
        ),
      }),
      execute: async (_toolCallId, params: { lineageId: string; inputs?: Record<string, string> }) => {
        const r = await bridge.runWorkflow(params.lineageId, params.inputs ?? {});
        const text = r.ok
          ? JSON.stringify(r.result, null, 2)
          : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_list",
      label: "List workflows",
      description: "List the workflows you own.",
      parameters: Type.Object({}),
      execute: async (_toolCallId, _params: Record<string, never>) => {
        const r = await bridge.listWorkflows();
        const text = r.ok
          ? JSON.stringify(r.items, null, 2)
          : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_publish",
      label: "Publish workflow",
      description:
        "Publish (share) a saved workflow to one or more groups you are a member of, e.g. ['security-team']. " +
        "This makes the workflow visible and runnable by all members of those groups. " +
        "Do NOT use the delegate tool for sharing workflows; use this tool instead.",
      parameters: Type.Object({
        lineageId: Type.String({ description: "lineageId of the workflow to publish (returned by workflow_save)" }),
        groupIds: Type.Array(Type.String(), { description: "groups to share with, e.g. ['security-team'] — you must be a member of each group" }),
        version: Type.Optional(Type.Integer({ description: "version number to publish; omit to publish the current version" })),
      }),
      execute: async (_toolCallId, params: { lineageId: string; groupIds: string[]; version?: number }) => {
        const r = await bridge.publishWorkflow(params.lineageId, params.groupIds, params.version ?? 0);
        const text = r.ok
          ? `Published to ${(r.groups ?? []).join(", ")} (visibility: ${r.visibilityKind}).`
          : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_propose",
      label: "Propose workflow improvement",
      description:
        "Propose an improved version of an existing workflow lineage. " +
        "Use this when the user wants to improve a workflow they own — provide the lineageId and the updated workflow definition. " +
        "This creates a proposed version through the owner-gated approve/reject loop; the owner must approve it before it becomes active. " +
        "Use workflow_save (not this tool) when creating a brand-new workflow from scratch.",
      parameters: Type.Object({
        lineageId: Type.String({ description: "lineageId of the existing workflow to improve (returned by workflow_save or workflow_list)" }),
        name: Type.String({ description: "workflow name" }),
        description: Type.Optional(Type.String({ description: "human-readable description" })),
        steps: Type.Array(
          Type.Object({
            kind: Type.Optional(Type.Union([Type.Literal("tool"), Type.Literal("reason")], { description: "step kind — 'tool' (default, invokes a aikonos tool) or 'reason' (a parent-side LLM reasoning/synthesis step, no tool call)" })),
            skill: Type.Optional(Type.String({ description: "aikonos tool id for this step — required for a tool step; MUST be one of your available tools (e.g. web.fetch, doc.read, doc.write). Do not invent skills. Omit for a reason step." })),
            args: Type.Optional(Type.Record(Type.String(), Type.Unknown(), { description: "static or ${inputs.<name>} templated args — tool steps only" })),
            instruction: Type.Optional(Type.String({ description: "required for a reason step: the instruction the parent-side LLM executes, written with ${inputs.*} and ${steps.N.output[.path]} references to earlier steps. Use this for computation or synthesis between tool calls instead of inventing a skill. Omit for a tool step." })),
            output_schema: Type.Optional(Type.Record(Type.String(), Type.Unknown(), { description: "optional JSON Schema object for a reason step's output, used when a later step references structured fields from this reason step's result" })),
          }),
          { description: "ordered list of steps" },
        ),
        inputs: Type.Optional(
          Type.Array(
            Type.Object({
              name: Type.String({ description: "input name" }),
              default: Type.Optional(Type.String({ description: "default value" })),
            }),
            { description: "parameterised inputs for the workflow" },
          ),
        ),
      }),
      execute: async (_toolCallId, params: { lineageId: string } & Record<string, unknown>) => {
        const { lineageId, ...def } = params;
        const r = await bridge.proposeWorkflow(lineageId, def);
        const text = r.ok
          ? `Proposed workflow version ${r.version}. The owner can approve or reject it from the Workflows view.`
          : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "workflow_schedule",
      label: "Schedule workflow",
      description:
        "Schedule a saved workflow to run automatically on a recurrence — a cron expression (repeating) " +
        "or a single future runAt datetime (once). The scheduler fires the workflow's steps directly with " +
        "no LLM in the loop; every step is still governed live. Do not supply an approved-tools list — " +
        "there is none for scheduled workflows.",
      parameters: Type.Object({
        lineageId: Type.String({ description: "the lineageId of the workflow to schedule (returned by workflow_save or workflow_list)" }),
        kind: Type.Union([Type.Literal("cron"), Type.Literal("once")], {
          description: "'cron' for a repeating schedule, 'once' for a single future run",
        }),
        cronExpr: Type.Optional(Type.String({ description: "5-field cron expression, required when kind is 'cron', e.g. '0 8 * * 1' for Mondays 08:00" })),
        runAt: Type.Optional(Type.String({ description: "ISO 8601 datetime in the future, required when kind is 'once'" })),
        inputs: Type.Optional(
          Type.Record(Type.String(), Type.String(), { description: "runtime values for the workflow's inputs" }),
        ),
      }),
      execute: async (
        _toolCallId,
        params: { lineageId: string; kind: "cron" | "once"; cronExpr?: string; runAt?: string; inputs?: Record<string, string> },
      ) => {
        const r = await bridge.scheduleWorkflow(params.lineageId, params.inputs ?? {}, {
          kind: params.kind,
          cronExpr: params.cronExpr,
          runAt: params.runAt,
        });
        let text = r.ok ? `Workflow schedule created (id: ${r.scheduleId}).` : `ERROR: ${r.error}`;
        if (r.ok && r.missingInputs && r.missingInputs.length > 0) {
          text += ` WARNING: missing required input(s): ${r.missingInputs.join(", ")} — the workflow will fail at run time unless these are supplied.`;
        }
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    defineTool({
      name: "analyze_image",
      label: "Analyze image",
      description:
        "Analyze an image file in the workspace using the tenant's vision-capable LLM provider. " +
        "Use this when the user references or attaches an image (a #<path> reference to an image file) " +
        "and asks about its contents. Provide the workspace path and, optionally, a prompt guiding the analysis.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the image file" }),
        prompt: Type.Optional(Type.String({ description: "guidance for what to look for in the image" })),
      }),
      execute: async (_toolCallId, params: { path: string; prompt?: string }) => {
        const r = await bridge.analyzeImage(params.path, params.prompt);
        const text = r.ok ? (r.text ?? "") : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: { path: params.path } };
      },
    }),
    defineTool({
      name: "spawn_subagents",
      label: "Spawn subagents",
      description:
        "Fan out independent subtasks to parallel subagents, each running with your own tool access under your own identity, then synthesize their results into one answer via an aggregator instruction. Fan-out width is capped (default 3) — a request over the cap fails fast; retry the remaining subtasks in a follow-up call rather than resending the same oversized request. Subagents cannot spawn further subagents. A subtask needing a tool call that requires human approval is denied immediately, not queued — such subtasks must be done directly in chat.",
      // executionMode:"sequential" forces pi-agent-core's executeToolCalls
      // (agent-loop.js) to serialize the WHOLE assistant turn whenever this
      // tool appears in it — otherwise two spawn_subagents calls in one turn
      // run concurrently via executeToolCallsParallel, sharing one
      // onBranchEvent sink where both fan-outs number branches from 0. Two
      // things depend on that never happening: (1) the per-call width-cap/
      // spend-cap pre-gate in subagent/run.ts, which reasons about "at most
      // one call's worth of overshoot"; (2) the webui's per-fan-out row
      // grouping (SubagentTimeline.vue / useAguiRun.js), which correlates
      // aikonos.subagent.spawned/completed by branch index scoped to one
      // fan-out — a concurrent second call reusing index 0 would corrupt
      // both fan-outs' rows. See subagent-run.test.ts's execution-mode pin.
      executionMode: "sequential",
      parameters: Type.Object({
        branches: Type.Array(
          Type.Object({
            task: Type.String({ description: "the subtask instruction for this subagent" }),
            role: Type.Optional(
              Type.String({
                description:
                  "name of an agent you are permitted to use, narrowing this subagent's tool access to that agent's own skills (intersected with your own); omit for your own tool surface minus spawn_subagents",
              }),
            ),
          }),
          { description: "the parallel subtasks to fan out, one subagent per entry" },
        ),
        aggregator_instruction: Type.String({
          description: "instruction for synthesizing every subtask's output (and any failures) into one final answer",
        }),
      }),
      execute: async (
        _toolCallId,
        params: { branches: { task: string; role?: string }[]; aggregator_instruction: string },
      ) => {
        if (!bridge.spawnSubagents) {
          return { content: [{ type: "text" as const, text: "ERROR: spawn_subagents is not supported by this bridge" }], details: {} };
        }
        const r = await bridge.spawnSubagents(params.branches, params.aggregator_instruction);
        const text = r.ok ? (r.synthesis ?? "") : `ERROR: ${r.error}`;
        return { content: [{ type: "text" as const, text }], details: r };
      },
    }),
    // ── Office document tools (office-worker backed) ──────────────────────
    // Each routes through the ordinary gate → SubmitPlan → InvokeTool path
    // (execute just triggers run(); the args are captured by the gate hook),
    // exactly like web_fetch/doc_write. Params mirror the broker toolproxy
    // plugin arg shapes in broker/internal/toolproxy/office_*.go.
    defineTool({
      name: "docx_create",
      label: "Create Word document",
      description:
        "Create a .docx from a model-authored script (docx-js/pandoc). write_local. Provide the script and the workspace output path.",
      parameters: Type.Object({
        script: Type.String({ description: "document-generation script the office-worker runs" }),
        output_path: Type.String({ description: "workspace-relative path for the created .docx" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "docx_edit",
      label: "Edit Word document",
      description:
        "Edit an existing .docx via declarative find/replace ops against archive members (raw XML). write_local.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the source .docx" }),
        output_path: Type.String({ description: "workspace-relative path for the edited .docx" }),
        ops: Type.Array(Type.Record(Type.String(), Type.Unknown()), {
          description: "ordered edit ops (e.g. member find/replace)",
        }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "docx_extract",
      label: "Extract from Word document",
      description:
        "Extract text (markdown, default) or raw XML from a .docx. read_only. Use format:'xml' with member+pattern to pull specific regions.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the .docx" }),
        format: Type.Optional(Type.String({ description: "'markdown' (default) or 'xml'" })),
        member: Type.Optional(Type.String({ description: "archive member selector (format:'xml')" })),
        pattern: Type.Optional(Type.String({ description: "regex to match regions (format:'xml')" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "xlsx_create",
      label: "Create Excel workbook",
      description:
        "Create a .xlsx from a model-authored openpyxl/pandas script. write_local. Provide the script and the workspace output path.",
      parameters: Type.Object({
        script: Type.String({ description: "workbook-generation script the office-worker runs" }),
        output_path: Type.String({ description: "workspace-relative path for the created .xlsx" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "xlsx_edit",
      label: "Edit Excel workbook",
      description:
        "Edit an existing .xlsx by running a model-authored openpyxl script against it. write_local.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the source .xlsx" }),
        output_path: Type.String({ description: "workspace-relative path for the edited .xlsx" }),
        script: Type.String({ description: "openpyxl script mutating the workbook" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "xlsx_extract",
      label: "Extract from Excel workbook",
      description:
        "Extract cell data/text from a .xlsx. read_only. Optionally scope to a sheet and/or A1 range, and choose an output format.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the .xlsx" }),
        sheet: Type.Optional(Type.String({ description: "sheet name to extract; omit for all" })),
        range: Type.Optional(Type.String({ description: "A1 range, e.g. 'A1:D20'" })),
        format: Type.Optional(Type.String({ description: "output format (e.g. 'json', 'csv', 'markdown')" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "xlsx_recalc",
      label: "Recalculate Excel workbook",
      description:
        "Recalculate all formulas in a .xlsx (LibreOffice headless) and report any broken cells (#REF!, #DIV/0!, …). write_local.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the source .xlsx" }),
        output_path: Type.String({ description: "workspace-relative path for the recalculated .xlsx" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pptx_create",
      label: "Create PowerPoint",
      description:
        "Create a .pptx from a model-authored pptxgenjs script. write_local. Provide the script and the workspace output path.",
      parameters: Type.Object({
        script: Type.String({ description: "deck-generation script the office-worker runs" }),
        output_path: Type.String({ description: "workspace-relative path for the created .pptx" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pptx_edit",
      label: "Edit PowerPoint",
      description:
        "Edit an existing .pptx via declarative find/replace ops against archive members (raw XML). write_local.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the source .pptx" }),
        output_path: Type.String({ description: "workspace-relative path for the edited .pptx" }),
        ops: Type.Array(Type.Record(Type.String(), Type.Unknown()), {
          description: "ordered edit ops (e.g. member find/replace)",
        }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pptx_extract",
      label: "Extract from PowerPoint",
      description:
        "Extract text (markdown, default) or raw XML from a .pptx. read_only. Use format:'xml' with member+pattern to pull specific regions.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the .pptx" }),
        format: Type.Optional(Type.String({ description: "'markdown' (default) or 'xml'" })),
        member: Type.Optional(Type.String({ description: "archive member selector (format:'xml')" })),
        pattern: Type.Optional(Type.String({ description: "regex to match regions (format:'xml')" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pptx_thumbnail",
      label: "Render PowerPoint thumbnails",
      description:
        "Render slides of a .pptx to page images (default cap 20 pages). write_local. Writes one image per slide under output_dir.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the .pptx" }),
        output_dir: Type.String({ description: "workspace-relative directory for the rendered images" }),
        dpi: Type.Optional(Type.Number({ description: "render DPI (default 150)" })),
        max_pages: Type.Optional(Type.Number({ description: "max slides to render (default 20)" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pdf_create",
      label: "Create PDF",
      description:
        "Create a .pdf from a model-authored reportlab script. write_local. Provide the script and the workspace output path.",
      parameters: Type.Object({
        script: Type.String({ description: "PDF-generation script the office-worker runs" }),
        output_path: Type.String({ description: "workspace-relative path for the created .pdf" }),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pdf_transform",
      label: "Transform PDF",
      description:
        "Transform PDFs (op: merge | split | rotate | decrypt | fill-forms). write_local. paths are the input files (order matters for merge); split writes one output per outputs[] range.",
      parameters: Type.Object({
        op: Type.String({ description: "merge | split | rotate | decrypt | fill-forms" }),
        paths: Type.Array(Type.String(), { description: "workspace-relative input PDF paths, order preserved" }),
        output_path: Type.Optional(Type.String({ description: "output path (all ops except split)" })),
        outputs: Type.Optional(
          Type.Array(
            Type.Object({
              pages: Type.String({ description: "page range for this split, e.g. '1-3'" }),
              output_path: Type.String({ description: "workspace-relative output path for this range" }),
            }),
            { description: "split ranges (op:'split' only)" },
          ),
        ),
        angle: Type.Optional(Type.Number({ description: "rotation angle (op:'rotate')" })),
        pages: Type.Optional(Type.String({ description: "pages to rotate (op:'rotate')" })),
        password: Type.Optional(Type.String({ description: "password (op:'decrypt')" })),
        fields: Type.Optional(Type.Record(Type.String(), Type.Unknown(), { description: "form field values (op:'fill-forms')" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "pdf_extract",
      label: "Extract from PDF",
      description:
        "Extract text (and tables) from a .pdf, with an optional OCR fallback for scanned pages. read_only.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the .pdf" }),
        ocr: Type.Optional(Type.Boolean({ description: "run OCR fallback for scanned/image pages" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "office_convert",
      label: "Convert document format",
      description:
        "Convert a document to another format via LibreOffice (incl. legacy .doc→.docx). write_local. Provide the source path, target format, and output path.",
      parameters: Type.Object({
        path: Type.String({ description: "workspace-relative path to the source document" }),
        target_format: Type.String({ description: "target format extension, e.g. 'docx', 'pdf'" }),
        output_path: Type.String({ description: "workspace-relative path for the converted file" }),
        source_extension: Type.Optional(Type.String({ description: "override the input filter extension" })),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    // ── Agent memory (OKF bundles) ────────────────────────────────────────
    // Fully gated like the office tools — real InvokeTool route, params mirror
    // broker/internal/toolproxy/memory.go's arg shapes.
    defineTool({
      name: "memory_read",
      label: "Read memory",
      description:
        "Recall durable knowledge from a memory bundle (markdown concepts with frontmatter). read_only. " +
        "Read progressively, never in bulk: start with mode:'index' to see what exists, then mode:'frontmatter' " +
        "(up to 20 ids) to judge which concepts are relevant, then mode:'concept' for at most 5 full bodies. " +
        "Scopes: 'user' = this user's own knowledge, 'agent' = what you have learned about your own job " +
        "(agent-bound runs only), 'group' = a team's shared knowledge (requires group_id and membership).",
      parameters: Type.Object({
        scope: Type.String({ description: "'user', 'agent' or 'group'" }),
        group_id: Type.Optional(Type.String({ description: "FGA group id; required when scope is 'group'" })),
        mode: Type.String({
          description: "'index' (bundle listing), 'frontmatter' (metadata for ids), 'concept' (metadata + body)",
        }),
        ids: Type.Optional(
          Type.Array(Type.String(), {
            description: "concept ids to read; required for 'frontmatter' (max 20) and 'concept' (max 5)",
          }),
        ),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
    defineTool({
      name: "memory_write",
      label: "Write memory",
      description:
        "Record one durable fact or convention as a memory concept. write_local. Write concisely and only what " +
        "stays true beyond this conversation — not transcripts, not one-off task state. Writing an existing id " +
        "updates it. To retire a concept, rewrite it with status:'deprecated' pointing at its replacement; there " +
        "is no delete. Provenance (who wrote it, when) is stamped server-side.",
      parameters: Type.Object({
        scope: Type.String({ description: "'user', 'agent' or 'group'" }),
        group_id: Type.Optional(Type.String({ description: "FGA group id; required when scope is 'group'" })),
        id: Type.String({
          description: "concept id, 1-2 lowercase path segments, e.g. 'invoicing' or 'invoicing/vat-rates'",
        }),
        type: Type.String({ description: "concept type, e.g. 'preference', 'convention', 'fact', 'procedure'" }),
        body: Type.String({ description: "the knowledge itself, as markdown" }),
        title: Type.Optional(Type.String({ description: "short human-readable title" })),
        description: Type.Optional(Type.String({ description: "one-line summary shown in the bundle index" })),
        tags: Type.Optional(Type.Array(Type.String(), { description: "topic tags aiding later recall" })),
        status: Type.Optional(Type.String({ description: "'draft', 'stable' (default) or 'deprecated'" })),
        stale_after: Type.Optional(
          Type.String({ description: "YYYY-MM-DD after which this must be re-verified before being trusted" }),
        ),
        sources: Type.Optional(
          Type.Array(Type.Record(Type.String(), Type.Unknown()), {
            description: "provenance entries, e.g. [{resource:'…', title:'…'}]",
          }),
        ),
      }),
      execute: async (toolCallId) => run(bridge, toolCallId),
    }),
  ];
}

export const TOOL_NAMES = ["web_fetch", "web_search", "workspace_read", "doc_read", "doc_write", "email_draft", "gdrive_read", "gdrive_write", "onedrive_read", "onedrive_write", "delegate", "workflow_save", "workflow_run", "workflow_list", "workflow_publish", "workflow_propose", "workflow_schedule", "analyze_image", "docx_create", "docx_edit", "docx_extract", "xlsx_create", "xlsx_edit", "xlsx_extract", "xlsx_recalc", "pptx_create", "pptx_edit", "pptx_extract", "pptx_thumbnail", "pdf_create", "pdf_transform", "pdf_extract", "office_convert", "memory_read", "memory_write", "spawn_subagents"];

// BROKER_TO_PI_TOOL maps broker skill ids (dotted) to the Pi underscore tool
// names the LLM sees. Pi function names cannot contain dots (OpenAI restriction).
// This is the single source of truth; session.ts and session-plan.ts both import
// from here to avoid the hardcoded inversion drifting out of sync.
export const BROKER_TO_PI_TOOL: Record<string, string> = {
  "web.fetch":      "web_fetch",
  "web.search":     "web_search",
  "workspace.read": "workspace_read",
  "doc.read":       "doc_read",
  "doc.write":      "doc_write",
  "email.draft":    "email_draft",
  "gdrive.read":    "gdrive_read",
  "gdrive.write":   "gdrive_write",
  "onedrive.read":  "onedrive_read",
  "onedrive.write": "onedrive_write",
  "docx.create":    "docx_create",
  "docx.edit":      "docx_edit",
  "docx.extract":   "docx_extract",
  "xlsx.create":    "xlsx_create",
  "xlsx.edit":      "xlsx_edit",
  "xlsx.extract":   "xlsx_extract",
  "xlsx.recalc":    "xlsx_recalc",
  "pptx.create":    "pptx_create",
  "pptx.edit":      "pptx_edit",
  "pptx.extract":   "pptx_extract",
  "pptx.thumbnail": "pptx_thumbnail",
  "pdf.create":     "pdf_create",
  "pdf.transform":  "pdf_transform",
  "pdf.extract":    "pdf_extract",
  "office.convert": "office_convert",
  "memory.read":    "memory_read",
  "memory.write":   "memory_write",
};

// piAllowedToBrokerIds converts a list of Pi tool names (as found in
// allowedToolNames from a SessionPlan) to the broker skill ids they map to —
// used by system-prompt.ts to bind workflow-authoring steps to the model's
// real tool vocabulary. Builtin tools use BROKER_TO_PI_TOOL (inverted); MCP
// tools follow mcp__connId__toolName → mcp:connId:toolName.
export function piAllowedToBrokerIds(piNames: string[]): string[] {
  // Build the inversion of BROKER_TO_PI_TOOL once per call. The map is small
  // so the cost is negligible; building it here keeps the function pure (no
  // module-level mutable state).
  const piToBroker: Record<string, string> = {};
  for (const [brokerId, piName] of Object.entries(BROKER_TO_PI_TOOL)) {
    piToBroker[piName] = brokerId;
  }

  const result: string[] = [];
  for (const piName of piNames) {
    const brokerId = piToBroker[piName];
    if (brokerId) {
      result.push(brokerId);
    } else if (piName.startsWith("mcp__")) {
      // mcp__connId__toolName → mcp:connId:toolName
      const withoutPrefix = piName.slice("mcp__".length);
      const sep = withoutPrefix.indexOf("__");
      if (sep !== -1) {
        result.push("mcp:" + withoutPrefix.slice(0, sep) + ":" + withoutPrefix.slice(sep + 2));
      }
    }
  }
  return result;
}
