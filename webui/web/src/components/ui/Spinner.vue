<script setup>
const props = defineProps({
  size: { type: String, default: "md" },
});

const sizeMap = { sm: "16px", md: "24px", lg: "40px" };
const dim = sizeMap[props.size] ?? sizeMap.md;
</script>

<template>
  <span
    class="spinner"
    :style="{ width: dim, height: dim }"
    role="status"
    aria-label="Loading"
  />
</template>

<style scoped>
.spinner {
  display: inline-block;
  border: 2px solid var(--fill-muted);
  border-top-color: var(--accent);
  border-radius: 50%;
  animation: spin 0.7s linear infinite;
  flex-shrink: 0;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}
</style>
