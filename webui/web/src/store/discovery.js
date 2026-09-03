import { defineStore } from "pinia";
import { ref } from "vue";
import { listUserSkillBundles } from "../api/admin.js";
import { listDelegatableUsers } from "../api/delegation.js";
import { listFiles } from "../api/files.js";
import { listSkills } from "../api/skills.js";

// Discovery-only datasets for chat's palette/mention sources: granted skill
// bundles, delegatable users/groups, workspace files. All failures degrade to
// an empty result — the UI keeps working rather than blocking chat on a
// non-critical fetch — but are also recorded in `errors` so surfaces (e.g. a
// chat-mount banner) can tell the user discovery is degraded.
export const useDiscoveryStore = defineStore("discovery", () => {
  // Granted skill bundles for the /command palette (discovery only).
  const grantedBundles    = ref([]);
  // Delegatable users for @ mentions — discovery only, authz re-checked on send.
  const delegatableUsers  = ref([]);
  // Delegatable groups for @ mentions — discovery only.
  const delegatableGroups = ref([]);
  // Workspace files for # mentions — mirrors grantedBundles pattern.
  const mentionFiles      = ref([]);

  const loading = ref(false);
  const loaded  = ref(false);

  // null when healthy, error message string when the last fetch failed.
  const errors = ref({
    grantedBundles: null,
    delegatableUsers: null,
    delegatableGroups: null,
    mentionFiles: null,
  });

  async function loadGrantedBundles() {
    let admin = [];
    try {
      const r = await listUserSkillBundles();
      admin = r.bundles ?? [];
      errors.value.grantedBundles = null;
    } catch (err) {
      console.warn("loadGrantedBundles failed — palette will be empty", err);
      grantedBundles.value = [];
      errors.value.grantedBundles = err?.message ?? "Failed to load skill bundles";
      return;
    }

    // Personal skills are a separate discovery
    // source unioned into the same palette. The gateway only resolves a
    // "/command" under the qualified "personal:<name>" name (its
    // PERSONAL_SKILL_PREFIX, agent-gateway/src/pi/load-skill.js) — a cross-tier
    // wire contract — so each entry carries both the bare display `name` and
    // the qualified `skillName` the composer must emit on select.
    let personal = [];
    try {
      const r = await listSkills();
      const skills = r.forbidden ? [] : (r.skills ?? []);
      personal = skills.map((s) => ({
        id: "personal:" + s.name,
        name: s.name,
        description: s.description,
        personal: true,
        skillName: "personal:" + s.name,
      }));
    } catch (err) {
      // Fail open: a broken personal-skills fetch must not empty the admin
      // palette, and this is not the admin-only `errors.grantedBundles` field.
      console.warn("loadGrantedBundles: personal skills fetch failed", err);
    }

    grantedBundles.value = [...admin, ...personal];
  }

  async function loadDelegatableUsers() {
    try {
      const r = await listDelegatableUsers();
      // forbidden = not enrolled in any delegatable group; treat as empty.
      delegatableUsers.value  = r.forbidden ? [] : (r.users  ?? []);
      delegatableGroups.value = r.forbidden ? [] : (r.groups ?? []);
      errors.value.delegatableUsers  = null;
      errors.value.delegatableGroups = null;
    } catch (err) {
      console.warn("loadDelegatableUsers failed — @ mentions will be empty", err);
      delegatableUsers.value  = [];
      delegatableGroups.value = [];
      const message = err?.message ?? "Failed to load delegatable users";
      errors.value.delegatableUsers  = message;
      errors.value.delegatableGroups = message;
    }
  }

  async function loadFiles() {
    try {
      const r = await listFiles();
      // forbidden = workspace access denied; treat as empty.
      mentionFiles.value = r.forbidden ? [] : (r.files ?? []);
      errors.value.mentionFiles = null;
    } catch (err) {
      console.warn("loadFiles failed — # mentions will be empty", err);
      mentionFiles.value = [];
      errors.value.mentionFiles = err?.message ?? "Failed to load files";
    }
  }

  async function fetchAll() {
    loading.value = true;
    try {
      await Promise.all([loadGrantedBundles(), loadDelegatableUsers(), loadFiles()]);
    } finally {
      loading.value = false;
    }
  }

  async function load() {
    if (loaded.value) return;
    await fetchAll();
    loaded.value = true;
  }

  async function refresh() {
    await fetchAll();
    loaded.value = true;
  }

  return {
    grantedBundles,
    delegatableUsers,
    delegatableGroups,
    mentionFiles,
    loading,
    loaded,
    errors,
    load,
    refresh,
  };
});
