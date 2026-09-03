<script setup>
import { computed } from "vue";
import Icon from "../../components/Icon.vue";

const props = defineProps({
  envelope: { type: Object, required: true },
});

const emit = defineEmits(["send-to-agent", "start-session", "dismiss", "review"]);

// A skill-transfer envelope carries no task to run — SendSkillTransfer
// never sets up a delegated
// intent, so it renders "Send to agent"/"New session" and reviews via the
// preview modal instead.
const isTransfer = computed(() => props.envelope.task?.kind === "skill_transfer");

// The name isn't a separate EnvelopeTask field — SendSkillTransfer fixes the
// envelope's intent to "Skill transfer: <name>" (same spec section), so that
// is the one place to read it from without an extra fetch.
const transferSkillName = computed(() => (props.envelope.task?.intent ?? "").replace(/^Skill transfer: /, ""));
</script>

<template>
  <li
    class="envelope-row"
    data-testid="envelope-row"
  >
    <div class="env-actions">
      <template v-if="isTransfer">
        <button
          class="btn-primary btn-sm"
          :data-testid="`review-${envelope.envelopeId}`"
          @click="emit('review', envelope)"
        >
          <Icon name="book" :size="13" />
          Review
        </button>
      </template>
      <template v-else>
        <button
          class="btn-primary btn-sm"
          :data-testid="`send-to-agent-${envelope.envelopeId}`"
          @click="emit('send-to-agent', envelope)"
        >
          <Icon name="check" :size="13" />
          Send to agent
        </button>
        <button
          class="btn-secondary btn-sm"
          :data-testid="`start-session-${envelope.envelopeId}`"
          @click="emit('start-session', envelope)"
        >
          <Icon name="send" :size="13" />
          New session
        </button>
      </template>
      <button
        class="btn-ghost btn-sm"
        :data-testid="`dismiss-${envelope.envelopeId}`"
        @click="emit('dismiss', envelope.envelopeId)"
      >
        OK
      </button>
    </div>
    <div class="env-main">
      <template v-if="isTransfer">
        <span class="env-from">
          <Icon name="send" :size="13" />
          {{ envelope.fromDisplayName || envelope.fromUserId }}
        </span>
        <div class="env-intent-scroll scroll-min" data-testid="transfer-info">
          <span class="transfer-badge">Skill transfer</span>
          {{ transferSkillName }}
        </div>
      </template>
      <template v-else>
        <span class="env-from">
          <Icon name="send" :size="13" />
          {{ envelope.fromDisplayName || envelope.fromUserId }}
        </span>
        <div
          class="env-intent-scroll scroll-min"
          data-testid="intent-scroll"
          tabindex="-1"
        >
          {{ envelope.task?.intent }}
        </div>
      </template>
    </div>
  </li>
</template>

<style scoped>
.envelope-row {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  transition: background 0.15s;
}

.envelope-row:hover {
  background: var(--bg-hover);
}

.env-main {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-width: 0;
}

.env-from {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  font-size: 0.75rem;
  color: var(--text-muted);
}

.env-intent-scroll {
  font-size: 0.9375rem;
  color: var(--text);
  max-height: 14rem;
  overflow-y: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.transfer-badge {
  display: inline-block;
  margin-right: var(--space-2);
  padding: 1px var(--space-2);
  border-radius: 999px;
  background: var(--fill-muted);
  color: var(--text-muted);
  font-size: 0.6875rem;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.03em;
}

.env-actions {
  display: flex;
  flex-direction: row;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-2);
  flex-shrink: 0;
}

/* Primary: solid --ok fill */
.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  background: var(--ok);
  border: 1px solid var(--ok);
  border-radius: var(--radius-sm);
  color: var(--text-on-accent);
  font-family: var(--font-sans);
  cursor: pointer;
  transition: background 0.15s;
}

.btn-primary:hover {
  background: color-mix(in srgb, var(--ok) 85%, black);
  border-color: color-mix(in srgb, var(--ok) 85%, black);
}

.btn-primary:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}

/* Secondary: outlined */
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  background: transparent;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  cursor: pointer;
  transition: background 0.15s;
}

.btn-secondary:hover {
  background: var(--bg-hover);
}

.btn-secondary:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}

/* Ghost: borderless */
.btn-ghost {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  font-family: var(--font-sans);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.btn-ghost:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.btn-ghost:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}

.btn-sm {
  padding: var(--space-1) var(--space-3);
  font-size: 0.8125rem;
}

/* Responsive: ≤640px — stack main and actions vertically, actions wrap */
@media (max-width: 640px) {
  .envelope-row {
    flex-direction: column;
  }

  .env-actions {
    width: 100%;
    flex-wrap: wrap;
    justify-content: flex-start;
  }
}
</style>
