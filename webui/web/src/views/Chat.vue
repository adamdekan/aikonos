<script setup>
import { ref, computed, watch, nextTick, onMounted } from "vue";
import { useRoute } from "vue-router";
import { usePromptStore } from "../store/prompt.js";
import { useChatStore } from "../store/chat.js";
import { useSessionsStore } from "../store/sessions.js";
import { useAgentsStore } from "../store/agents.js";
import MessageList from "../components/MessageList.vue";
import Composer from "../components/Composer.vue";
import SessionUsage from "../components/SessionUsage.vue";
import ApprovalModal from "../components/ApprovalModal.vue";
import Modal from "../components/ui/Modal.vue";
import FormField from "../components/ui/FormField.vue";
import ErrorBanner from "../components/ui/ErrorBanner.vue";
import { useDiscoveryStore } from "../store/discovery.js";
import { useApprovals } from "../composables/useApprovals.js";
import { useSoulEditor } from "../composables/useSoulEditor.js";
import { useSessionLifecycle } from "../composables/useSessionLifecycle.js";
import { useDelegation } from "../composables/useDelegation.js";
import { createMessageHelpers, useAguiRun } from "../composables/useAguiRun.js";

const route         = useRoute();
const promptStore   = usePromptStore();
const chatStore     = useChatStore();
const sessionsStore = useSessionsStore();
const agentsStore   = useAgentsStore();

const agentId = computed(() => route.query.agent || "");

// Bind to the current buffer via computed so messages + threadId stay reactive to
// store resets and session switches (currentBuffer() reads activeSessionId ref).
const buffer   = computed(() => chatStore.currentBuffer());
const messages = computed(() => buffer.value.messages);
const threadId = computed(() => buffer.value.threadId);

function agentName() {
  if (!agentId.value) return "";
  const agent = agentsStore.assigned.find(a => a.id === agentId.value);
  return agent?.name ?? "";
}

const discoveryStore = useDiscoveryStore();
// True while any discovery dataset (mention/tool palette source) is failing.
const discoveryFailed = computed(() =>
  Object.values(discoveryStore.errors).some((e) => e !== null)
);

const { approval, pendingCount, onApprovalRequest, scheduleOneShotPoll, stopPolling, resetApprovals, onApprovalClose } = useApprovals();

const {
  soulEditable, showSoulModal, soulDraft, soulDraftBytes, soulError, soulSaving,
  loadSoul, openSoulModal, saveSoul,
} = useSoulEditor(agentId);

const { maybeCreateSession, persistSessionMessages, initResume } = useSessionLifecycle({
  route, chatStore, sessionsStore, agentId, agentName, messages,
});

const { addUser, addAssistant, currentAssistant, findTool } = createMessageHelpers(messages, chatStore);

// Shared with useAguiRun (submit/runChat) — created here rather than inside either
// composable to avoid a circular construction order (useDelegation needs draft +
// addUser; useAguiRun needs pendingDelegation from useDelegation).
const draft = ref("");

const { pendingDelegation, cancelDelegate, confirmDelegate } = useDelegation({
  draft,
  addUser,
  messages,
  persist: () => chatStore.persist(),
});

const { running, textStreaming, stop, submit, editAndResend } = useAguiRun({
  draft,
  messages,
  chatStore,
  threadId,
  agentId,
  addUser,
  addAssistant,
  currentAssistant,
  findTool,
  onApprovalRequest,
  scheduleOneShotPoll,
  stopPolling,
  resetApprovals,
  maybeCreateSession,
  persistSessionMessages,
  pendingDelegation,
});

// ── per-message actions ───────────────────────────────────────────────────────

const composerRef = ref(null);

// Reply: seed the composer with a short blockquote of the assistant message so
// the user's follow-up carries visible context, then focus the input.
function onReply(text) {
  const snippet = (text ?? "").split("\n").find((l) => l.trim()) ?? "";
  const quoted = snippet.slice(0, 200);
  draft.value = quoted ? `> ${quoted}\n\n` : "";
  nextTick(() => composerRef.value?.focus?.());
}

// Edit: replace the user turn at `index` and re-run from there.
function onEdit(index, newText) {
  editAndResend(index, newText);
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// Reload soul whenever the active named agent changes.
watch(agentId, (id) => { loadSoul(id); });

onMounted(async () => {
  // Load soul for the named agent (if any) — non-blocking, errors hidden.
  loadSoul(agentId.value);
  // Load discovery datasets (skill bundles, delegatable users/groups, files) —
  // non-blocking, errors are recorded on the store; palettes degrade to empty.
  discoveryStore.load();

  await initResume();

  // Two independent prefill paths — callers set exactly ONE, never both.
  // pending = auto-submit path (sets draft AND calls submit immediately).
  // prefill = draft-only path (sets draft, no submit; used by inbox "Start a new session").
  const pending = promptStore.pending;
  if (pending) {
    promptStore.clear();
    draft.value = pending;
    await submit();
  }

  if (promptStore.prefill) {
    draft.value = promptStore.prefill;
    promptStore.clearPrefill();
  }
});
</script>

<template>
  <div class="chat">
    <!-- Per-agent personality button — only shown when caller can edit -->
    <div v-if="agentId && soulEditable !== null" class="chat-header">
      <button
        class="btn-personality"
        data-testid="personality-btn"
        @click="openSoulModal"
      >
        Personality
      </button>
    </div>

    <ErrorBanner
      v-if="discoveryFailed"
      class="discovery-banner"
      message="Mention and tool palettes unavailable"
    >
      <template #action>
        <button
          class="btn-ghost-chat"
          data-testid="discovery-retry-btn"
          @click="discoveryStore.refresh()"
        >Retry</button>
      </template>
    </ErrorBanner>

    <MessageList
      :messages="messages"
      :running="running"
      :text-streaming="textStreaming"
      @reply="onReply"
      @edit="onEdit"
    />

    <ApprovalModal
      :visible="!!approval"
      :request="approval"
      :pending-count="pendingCount"
      @close="onApprovalClose"
    />

    <!-- Personality editor modal -->
    <Modal
      :open="showSoulModal"
      title="Agent Personality"
      @close="showSoulModal = false"
    >
      <FormField
        label="Personality (markdown, ≤ 4096 bytes)"
        :error="soulError"
      >
        <textarea
          v-model="soulDraft"
          data-testid="soul-textarea"
          rows="8"
          style="font-family: monospace; resize: vertical;"
          placeholder="Describe how this agent should behave…"
        />
        <span
          class="soul-counter"
          :class="{ 'soul-counter--over': soulDraftBytes > 4096 }"
        >
          {{ soulDraftBytes }} / 4096 bytes
        </span>
      </FormField>

      <template #footer>
        <button class="btn-ghost-chat" @click="showSoulModal = false">Cancel</button>
        <button
          class="btn-primary-chat"
          data-testid="soul-save-btn"
          :disabled="soulSaving || soulDraftBytes > 4096"
          @click="saveSoul"
        >
          {{ soulSaving ? "Saving…" : "Save" }}
        </button>
      </template>
    </Modal>

    <!-- Delegation confirm modal -->
    <Modal
      :open="!!pendingDelegation"
      title="Delegate task"
      @close="cancelDelegate"
    >
      <template v-if="pendingDelegation">
        <p v-if="pendingDelegation.target.groupId != null">
          Delegate to group <strong>{{ pendingDelegation.target.displayName }}</strong>
          ({{ pendingDelegation.target.memberCount }} people)?
        </p>
        <p v-else>
          Delegate to <strong>{{ pendingDelegation.target.displayName }}</strong>:
        </p>
        <p style="margin-top: 8px; word-break: break-word;">
          {{ pendingDelegation.text }}
        </p>
      </template>

      <template #footer>
        <button
          class="btn-ghost-chat"
          data-testid="delegate-cancel-btn"
          @click="cancelDelegate"
        >Cancel</button>
        <button
          class="btn-primary-chat"
          data-testid="delegate-confirm-btn"
          @click="confirmDelegate"
        >Confirm</button>
      </template>
    </Modal>

    <div class="chat-footer">
      <div class="chat-footer-inner">
        <SessionUsage
          :session-id="chatStore.activeSessionId || ''"
          :running="running"
          :has-content="messages.length > 0"
        />
        <Composer
          ref="composerRef"
          v-model="draft"
          :running="running"
          placeholder="Message Aikonos…"
          @submit="submit"
          @stop="stop"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.chat {
  display: flex;
  flex-direction: column;
  height: 100%;
  position: relative;
}

.chat-header {
  padding: 8px 1.5rem 0;
  display: flex;
  justify-content: flex-end;
  flex-shrink: 0;
}

.btn-personality {
  background: transparent;
  color: var(--text-muted);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm, 6px);
  padding: 4px 12px;
  font-size: 12px;
  cursor: pointer;
}
.btn-personality:hover { background: var(--bg-hover); color: var(--text); }

.discovery-banner {
  margin: 8px 1.5rem 0;
  flex-shrink: 0;
}

.btn-ghost-chat {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm, 6px); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary-chat {
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm, 6px);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary-chat:disabled { opacity: 0.5; cursor: not-allowed; }

.soul-counter { font-size: 11px; color: var(--text-muted); }
.soul-counter--over { color: var(--danger); font-weight: 600; }

.chat-footer {
  padding: 0.75rem 1.5rem 1.25rem;
  flex-shrink: 0;
}

/* Same 800px centered column as the message list so input lines up with the bubbles. */
.chat-footer-inner {
  max-width: 800px;
  margin: 0 auto;
}
</style>
