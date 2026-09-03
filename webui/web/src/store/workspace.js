import { defineStore } from "pinia";
import { ref } from "vue";
import { getWorkspaceBackend, setWorkspaceBackend as putWorkspaceBackend } from "../api/workspace.js";
import { useDiscoveryStore } from "./discovery.js";

// Working-folder backend preference (local vs the tenant's OneDrive OBO
// connection,  CP6). Mirrors discovery.js's
// load-once/refresh shape. A load() failure degrades non-fatally — callers
// (the Composer control) treat "not loaded" or "errored" the same as
// "unavailable" and hide themselves, so an unconfigured/Keycloak-only tenant
// never shows a broken control.
export const useWorkspaceStore = defineStore("workspace", () => {
  const backend            = ref("local");
  const onedriveFolderPath = ref("");
  const onedriveAvailable  = ref(false);
  const onedriveStatus     = ref("");

  const loading = ref(false);
  const loaded  = ref(false);
  const error   = ref(null);

  function applyPref(pref) {
    backend.value            = pref?.backend ?? backend.value;
    onedriveFolderPath.value = pref?.onedriveFolderPath ?? onedriveFolderPath.value;
  }

  async function fetchBackend() {
    loading.value = true;
    try {
      const resp = await getWorkspaceBackend();
      // 403 surfaces as { forbidden: true } (api/client.js contract), not a
      // thrown error — treat it the same as any other "unavailable" outcome:
      // hide the control, record why, never throw (this is a load, not a mutation).
      if (resp.forbidden) {
        onedriveAvailable.value = false;
        onedriveStatus.value = "";
        error.value = resp.error ?? "Forbidden";
        return;
      }
      applyPref(resp.pref);
      onedriveAvailable.value = !!resp.onedriveAvailable;
      onedriveStatus.value = resp.onedriveStatus ?? "";
      error.value = null;
    } catch (err) {
      console.warn("[workspace] load failed — working-folder control will be hidden", err);
      error.value = err?.message ?? "Failed to load workspace backend";
    } finally {
      loading.value = false;
    }
  }

  async function load() {
    if (loaded.value) return;
    await fetchBackend();
    loaded.value = true;
  }

  async function refresh() {
    await fetchBackend();
    loaded.value = true;
  }

  // Throws on failure (unlike load/refresh) so the caller (Composer) can toast
  // the error without silently reverting the UI to a stale backend label —
  // state is only mutated after the PUT resolves, so a failure leaves the
  // last-known-good backend/path in place.
  async function setBackend({ backend: newBackend, onedriveFolderPath: newPath = "" } = {}) {
    const resp = await putWorkspaceBackend({ backend: newBackend, onedriveFolderPath: newPath });
    // Unlike fetchBackend's load path, a mutation must not silently no-op on
    // forbidden — the caller (Composer) needs to know the switch didn't take
    // effect so it can toast instead of showing a backend that never changed.
    if (resp.forbidden) {
      throw new Error(resp.error || "Forbidden");
    }
    applyPref(resp.pref);
    // Switching backend must repopulate the #-mention file palette from the
    // newly-active backend (spec invariant) — never stale-list the old one.
    await useDiscoveryStore().refresh();
  }

  return {
    backend,
    onedriveFolderPath,
    onedriveAvailable,
    onedriveStatus,
    loading,
    loaded,
    error,
    load,
    refresh,
    setBackend,
  };
});
