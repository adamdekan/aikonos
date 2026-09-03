import { defineStore } from "pinia";
import { ref } from "vue";

export const usePromptStore = defineStore("prompt", () => {
  const pending = ref("");
  function set(text) {
    pending.value = text;
  }
  function clear() {
    pending.value = "";
  }

  // prefill: lands text in the chat composer draft WITHOUT auto-submitting.
  const prefill = ref("");
  function setPrefill(text) {
    prefill.value = text;
  }
  function clearPrefill() {
    prefill.value = "";
  }

  return { pending, set, clear, prefill, setPrefill, clearPrefill };
});
