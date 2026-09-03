import { ref, computed } from "vue";
import {
  listAssignments,
  assignRole,
  revokeRole,
  listMcpConnections,
  listAgents,
  listSkills,
  listSkillBundles,
} from "../../api/admin.js";
import { subjectRef } from "../../sections.js";
import { filterTuples, filterPrincipals } from "../../utils/svc-filter.js";
import {
  deriveGroups,
  tenantRef,
} from "./access-derive.js";
import { useToast } from "../../components/ui/useToast.js";

// Provide/inject symbol — imported by shell and (future) tab components.
export const ACCESS_CTX = Symbol("AccessControl");

// Shell passes its tab-local selection refs so doAssign/doRevoke can
// re-resolve them after load() without owning the refs themselves.
// This keeps the doAssign(user, relation, object) call signature unchanged
// at every call site in the template.
export function useAccessControl({ selectedUser, selectedGroup, selectedAgent } = {}) {
  const { push: toast } = useToast();

  // ── raw data refs ──────────────────────────────────────────────────────────
  const rawTuples      = ref([]);
  const rawPrincipals  = ref([]);
  const fgaEnabled     = ref(true);
  const warnings       = ref([]);
  const forbidden      = ref(false);
  const loading        = ref(false);
  const error          = ref("");
  const agents         = ref([]);
  const mcpConnections = ref([]);
  const skills         = ref([]);
  const skillBundles   = ref([]);

  // ── shared computed ────────────────────────────────────────────────────────
  const tuples     = computed(() => filterTuples(rawTuples.value));
  const principals = computed(() => filterPrincipals(rawPrincipals.value));

  const derivedGroups = computed(() => deriveGroups(tuples.value));
  const tRef          = computed(() => tenantRef(tuples.value));

  const agentById = computed(() => {
    const m = {};
    for (const a of agents.value) m[a.id] = a;
    return m;
  });

  const displayName = computed(() => {
    const m = {};
    for (const p of principals.value) {
      if (p.displayName) m[p.id] = p.displayName;
      else if (p.email)  m[p.id] = p.email;
    }
    return m;
  });

  const bundleById = computed(() => {
    const m = {};
    for (const b of skillBundles.value) m[b.id] = b;
    return m;
  });

  const userPrincipals = computed(() =>
    principals.value.filter((p) => p.kind === "user" || p.id.startsWith("user:")),
  );

  // ── shared helpers ─────────────────────────────────────────────────────────
  function userName(userRef) {
    return displayName.value[userRef] ?? userRef.replace(/^user:/, "");
  }

  function agentName(agentRef) {
    const id = agentRef.replace(/^agent:/, "");
    return agentById.value[id]?.name ?? id;
  }

  function bundleName(id) {
    return bundleById.value[id]?.name ?? id;
  }

  function skillScope(toolId) {
    return skills.value.find((s) => s.toolId === toolId)?.scope ?? "";
  }

  function isRegisteredSkill(toolId) {
    return skills.value.some((s) => s.toolId === toolId);
  }

  // ── load ───────────────────────────────────────────────────────────────────
  async function loadSideData() {
    try {
      const resp = await listMcpConnections();
      mcpConnections.value = resp?.connections ?? [];
    } catch (e) {
      warnings.value.push(`MCP connections unavailable: ${e.message}`);
    }
    try {
      const resp = await listAgents();
      if (!resp?.forbidden) agents.value = resp?.agents ?? [];
    } catch (e) {
      warnings.value.push(`agents list unavailable: ${e.message}`);
    }
    try {
      const resp = await listSkills();
      if (!resp?.forbidden) skills.value = resp?.skills ?? [];
    } catch (e) {
      warnings.value.push(`tools list unavailable: ${e.message}`);
    }
    try {
      const resp = await listSkillBundles();
      if (!resp?.forbidden) skillBundles.value = resp?.bundles ?? [];
    } catch (e) {
      warnings.value.push(`skill bundles list unavailable: ${e.message}`);
    }
  }

  async function load() {
    loading.value   = true;
    error.value     = "";
    forbidden.value = false;
    try {
      const [data] = await Promise.all([listAssignments(), loadSideData()]);
      if (data.forbidden) { forbidden.value = true; return; }
      rawTuples.value     = data.tuples     ?? [];
      rawPrincipals.value = data.principals ?? [];
      fgaEnabled.value    = data.fgaEnabled ?? true;
      warnings.value      = data.warnings   ?? [];
    } catch (e) {
      error.value = e.message;
    } finally {
      loading.value = false;
    }
  }

  // ── mutation core ──────────────────────────────────────────────────────────
  const mutating = ref(false);
  const mutError  = ref("");

  // Re-resolve tab-local selections against freshly loaded data.
  // selectedUser/Group/Agent refs are provided by the shell at instantiation;
  // if not provided (e.g. isolated test of the composable), reselect is a no-op.
  function _reselect() {
    if (selectedUser?.value) {
      const refreshed = principals.value.find((p) => p.id === selectedUser.value.id);
      if (refreshed) selectedUser.value = refreshed;
    }
    if (selectedGroup?.value) {
      const refreshed = derivedGroups.value.find((g) => g.name === selectedGroup.value.name);
      if (refreshed) selectedGroup.value = refreshed;
    }
    if (selectedAgent?.value) {
      const refreshed = agents.value.find((a) => a.id === selectedAgent.value.id);
      if (refreshed) selectedAgent.value = refreshed;
    }
  }

  async function doAssign(user, relation, object) {
    mutating.value = true;
    mutError.value = "";
    try {
      await assignRole({ user: subjectRef(user), relation, object });
      toast("ok", "Assignment saved.");
      await load();
      _reselect();
    } catch (e) {
      mutError.value = e.message;
    } finally {
      mutating.value = false;
    }
  }

  async function doRevoke(user, relation, object) {
    mutating.value = true;
    mutError.value = "";
    try {
      await revokeRole({ user: subjectRef(user), relation, object });
      toast("ok", "Assignment revoked.");
      await load();
      _reselect();
    } catch (e) {
      mutError.value = e.message;
    } finally {
      mutating.value = false;
    }
  }

  return {
    // raw refs
    rawTuples, rawPrincipals, fgaEnabled, warnings, forbidden,
    loading, error, agents, mcpConnections, skills, skillBundles,
    // shared computed
    tuples, principals, derivedGroups, tRef, agentById,
    displayName, bundleById, userPrincipals,
    // shared helpers
    userName, agentName, bundleName, skillScope, isRegisteredSkill,
    // mutation core
    load, loadSideData, doAssign, doRevoke, mutating, mutError,
    // selection refs — exposed so tab components injecting ACCESS_CTX can share
    // the same refs that _reselect() closes over (avoids double-ref drift)
    selectedUser, selectedGroup, selectedAgent,
  };
}
