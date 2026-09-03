<script setup>
// A pill toggle switch matching the Aikonos admin visual language. Controlled:
// bind :modelValue and listen for update:modelValue (v-model).
const props = defineProps({
  modelValue: { type: Boolean, required: true },
  disabled: { type: Boolean, default: false },
  ariaLabel: { type: String, default: "" },
});
const emit = defineEmits(["update:modelValue"]);

function toggle() {
  if (props.disabled) return;
  emit("update:modelValue", !props.modelValue);
}
</script>

<template>
  <button
    type="button"
    role="switch"
    :aria-checked="modelValue"
    :aria-label="ariaLabel || undefined"
    :disabled="disabled"
    class="toggle"
    :class="{ on: modelValue }"
    @click="toggle"
  >
    <span class="knob"></span>
  </button>
</template>

<style scoped>
.toggle {
  position: relative;
  width: 40px; height: 22px;
  border-radius: 999px;
  border: none;
  background: var(--fill-muted);
  cursor: pointer;
  padding: 0;
  transition: background 0.15s ease;
  flex-shrink: 0;
}
.toggle.on { background: var(--accent); }
.toggle:disabled { opacity: 0.5; cursor: not-allowed; }
.toggle:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }

.knob {
  position: absolute;
  top: 3px; left: 3px;
  width: 16px; height: 16px;
  border-radius: 50%;
  background: var(--text-on-accent);
  transition: transform 0.15s ease;
}
.toggle.on .knob { transform: translateX(18px); }
</style>
