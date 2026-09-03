<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from "vue";
import { storeToRefs } from "pinia";
import Icon from "./Icon.vue";
import WorkspaceFolderPicker from "./WorkspaceFolderPicker.vue";
import { uploadFile, createDir } from "../api/files.js";
import { fileToBase64 } from "../lib/base64.js";
import { useToast } from "./ui/useToast.js";
import { useDiscoveryStore } from "../store/discovery.js";
import { useWorkspaceStore } from "../store/workspace.js";

const { push: toast } = useToast();

// Granted skill bundles / delegatable users & groups / mention files are all
// discovery-only, sourced from the shared discovery store — authz is
// re-checked server-side on submit/delegation, never trusted from these lists.
const { grantedBundles, delegatableUsers, delegatableGroups, mentionFiles } =
  storeToRefs(useDiscoveryStore());

// ── working-folder control ──────────
const workspaceStore = useWorkspaceStore();
const {
  backend: workspaceBackend,
  onedriveFolderPath,
  onedriveAvailable,
  loaded: workspaceLoaded,
  error: workspaceError,
} = storeToRefs(workspaceStore);

onMounted(() => workspaceStore.load());

// Hidden unless the store has loaded healthily AND OneDrive is actually
// available — an unconfigured tenant (or dev/Keycloak, which never resolves
// OBO) must never show a broken control.
const workspaceControlVisible = computed(
  () => workspaceLoaded.value && !workspaceError.value && onedriveAvailable.value,
);

const workspaceLabel = computed(() =>
  workspaceBackend.value === "onedrive"
    ? `Working folder: OneDrive · /${onedriveFolderPath.value} ▾`
    : "Working folder: Local ▾",
);

const workspaceMenuOpen = ref(false);
const workspaceControlRef = ref(null);
const folderPickerVisible = ref(false);

// Outside-click dismissal for the workspace menu — mirrors SessionItem.vue's
// idiom: a document-level capturing click listener, close when the click
// target lands outside the containing element.
function onWorkspaceDocClick(e) {
  if (
    workspaceMenuOpen.value &&
    workspaceControlRef.value &&
    !workspaceControlRef.value.contains(e.target)
  ) {
    workspaceMenuOpen.value = false;
  }
}
onMounted(() => document.addEventListener("click", onWorkspaceDocClick, true));
onUnmounted(() => document.removeEventListener("click", onWorkspaceDocClick, true));

async function chooseLocal() {
  workspaceMenuOpen.value = false;
  try {
    await workspaceStore.setBackend({ backend: "local" });
  } catch (err) {
    toast("error", err?.message || "Failed to switch workspace");
  }
}

function openFolderPicker() {
  workspaceMenuOpen.value = false;
  folderPickerVisible.value = true;
}

async function onFolderSelected(path) {
  folderPickerVisible.value = false;
  try {
    await workspaceStore.setBackend({ backend: "onedrive", onedriveFolderPath: path });
  } catch (err) {
    toast("error", err?.message || "Failed to switch workspace");
  }
}

const props = defineProps({
  modelValue:       { type: String,  default: "" },
  disabled:         { type: Boolean, default: false },
  running:          { type: Boolean, default: false },
  placeholder:      { type: String,  default: "Message…" },
});

const emit = defineEmits(["update:modelValue", "submit", "stop"]);

// ── /skill palette ──────────────────────────────────────────────────────────

// Palette is open when the input starts with "/" and there is at least one match.
const slashPrefix = computed(() => {
  const v = props.modelValue;
  if (!v.startsWith("/")) return null;
  return v.slice(1).toLowerCase();
});

const paletteItems = computed(() => {
  if (slashPrefix.value === null) return [];
  const prefix = slashPrefix.value;
  return grantedBundles.value.filter(
    (b) => b.name.toLowerCase().startsWith(prefix),
  );
});

const paletteOpen = computed(() => paletteItems.value.length > 0);

// Keyboard highlight for the /skill palette (mirrors the mention popover's
// highlightedIndex). Reset whenever the match list changes.
const paletteIndex = ref(0);
watch(paletteItems, () => {
  paletteIndex.value = 0;
});

function selectBundle(bundle) {
  // Clear the draft; close palette; emit structured {skillName} for server-side
  // resolution. Personal skills carry a qualified `skillName` (e.g.
  // "personal:mine") that differs from the displayed bare `name` — admin
  // bundles have no `.skillName`, so this falls back to the bare name unchanged.
  emit("update:modelValue", "");
  emit("submit", { skillName: bundle.skillName ?? bundle.name });
}

// ── @/# mention palette ─────────────────────────────────────────────────────

const textareaRef = ref(null);

function focusInput() {
  if (props.disabled) return;
  textareaRef.value?.focus();
}

// Focus the textarea on mount so the user can type immediately.
onMounted(() => focusInput());

// Let the parent (Chat.vue "Reply" action) refocus the composer after seeding it.
defineExpose({ focus: focusInput });

// ── rotating placeholder hints ────────────────────────────────────────────────
// While the input is empty, cycle short "what you can do" examples every 5s with
// an opacity crossfade. The native placeholder is suppressed (aria-label carries
// the accessible name) so the animated overlay is the only visible hint.
const HINTS = [
  props.placeholder,
  "# mention a file for context",
  "@ delegate to a teammate or group",
  "/ run a skill",
];
const hintIndex = ref(0);
const hintVisible = ref(true);
const currentHint = computed(() => HINTS[hintIndex.value] ?? props.placeholder);

let hintTimer = null;
let hintFade = null;
onMounted(() => {
  hintTimer = setInterval(() => {
    hintVisible.value = false; // fade out
    hintFade = setTimeout(() => {
      hintIndex.value = (hintIndex.value + 1) % HINTS.length;
      hintVisible.value = true; // fade in
    }, 300); // matches .composer-hint transition duration
  }, 5000);
});
onUnmounted(() => {
  if (hintTimer !== null) clearInterval(hintTimer);
  if (hintFade !== null) clearTimeout(hintFade);
});

// When a send completes (running: true→false) the textarea transitions from
// :disabled to enabled. Guard on wasRunning so unrelated false→false changes
// (e.g. initial watcher flush) don't steal focus. nextTick defers the focus
// call until Vue has re-enabled the element (disabled elements can't be focused).
watch(() => props.running, (running, wasRunning) => {
  if (wasRunning && !running) nextTick(() => focusInput());
});

// mention = { kind: "@"|"#", query: string, start: number } | null
const mention = ref(null);
const highlightedIndex = ref(0);

// Derive active trigger from the textarea's live value + caret position.
// Reads from the element directly — props.modelValue is not updated synchronously
// enough by jsdom for caret-based detection to work off props.
function detectMention(value, caret) {
  // Walk backwards from caret to find the nearest @ or # preceded by start-of-string
  // or whitespace, with no whitespace between it and the caret.
  for (let i = caret - 1; i >= 0; i--) {
    const ch = value[i];
    if (ch === " " || ch === "\n" || ch === "\t") {
      // Whitespace between the trigger candidate and the caret — no active trigger.
      return null;
    }
    if (ch === "@" || ch === "#") {
      // Trigger char found. Preceding char must be start-of-string or whitespace.
      const prev = i > 0 ? value[i - 1] : null;
      if (prev !== null && prev !== " " && prev !== "\n" && prev !== "\t") {
        // Trigger is embedded in a word — not a valid trigger position.
        return null;
      }
      const query = value.slice(i + 1, caret);
      return { kind: ch, query, start: i };
    }
  }
  return null;
}

const mentionItems = computed(() => {
  if (!mention.value) return [];
  const { kind, query } = mention.value;
  const q = query.toLowerCase();
  if (kind === "@") {
    const users = delegatableUsers.value.filter(
      (u) =>
        u.displayName.toLowerCase().includes(q) ||
        u.userId.toLowerCase().includes(q),
    );
    const groups = delegatableGroups.value.filter(
      (g) =>
        g.displayName.toLowerCase().includes(q) ||
        g.groupId.toLowerCase().includes(q),
    );
    return [...users, ...groups].slice(0, 8);
  }
  // kind === "#"
  return mentionFiles.value
    .filter((f) => f.path.toLowerCase().includes(q))
    .slice(0, 8);
});

const mentionOpen = computed(() => mention.value !== null && mentionItems.value.length > 0);

// ── mention palette ARIA (listbox/option pattern, F39) ──────────────────────
// Static id is safe only because Composer is single-instance (one mount in
// Chat.vue) — a duplicate id would collide if that ever changes.
const mentionListboxId = "composer-mention-listbox";
function mentionOptionId(idx) {
  return `composer-mention-option-${idx}`;
}
const activeDescendantId = computed(() =>
  mentionOpen.value ? mentionOptionId(highlightedIndex.value) : undefined,
);

function onInput(e) {
  const el = e.target;
  // Always emit the new value so the parent's v-model stays in sync.
  emit("update:modelValue", el.value);

  const detected = detectMention(el.value, el.selectionStart);
  mention.value = detected;
  highlightedIndex.value = 0;
}

// Peers selected via the @-palette this composing session. Never cleared on input;
// delegationTarget() filters by whether the @<displayName> substring survives in the draft.
const selectedPeers = ref([]);

// Replaces el.value[start:end] with `insertion`, emits the new draft, and moves
// the caret to just after the inserted text. Shared by selectMention (replacing
// a trigger token, start !== end) and the attach-image flow (plain caret
// insertion, start === end) — same splice/emit/refocus shape either way.
// nextTick isn't available in the mousedown/change handlers that call this
// (fires before Vue re-renders), so the caret move is scheduled via setTimeout(0).
function insertAtCaret(el, start, end, insertion) {
  const before = el.value.slice(0, start);
  const after  = el.value.slice(end);
  const newValue = before + insertion + after;

  emit("update:modelValue", newValue);

  const newCaret = before.length + insertion.length;
  setTimeout(() => {
    if (el) {
      el.value = newValue;
      el.setSelectionRange(newCaret, newCaret);
    }
  }, 0);

  return newValue;
}

function selectMention(item) {
  const el = textareaRef.value;
  const { kind, query, start } = mention.value;
  const insertion = kind === "@" ? `@${item.displayName} ` : `#${item.path} `;

  // Record @-selections for delegation target resolution; # selections are ignored.
  // Discriminate groups (have groupId) from users (have userId).
  if (kind === "@") {
    if (item.groupId != null) {
      selectedPeers.value.push({ groupId: item.groupId, displayName: item.displayName, memberCount: item.memberCount });
    } else {
      selectedPeers.value.push({ userId: item.userId, displayName: item.displayName });
    }
  }

  // Replace "kind+query" (the trigger token) with the insertion text.
  insertAtCaret(el, start, start + 1 + query.length, insertion); // +1 for the trigger char

  mention.value = null;
}

// ── attach file (images + documents) ─────────────────────────────────────────

const attachImageInputRef = ref(null);

function triggerAttachImage() {
  attachImageInputRef.value?.click();
}

// Images upload into references/<name> so the F7 vision path (analyze_image) can
// find them; every other document type uploads into the workspace root so agents
// can read it via a bare #<name> mention. Mkdir isn't needed up front — the broker's
// workspacefs.Store.Write auto-creates missing intermediate directories (os.MkdirAll) —
// but createDir("references") is retried once as a defensive fallback for the image
// branch in case a future backend change makes the directory a hard prerequisite; a
// pre-existing references/ dir (Mkdir's ErrExists) is swallowed.
async function uploadAttachment(file) {
  const base64 = await fileToBase64(file);
  const isImage = (file.type || "").startsWith("image/");
  if (!isImage) {
    return await uploadFile(file.name, base64);
  }
  const path = `references/${file.name}`;
  try {
    return await uploadFile(path, base64);
  } catch (e) {
    try {
      await createDir("references");
    } catch {
      // references/ already exists (or the create itself failed) — surface the
      // original upload error below, not this fallback attempt.
    }
    return await uploadFile(path, base64);
  }
}

async function onAttachImageChange(e) {
  const file = e.target.files?.[0];
  // Reset synchronously so re-selecting the same file fires change again;
  // mirrors Files.vue's onFileInputChange (must not wait on the upload).
  e.target.value = "";
  if (!file) return;

  const el = textareaRef.value;
  const caret = el?.selectionStart ?? props.modelValue.length;

  try {
    const result = await uploadAttachment(file);
    insertAtCaret(el, caret, caret, `#${result.file.path} `);
  } catch (err) {
    toast("error", err?.message || "Upload failed");
  }
}

// Returns the first recorded peer whose @<displayName> still appears in text (by index),
// or null if none survive. Invariant: hand-typed @text with no recorded selection → null.
// Returns the full peer record: { userId, displayName } for users or
// { groupId, displayName, memberCount } for groups.
function delegationTarget(text) {
  let earliest = null;
  let earliestIdx = Infinity;
  for (const peer of selectedPeers.value) {
    const idx = text.indexOf(`@${peer.displayName}`);
    if (idx !== -1 && idx < earliestIdx) {
      earliestIdx = idx;
      earliest = peer;
    }
  }
  return earliest;
}

// Central submit path: plain Enter and Send button both flow through here.
// The /skill palette path (selectBundle) bypasses this — that emit is unchanged.
function doSubmit() {
  const text = textareaRef.value?.value ?? props.modelValue;
  // A draft that is exactly "/<granted-skill-name>" activates the skill even
  // when submitted via the Send button or with the palette already closed —
  // the raw slash text must never reach the agent as chat prose.
  if (text.startsWith("/")) {
    const name = text.slice(1).trim();
    const bundle = grantedBundles.value.find((b) => b.name === name);
    if (bundle) {
      selectBundle(bundle);
      return;
    }
  }
  const target = delegationTarget(text);
  if (target) {
    // Emit the full peer record as delegateTo — Chat.vue discriminates by which id field is present.
    emit("submit", { delegateTo: target });
  } else {
    emit("submit");
  }
}

function onEnter(e) {
  // When the /skill palette is open, Enter activates the highlighted skill
  // instead of submitting the raw "/name" text as a chat message.
  if (paletteOpen.value) {
    e.preventDefault();
    const bundle = paletteItems.value[paletteIndex.value];
    if (bundle) selectBundle(bundle);
    return;
  }
  // When the mention popover is open, Enter selects the highlighted item instead of submitting.
  if (mentionOpen.value) {
    e.preventDefault();
    const item = mentionItems.value[highlightedIndex.value];
    if (item) selectMention(item);
    return;
  }
  // Shift+Enter inserts a newline; plain Enter submits.
  // Do NOT clear modelValue here: emit() runs the parent listener synchronously,
  // so clearing before "submit" would empty the bound draft before the parent's
  // submit handler reads it (message lost). The parent clears the draft after it
  // has captured the text.
  if (!e.shiftKey) {
    e.preventDefault();
    doSubmit();
  }
}

function onKeydown(e) {
  if (paletteOpen.value) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      paletteIndex.value = (paletteIndex.value + 1) % paletteItems.value.length;
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      paletteIndex.value =
        (paletteIndex.value - 1 + paletteItems.value.length) % paletteItems.value.length;
    } else if (e.key === "Tab") {
      e.preventDefault();
      const bundle = paletteItems.value[paletteIndex.value];
      if (bundle) selectBundle(bundle);
    }
    return;
  }
  if (!mentionOpen.value) return;
  if (e.key === "ArrowDown") {
    e.preventDefault();
    highlightedIndex.value = (highlightedIndex.value + 1) % mentionItems.value.length;
  } else if (e.key === "ArrowUp") {
    e.preventDefault();
    highlightedIndex.value =
      (highlightedIndex.value - 1 + mentionItems.value.length) % mentionItems.value.length;
  } else if (e.key === "Tab") {
    e.preventDefault();
    const item = mentionItems.value[highlightedIndex.value];
    if (item) selectMention(item);
  } else if (e.key === "Escape") {
    mention.value = null;
  }
  // Enter is handled by onEnter (bound via @keydown.enter) — not here.
}
</script>

<template>
  <form class="composer" @submit.prevent="doSubmit()">
    <!-- /command palette — shown when value starts with / and grants match -->
    <ul
      v-if="paletteOpen"
      class="skill-palette"
      data-testid="skill-palette"
    >
      <li
        v-for="(bundle, idx) in paletteItems"
        :key="bundle.id"
        class="palette-item"
        :class="{ 'palette-item--highlighted': idx === paletteIndex }"
        data-testid="palette-item"
        @mousedown.prevent="selectBundle(bundle)"
        @mouseenter="paletteIndex = idx"
      >
        <span class="palette-name">{{ bundle.name }}</span>
        <span v-if="bundle.personal" class="palette-badge">personal</span>
        <span v-if="bundle.description" class="palette-desc">{{ bundle.description }}</span>
      </li>
    </ul>

    <!-- @/# mention palette -->
    <ul
      v-if="mentionOpen"
      :id="mentionListboxId"
      role="listbox"
      class="skill-palette"
      data-testid="mention-palette"
    >
      <li
        v-for="(item, idx) in mentionItems"
        :key="mention.kind === '@' ? (item.groupId ?? item.userId) : item.path"
        :id="mentionOptionId(idx)"
        role="option"
        :aria-selected="idx === highlightedIndex"
        class="palette-item"
        :class="{ 'palette-item--highlighted': idx === highlightedIndex }"
        data-testid="mention-item"
        @mousedown.prevent="selectMention(item)"
      >
        <template v-if="mention.kind === '@' && item.groupId != null">
          <span class="palette-name">{{ item.displayName }}</span>
          <span class="palette-desc">group · {{ item.memberCount }} people</span>
        </template>
        <template v-else-if="mention.kind === '@'">
          <span class="palette-name">{{ item.displayName }}</span>
          <span class="palette-desc">{{ item.userId }}</span>
        </template>
        <template v-else>
          <span class="palette-name">{{ item.path }}</span>
        </template>
      </li>
    </ul>

    <!-- Animated hint overlay: only while the input is empty. Native placeholder
         is suppressed so this crossfading hint is the sole visible prompt. -->
    <div
      v-if="!modelValue"
      class="composer-hint"
      :class="{ 'composer-hint--hidden': !hintVisible }"
      data-testid="composer-hint"
      aria-hidden="true"
    >{{ currentHint }}</div>

    <textarea
      ref="textareaRef"
      class="composer-input"
      :value="modelValue"
      :disabled="disabled || running"
      :placeholder="''"
      :aria-label="placeholder"
      role="combobox"
      aria-haspopup="listbox"
      :aria-expanded="mentionOpen"
      :aria-controls="mentionOpen ? mentionListboxId : undefined"
      :aria-activedescendant="activeDescendantId"
      rows="1"
      @input="onInput"
      @keydown="onKeydown"
      @keydown.enter="onEnter"
    />
    <input
      ref="attachImageInputRef"
      type="file"
      accept="image/*,.pdf,.doc,.docx,.xls,.xlsx,.csv,.ppt,.pptx,.md,.txt,.json,.rtf,.odt,.ods,.odp"
      class="attach-image-input-hidden"
      data-testid="attach-image-input"
      @change="onAttachImageChange"
    />
    <button
      type="button"
      class="composer-attach"
      :disabled="disabled"
      data-testid="attach-image-btn"
      aria-label="Attach file"
      @click="triggerAttachImage"
    >
      <Icon name="upload" :size="16" />
    </button>
    <button
      v-if="!running"
      type="submit"
      class="composer-send"
      :disabled="disabled"
      aria-label="Send"
    >
      <Icon name="send" :size="16" />
    </button>
    <button
      v-else
      type="button"
      class="composer-send composer-stop"
      @click="emit('stop')"
      aria-label="Stop"
    >
      <Icon name="stop" :size="16" />
    </button>
  </form>

  <div
    v-if="workspaceControlVisible"
    ref="workspaceControlRef"
    class="workspace-control"
    data-testid="workspace-control"
  >
    <button
      type="button"
      class="workspace-control-btn"
      data-testid="workspace-control-btn"
      aria-haspopup="true"
      :aria-expanded="workspaceMenuOpen"
      @click="workspaceMenuOpen = !workspaceMenuOpen"
    >{{ workspaceLabel }}</button>
    <!-- Plain buttons, no menu role — no arrow-key navigation is implemented,
         so promising role="menu"/"menuitem" keyboard behavior would be false
         advertising to assistive tech. -->
    <div v-if="workspaceMenuOpen" class="workspace-menu" data-testid="workspace-menu">
      <button
        type="button"
        class="workspace-menu-item"
        data-testid="workspace-menu-local"
        @click="chooseLocal"
      >Local workspace</button>
      <button
        type="button"
        class="workspace-menu-item"
        data-testid="workspace-menu-onedrive"
        @click="openFolderPicker"
      >OneDrive folder…</button>
    </div>
  </div>

  <WorkspaceFolderPicker
    :visible="folderPickerVisible"
    @close="folderPickerVisible = false"
    @select="onFolderSelected"
  />
</template>

<style scoped>
.composer {
  padding: 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  position: relative;
}

.skill-palette {
  position: absolute;
  bottom: calc(100% + 4px);
  left: 0;
  right: 0;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  list-style: none;
  margin: 0;
  padding: 0.25rem 0;
  z-index: 100;
  max-height: 220px;
  overflow-y: auto;
}

.palette-item {
  display: flex;
  flex-direction: column;
  padding: 0.45rem 0.75rem;
  cursor: pointer;
  gap: 2px;
}

.palette-item:hover,
.palette-item--highlighted {
  background: var(--bg-hover);
}

.palette-name {
  font-size: 0.875rem;
  font-weight: 500;
  color: var(--text);
}

.palette-desc {
  font-size: 0.75rem;
  color: var(--text-muted);
}

.palette-badge {
  align-self: flex-start;
  font-size: 0.625rem;
  color: var(--text-muted);
  background: var(--bg-hover);
  border-radius: var(--radius-sm);
  padding: 0 0.35rem;
  line-height: 1.4;
}

.composer-input {
  flex: 1;
  background: transparent;
  border: none;
  outline: none;
  resize: none;
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.9375rem;
  line-height: 1.5;
  max-height: 200px;
  overflow-y: auto;
  field-sizing: content;
  width: 100%;
  /* Symmetric vertical padding so the caret (which spans the full 1.5em line box)
     sits with breathing room instead of filling the input edge-to-edge; reserve
     room on the right so text never runs under the absolutely-positioned
     attach/send buttons. */
  padding: 0.375rem 4.75rem 0.375rem 0;
  /* Chromium collapses a :disabled empty field-sizing:content textarea below one
     line box; the run-in-progress state (value cleared + disabled) hit exactly
     that, shrinking the footer and displacing the buttons. Pin one line box
     (1.5em line-height) + the vertical padding as the floor. */
  min-height: calc(1.5em + 0.75rem);
}

.composer-input::placeholder {
  color: var(--text-faint);
}

.composer-hint {
  position: absolute;
  left: 0.75rem;
  /* form padding (0.75rem) + textarea top padding (0.375rem) so the hint sits on
     the first text line, aligned with where the empty-input caret renders. */
  top: calc(0.75rem + 0.375rem);
  font-size: 0.9375rem;
  line-height: 1.5;
  color: var(--text-faint);
  pointer-events: none;
  transition: opacity 0.3s ease;
  opacity: 1;
  /* stay clear of the attach/send buttons */
  max-width: calc(100% - 5.5rem);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.composer-hint--hidden {
  opacity: 0;
}

.composer-input:disabled {
  opacity: 0.5;
}

.attach-image-input-hidden {
  position: absolute;
  width: 0;
  height: 0;
  opacity: 0;
  overflow: hidden;
}

.composer-attach {
  position: absolute;
  right: 3rem;
  bottom: 0.75rem;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  background: transparent;
  color: var(--text-muted);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.composer-attach:hover:not(:disabled) {
  background: var(--bg-hover);
  color: var(--text);
}

.composer-attach:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.composer-send {
  position: absolute;
  right: 0.75rem;
  bottom: 0.75rem;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  background: var(--accent);
  color: var(--text-on-accent);
  border: none;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 0.15s;
}

.composer-send:hover:not(:disabled) {
  background: var(--accent-hover);
}

.composer-send:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.composer-stop {
  background: var(--accent);
}

.composer-stop:hover {
  background: var(--accent-hover);
}

.workspace-control {
  position: relative;
  margin-top: 0.375rem;
  display: flex;
}

.workspace-control-btn {
  background: transparent;
  border: none;
  padding: 0.125rem 0.25rem;
  color: var(--text-muted);
  font-family: var(--font-sans);
  font-size: 0.75rem;
  cursor: pointer;
  border-radius: var(--radius-sm);
}

.workspace-control-btn:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.workspace-menu {
  position: absolute;
  bottom: calc(100% + 4px);
  left: 0;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0.25rem 0;
  z-index: 100;
  min-width: 180px;
}

.workspace-menu-item {
  display: block;
  width: 100%;
  text-align: left;
  background: transparent;
  border: none;
  padding: 0.45rem 0.75rem;
  cursor: pointer;
  font-family: var(--font-sans);
  font-size: 0.8125rem;
  color: var(--text);
}

.workspace-menu-item:hover {
  background: var(--bg-hover);
}
</style>
