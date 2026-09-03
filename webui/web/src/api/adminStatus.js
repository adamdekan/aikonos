// Separated from store/user.js to avoid circular import (client.js → user store).
import { get } from "./client.js";
import { useUserStore } from "../store/user.js";

export async function refreshAdminStatus() {
  const store = useUserStore();
  try {
    const result = await get("/admin/assignments");
    store.isAdmin            = !result.forbidden;
    store.adminStatusUnknown = false;
  } catch (err) {
    // Transport failure (network error, 5xx, timeout) — not a definitive deny.
    // Preserve the prior isAdmin value so a confirmed admin is not silently
    // downgraded on a transient outage. Signal uncertainty via adminStatusUnknown.
    console.warn("refreshAdminStatus: could not determine admin status:", err);
    store.adminStatusUnknown = true;
  }
}
