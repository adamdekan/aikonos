<script setup>
const props = defineProps({
  value: { type: Number, required: true },
  max:   { type: Number, default: 4096 },
});
</script>

<template>
  <span
    class="byte-counter"
    :class="{ 'byte-counter--over': value > max }"
    aria-live="polite"
  >{{ value }} / {{ max }} bytes</span>
</template>

<style scoped>
.byte-counter {
  font-size: 11px;
  color: var(--text-muted);
}

.byte-counter--over {
  color: var(--danger);
  font-weight: 600;
}
</style>
