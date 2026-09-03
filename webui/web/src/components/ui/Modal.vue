<script setup>
import { onMounted, onUnmounted, watch } from "vue";
import { autoScrollbar } from "../../lib/autoScrollbar.js";

// Local registration so the auto-hiding scrollbar works without relying on
// global directive registration (and resolves cleanly under unit tests).
const vAutoscroll = autoScrollbar;

const props = defineProps({
  open:  { type: Boolean, required: true },
  title: { type: String,  default: "" },
  size:  { type: String,  default: "default" }, // "default" | "wide"
});

const emit = defineEmits(["close"]);

function onKey(e) {
  if (e.key === "Escape") emit("close");
}

function attachKey() { document.addEventListener("keydown", onKey); }
function detachKey() { document.removeEventListener("keydown", onKey); }

watch(() => props.open, (val) => {
  if (val) attachKey();
  else detachKey();
}, { immediate: true });

onUnmounted(detachKey);
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="modal-overlay" @click.self="emit('close')">
      <div
        class="modal-box"
        :class="size === 'wide' ? 'modal-box--wide' : ''"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
      >
        <div class="modal-header">
          <span class="modal-title">{{ title }}</span>
        </div>
        <div
          class="modal-body"
          :class="size === 'wide' ? 'modal-body--wide scroll-min' : ''"
          v-autoscroll
        >
          <slot />
        </div>
        <div v-if="$slots.footer" class="modal-footer">
          <slot name="footer" />
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.modal-overlay {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 1000;
}

.modal-box {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  min-width: 360px;
  max-width: 560px;
  width: 100%;
  overflow: hidden;
}

/* Wide variant — used by the agent creation/edit modal */
.modal-box--wide {
  max-width: 720px;
  display: flex;
  flex-direction: column;
  max-height: min(80vh, 680px);
  overflow: visible; /* let header/footer be sticky, body scrolls */
}
.modal-box--wide .modal-header {
  flex-shrink: 0;
}
.modal-box--wide .modal-footer {
  flex-shrink: 0;
}
.modal-body--wide {
  overflow-y: auto;
  padding: var(--space-6) var(--space-6);
  flex: 1;
}

.modal-header {
  padding: 16px 20px 12px;
  border-bottom: 1px solid var(--border);
}

.modal-title {
  font-weight: 500;
  font-size: 0.95rem;
}

.modal-body {
  padding: 16px 20px;
}

.modal-footer {
  padding: 12px 20px 16px;
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  border-top: 1px solid var(--border);
}
</style>
