import { defineStore } from "pinia";
import { ref } from "vue";

export const usePromptStore = defineStore("prompt", () => {
  // envelopeId: the inbox delegation the next chat handles (AG-17). Set with
  // either path below; Chat hands it to the first run only.
  const envelopeId = ref("");
  // delegation: { from, scopes, maxCost } of that envelope (AG-11), shown as
  // the chat banner. Set and taken together with envelopeId.
  const delegation = ref(null);

  const pending = ref("");
  function set(text, envId = "", deleg = null) {
    pending.value = text;
    envelopeId.value = envId;
    delegation.value = deleg;
  }
  function clear() {
    pending.value = "";
  }

  // prefill: lands text in the chat composer draft WITHOUT auto-submitting.
  const prefill = ref("");
  function setPrefill(text, envId = "", deleg = null) {
    prefill.value = text;
    envelopeId.value = envId;
    delegation.value = deleg;
  }
  function clearPrefill() {
    prefill.value = "";
  }

  // takeEnvelopeId returns and clears the pending envelope id.
  function takeEnvelopeId() {
    const id = envelopeId.value;
    envelopeId.value = "";
    return id;
  }

  // takeDelegation returns and clears the pending delegation limits.
  function takeDelegation() {
    const d = delegation.value;
    delegation.value = null;
    return d;
  }

  return { pending, set, clear, prefill, setPrefill, clearPrefill, envelopeId, takeEnvelopeId, delegation, takeDelegation };
});
