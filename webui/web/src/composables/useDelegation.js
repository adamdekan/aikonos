import { ref, watch, onUnmounted } from "vue";
import { delegate } from "../api/inbox.js";
import { stripMention } from "../lib/mention.js";
import { useToast } from "../components/ui/useToast.js";

// Delegation confirm modal: @-mention delegation flow (confirm/cancel) plus the
// Enter-confirms-modal keydown listener, scoped to pendingDelegation so it never
// interferes with other modals (e.g. the soul-editor textarea).
//
// draft: ref<string> — the composer draft, restored/cleared around delegation.
// addUser: fn(text) — appends a user message bubble (owned by useAguiRun).
// messages: computed/ref array — the active buffer's message list.
// persist: fn() — chatStore.persist, called after appending the transcript note.
export function useDelegation({ draft, addUser, messages, persist }) {
  const { push: toast } = useToast();

  // { text: string, target: { userId, displayName } | { groupId, displayName, memberCount } }
  // while modal is open; null otherwise.
  const pendingDelegation = ref(null);

  async function confirmDelegate() {
    if (!pendingDelegation.value) return;
    const { text, target } = pendingDelegation.value;
    pendingDelegation.value = null;
    try {
      const isGroup = target.groupId != null;
      const stripped = stripMention(text, target.displayName);
      const delegateArgs = isGroup
        ? { group: target.groupId, intent: stripped, scopes: [], maxCost: 50 }
        : { to: target.userId,    intent: stripped, scopes: [], maxCost: 50 };
      const result = await delegate(delegateArgs);
      if (result && result.ok === false) {
        toast("error", result.error ?? "Delegation failed");
        draft.value = text;
        return;
      }
      const successMsg = isGroup
        ? `✓ Task delegated to group ${target.displayName} (${target.memberCount} people)`
        : `✓ Task delegated to ${target.displayName}`;
      // Group: toast mirrors the transcript note. User: original pre-CP4 string (invariant 4).
      toast("ok", isGroup ? successMsg : `Delegated to ${target.displayName}`);
      addUser(text);
      messages.value.push({ role: "assistant", text: successMsg, tools: [], error: null });
      persist();
      draft.value = "";
    } catch (e) {
      toast("error", e?.message ?? "Delegation failed");
      draft.value = text;
    }
  }

  function cancelDelegate() {
    const pending = pendingDelegation.value;
    pendingDelegation.value = null;
    if (pending) draft.value = pending.text;
  }

  // Enter confirms the delegate modal while it is open. The listener is added
  // only when pendingDelegation is set so it never interferes with other modals
  // (e.g. the soul-editor modal, which has a textarea where Enter must insert a newline).
  function _onDelegateKeydown(e) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      confirmDelegate();
    }
  }

  // Non-immediate (default) watch is correct: this runs inside setup() before any
  // user gesture can set pendingDelegation, so there is never a value to handle at
  // registration time — the listener is only ever needed after an interactive submit.
  //
  // truthy→truthy edge: if the user submits a second delegation while one is already
  // pending (rapid re-submit), val is truthy both before and after the callback fires,
  // so addEventListener is called again on an already-registered listener — the browser
  // deduplicates identical (type, listener, options) tuples, leaving exactly one active
  // listener, which is correct (the modal is still open).
  watch(pendingDelegation, (val) => {
    if (val) {
      document.addEventListener("keydown", _onDelegateKeydown);
    } else {
      document.removeEventListener("keydown", _onDelegateKeydown);
    }
  });

  onUnmounted(() => {
    document.removeEventListener("keydown", _onDelegateKeydown);
  });

  return {
    pendingDelegation,
    confirmDelegate,
    cancelDelegate,
  };
}
