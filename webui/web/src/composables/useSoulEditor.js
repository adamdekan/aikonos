import { ref, computed } from "vue";
import { getAgentSoul, setAgentSoul } from "../api/agents.js";
import { useToast } from "../components/ui/useToast.js";

// Per-agent personality ("soul") editor: fetch-on-agent-change, modal state,
// save flow. Caller (Chat.vue) owns calling loadSoul on mount and on agentId
// change — kept explicit rather than an internal watch so composable creation
// order doesn't dictate when the fetch fires.
export function useSoulEditor(agentId) {
  const { push: toast } = useToast();

  // null = not yet fetched / hidden (forbidden). "" = fetched, empty soul.
  const soulEditable    = ref(null);   // null means editor is hidden
  const showSoulModal   = ref(false);
  const soulDraft       = ref("");
  const soulDraftBytes  = computed(() => new Blob([soulDraft.value]).size);
  const soulError       = ref("");
  const soulSaving      = ref(false);

  async function loadSoul(id) {
    if (!id) { soulEditable.value = null; return; }
    try {
      const r = await getAgentSoul(id);
      if (r.forbidden) { soulEditable.value = null; return; }
      soulEditable.value = r.soul ?? "";
    } catch {
      soulEditable.value = null;
    }
  }

  function openSoulModal() {
    soulDraft.value  = soulEditable.value ?? "";
    soulError.value  = "";
    showSoulModal.value = true;
  }

  async function saveSoul() {
    soulError.value  = "";
    soulSaving.value = true;
    try {
      const r = await setAgentSoul(agentId.value, soulDraft.value);
      soulEditable.value = r.soul ?? soulDraft.value;
      showSoulModal.value = false;
      toast("ok", "Personality saved.");
    } catch (e) {
      soulError.value = e.message;
    } finally {
      soulSaving.value = false;
    }
  }

  return {
    soulEditable,
    showSoulModal,
    soulDraft,
    soulDraftBytes,
    soulError,
    soulSaving,
    loadSoul,
    openSoulModal,
    saveSoul,
  };
}
