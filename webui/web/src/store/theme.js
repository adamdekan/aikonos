import { defineStore } from "pinia";
import { ref } from "vue";

const THEME_KEY = "aikonos.theme";

function resolveInitialMode() {
  try {
    const stored = typeof localStorage !== "undefined"
      ? localStorage.getItem(THEME_KEY)
      : null;
    if (stored === "dark" || stored === "light" || stored === "system") return stored;
  } catch (e) {
    console.warn("[theme] localStorage read failed", e);
  }
  try {
    if (
      typeof window !== "undefined" &&
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-color-scheme: light)").matches
    ) {
      return "light";
    }
  } catch (e) {
    console.warn("[theme] matchMedia unavailable", e);
  }
  return "dark";
}

function systemPrefersLight() {
  try {
    if (
      typeof window !== "undefined" &&
      typeof window.matchMedia === "function"
    ) {
      return window.matchMedia("(prefers-color-scheme: light)").matches;
    }
  } catch (e) {
    console.warn("[theme] matchMedia unavailable", e);
  }
  return false;
}

export const useThemeStore = defineStore("theme", () => {
  const mode = ref(resolveInitialMode());
  let mediaQuery = null;
  let mediaListener = null;

  function appliedTheme() {
    if (mode.value === "system") return systemPrefersLight() ? "light" : "dark";
    return mode.value;
  }

  function apply() {
    if (typeof document !== "undefined") {
      document.documentElement.setAttribute("data-theme", appliedTheme());
    }
  }

  function detachMediaListener() {
    try {
      if (mediaQuery && mediaListener) {
        mediaQuery.removeEventListener("change", mediaListener);
      }
    } catch (e) {
      console.warn("[theme] matchMedia listener removal failed", e);
    }
    mediaQuery = null;
    mediaListener = null;
  }

  function attachMediaListener() {
    try {
      if (
        typeof window !== "undefined" &&
        typeof window.matchMedia === "function"
      ) {
        mediaQuery = window.matchMedia("(prefers-color-scheme: light)");
        mediaListener = () => apply();
        mediaQuery.addEventListener("change", mediaListener);
      }
    } catch (e) {
      console.warn("[theme] matchMedia listener attach failed", e);
    }
  }

  function setMode(next) {
    mode.value = next;
    detachMediaListener();
    if (mode.value === "system") {
      attachMediaListener();
    }
    apply();
    try {
      if (typeof localStorage !== "undefined") {
        localStorage.setItem(THEME_KEY, mode.value);
      }
    } catch (e) {
      console.warn("[theme] localStorage write failed", e);
    }
  }

  function toggle() {
    setMode(appliedTheme() === "dark" ? "light" : "dark");
  }

  // Apply immediately on store creation so the attribute reflects mode.
  if (mode.value === "system") {
    attachMediaListener();
  }
  apply();

  return { mode, apply, setMode, toggle };
});
