import { defineStore } from "pinia";
import { ref } from "vue";
import { listMyAgents } from "../api/agents.js";

export const useAgentsStore = defineStore("agents", () => {
  const assigned = ref([]);

  async function refresh() {
    try {
      const data = await listMyAgents();
      if (data.forbidden) { assigned.value = []; return; }
      assigned.value = data.agents ?? [];
    } catch {
      assigned.value = [];
    }
  }

  function setAssigned(list) {
    assigned.value = list;
  }

  return { assigned, refresh, setAssigned };
});
