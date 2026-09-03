<script setup>
const props = defineProps({
  label:    { type: String,  default: "" },
  error:    { type: String,  default: "" },
  required: { type: Boolean, default: false },
  hint:     { type: String,  default: "" },
});
</script>

<template>
  <div class="form-field">
    <label v-if="label" class="field-label">
      {{ label }}<span v-if="required" class="field-required" aria-hidden="true"> *</span>
    </label>
    <span v-if="hint" class="field-hint">{{ hint }}</span>
    <slot />
    <span v-if="error" class="field-error" role="alert">{{ error }}</span>
  </div>
</template>

<style scoped>
.form-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.field-label {
  font-size: 0.8rem;
  color: var(--text-muted);
  font-weight: 500;
}

.field-error {
  font-size: 0.78rem;
  color: var(--danger);
}

.field-required {
  color: var(--danger);
  font-weight: 600;
}

.field-hint {
  font-size: 0.75rem;
  color: var(--text-faint);
}
</style>
