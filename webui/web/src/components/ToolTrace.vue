<script setup>
import { computed } from "vue";
import { toolLabel } from "../lib/toolLabels.js";

const props = defineProps({
  // Live assistant tool entries: [{ id, name, description, result, isError }]
  tools: { type: Array, default: () => [] },
  // active: this is the live assistant message of an in-flight run. Gates every
  // dots animation — a settled/historical message shows static lines only.
  active: { type: Boolean, default: false },
  // textStreaming: the run is currently emitting assistant text. When true the
  // text itself is the progress indicator, so no standalone dots are shown.
  textStreaming: { type: Boolean, default: false },
});

// A tool is "running" while the run is active and it has neither a result nor an
// error — its inline dots stand in for the pending result.
function isRunning(tool) {
  return props.active && tool.result == null && !tool.isError;
}

// Standalone dots: the run is active, no text is streaming, and no tool is
// mid-flight — i.e. we are waiting on the model's next step. Guarantees the
// no-blank-progress invariant between a tool result and the next event.
const showStandaloneDots = computed(
  () => props.active && !props.textStreaming && !props.tools.some(isRunning),
);
</script>

<template>
  <div
    v-if="tools.length > 0 || showStandaloneDots"
    class="tool-trace"
    data-testid="tool-trace"
  >
    <div
      v-for="tool in tools"
      :key="tool.id"
      class="tool-trace-line"
      :class="{ 'tool-trace-line--error': tool.isError }"
    >
      <span class="tool-trace-label">{{ toolLabel(tool) }}</span>
      <span v-if="isRunning(tool)" class="tool-trace-dots" aria-label="running">
        <span></span><span></span><span></span>
      </span>
    </div>

    <span
      v-if="showStandaloneDots"
      class="tool-trace-dots tool-trace-dots--standalone"
      aria-label="working"
      data-testid="tool-trace-standalone-dots"
    >
      <span></span><span></span><span></span>
    </span>
  </div>
</template>

<style scoped>
.tool-trace {
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
}

.tool-trace-line {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.78rem;
  color: var(--text-muted);
  max-width: 100%;
  min-width: 0;
}

.tool-trace-label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tool-trace-line--error .tool-trace-label {
  color: var(--danger);
}

/* Three bouncing dots — same idiom as Chat.vue's footer typing-indicator, sized
   down to sit inline with a 0.78rem line. */
.tool-trace-dots {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  flex-shrink: 0;
}
.tool-trace-dots--standalone {
  padding: 0.1rem 0;
}
.tool-trace-dots span {
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: var(--text-muted);
  animation: tool-trace-bounce 1.2s infinite;
}
.tool-trace-dots span:nth-child(2) { animation-delay: 0.2s; }
.tool-trace-dots span:nth-child(3) { animation-delay: 0.4s; }

@keyframes tool-trace-bounce {
  0%, 60%, 100% { transform: translateY(0); }
  30%           { transform: translateY(-4px); }
}
</style>
