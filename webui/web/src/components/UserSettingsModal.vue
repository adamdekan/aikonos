<script setup>
// Sidebar-gear settings modal: Account (read-only identity + sign out),
// Appearance (theme mode), Chat (persistence prefs — all frontend-only Pinia +
// localStorage,  CP4), Memory.
import { ref } from "vue";
import Modal from "./ui/Modal.vue";
import FormField from "./ui/FormField.vue";
import ToggleSwitch from "./ui/ToggleSwitch.vue";
import MemorySettings from "./MemorySettings.vue";
import FormSection from "../views/admin/FormSection.vue";
import { useUserStore } from "../store/user.js";
import { useThemeStore } from "../store/theme.js";
import { usePrefsStore, MAX_CHAT_INSTRUCTIONS_CHARS } from "../store/prefs.js";
import { logout } from "../auth/oidc.js";

defineProps({
  open: { type: Boolean, required: true },
});

const emit = defineEmits(["close"]);

const CATEGORIES = [
  { id: "account", label: "Account" },
  { id: "appearance", label: "Appearance" },
  { id: "chat", label: "Chat" },
  { id: "memory", label: "Memory" },
];

const activeCategory = ref("account");

const userStore = useUserStore();
const themeStore = useThemeStore();
const prefsStore = usePrefsStore();

async function handleSignOut() {
  userStore.clear();
  await logout();
}
</script>

<template>
  <Modal :open="open" size="wide" title="Settings" @close="emit('close')">
    <div class="settings-body">
      <nav class="settings-rail" aria-label="Settings categories">
        <button
          v-for="cat in CATEGORIES"
          :key="cat.id"
          type="button"
          class="rail-item"
          :class="{ active: activeCategory === cat.id }"
          :aria-current="activeCategory === cat.id ? 'true' : undefined"
          :data-testid="`settings-cat-${cat.id}`"
          @click="activeCategory = cat.id"
        >
          {{ cat.label }}
        </button>
      </nav>

      <div class="settings-pane">
        <FormSection v-if="activeCategory === 'account'" title="Account">
          <FormField label="Display name">
            <span class="readonly-value" data-testid="account-display-name">{{ userStore.displayName || userStore.user }}</span>
          </FormField>
          <FormField label="Email">
            <span class="readonly-value" data-testid="account-email">{{ userStore.user }}</span>
          </FormField>
          <FormField label="Debug broker">
            <label class="checkbox-item">
              <ToggleSwitch
                data-testid="debug-broker-toggle"
                :model-value="prefsStore.debugBroker"
                aria-label="Show broker governance tool chips in chat"
                @update:model-value="prefsStore.setDebugBroker($event)"
              />
              Show broker governance details in chat
            </label>
          </FormField>
        </FormSection>

        <FormSection v-else-if="activeCategory === 'appearance'" title="Appearance">
          <FormField label="Theme">
            <div class="radio-group">
              <label class="radio-item">
                <input
                  type="radio"
                  name="theme-mode"
                  value="dark"
                  data-testid="theme-radio-dark"
                  :checked="themeStore.mode === 'dark'"
                  @change="themeStore.setMode('dark')"
                />
                Dark
              </label>
              <label class="radio-item">
                <input
                  type="radio"
                  name="theme-mode"
                  value="light"
                  data-testid="theme-radio-light"
                  :checked="themeStore.mode === 'light'"
                  @change="themeStore.setMode('light')"
                />
                Light
              </label>
              <label class="radio-item">
                <input
                  type="radio"
                  name="theme-mode"
                  value="system"
                  data-testid="theme-radio-system"
                  :checked="themeStore.mode === 'system'"
                  @change="themeStore.setMode('system')"
                />
                System
              </label>
            </div>
          </FormField>
        </FormSection>

        <FormSection v-else-if="activeCategory === 'chat'" title="Chat">
          <FormField label="History">
            <label class="checkbox-item">
              <ToggleSwitch
                data-testid="chat-persist-checkbox"
                :model-value="prefsStore.chatPersistEnabled"
                aria-label="Persist chat history on this device"
                @update:model-value="prefsStore.setChatPersistEnabled($event)"
              />
              Persist chat history on this device
            </label>
          </FormField>
          <FormField label="Turns to keep">
            <input
              type="number"
              min="5"
              max="200"
              data-testid="chat-persist-turns"
              :value="prefsStore.chatPersistTurns"
              :disabled="!prefsStore.chatPersistEnabled"
              @change="prefsStore.setChatPersistTurns($event.target.value)"
            />
          </FormField>
          <FormField label="Agent instructions">
            <textarea
              class="instructions-input"
              rows="6"
              :maxlength="MAX_CHAT_INSTRUCTIONS_CHARS"
              placeholder="How should your agents behave? e.g. answer in German, keep replies short, always cite file paths…"
              data-testid="chat-instructions"
              :value="prefsStore.chatInstructions"
              @input="prefsStore.setChatInstructions($event.target.value)"
            ></textarea>
            <div class="char-counter" data-testid="chat-instructions-counter">
              {{ prefsStore.chatInstructions.length }} / {{ MAX_CHAT_INSTRUCTIONS_CHARS }}
            </div>
            <div class="field-hint">
              Applied to every new chat on this device. Running chats keep the
              instructions they started with.
            </div>
          </FormField>
        </FormSection>

        <!-- Explicit, not a v-else: a 5th category must render its own pane, never
             fall through to Memory (which then loads bundles nobody asked for). -->
        <FormSection v-else-if="activeCategory === 'memory'" title="Memory">
          <MemorySettings />
        </FormSection>
      </div>
    </div>

    <template #footer>
      <button
        class="btn-danger"
        data-testid="settings-signout-btn"
        @click="handleSignOut"
      >
        Sign out
      </button>
      <button
        class="btn-ghost"
        data-testid="settings-close-btn"
        @click="emit('close')"
      >
        Close
      </button>
    </template>
  </Modal>
</template>

<style scoped>
.settings-body {
  display: flex;
  gap: var(--space-6);
  min-height: 280px;
}

.settings-rail {
  display: flex;
  flex-direction: column;
  gap: 2px;
  width: 140px;
  flex-shrink: 0;
}

.rail-item {
  display: block;
  text-align: left;
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  border: none;
  background: none;
  color: var(--text-muted);
  font-size: 13px;
  cursor: pointer;
  transition: background 0.12s, color 0.12s;
}

.rail-item:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.rail-item.active {
  background: var(--bg-active);
  color: var(--text);
  font-weight: 500;
}

.settings-pane {
  flex: 1;
  min-width: 0;
}

.radio-group {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.radio-item,
.checkbox-item {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--text);
}

.readonly-value {
  font-size: 13px;
  color: var(--text);
}

.instructions-input {
  width: 100%;
  resize: vertical;
  min-height: 96px;
  padding: 8px 10px;
  background: var(--bg-input, var(--bg));
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 13px;
  line-height: 1.4;
}

.instructions-input:focus {
  outline: none;
  border-color: var(--accent);
}

.char-counter {
  margin-top: 4px;
  font-size: 11px;
  color: var(--text-muted);
  text-align: right;
}

.field-hint {
  margin-top: 2px;
  font-size: 11px;
  color: var(--text-muted);
}

.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-family: var(--font-sans);
  padding: 7px 13px;
  font-size: 13px;
  cursor: pointer;
  transition: background 0.15s;
}

.btn-danger:hover {
  background: var(--danger);
  color: var(--text-on-accent);
}

.btn-ghost {
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 7px 13px;
  font-size: 13px;
  cursor: pointer;
}

.btn-ghost:hover {
  background: var(--bg-hover);
}
</style>
