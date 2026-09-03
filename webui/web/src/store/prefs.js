import { defineStore } from "pinia";
import { ref } from "vue";

const PREFS_KEY = "aikonos.prefs";
const DEFAULT_CHAT_PERSIST_ENABLED = true;
const DEFAULT_DEBUG_BROKER = false;
const DEFAULT_CHAT_PERSIST_TURNS = 30;
const MIN_CHAT_PERSIST_TURNS = 5;
const MAX_CHAT_PERSIST_TURNS = 200;
export const MAX_CHAT_INSTRUCTIONS_CHARS = 2000;

function clampInstructions(v) {
  if (typeof v !== "string") return "";
  return v.slice(0, MAX_CHAT_INSTRUCTIONS_CHARS);
}

function clampTurns(n) {
  const num = Number(n);
  if (!Number.isFinite(num)) return DEFAULT_CHAT_PERSIST_TURNS;
  return Math.min(MAX_CHAT_PERSIST_TURNS, Math.max(MIN_CHAT_PERSIST_TURNS, Math.round(num)));
}

function loadStored() {
  try {
    const raw = typeof localStorage !== "undefined" ? localStorage.getItem(PREFS_KEY) : null;
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return {};
    return parsed;
  } catch (e) {
    console.warn("[prefs] localStorage read failed", e);
    return {};
  }
}

function writeStored(value) {
  try {
    if (typeof localStorage !== "undefined") {
      localStorage.setItem(PREFS_KEY, JSON.stringify(value));
    }
  } catch (e) {
    console.warn("[prefs] localStorage write failed", e);
  }
}

export const usePrefsStore = defineStore("prefs", () => {
  const stored = loadStored();

  const chatPersistEnabled = ref(
    typeof stored.chatPersistEnabled === "boolean"
      ? stored.chatPersistEnabled
      : DEFAULT_CHAT_PERSIST_ENABLED
  );
  const chatPersistTurns = ref(
    stored.chatPersistTurns !== undefined
      ? clampTurns(stored.chatPersistTurns)
      : DEFAULT_CHAT_PERSIST_TURNS
  );
  const chatInstructions = ref(clampInstructions(stored.chatInstructions));
  const debugBroker = ref(
    typeof stored.debugBroker === "boolean" ? stored.debugBroker : DEFAULT_DEBUG_BROKER
  );

  function persist() {
    writeStored({
      chatPersistEnabled: chatPersistEnabled.value,
      chatPersistTurns: chatPersistTurns.value,
      chatInstructions: chatInstructions.value,
      debugBroker: debugBroker.value,
    });
  }

  function setChatPersistEnabled(v) {
    chatPersistEnabled.value = Boolean(v);
    persist();
  }

  function setChatPersistTurns(n) {
    chatPersistTurns.value = clampTurns(n);
    persist();
  }

  function setChatInstructions(v) {
    chatInstructions.value = clampInstructions(v);
    persist();
  }

  function setDebugBroker(v) {
    debugBroker.value = Boolean(v);
    persist();
  }

  return {
    chatPersistEnabled,
    chatPersistTurns,
    chatInstructions,
    debugBroker,
    setChatPersistEnabled,
    setChatPersistTurns,
    setChatInstructions,
    setDebugBroker,
  };
});
