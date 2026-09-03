<script setup>
import { ref, computed, watch } from "vue";
import { buildCron, parseCron, describeCron } from "../lib/cron.js";

const props = defineProps({
  modelValue: { type: String, default: "" },
});

const emit = defineEmits(["update:modelValue"]);

const MINUTE_INTERVALS = [1, 5, 10, 15, 30];
const HOUR_INTERVALS = [1, 2, 3, 4, 6, 12];
const WEEKDAY_PRESET = [1, 2, 3, 4, 5];
const DAY_LABELS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

const advanced = ref(false);
const rawCron = ref(props.modelValue);

const freq = ref("daily");
const interval = ref(1);
const hour = ref(9);
const minute = ref(0);
const weekdays = ref([1, 2, 3, 4, 5]);
const dom = ref(1);

// Seed guided controls from the incoming cron string. If it can't be parsed
// and isn't empty, start in Advanced mode so nothing silently changes what
// the user (or a previous session) already set.
function seedFromCron(expr) {
  const r = parseCron(expr);
  if (!r) {
    if (expr) {
      advanced.value = true;
      rawCron.value = expr;
    }
    return;
  }
  freq.value = r.freq;
  if (r.interval !== undefined) interval.value = r.interval;
  if (r.hour !== undefined) hour.value = r.hour;
  if (r.minute !== undefined) minute.value = r.minute;
  if (r.weekdays !== undefined) weekdays.value = r.weekdays;
  if (r.dom !== undefined) dom.value = r.dom;
}

seedFromCron(props.modelValue);

const timeValue = computed({
  get: () => `${String(hour.value).padStart(2, "0")}:${String(minute.value).padStart(2, "0")}`,
  set: (v) => {
    const [h, m] = v.split(":").map(Number);
    hour.value = h;
    minute.value = m;
  },
});

function currentRecurrence() {
  return {
    freq: freq.value,
    interval: interval.value,
    hour: hour.value,
    minute: minute.value,
    weekdays: weekdays.value,
    dom: dom.value,
  };
}

const guidedCron = computed(() => buildCron(currentRecurrence()));

const currentCron = computed(() => (advanced.value ? rawCron.value : guidedCron.value));

watch(currentCron, (v) => emit("update:modelValue", v));

// Emit immediately once on mount so a submit-without-touching-anything still
// carries the guided default (e.g. daily 09:00) rather than the raw prop.
emit("update:modelValue", currentCron.value);

// Re-seed when the prop changes to a value we did NOT just emit ourselves —
// e.g. the parent resets modelValue after submit without remounting this
// instance. Guard against the v-model echo (parent reflecting our own emit
// straight back) so this never re-triggers on our own updates.
watch(
  () => props.modelValue,
  (v) => {
    if (v !== currentCron.value) seedFromCron(v);
  },
);

function toggleWeekday(n) {
  const idx = weekdays.value.indexOf(n);
  if (idx === -1) {
    weekdays.value = [...weekdays.value, n].sort((a, b) => a - b);
  } else {
    weekdays.value = weekdays.value.filter((d) => d !== n);
  }
}

function setWeekdayPreset() {
  weekdays.value = [...WEEKDAY_PRESET];
}

function toggleAdvanced() {
  if (!advanced.value) {
    // Entering advanced: show the current guided cron as the raw starting point.
    rawCron.value = guidedCron.value;
    advanced.value = true;
  } else {
    // Returning to guided: re-derive controls from whatever was typed.
    advanced.value = false;
    seedFromCron(rawCron.value);
  }
}

const description = computed(() => describeCron(currentCron.value));
</script>

<template>
  <div class="recurrence-selector">
    <template v-if="!advanced">
      <div class="rec-row">
        <select v-model="freq" class="field" data-testid="recurrence-freq">
          <option value="minutes">Every N minutes</option>
          <option value="hourly">Hourly</option>
          <option value="daily">Daily</option>
          <option value="weekly">Weekly</option>
          <option value="monthly">Monthly</option>
        </select>

        <select
          v-if="freq === 'minutes'"
          v-model.number="interval"
          class="field"
          data-testid="cron-interval"
        >
          <option v-for="n in MINUTE_INTERVALS" :key="n" :value="n">Every {{ n }} min</option>
        </select>

        <template v-if="freq === 'hourly'">
          <select v-model.number="interval" class="field" data-testid="cron-interval">
            <option v-for="n in HOUR_INTERVALS" :key="n" :value="n">Every {{ n }}h</option>
          </select>
          <input
            v-model.number="minute"
            class="field field-narrow"
            type="number"
            min="0"
            max="59"
            aria-label="Minute"
          />
        </template>

        <input
          v-if="freq === 'daily' || freq === 'weekly' || freq === 'monthly'"
          v-model="timeValue"
          class="field field-narrow"
          type="time"
          data-testid="cron-time"
        />

        <input
          v-if="freq === 'monthly'"
          v-model.number="dom"
          class="field field-narrow"
          type="number"
          min="1"
          max="31"
          data-testid="cron-dom"
        />
      </div>

      <div v-if="freq === 'weekly'" class="rec-row weekday-row">
        <button
          v-for="(label, n) in DAY_LABELS"
          :key="n"
          type="button"
          class="weekday-toggle"
          :class="{ 'weekday-toggle--active': weekdays.includes(n) }"
          :data-testid="`weekday-${n}`"
          @click="toggleWeekday(n)"
        >
          {{ label }}
        </button>
        <button type="button" class="weekday-preset" @click="setWeekdayPreset">Weekdays</button>
      </div>
    </template>

    <template v-else>
      <input
        v-model="rawCron"
        class="field"
        placeholder="Cron expression (e.g. 0 9 * * *)"
        data-testid="new-cron"
      />
    </template>

    <div class="rec-footer">
      <span class="cron-description" data-testid="cron-description">{{ description }}</span>
      <button type="button" class="advanced-toggle" data-testid="cron-advanced-toggle" @click="toggleAdvanced">
        {{ advanced ? "Guided" : "Advanced" }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.recurrence-selector {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  width: 100%;
}

.rec-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}

.field {
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  padding: 0.5rem 0.75rem;
  outline: none;
}

.field:focus {
  border-color: var(--accent);
}

.field-narrow {
  width: auto;
  min-width: 5rem;
}

.weekday-row {
  gap: 0.375rem;
}

.weekday-toggle {
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  font-family: var(--font-sans);
  font-size: 0.75rem;
  padding: 0.375rem 0.5rem;
  cursor: pointer;
  transition: background 0.15s, color 0.15s, border-color 0.15s;
}

.weekday-toggle--active {
  background: var(--fill-accent);
  border-color: var(--accent);
  color: var(--accent);
}

.weekday-preset {
  background: transparent;
  border: 1px dashed var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  font-family: var(--font-sans);
  font-size: 0.75rem;
  padding: 0.375rem 0.5rem;
  cursor: pointer;
}

.weekday-preset:hover {
  color: var(--text);
}

.rec-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
}

.cron-description {
  font-size: 0.8125rem;
  color: var(--text-muted);
}

.advanced-toggle {
  background: transparent;
  border: none;
  color: var(--accent);
  font-family: var(--font-sans);
  font-size: 0.75rem;
  cursor: pointer;
  padding: 0;
  text-decoration: underline;
  flex-shrink: 0;
}
</style>
