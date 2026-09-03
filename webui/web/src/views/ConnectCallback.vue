<script setup>
import { ref, onMounted } from "vue";
import { useRoute, useRouter } from "vue-router";
import Icon from "../components/Icon.vue";
import { completeConnectorAuth } from "../api/connectors.js";

const route = useRoute();
const router = useRouter();

const status = ref("pending"); // pending | success | error
const errorMsg = ref("");
const connectorId = ref("");

onMounted(async () => {
  const code  = route.query.code  ?? "";
  const state = route.query.state ?? "";
  try {
    const data = await completeConnectorAuth(code, state);
    connectorId.value = data.connectorId ?? "";
    status.value = "success";
  } catch (e) {
    errorMsg.value = e.message || "OAuth exchange failed";
    status.value = "error";
  }
});
</script>

<template>
  <div class="callback">
    <div v-if="status === 'pending'" class="state-pending">
      <Icon name="spinner" :size="24" class="spin" />
      <span>Completing connection…</span>
    </div>

    <div v-else-if="status === 'success'" class="state-success" data-testid="callback-success">
      <Icon name="check" :size="24" />
      <p>Connected successfully.</p>
      <a href="/connections" @click.prevent="router.push('/connections')">Back to Connections</a>
    </div>

    <div v-else class="state-error" data-testid="callback-error">
      <Icon name="close" :size="24" />
      <p>{{ errorMsg }}</p>
      <a href="/connections" @click.prevent="router.push('/connections')">Back to Connections</a>
    </div>
  </div>
</template>

<style scoped>
.callback {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 60vh;
  padding: 2rem;
}

.state-pending,
.state-success,
.state-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 1rem;
  text-align: center;
}

.state-success {
  color: var(--ok);
}

.state-error {
  color: var(--danger);
}

.state-pending {
  color: var(--text-muted);
}

@keyframes spin {
  to { transform: rotate(360deg); }
}

.spin {
  animation: spin 1s linear infinite;
}

p {
  margin: 0;
  font-size: 0.9375rem;
  color: var(--text);
}
</style>
