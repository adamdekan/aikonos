import { reactive } from "vue";

// Module-level queue — shared across all callers in the same app instance.
const toasts = reactive([]);
let _nextId = 1;

function push(type, msg) {
  const id = _nextId++;
  toasts.push({ id, type, msg });
  setTimeout(() => dismiss(id), 4000);
  return id;
}

function dismiss(id) {
  const idx = toasts.findIndex((t) => t.id === id);
  if (idx !== -1) toasts.splice(idx, 1);
}

export function useToast() {
  return { toasts, push, dismiss };
}
