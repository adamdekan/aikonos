import { defineStore } from "pinia";
import { ref } from "vue";

export const usePromptStore = defineStore("prompt", () => {
  // envelopeId: the inbox delegation the next chat handles (AG-17). Set with
  // either path below; Chat hands it to the first run only.
  const envelopeId = ref("");

  const pending = ref("");
  function set(text, envId = "") {
    pending.value = text;
    envelopeId.value = envId;
  }
  function clear() {
    pending.value = "";
  }

  // prefill: lands text in the chat composer draft WITHOUT auto-submitting.
  const prefill = ref("");
  function setPrefill(text, envId = "") {
    prefill.value = text;
    envelopeId.value = envId;
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

  return { pending, set, clear, prefill, setPrefill, clearPrefill, envelopeId, takeEnvelopeId };
});
