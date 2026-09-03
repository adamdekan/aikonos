import { createRouter, createWebHistory } from "vue-router";
import Home from "./views/Home.vue";
const Chat = () => import("./views/Chat.vue");
import Files from "./views/Files.vue";
import Connections from "./views/Connections.vue";
import ConnectCallback from "./views/ConnectCallback.vue";
import AuthCallback from "./views/AuthCallback.vue";
import Schedules from "./views/Schedules.vue";
import Inbox from "./views/Inbox.vue";
import Workflows from "./views/Workflows.vue";
import PersonalSkills from "./views/Skills.vue";
import AccessControl from "./views/admin/AccessControl.vue";
import Mcp from "./views/admin/Mcp.vue";
import Runs from "./views/admin/Runs.vue";
import Settings from "./views/admin/Settings.vue";
import PolicyTabs from "./views/admin/PolicyTabs.vue";
import Agents from "./views/admin/Agents.vue";
import Providers from "./views/admin/Providers.vue";
import Provisioning from "./views/admin/Provisioning.vue";
import Skills from "./views/admin/Skills.vue";
import SkillBundles from "./views/admin/SkillBundles.vue";
import { getUser, login } from "./auth/oidc.js";
import { useUserStore } from "./store/user.js";

export const routes = [
  // Public — no auth required; handles the Keycloak redirect.
  { path: "/auth/callback", component: AuthCallback, meta: { public: true } },

  { path: "/", component: Home },
  { path: "/chat", component: Chat },
  { path: "/files", component: Files },
  { path: "/connections", component: Connections },
  { path: "/connect/callback", component: ConnectCallback, meta: { public: true } },
  { path: "/schedules", component: Schedules },
  { path: "/workflows", component: Workflows },
  { path: "/skills", component: PersonalSkills },
  { path: "/inbox", component: Inbox },
  { path: "/admin/roles", component: AccessControl },
  { path: "/admin/mcp", component: Mcp },
  { path: "/admin/runs", component: Runs },
  { path: "/admin/policy", component: PolicyTabs },
  { path: "/admin/audit", redirect: "/admin/policy?tab=audit" },
  { path: "/admin/audit/history", redirect: "/admin/policy?tab=history" },
  { path: "/admin/decisions", redirect: "/admin/policy?tab=decisions" },
  { path: "/admin/policy/simulate", redirect: "/admin/policy?tab=simulator" },
  { path: "/admin/alerts", redirect: "/admin/policy?tab=alerts" },
  { path: "/admin/settings", component: Settings },
  { path: "/admin/agents", component: Agents },
  { path: "/admin/providers", component: Providers },
  { path: "/admin/provisioning", component: Provisioning },
  { path: "/admin/skills",        component: Skills },
  { path: "/admin/skill-bundles", component: SkillBundles },
  // Org-governance pages now live as categories inside Settings. Keep the old
  // top-level paths as redirects so existing links resolve to the right tab.
  { path: "/admin/org-instructions", redirect: "/admin/settings?tab=org-instructions" },
  { path: "/admin/automation", redirect: "/admin/settings?tab=automation" },
  { path: "/admin/workflow-governance", redirect: "/admin/settings?tab=workflow-governance" },
  { path: "/admin/capabilities", redirect: "/admin/settings?tab=capabilities" },
  { path: "/admin/connector-allowlist", redirect: "/admin/settings?tab=connector-allowlist" },
  { path: "/admin/m365", redirect: "/admin/settings?tab=m365" },
  { path: "/admin/network", redirect: "/admin/settings?tab=network" },
  { path: "/admin/rate-limits", redirect: "/admin/settings?tab=rate-limits" },
  { path: "/admin/spend-caps", redirect: "/admin/settings?tab=spend-caps" },
  // Members is the first (default) tab inside Access Control.
  { path: "/admin/members", redirect: "/admin/roles" },
  // Catch-all: any path not declared above (typos, stale links, probing)
  // lands on /chat instead of a blank router-view. Must stay last.
  { path: "/:pathMatch(.*)*", redirect: "/chat" },
];

const router = createRouter({
  history: createWebHistory(),
  routes,
});

// Auth guard: routes without meta.public require an active OIDC session.
// If none, redirect to Keycloak (signinRedirect navigates away).
// Exported for unit tests that need to install it on a fresh router instance.
export async function authGuard(to) {
  if (to.meta.public) return true;

  const oidcUser = await getUser();
  if (oidcUser && !oidcUser.expired) {
    // Populate the store on every navigation in case of a page reload where the
    // store is empty but oidc-client-ts restored the session from sessionStorage.
    const userStore = useUserStore();
    if (!userStore.user && oidcUser.profile) {
      userStore.setFromProfile({
        // Entra personal accounts emit no `email` claim; fall back to
        // preferred_username (the email) / name before the opaque sub.
        email: oidcUser.profile.email ?? oidcUser.profile.preferred_username ?? oidcUser.profile.name ?? oidcUser.profile.sub,
        sub:   oidcUser.profile.sub,
        name:  oidcUser.profile.name,
      });
    }
    return true;
  }

  // No valid session — kick off the login redirect.
  await login();
  // login() triggers a full-page redirect; returning false prevents vue-router
  // from also trying to render the target route in the same tick.
  return false;
}

router.beforeEach(authGuard);

export default router;
