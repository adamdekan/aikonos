<script setup>
import { nextTick, ref, watch } from "vue";
import ToolCard from "./ToolCard.vue";
import ToolTrace from "./ToolTrace.vue";
import MarkdownMessage from "./MarkdownMessage.vue";
import SkillTimeline from "./SkillTimeline.vue";
import MemoryTimeline from "./MemoryTimeline.vue";
import SubagentTimeline from "./SubagentTimeline.vue";
import Icon from "./Icon.vue";
import { useToast } from "./ui/useToast.js";
import { usePrefsStore } from "../store/prefs.js";

const prefsStore = usePrefsStore();

const props = defineProps({
  // Array of message objects: { role, text, tools, error }
  messages: { type: Array, default: () => [] },
  // Suppresses the edit affordance while a run is streaming — editing mid-run
  // would splice the buffer out from under the active assistant message. Also
  // gates the live tool-trace dots on the last assistant message.
  running: { type: Boolean, default: false },
  // True while assistant text is actively streaming — the tool trace uses it to
  // suppress the standalone dots (the text itself is the progress indication).
  textStreaming: { type: Boolean, default: false },
});

// The live assistant message is the last one in the buffer; only its trace shows
// the in-flight dots. isActiveAssistant(idx) is false for every historical turn.
function isActiveAssistant(idx) {
  return props.running && idx === props.messages.length - 1
    && props.messages[idx]?.role === "assistant";
}

// reply: user wants to compose a follow-up referencing this assistant message.
// edit:  user edited their own message at `index`; the conversation is re-run
//        from that point with the new text (everything after it is discarded).
const emit = defineEmits(["reply", "edit"]);

const { push: toast } = useToast();

// ── inline edit state (user messages only) ───────────────────────────────────
const editingIdx = ref(null);
const editDraft   = ref("");
const editRef     = ref(null);

function startEdit(idx, text) {
  editingIdx.value = idx;
  editDraft.value  = text;
  nextTick(() => {
    const el = editRef.value?.[0] ?? editRef.value;
    el?.focus?.();
  });
}

function cancelEdit() {
  editingIdx.value = null;
  editDraft.value  = "";
}

function saveEdit(idx) {
  const text = editDraft.value.trim();
  if (!text) return;
  editingIdx.value = null;
  emit("edit", idx, text);
}

function onEditKeydown(e, idx) {
  if (e.key === "Escape") {
    e.preventDefault();
    cancelEdit();
  } else if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    saveEdit(idx);
  }
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text ?? "");
    toast("ok", "Copied to clipboard.");
  } catch {
    toast("error", "Copy failed.");
  }
}

// Scroll to bottom after new messages.
watch(
  () => props.messages,
  async () => {
    await nextTick();
    const container = document.querySelector(".message-list");
    if (container) container.scrollTop = container.scrollHeight;
  },
  { deep: true }
);
</script>

<template>
  <div class="message-list">
    <div class="message-list-inner">
    <template v-for="(msg, idx) in messages" :key="idx">
      <!-- User row: bubble + edit affordance / inline editor -->
      <div v-if="msg.role === 'user'" class="msg-row msg-row--user">
        <template v-if="editingIdx === idx">
          <div class="edit-box">
            <textarea
              ref="editRef"
              v-model="editDraft"
              class="edit-input"
              rows="2"
              data-testid="edit-input"
              @keydown="onEditKeydown($event, idx)"
            />
            <div class="edit-actions">
              <button class="edit-btn" data-testid="edit-cancel" @click="cancelEdit">Cancel</button>
              <button
                class="edit-btn edit-btn--save"
                data-testid="edit-save"
                :disabled="!editDraft.trim()"
                @click="saveEdit(idx)"
              >Save &amp; resend</button>
            </div>
          </div>
        </template>
        <template v-else>
          <div class="bubble bubble--user">{{ msg.text }}</div>
          <div class="msg-actions msg-actions--user">
            <button
              class="msg-action"
              data-testid="msg-edit"
              title="Edit and resend from here"
              aria-label="Edit message"
              :disabled="running"
              @click="startEdit(idx, msg.text)"
            >
              <Icon name="edit" :size="14" />
            </button>
          </div>
        </template>
      </div>

      <!-- Skills timeline row: static, no hover actions -->
      <div v-else-if="msg.role === 'skills'" class="msg-row msg-row--skills">
        <SkillTimeline :skills="msg.skills" />
      </div>

      <!-- Recalled-memory row: static, no hover actions -->
      <div v-else-if="msg.role === 'memory'" class="msg-row msg-row--skills">
        <MemoryTimeline :concepts="msg.concepts" />
      </div>

      <!-- Subagent fan-out timeline row: static, no hover actions -->
      <div v-else-if="msg.role === 'subagents'" class="msg-row msg-row--skills">
        <SubagentTimeline :branches="msg.branches" />
      </div>

      <!-- Assistant row: text stream + tool cards + copy/reply actions -->
      <div v-else-if="msg.role === 'assistant'" class="msg-row msg-row--assistant">
        <div class="bubble bubble--assistant">
          <ToolTrace
            :tools="msg.tools || []"
            :active="isActiveAssistant(idx)"
            :text-streaming="textStreaming"
          />

          <MarkdownMessage v-if="msg.text" :text="msg.text" />

          <ToolCard
            v-for="tool in (prefsStore.debugBroker ? msg.tools : [])"
            :key="tool.id"
            :id="tool.id"
            :name="tool.name"
            :args-json="tool.argsJson"
            :result="tool.result"
            :is-error="tool.isError"
            :done="tool.done"
          />

          <div v-if="msg.error" class="bubble-error">
            {{ msg.error }}
          </div>
        </div>
        <div v-if="msg.text" class="msg-actions msg-actions--assistant">
          <button
            class="msg-action"
            data-testid="msg-copy"
            title="Copy response"
            aria-label="Copy response"
            @click="copyText(msg.text)"
          >
            <Icon name="copy" :size="14" />
          </button>
          <button
            class="msg-action"
            data-testid="msg-reply"
            title="Reply to this response"
            aria-label="Reply"
            @click="emit('reply', msg.text)"
          >
            <Icon name="reply" :size="14" />
          </button>
        </div>
      </div>
    </template>
    </div>
  </div>
</template>

<style scoped>
.message-list {
  overflow-y: auto;
  flex: 1;
  padding: 1rem 1.5rem;
}

/* Centered 800px column; bubbles align within it, sharing the footer's column. */
.message-list-inner {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  max-width: 800px;
  width: 100%;
  margin: 0 auto;
}

/* Row wraps the bubble + its hover action toolbar so the actions align to the
   bubble edge and reveal on row hover without shifting layout. */
.msg-row {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.msg-row--user {
  align-items: flex-end;
}
.msg-row--assistant {
  align-items: stretch;
}
.msg-row--skills {
  align-items: stretch;
}

.msg-actions {
  display: flex;
  gap: 4px;
  opacity: 0;
  transition: opacity 0.12s ease;
}
.msg-row:hover .msg-actions,
.msg-actions:focus-within {
  opacity: 1;
}
.msg-actions--assistant {
  justify-content: flex-start;
}

.msg-action {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  background: transparent;
  color: var(--text-muted);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 0.12s, color 0.12s;
}
.msg-action:hover:not(:disabled) {
  background: var(--bg-hover);
  color: var(--text);
}
.msg-action:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}

/* ── inline edit box ── */
.edit-box {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
  max-width: 70%;
}
.edit-input {
  width: 100%;
  background: var(--bg-elevated);
  border: 1px solid var(--accent);
  border-radius: var(--radius);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.9375rem;
  line-height: 1.5;
  padding: 0.5rem 0.75rem;
  resize: vertical;
  outline: none;
}
.edit-actions {
  display: flex;
  gap: 6px;
  justify-content: flex-end;
}
.edit-btn {
  background: transparent;
  color: var(--text-muted);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 4px 12px;
  font-size: 12px;
  cursor: pointer;
}
.edit-btn:hover:not(:disabled) { background: var(--bg-hover); color: var(--text); }
.edit-btn--save {
  background: var(--accent);
  color: var(--text-on-accent);
  border-color: transparent;
}
.edit-btn--save:hover:not(:disabled) { background: var(--accent-hover); }
.edit-btn:disabled { opacity: 0.5; cursor: not-allowed; }

.bubble {
  max-width: 70%;
  padding: 0.625rem 0.875rem;
  border-radius: var(--radius);
  font-size: 0.9375rem;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.bubble--user {
  align-self: flex-end;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  color: var(--text);
}

.bubble--assistant {
  align-self: stretch;
  background: transparent;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  max-width: 100%;
  padding: 0;
}

.bubble-error {
  color: var(--danger);
  font-size: 0.875rem;
  padding: 0.5rem 0.75rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
}
</style>
