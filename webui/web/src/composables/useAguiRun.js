import { ref } from "vue";
import { runAgui } from "../api/agui.js";
import { usePrefsStore } from "../store/prefs.js";

// Message-buffer helpers shared by the AG-UI run loop and delegation (both
// append transcript entries). Plain factory — not a Vue composable itself —
// so callers can share one instance via explicit arguments rather than a
// seventh composable or module-level state.
export function createMessageHelpers(messages, chatStore) {
  function addUser(text) {
    messages.value.push({ role: "user", text });
    chatStore.persist();
  }

  function addAssistant() {
    messages.value.push({ role: "assistant", text: "", tools: [], error: null });
    return messages.value[messages.value.length - 1];
  }

  function currentAssistant() {
    const last = messages.value[messages.value.length - 1];
    if (last && last.role === "assistant") return last;
    return addAssistant();
  }

  function findTool(msg, id) {
    return msg.tools.find(t => t.id === id);
  }

  return { addUser, addAssistant, currentAssistant, findTool };
}

// The AG-UI run loop: handler factory, runChat/submit, and user-initiated abort.
//
// draft: ref<string> — the composer draft; created by the caller and shared with
// useDelegation (both mutate it around a send/cancel), passed in rather than
// created here to avoid a circular composable-construction order.
// messages/chatStore/threadId/agentId: the active buffer's reactive slices.
// addUser/addAssistant/currentAssistant/findTool: message helpers (createMessageHelpers).
// onApprovalRequest: from useApprovals — starts the poll fallback itself (F37: no
// unconditional startPolling call here). scheduleOneShotPoll: from useApprovals — a
// one-time /approvals check fired on RUN_STARTED as a safety net for a dropped
// aikonos.approval.request SSE frame (F37 follow-up). stopPolling/resetApprovals:
// from useApprovals.
// maybeCreateSession/persistSessionMessages: from useSessionLifecycle.
// pendingDelegation: ref from useDelegation — submit() routes @-mention delegation there.
export function useAguiRun({
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
}) {
  const running = ref(false);
  const textStreaming = ref(false);  // TEXT_MESSAGE_START → TEXT_MESSAGE_END

  let abortController = null;

  function makeHandlers() {
    let assistantMsg = null;
    let settled = false;

    return {
      // Exposed so runChat can detect a stream that ended without a verdict.
      get settled() { return settled; },
      get assistantMsg() { return assistantMsg; },
      onRunStarted() {
        assistantMsg = addAssistant();
        // Safety net for a dropped approval SSE frame — see useApprovals.
        scheduleOneShotPoll();
      },

      onTextStart() {
        if (!assistantMsg) assistantMsg = currentAssistant();
        textStreaming.value = true;
      },

      onText(delta) {
        if (!assistantMsg) assistantMsg = currentAssistant();
        assistantMsg.text += delta;
      },

      onTextEnd() {
        // text is already accumulated; the stream is no longer producing text
        textStreaming.value = false;
      },

      onToolCall({ id, name, description }) {
        if (!assistantMsg) assistantMsg = currentAssistant();
        assistantMsg.tools.push({ id, name, description, argsJson: "", result: null, isError: false, done: false });
      },

      onToolArgs({ id, argsJson }) {
        if (!assistantMsg) assistantMsg = currentAssistant();
        const tool = findTool(assistantMsg, id);
        if (tool) tool.argsJson = argsJson;
      },

      onToolEnd(id) {
        if (!assistantMsg) return;
        const tool = findTool(assistantMsg, id);
        // done flag set to true when result arrives; toolEnd just marks it pending-done.
      },

      onToolResult({ id, content, isError }) {
        if (!assistantMsg) assistantMsg = currentAssistant();
        const tool = findTool(assistantMsg, id);
        if (tool) {
          tool.result  = content;
          tool.isError = isError;
          tool.done    = true;
        } else {
          // Result arrived without a prior TOOL_CALL_START — add an inline card.
          assistantMsg.tools.push({ id, name: id, argsJson: "", result: content, isError, done: true });
        }
      },

      onApprovalRequest,

      onToolError(info) {
        if (!assistantMsg) assistantMsg = currentAssistant();
        const tool = findTool(assistantMsg, info.toolCallId);
        if (tool) {
          tool.result  = info.content;
          tool.isError = true;
          tool.done    = true;
        }
      },

      onUser(_val) { /* no-op */ },

      // Per-session announcement dedup lives here (display-only — activation
      // already happened server-side regardless of what the timeline shows).
      // Insert before the assistant placeholder RUN_STARTED just pushed so
      // the timeline lands between the user bubble and the reply.
      onSkillsLoaded(payload) {
        const incoming = Array.isArray(payload?.skills) ? payload.skills : [];
        if (incoming.length === 0) return;

        const seen = new Set();
        for (const m of messages.value) {
          if (m.role === "skills" && Array.isArray(m.skills)) {
            for (const s of m.skills) seen.add(`${s.name}::${s.status}`);
          }
        }
        const fresh = incoming.filter((s) => !seen.has(`${s.name}::${s.status}`));
        if (fresh.length === 0) return;

        const idx = assistantMsg ? messages.value.indexOf(assistantMsg) : -1;
        messages.value.splice(idx >= 0 ? idx : messages.value.length, 0, { role: "skills", skills: fresh });
        chatStore.persist();
      },

      // Same dedup+splice contract as onSkillsLoaded, keyed by (id, scope) — the
      // pair that identifies one concept across the three bundle scopes.
      onMemoryRecalled(payload) {
        const incoming = Array.isArray(payload?.concepts) ? payload.concepts : [];
        if (incoming.length === 0) return;

        const seen = new Set();
        for (const m of messages.value) {
          if (m.role === "memory" && Array.isArray(m.concepts)) {
            for (const c of m.concepts) seen.add(`${c.id}::${c.scope}`);
          }
        }
        const fresh = incoming.filter((c) => !seen.has(`${c.id}::${c.scope}`));
        if (fresh.length === 0) return;

        const idx = assistantMsg ? messages.value.indexOf(assistantMsg) : -1;
        messages.value.splice(idx >= 0 ? idx : messages.value.length, 0, { role: "memory", concepts: fresh });
        chatStore.persist();
      },

      // Fan-out timeline. Unlike
      // onSkillsLoaded/onMemoryRecalled (one-shot), this is two-phase: spawned
      // creates a row, completed mutates that same row in place. Correlation
      // key is `index`, scoped to one {role:"subagents"} message (a "fan-out
      // group") — indexes restart at 0 per fan-out, so the scoping rule is:
      // reuse the last subagents message unless it already has a branch at
      // this index (spawned re-using an index, or a completed whose spawn
      // frame arrived earlier in a now-closed group), in which case a fresh
      // fan-out has started and gets its own message.
      //
      // This is sound ONLY because two spawn_subagents calls in one turn can
      // never run concurrently: the tool is registered with
      // executionMode:"sequential" (agent-gateway/src/pi/tools.ts, pinned by
      // subagent-spawn-tool.test.ts), which forces pi-agent-core to serialize
      // the whole assistant turn whenever it appears. Without that, two
      // fan-outs could share one event stream and both restart indexing at 0
      // concurrently, and this index-based grouping would merge their rows.
      onSubagentSpawned(payload) {
        const { index, task, role } = payload ?? {};
        if (typeof index !== "number") return;

        const last = [...messages.value].reverse().find((m) => m.role === "subagents");
        let msg;
        if (last && !last.branches.some((b) => b.index === index)) {
          msg = last;
        } else {
          msg = { role: "subagents", branches: [] };
          const idx = assistantMsg ? messages.value.indexOf(assistantMsg) : -1;
          messages.value.splice(idx >= 0 ? idx : messages.value.length, 0, msg);
        }
        msg.branches.push({ index, task, role: role ?? null, status: "running", cost: 0 });
        chatStore.persist();
      },

      onSubagentCompleted(payload) {
        const { index, task, role, ok, failure, cost } = payload ?? {};
        if (typeof index !== "number") return;

        // Find the most recent still-open (running) branch at this index —
        // the ordinary spawned-then-completed path.
        let target = null;
        for (let i = messages.value.length - 1; i >= 0; i--) {
          const m = messages.value[i];
          if (m.role !== "subagents") continue;
          const b = m.branches.find((b) => b.index === index && b.status === "running");
          if (b) { target = b; break; }
        }

        if (target) {
          target.task = task ?? target.task;
          target.role = role ?? target.role;
          target.status = ok ? "ok" : "failure";
          target.failure = failure;
          target.cost = cost ?? 0;
        } else {
          // No matching spawned (SSE reconnect) — still render a standalone row,
          // scoped the same way onSubagentSpawned is.
          const last = [...messages.value].reverse().find((m) => m.role === "subagents");
          let msg;
          if (last && !last.branches.some((b) => b.index === index)) {
            msg = last;
          } else {
            msg = { role: "subagents", branches: [] };
            const idx = assistantMsg ? messages.value.indexOf(assistantMsg) : -1;
            messages.value.splice(idx >= 0 ? idx : messages.value.length, 0, msg);
          }
          msg.branches.push({ index, task, role: role ?? null, status: ok ? "ok" : "failure", failure, cost: cost ?? 0 });
        }
        chatStore.persist();
      },

      onFinished() {
        settled = true;
        textStreaming.value = false;
        running.value = false;
        stopPolling();
        resetApprovals();
        chatStore.persist();
        persistSessionMessages();
      },

      onError(message) {
        settled = true;
        textStreaming.value = false;
        running.value = false;
        stopPolling();
        resetApprovals();
        const msg = assistantMsg ?? addAssistant();
        msg.error = message;
        chatStore.persist();
        persistSessionMessages();
      },
    };
  }

  // ── stop (user-initiated abort) ───────────────────────────────────────────

  function stop() {
    abortController?.abort();
    running.value = false;
    textStreaming.value = false;
    stopPolling();
    resetApprovals();
  }

  // Extracted normal-chat flow. Called by submit() for the plain / skill path,
  // and by cancelDelegate() so the @-text sends as a normal prompt.
  // skillName is non-null only when invoked via the /command palette.
  async function runChat(text, skillName = null) {
    running.value = true;
    textStreaming.value = false;

    // Build history from all turns before the new user message (which addUser is
    // about to append). Slice taken before addUser so the new turn is excluded.
    const priorMessages = messages.value.slice();
    addUser(text);

    // Create session file on the first user message of a new conversation.
    await maybeCreateSession(text);

    const history = priorMessages
      .filter((m) => (m.role === "user" || m.role === "assistant") && m.text)
      .map((m) => ({ role: m.role, content: m.text }));

    const handlers = makeHandlers();

    // Per-user agent instructions (settings modal) ride every run; the gateway
    // folds them into the system prompt when the thread session is created.
    const userInstructions = usePrefsStore().chatInstructions.trim() || undefined;

    abortController = new AbortController();
    await runAgui(
      {
        prompt: text,
        threadId: threadId.value,
        agentId: agentId.value || undefined,
        history,
        signal: abortController.signal,
        // Spend attribution only — null
        // until maybeCreateSession above promotes the draft buffer, which is why
        // it is read after that await rather than at the top of runChat.
        ...(chatStore.activeSessionId ? { sessionId: chatStore.activeSessionId } : {}),
        ...(skillName ? { skillName } : {}),
        ...(userInstructions ? { userInstructions } : {}),
      },
      handlers,
    );

    // Connection-lost: stream ended without a verdict (RUN_FINISHED/RUN_ERROR) and the
    // user did not abort — mark the in-flight assistant message so the loss is visible.
    if (!handlers.settled && !abortController.signal.aborted) {
      const msg = handlers.assistantMsg ?? addAssistant();
      msg.error = "Connection lost — the response may be incomplete.";
      chatStore.persist();
      persistSessionMessages();
    }

    running.value = false;
    textStreaming.value = false;
    stopPolling();
    resetApprovals();
    abortController = null;
  }

  // Edit a prior user message and continue the conversation from that point.
  // Everything at `index` and after (the edited user turn plus all later turns)
  // is dropped, then the edited text is re-sent — runChat rebuilds history from
  // the truncated prefix and appends the new user turn.
  async function editAndResend(index, newText) {
    if (running.value) return;
    const text = (newText ?? "").trim();
    if (!text) return;
    if (index < 0 || index >= messages.value.length) return;
    messages.value.splice(index);
    await runChat(text);
  }

  // payload is either undefined (plain-text submit from Enter/Send),
  // {skillName: string} from the /command palette, or
  // {delegateTo: {userId, displayName}} (user) | {delegateTo: {groupId, displayName, memberCount}} (group)
  // from Composer @-mention. When skillName is present the gateway re-resolves and gates it server-side.
  async function submit(payload) {
    const skillName = payload?.skillName ?? null;
    const text = skillName
      ? `/${skillName}`   // display label for the user bubble when invoked via palette
      : draft.value.trim();
    if (!text || running.value) return;

    if (payload?.delegateTo) {
      // Store for the confirm modal; clear draft optimistically.
      draft.value = "";
      pendingDelegation.value = { text, target: payload.delegateTo };
      return;
    }

    draft.value = "";
    await runChat(text, skillName);
  }

  return {
    draft,
    running,
    textStreaming,
    stop,
    submit,
    runChat,
    editAndResend,
  };
}
