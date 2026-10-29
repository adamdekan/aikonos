<script setup>
// Handles the OAuth2 auth-code redirect from Keycloak.
// Keycloak redirects here with ?code=...&state=... after login.
// We complete the exchange, populate the user store, then navigate to the app.
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { handleCallback, safeReturnTo } from "../auth/oidc.js";
import { useUserStore } from "../store/user.js";

const router        = useRouter();
const userStore     = useUserStore();
const callbackError = ref(null);

onMounted(async () => {
  try {
    const oidcUser = await handleCallback();
    // A step-up popup (AG-13) returns no user: the opener finishes and the
    // popup closes. Do not navigate it.
    if (!oidcUser) return;
    if (oidcUser?.profile) {
      userStore.setFromProfile({
        // Entra personal accounts emit no `email` claim; fall back to
        // preferred_username (the email) / name before the opaque sub.
        email: oidcUser.profile.email ?? oidcUser.profile.preferred_username ?? oidcUser.profile.name ?? oidcUser.profile.sub,
        sub:   oidcUser.profile.sub,
        name:  oidcUser.profile.name,
      });
    }
    // Land on the page that sent the user to sign in (deep links survive login).
    await router.replace(safeReturnTo(oidcUser.state?.returnTo));
  } catch (err) {
    // State mismatch, replay, or network error — do NOT redirect to "/" because
    // the auth guard would immediately bounce back to Keycloak, creating a loop.
    // Surface the error in the component so the user can retry manually.
    console.error("auth callback error:", err);
    callbackError.value = err?.message ?? String(err);
  }
});
</script>

<template>
  <div class="auth-callback">
    <template v-if="callbackError">
      <p class="auth-error">Sign-in failed: {{ callbackError }}</p>
      <p><a href="/">Return to home</a> to try again.</p>
    </template>
    <p v-else>Completing sign-in…</p>
  </div>
</template>

<style scoped>
.auth-callback {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 100vh;
  color: var(--text-muted);
}
.auth-error {
  color: var(--danger);
}
</style>
