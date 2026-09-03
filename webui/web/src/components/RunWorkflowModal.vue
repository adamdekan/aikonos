<script setup>
import { ref, computed, watch } from "vue";
import { useRouter } from "vue-router";
import Icon from "./Icon.vue";
import { runWorkflow, runWorkflowStream, rateWorkflow } from "../api/workflows.js";
import { writeSession } from "../api/sessions.js";
import { uuid } from "../lib/uuid.js";
import { useSessionsStore } from "../store/sessions.js";

const props = defineProps({
  // The whole workflow row merged with the fetched version, plus a parsed
  // `definition` (from definitionJson) — only lineageId/name/version/definition
  // are read below; definition has { inputs: [{ name, default? }] }
  workflow: { type: Object, default: null },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close"]);

const router = useRouter();
const sessionsStore = useSessionsStore();

// ── state ──
const inputValues = ref({});
const running = ref(false);
const runResult = ref(null);   // { ok, result, error } from bridge.runWorkflow
const runError = ref(null);
// Live per-step progress while a streamed run is in flight. Seeded pending from
// the definition steps; each `step` SSE event flips one to done/failed by index.
const liveSteps = ref([]);

const rating = ref(null);      // "RATING_SUCCESS" | "RATING_BAD"
const ratingNote = ref("");
const ratingPending = ref(false);
const ratingDone = ref(false);
const ratingError = ref(null);

// Maps a top-level schema hint to an input widget. Only `enum` and top-level
// `type` (boolean/number/integer) are honoured — not a JSON-Schema engine.
// enum wins over type so `{type:"string", enum:[…]}` renders a select.
function widgetFor(inp) {
  if (Array.isArray(inp.schema?.enum)) return "enum";
  const t = inp.schema?.type;
  if (t === "boolean") return "boolean";
  if (t === "number" || t === "integer") return "number";
  return "text";
}

// An input with no declared default is required.
function isRequired(inp) {
  return inp.default === undefined || inp.default === null;
}

// A boolean has a concrete value at all times (checkbox); others count as
// filled only when non-blank.
function hasValue(inp) {
  if (widgetFor(inp) === "boolean") return true;
  const v = inputValues.value[inp.name];
  return v !== undefined && v !== null && String(v).trim() !== "";
}

// Run is blocked while any required input is empty.
const canRun = computed(() =>
  (props.workflow?.definition?.inputs ?? []).every((inp) => !isRequired(inp) || hasValue(inp)),
);

// Initialise input fields from the workflow definition whenever the modal opens
// or the workflow changes.
watch(
  () => [props.workflow, props.visible],
  () => {
    if (!props.visible || !props.workflow) return;
    const inputs = props.workflow.definition?.inputs ?? [];
    const values = {};
    for (const inp of inputs) {
      values[inp.name] = inp.default ?? (widgetFor(inp) === "boolean" ? false : "");
    }
    inputValues.value = values;
    liveSteps.value = [];
    running.value = false;
    runResult.value = null;
    runError.value = null;
    rating.value = null;
    ratingNote.value = "";
    ratingPending.value = false;
    ratingDone.value = false;
    ratingError.value = null;
  },
  { immediate: true },
);

// Renders a step's output for display. Strings pass through; for tool steps,
// objects that carry a text `content` field (doc.read / web.fetch results)
// render as that content; anything else is pretty-printed JSON. Avoids the
// "[object Object]" a bare String() produces on a structured tool result.
// The `.content` shortcut is scoped to kind:"tool" — a reason step's
// output_schema can legitimately carry its own top-level `content` field
// alongside other fields, and collapsing to just that field would silently
// hide the rest of the object.
function renderStepOutput(output, kind) {
  if (output === null || output === undefined) return "";
  if (typeof output === "string") return output;
  if (kind === "tool" && typeof output === "object" && typeof output.content === "string") {
    return output.content;
  }
  try {
    return JSON.stringify(output, null, 2);
  } catch {
    return String(output);
  }
}

// Shared step label: reason steps have no skill id, so they render as "reason".
function stepLabel(step) {
  return step.kind === "reason" ? "reason" : step.skill;
}

// Defensive cap on the raw step output stored per tool entry. store/chat.js
// truncates results to 2 KB for localStorage anyway; this keeps the in-memory
// session record bounded too (a scraped web page or generated script can be
// tens of KB).
const RESULT_STORE_CAP = 4096;
// Cap for compacted non-text output (JSON dumps). Text answers are never cut —
// a workflow whose last step produces prose IS the deliverable.
const FINAL_LINE_CAP = 300;
// Object fields that carry a workflow's textual answer, in preference order.
const TEXT_ANSWER_KEYS = ["content", "text", "summary", "answer", "message"];

function truncate(text, max) {
  const t = String(text ?? "");
  return t.length > max ? t.slice(0, max) + "…" : t;
}

function humanSize(bytes) {
  if (typeof bytes !== "number" || bytes <= 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// Summarises the run's final step. Text answers (string output, or an object's
// text-answer field) pass through in full — they are the deliverable. Objects
// that name a written file collapse to "Created <path>"; anything else renders
// compactly. Raw output lives on the tool entries, not here.
function finalResultLine(step) {
  if (!step) return "";
  const out = step.output;
  if (typeof out === "string") return out;
  if (out && typeof out === "object") {
    const key = TEXT_ANSWER_KEYS.find((k) => typeof out[k] === "string" && out[k].trim());
    if (key) return out[key];
    if (typeof out.path === "string") {
      const size = humanSize(out.size);
      return size ? `Created ${out.path} (${size})` : `Created ${out.path}`;
    }
  }
  return truncate(renderStepOutput(out, step.kind), FINAL_LINE_CAP);
}

// Compact completion message for the persisted chat session. The raw output of
// every step used to be concatenated here (tens of KB of scraped text / a full
// generated script dumped into the assistant bubble); it now lives on the
// tools[] entries, behind the debug ToolCard. The visible text is just the
// workflow name + step count and a one-line final result (or the failure).
function buildAssistantText(result) {
  const steps = result.result?.steps ?? [];
  const count = steps.length;
  const header = `${props.workflow.name} — ${count} step${count === 1 ? "" : "s"}`;

  // A denied/failed step halts the run — describe it instead of a final result.
  const failed = steps.find((s) => !s.allowed || s.error);
  if (failed) {
    const n = (failed.stepIndex ?? steps.indexOf(failed)) + 1;
    const verb = failed.allowed ? "failed" : "was denied";
    const reason = failed.denyReason ?? failed.error ?? "";
    return `${header}\n\nStep ${n} (${stepLabel(failed)}) ${verb}: ${reason}`;
  }

  const line = finalResultLine(steps[steps.length - 1]);
  return line ? `${header}\n\n${line}` : header;
}

// Builds a Chat.vue-compatible session record from a completed run result.
// Each step becomes one tool entry in the assistant message (same shape as
// onToolCall + onToolResult produce during a live chat session), and the
// assistant text carries the rendered run output so it is visible in chat.
function buildWorkflowSession(result, id) {
  const steps = result.result?.steps ?? [];
  const now = new Date().toISOString();
  const inputSummary = Object.entries(inputValues.value)
    .map(([k, v]) => `${k}=${v}`)
    .join(", ");
  const invocationText = inputSummary
    ? `${props.workflow.name} (${inputSummary})`
    : props.workflow.name;

  const tools = steps.map((step, i) => ({
    id:       `wf-step-${i}`,
    name:     stepLabel(step),
    argsJson: JSON.stringify(step.resolvedArgs ?? {}),
    result:   truncate(
      step.allowed
        ? renderStepOutput(step.output, step.kind)
        : (step.denyReason ?? "denied"),
      RESULT_STORE_CAP,
    ),
    isError:  !step.allowed || !!step.error,
    done:     true,
  }));

  return {
    id,
    title:       props.workflow.name,
    agent_id:    null,
    agent_name:  null,
    pinned:      false,
    pinned_at:   null,
    created_at:  now,
    updated_at:  now,
    thread_id:   uuid(),
    first_message: invocationText,
    source:      "workflow",
    messages: [
      { role: "user",      text: invocationText },
      { role: "assistant", text: buildAssistantText(result), tools, error: null },
    ],
  };
}

// Persists the session record and updates the store's reactive list, then
// navigates to the session. Fire-and-forget inside submit — errors are warned
// so a persistence failure never blocks the result view or the rating UI.
async function persistAndNavigate(record) {
  try {
    await writeSession(record);
    await sessionsStore.upsertFromRecord(record);
    router.push({ path: "/chat", query: { session: record.id } });
  } catch (err) {
    console.warn("[RunWorkflowModal] persistAndNavigate failed — session may not appear in sidebar:", err);
    // swallow — result view and rating remain accessible regardless
  }
}

// Settles a completed run into the result view (+ session persist on success).
// Shared by the streaming and blocking-fallback paths.
function finishRun(result, sessionId) {
  runResult.value = result;
  if (result.ok) {
    // persist + navigate without awaiting so rating UI renders immediately
    persistAndNavigate(buildWorkflowSession(result, sessionId));
  }
}

async function submit() {
  if (running.value || !props.workflow || !canRun.value) return;
  running.value = true;
  runError.value = null;
  runResult.value = null;
  // Minted before the run, not when the record is built: the id is sent with the
  // run so the reason steps' LLM usage — a workflow's entire LLM cost — is
  // attributed to the session this run will be filed under. Deriving it
  // afterwards would leave that usage unattributed and the usage strip at zero.
  const sessionId = uuid();
  // Seed the live list pending from the definition; step events flip by index.
  liveSteps.value = (props.workflow.definition?.steps ?? []).map((s) => ({
    skill: stepLabel(s),
    status: "pending",
    denyReason: "",
  }));
  const inputs = { ...inputValues.value };
  const lineageId = props.workflow.lineageId;
  try {
    const result = await runWorkflowStream(lineageId, inputs, {
      onStep: (ev) => {
        const s = liveSteps.value[ev.index];
        if (!s) return;
        s.status = ev.ok ? "done" : "failed";
        if (ev.skill) s.skill = ev.skill;
        s.denyReason = ev.denyReason ?? "";
      },
      sessionId,
    });
    finishRun(result, sessionId);
  } catch {
    // Stream errored / proxy stripped SSE / no result frame — fall back to the
    // blocking POST once and render as before.
    try {
      finishRun(await runWorkflow(lineageId, inputs, sessionId), sessionId);
    } catch (err) {
      runError.value = err?.message ?? "Run failed";
    }
  } finally {
    running.value = false;
  }
}

async function submitRating() {
  if (ratingPending.value || !rating.value || !props.workflow) return;
  ratingPending.value = true;
  ratingError.value = null;
  try {
    await rateWorkflow(props.workflow.lineageId, {
      version: props.workflow.version ?? 0,
      rating: rating.value,
      note: ratingNote.value,
    });
    ratingDone.value = true;
  } catch (err) {
    ratingError.value = err?.message ?? "Rating failed";
  } finally {
    ratingPending.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible && workflow" class="run-backdrop">
    <div class="run-modal" role="dialog" aria-modal="true" aria-labelledby="run-modal-title">

      <header class="run-header">
        <Icon name="play-circle" :size="16" />
        <span id="run-modal-title" class="run-name">{{ workflow.name }}</span>
        <button class="run-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="run-divider" />

      <!-- Input form + preview — only shown before a run completes -->
      <template v-if="!runResult">
        <!-- Live per-step progress while the streamed run is in flight -->
        <section v-if="running" class="run-progress" data-testid="run-progress">
          <h3 class="preview-label">Running…</h3>
          <ul v-if="liveSteps.length > 0" class="step-list">
            <li
              v-for="(s, i) in liveSteps"
              :key="i"
              class="step-item"
              :class="{ 'step-ok': s.status === 'done', 'step-denied': s.status === 'failed' }"
              data-testid="live-step"
            >
              <span class="step-index">{{ i + 1 }}</span>
              <span class="step-skill">{{ s.skill }}</span>
              <Icon v-if="s.status === 'done'" name="check" :size="12" />
              <Icon v-else-if="s.status === 'failed'" name="close" :size="12" />
              <Icon v-else name="spinner" :size="12" class="spin" />
              <span v-if="s.status === 'failed' && s.denyReason" class="step-deny-reason">{{ s.denyReason }}</span>
            </li>
          </ul>
          <p v-else class="no-inputs">Executing…</p>
        </section>

        <!-- Preview: steps and required skills from the definition -->
        <section v-if="!running" class="run-preview" data-testid="run-preview">
          <div
            v-if="(workflow.definition?.steps ?? []).length > 0"
            class="preview-block"
            data-testid="preview-steps"
          >
            <h3 class="preview-label">Steps</h3>
            <ol class="preview-list">
              <li
                v-for="(step, i) in (workflow.definition?.steps ?? [])"
                :key="i"
                class="preview-item"
              >
                <span class="preview-skill">{{ step.skill }}</span>
              </li>
            </ol>
          </div>
          <div
            v-if="(workflow.definition?.requires ?? []).length > 0"
            class="preview-block"
            data-testid="preview-requires"
          >
            <h3 class="preview-label">Requires</h3>
            <ul class="preview-list preview-list--flat">
              <li
                v-for="req in (workflow.definition?.requires ?? [])"
                :key="req"
                class="preview-item"
              >
                <span class="preview-skill">{{ req }}</span>
              </li>
            </ul>
          </div>
        </section>

        <hr v-if="!running && ((workflow.definition?.steps ?? []).length > 0 || (workflow.definition?.requires ?? []).length > 0)" class="run-divider" />

        <section v-if="!running" class="run-inputs" data-testid="run-inputs">
          <template v-if="(workflow.definition?.inputs ?? []).length > 0">
            <div
              v-for="inp in (workflow.definition?.inputs ?? [])"
              :key="inp.name"
              class="input-row"
            >
              <label :for="`run-input-${inp.name}`" class="input-label">
                {{ inp.name }}<span v-if="isRequired(inp)" class="req-marker" aria-hidden="true"> *</span>
              </label>
              <input
                v-if="widgetFor(inp) === 'boolean'"
                :id="`run-input-${inp.name}`"
                v-model="inputValues[inp.name]"
                type="checkbox"
                :data-testid="`input-${inp.name}`"
              />
              <select
                v-else-if="widgetFor(inp) === 'enum'"
                :id="`run-input-${inp.name}`"
                v-model="inputValues[inp.name]"
                class="input-field"
                :data-testid="`input-${inp.name}`"
              >
                <option v-for="opt in inp.schema.enum" :key="opt" :value="opt">{{ opt }}</option>
              </select>
              <input
                v-else-if="widgetFor(inp) === 'number'"
                :id="`run-input-${inp.name}`"
                v-model.number="inputValues[inp.name]"
                class="input-field"
                type="number"
                :placeholder="inp.default ?? ''"
                :data-testid="`input-${inp.name}`"
              />
              <input
                v-else
                :id="`run-input-${inp.name}`"
                v-model="inputValues[inp.name]"
                class="input-field"
                type="text"
                :placeholder="inp.default ?? ''"
                :data-testid="`input-${inp.name}`"
              />
            </div>
          </template>
          <p v-else class="no-inputs">No inputs required.</p>
        </section>

        <p v-if="runError" class="run-error" role="alert" data-testid="run-error">{{ runError }}</p>

        <footer class="run-footer">
          <button class="btn-cancel" :disabled="running" @click="close">Cancel</button>
          <button
            class="btn-run"
            :disabled="running || !canRun"
            :aria-busy="running"
            data-testid="btn-run"
            @click="submit"
          >
            <Icon v-if="running" name="spinner" :size="14" class="spin" />
            <Icon v-else name="play-circle" :size="14" />
            {{ running ? "Running…" : "Run" }}
          </button>
        </footer>
      </template>

      <!-- Run result -->
      <template v-else>
        <section class="run-result" data-testid="run-result">
          <div
            class="result-status"
            :class="runResult.ok ? 'result-ok' : 'result-failed'"
            data-testid="result-status"
          >
            <Icon :name="runResult.ok ? 'check' : 'close'" :size="14" />
            {{ runResult.ok ? "Completed" : "Failed" }}
          </div>

          <p v-if="runResult.error" class="result-error" data-testid="result-error">
            {{ runResult.error }}
          </p>

          <!-- Per-step outcomes -->
          <template v-if="runResult.result">
            <div v-if="runResult.result.halted" class="result-halted" data-testid="result-halted">
              Halted at step {{ runResult.result.haltedAtStep + 1 }}:
              {{ runResult.result.haltReason }}
            </div>
            <ul v-if="(runResult.result.steps ?? []).length > 0" class="step-list">
              <li
                v-for="(step, i) in runResult.result.steps"
                :key="i"
                class="step-item"
                :class="step.allowed ? 'step-ok' : 'step-denied'"
                data-testid="step-item"
              >
                <span class="step-index">{{ i + 1 }}</span>
                <span class="step-skill">{{ step.skill ?? step.toolName ?? "" }}</span>
                <span v-if="!step.allowed" class="step-deny-reason">{{ step.denyReason }}</span>
              </li>
            </ul>
          </template>
        </section>

        <!-- Rating — shown after run; hidden once rated -->
        <section v-if="!ratingDone" class="rating-section" data-testid="rating-section">
          <p class="rating-label">How did it go?</p>
          <div class="rating-buttons">
            <button
              class="btn-rate"
              :class="{ 'btn-rate--selected': rating === 'RATING_SUCCESS' }"
              data-testid="btn-rate-success"
              @click="rating = 'RATING_SUCCESS'"
            >
              <Icon name="check" :size="14" /> Good
            </button>
            <button
              class="btn-rate"
              :class="{ 'btn-rate--selected': rating === 'RATING_BAD' }"
              data-testid="btn-rate-bad"
              @click="rating = 'RATING_BAD'"
            >
              <Icon name="close" :size="14" /> Bad
            </button>
          </div>
          <textarea
            v-if="rating"
            v-model="ratingNote"
            class="rating-note"
            placeholder="Optional note…"
            rows="2"
            data-testid="rating-note"
          />
          <p v-if="ratingError" class="run-error" role="alert" data-testid="rating-error">{{ ratingError }}</p>
          <footer class="run-footer">
            <button class="btn-cancel" @click="close">Skip</button>
            <button
              class="btn-run"
              :disabled="!rating || ratingPending"
              :aria-busy="ratingPending"
              data-testid="btn-submit-rating"
              @click="submitRating"
            >
              {{ ratingPending ? "Saving…" : "Submit Rating" }}
            </button>
          </footer>
        </section>

        <section v-else class="rating-done" data-testid="rating-done">
          <Icon name="check" :size="14" /> Rating saved.
          <footer class="run-footer">
            <button class="btn-run" @click="close">Close</button>
          </footer>
        </section>
      </template>

    </div>
  </div>
</template>

<style scoped>
.run-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.run-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 360px;
  max-width: 560px;
  width: 100%;
  max-height: min(80vh, 640px);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  overflow: hidden;
}

.run-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.run-name {
  flex: 1;
  font-size: 0.9375rem;
}

.run-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.run-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.run-inputs {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.no-inputs {
  font-size: 0.875rem;
  color: var(--text-muted);
  margin: 0;
}

.input-row {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.input-label {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  font-family: var(--font-mono);
}

.req-marker {
  color: var(--danger);
}

.run-progress {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.input-field {
  font-size: 0.875rem;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg);
  color: var(--text);
  font-family: var(--font-sans);
}

.input-field:focus {
  outline: none;
  border-color: var(--accent);
}

.run-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
  flex-shrink: 0;
}

.run-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

.btn-run {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--accent);
  color: var(--text-on-accent);
  border: 1px solid transparent;
  transition: background 0.15s, opacity 0.15s;
}

.btn-run:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.btn-run:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-cancel {
  display: inline-flex;
  align-items: center;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  transition: background 0.15s;
}

.btn-cancel:hover {
  background: var(--bg-hover);
}

/* ── Result ── */
.run-result {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.result-status {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.875rem;
  font-weight: 500;
}

.result-ok   { color: var(--ok); }
.result-failed { color: var(--danger); }

.result-error {
  font-size: 0.8125rem;
  color: var(--danger);
  margin: 0;
}

.result-halted {
  font-size: 0.8125rem;
  color: var(--danger);
  padding: var(--space-2) var(--space-3);
  background: var(--fill-danger);
  border-radius: var(--radius-sm);
}

.step-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.step-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.8125rem;
  padding: var(--space-1) var(--space-2);
  border-radius: var(--radius-sm);
}

.step-ok     { color: var(--text-muted); }
.step-denied { color: var(--danger); }

.step-index {
  font-family: var(--font-mono);
  color: var(--text-faint);
  min-width: 1.25rem;
}

.step-skill { font-family: var(--font-mono); }

.step-deny-reason {
  font-style: italic;
  color: var(--danger);
}

/* ── Rating ── */
.rating-section {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  border-top: 1px solid var(--border);
  padding-top: var(--space-3);
  flex-shrink: 0;
}

.rating-label {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text-muted);
}

.rating-buttons {
  display: flex;
  gap: var(--space-2);
}

.btn-rate {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  cursor: pointer;
  background: var(--bg);
  border: 1px solid var(--border);
  color: var(--text-muted);
  transition: background 0.15s, border-color 0.15s, color 0.15s;
}

.btn-rate:hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-rate--selected {
  border-color: var(--accent);
  color: var(--accent);
  background: var(--fill-accent);
}

.rating-note {
  font-family: var(--font-sans);
  font-size: 0.875rem;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg);
  color: var(--text);
  resize: vertical;
}

.rating-done {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-direction: column;
  font-size: 0.875rem;
  color: var(--ok);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border);
}

.spin {
  animation: spin 0.8s linear infinite;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}

/* ── Preview ── */
.run-preview {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  flex-shrink: 0;
}

.preview-block {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.preview-label {
  margin: 0;
  font-size: 0.75rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-faint);
}

.preview-list {
  margin: 0;
  padding: 0 0 0 1.25rem;
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.preview-list--flat {
  list-style: none;
  padding: 0;
}

.preview-item {
  font-size: 0.8125rem;
  color: var(--text-muted);
}

.preview-skill {
  font-family: var(--font-mono);
  color: var(--text);
}
</style>
