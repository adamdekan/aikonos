import { defineStore } from "pinia";
import { ref } from "vue";
import { listInbox } from "../api/inbox.js";

export const useInboxStore = defineStore("inbox", () => {
  const count = ref(0);

  async function refresh() {
    try {
      const data = await listInbox();
      count.value = (data.envelopes ?? []).length;
    } catch {
      count.value = 0;
    }
  }

  function setCount(n) {
    count.value = n;
  }

  return { count, refresh, setCount };
});
