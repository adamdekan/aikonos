import { ref } from "vue";
import { delegate } from "../api/inbox.js";
import { stripMention } from "../lib/mention.js";
import { useToast } from "../components/ui/useToast.js";

// Delegation confirm modal: @-mention delegation flow (confirm/cancel).
// F212: no document-wide Enter shortcut. Only the focused Confirm button
// confirms (the browser turns Enter on it into a click), so Enter on Cancel,
// or the still-held Enter that opened the modal, never delegates.
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

  return {
    pendingDelegation,
    confirmDelegate,
    cancelDelegate,
  };
}
