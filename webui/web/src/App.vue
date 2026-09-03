<script setup>
import { computed } from "vue";
import { useRoute } from "vue-router";
import Sidebar from "./components/Sidebar.vue";
import Toast from "./components/ui/Toast.vue";
import { useUserStore } from "./store/user";

const userStore = useUserStore();
const route = useRoute();

// Defense in depth: the app shell — the Sidebar (Sign out, New chat, nav) and
// every view — renders ONLY once an authenticated OIDC session has populated
// the user store. The router guard already redirects unauthenticated visitors
// to the IdP, but signinRedirect() is async; without this gate the shell would
// paint during the redirect window (or indefinitely if the redirect stalls).
// Public routes (the OIDC callback) render their bare view because they run
// mid-authentication, before the user store is populated.
const authed = computed(() => !!userStore.user);
const isPublic = computed(() => route.meta.public === true);
</script>

<template>
  <div v-if="authed" class="app-shell">
    <Sidebar />
    <main class="main-pane scroll-min" v-autoscroll>
      <router-view :key="userStore.user" />
    </main>
  </div>
  <router-view v-else-if="isPublic" />
  <div v-else class="auth-splash" data-testid="auth-splash">
    <p>Authenticating…</p>
  </div>
  <Toast />
</template>

<style>
@import "./styles/tokens.css";

.app-shell {
  display: flex;
  height: 100vh;
  overflow: hidden;
}

.main-pane {
  flex: 1;
  min-width: 0;
  height: 100vh;
  overflow-y: auto;
  /* Reserve the scrollbar gutter so switching between page-scroll views
     (Chat, Files) and inner-scroll views (Audit, Access Control) doesn't
     shift content horizontally as the scrollbar appears/disappears. */
  scrollbar-gutter: stable;
}

.auth-splash {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 100vh;
  color: var(--text-muted);
  font-size: 14px;
}
</style>
