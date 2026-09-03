<script setup>
import { ref } from "vue";
import { useRouter } from "vue-router";
import { useUserStore } from "../store/user.js";
import { usePromptStore } from "../store/prompt.js";
import Icon from "../components/Icon.vue";

const router = useRouter();
const userStore = useUserStore();
const promptStore = usePromptStore();

const draft = ref("");

const chips = [
  { label: "Write",      icon: "design" },
  { label: "Learn",      icon: "star" },
  { label: "Code",       icon: "code" },
  { label: "Life stuff", icon: "home" },
  { label: "From Drive", icon: "drive" },
];

function submit() {
  const text = draft.value.trim();
  if (!text) return;
  promptStore.set(text);
  router.push("/chat");
}

function pickChip(label) {
  draft.value = label;
  submit();
}
</script>

<template>
  <div class="home">
    <!-- 8-point coral asterisk accent mark -->
    <div class="home-mark" aria-hidden="true">✳</div>

    <h1 class="home-greeting">Hey there, {{ userStore.displayName }}</h1>

    <form class="home-composer" @submit.prevent="submit">
      <textarea
        v-model="draft"
        class="home-textarea"
        placeholder="How can I help you today?"
        rows="3"
        @keydown.enter.exact.prevent="submit"
      />
      <button type="submit" class="home-send" aria-label="Send">
        <Icon name="send" :size="18" />
      </button>
    </form>

    <div class="home-chips" role="list">
      <button
        v-for="chip in chips"
        :key="chip.label"
        class="chip"
        type="button"
        role="listitem"
        @click="pickChip(chip.label)"
      >
        <Icon :name="chip.icon" :size="14" />
        {{ chip.label }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.home {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  min-height: 100%;
  padding: 3rem 1.5rem;
  gap: 1.5rem;
}

.home-mark {
  font-size: 3rem;
  color: var(--accent);
  line-height: 1;
  user-select: none;
}

.home-greeting {
  font-family: var(--font-serif);
  font-size: 2rem;
  font-weight: 400;
  color: var(--text);
  margin: 0;
  text-align: center;
}

.home-composer {
  position: relative;
  width: 100%;
  max-width: 640px;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  display: flex;
  align-items: flex-end;
  padding: 0.75rem;
  gap: 0.5rem;
}

.home-textarea {
  flex: 1;
  background: transparent;
  border: none;
  outline: none;
  resize: none;
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.9375rem;
  line-height: 1.5;
}

.home-textarea::placeholder {
  color: var(--text-faint);
}

.home-send {
  flex-shrink: 0;
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

.home-send:hover {
  background: var(--accent-hover);
}

.home-chips {
  display: flex;
  flex-wrap: wrap;
  gap: 0.5rem;
  justify-content: center;
  max-width: 640px;
}

.chip {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  padding: 0.375rem 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--text-muted);
  font-family: var(--font-sans);
  font-size: 0.8125rem;
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.chip:hover {
  background: var(--bg-hover);
  color: var(--text);
}
</style>
