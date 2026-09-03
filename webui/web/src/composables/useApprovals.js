import { ref, computed, onUnmounted } from "vue";
import { get } from "../api/client.js";
import { useToast } from "../components/ui/useToast.js";

// HITL approval state: a FIFO queue of pending approvals plus the polling
// fallback that keeps the queue fed (the CUSTOM event and /approvals poll
// both feed the same dedup path via the handled-toolCallId set).
//
// Polling start condition (F37): there is no existing "pending tool call"
// signal in useAguiRun to key a quiet-stream heuristic off of, so the
// accepted fallback is (a)-only — polling starts on the first
// aikonos.approval.request arrival (CUSTOM event or a poll result) and stays
// active until the run ends (stopPolling, called by useAguiRun on
// finish/error/stop, is unchanged).
//
// One-shot safety net (reviewer finding, closed in-iteration): (a)-only has a
// gap — if the SSE frame carrying aikonos.approval.request is dropped while the
// connection otherwise stays live, polling never starts and a server-side
// pending approval hangs invisibly forever (no client-side timeout). A single
// GET /approvals fired one poll interval after RUN_STARTED closes that gap
// cheaply: if it finds anything, enqueue + start continuous polling; if not,
// do nothing (no recurring poll spun up speculatively on every run).
export function useApprovals() {
  const queue = ref([]);          // FIFO of pending approval info objects
  const handled = new Set();      // toolCallIds already enqueued this run — cleared on run end
  let pollTimer = null;
  let oneShotTimer = null;        // safety-net timer, see module doc above
  let toastedThisSession = false; // first poll failure per polling session surfaces a toast

  const { push: toast } = useToast();

  const approval = computed(() => queue.value[0] ?? null);
  const pendingCount = computed(() => queue.value.length);

  function enqueue(info) {
    if (!info || handled.has(info.toolCallId)) return;
    handled.add(info.toolCallId);
    queue.value.push(info);
  }

  function onApprovalRequest(info) {
    enqueue(info);
    // (a): an approval event is itself the trigger to start the poll fallback.
    startPolling();
  }

  // Called from useAguiRun on RUN_STARTED. Schedules a single /approvals check
  // one interval out, in case the SSE approval-request frame is lost. A no-op
  // if continuous polling is already running or already scheduled.
  function scheduleOneShotPoll() {
    if (pollTimer || oneShotTimer) return;
    oneShotTimer = setTimeout(async () => {
      oneShotTimer = null;
      try {
        const data = await get("/approvals");
        const approvals = data.approvals ?? [];
        for (const ap of approvals) enqueue(ap);
        // Only escalate to continuous polling if the safety net actually
        // found something — an empty result means nothing to watch for yet.
        if (approvals.length > 0) startPolling();
      } catch (e) {
        // Swallow: this is a best-effort one-shot check, not the polling
        // session itself — continuous polling (if it ever starts) surfaces
        // its own failures via the toast-once path below.
        console.warn("approval one-shot poll error", e);
      }
    }, 2000);
  }

  function startPolling() {
    if (oneShotTimer) {
      clearTimeout(oneShotTimer);
      oneShotTimer = null;
    }
    if (pollTimer) return;
    toastedThisSession = false;
    pollTimer = setInterval(async () => {
      try {
        const data = await get("/approvals");
        for (const ap of (data.approvals ?? [])) {
          // /approvals returns ApprovalInfo[] — keyed by toolCallId (same as CUSTOM event).
          enqueue(ap);
        }
      } catch (e) {
        if (!toastedThisSession) {
          toastedThisSession = true;
          toast("error", "Couldn't check pending approvals — retrying…");
        }
        console.warn("approval poll error", e);
      }
    }, 2000);
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
    if (oneShotTimer) {
      clearTimeout(oneShotTimer);
      oneShotTimer = null;
    }
  }

  // Resolving (approve/deny) the current approval advances the queue to the next.
  function onApprovalClose() {
    queue.value.shift();
  }

  // Called on run end (finish/error/stop) alongside stopPolling — the handled
  // set and any stale queue entries must not leak into the next run.
  function resetApprovals() {
    queue.value = [];
    handled.clear();
    toastedThisSession = false;
  }

  onUnmounted(() => {
    stopPolling();
  });

  return {
    approval,
    pendingCount,
    onApprovalRequest,
    startPolling,
    scheduleOneShotPoll,
    stopPolling,
    onApprovalClose,
    resetApprovals,
  };
}
