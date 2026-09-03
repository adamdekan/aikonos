<script setup>
import { ref } from "vue";
import Icon from "../../components/Icon.vue";

const props = defineProps({
  title:       { type: String,  required: true },
  icon:        { type: String,  default: "" },
  description: { type: String,  default: "" },
  collapsible: { type: Boolean, default: false },
  open:        { type: Boolean, default: true },
});

const emit = defineEmits(["update:open"]);

// Local collapse state; prop is initial value only (uncontrolled by default).
const isOpen = ref(props.open);
const bodyId = `fs-body-${Math.random().toString(36).slice(2)}`;

function toggle() {
  if (!props.collapsible) return;
  isOpen.value = !isOpen.value;
  emit("update:open", isOpen.value);
}
</script>

<template>
  <fieldset class="form-section">
    <legend class="sr-only">{{ title }}</legend>
    <div
      class="section-header"
      :class="{ 'section-header--collapsible': collapsible }"
    >
      <button
        v-if="collapsible"
        type="button"
        class="section-toggle"
        :aria-expanded="isOpen.toString()"
        :aria-controls="bodyId"
        @click="toggle"
      >
        <Icon v-if="icon" :name="icon" :size="16" class="section-icon" aria-hidden="true" />
        <span class="section-title">{{ title }}</span>
        <Icon
          name="chevron-down"
          :size="14"
          class="section-chevron"
          :class="{ 'section-chevron--collapsed': !isOpen }"
          aria-hidden="true"
        />
      </button>
      <div v-else class="section-static">
        <Icon v-if="icon" :name="icon" :size="16" class="section-icon" aria-hidden="true" />
        <span class="section-title">{{ title }}</span>
      </div>
      <p v-if="description" class="section-desc">{{ description }}</p>
    </div>
    <div
      :id="bodyId"
      class="section-body"
      :hidden="collapsible && !isOpen"
    >
      <slot />
    </div>
  </fieldset>
</template>

<style scoped>
.form-section {
  border: none;
  margin: 0;
  padding: 0;
}

.section-header {
  margin-bottom: var(--space-3);
}

.section-toggle {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  background: none;
  border: none;
  padding: 0;
  cursor: pointer;
  color: var(--text);
  width: 100%;
  text-align: left;
}

.section-toggle:hover .section-title {
  color: var(--accent);
}

.section-static {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.section-icon {
  color: var(--text-muted);
  flex-shrink: 0;
}

.section-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text);
}

.section-chevron {
  color: var(--text-muted);
  margin-left: auto;
  transition: transform 0.15s ease;
}

.section-chevron--collapsed {
  transform: rotate(-90deg);
}

.section-desc {
  margin: var(--space-1) 0 0;
  font-size: 12px;
  color: var(--text-muted);
}

.section-body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.section-body[hidden] {
  display: none;
}

.sr-only {
  position: absolute;
  width: 1px; height: 1px;
  padding: 0; margin: -1px;
  overflow: hidden;
  clip: rect(0,0,0,0);
  white-space: nowrap;
  border: 0;
}
</style>
