import { defineStore } from "pinia";
import { ref, computed } from "vue";
import { useChatStore } from "./chat.js";

export const useUserStore = defineStore("user", () => {
  // Populated from the OIDC profile after login / callback.
  const user               = ref("");
  const sub                = ref("");
  const name               = ref("");
  const isAdmin            = ref(false);
  // True when the last admin-status probe threw (network/5xx/timeout) — not a
  // definitive deny, so the prior isAdmin value is preserved. Cleared on a
  // successful 200 or 403 resolution.
  const adminStatusUnknown = ref(false);

  /** Set the principal from an OIDC profile object {email, sub, name}. */
  function setFromProfile(profile) {
    user.value = profile.email ?? profile.sub ?? "";
    sub.value  = profile.sub   ?? "";
    name.value = profile.name  ?? "";
    // localStorage is intentionally NOT written — oidc-client-ts restores the
    // session via silent sign-in on the next page load.
  }

  /** Clear the principal (called on logout or when the OIDC session is gone). */
  function clear() {
    user.value               = "";
    sub.value                = "";
    name.value               = "";
    isAdmin.value            = false;
    adminStatusUnknown.value = false;
    // Wipe the chat buffer so a subsequent user on a shared browser cannot read
    // the prior user's draft. chat.js does NOT import user.js — no cycle.
    useChatStore().clearAll();
  }

  // Prefer the `name` claim ("Adam Dekan"); else derive from the email local part.
  const displayName = computed(() => {
    if (name.value) return name.value;
    if (!user.value) return "";
    const local = user.value.split("@")[0];
    return local.charAt(0).toUpperCase() + local.slice(1);
  });

  return { user, sub, name, setFromProfile, clear, isAdmin, adminStatusUnknown, displayName };
});
