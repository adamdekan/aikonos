// CP3 (F9): graceful shutdown orchestration.
//
// Order (per  CP3 invariant): stop scheduler
// ticker → close HTTP surfaces (main app + external server) → drain pending
// approvals as denied → supervisor.dispose() → egressProxy.stop() → stop audit
// consumer → close broker clients → exit 0. A hard force-exit timer guards
// wedged steps; a second signal forces immediate exit(1).
//
// gracefulShutdown() is a pure function over injected handles so tests can
// assert call order and the force-exit/double-signal paths with fakes and
// fake timers — it never touches process.on itself. installShutdownHandlers()
// is the one production caller that wires it to SIGTERM/SIGINT.
import type { Logger } from "pino";

export interface ShutdownDeps {
  log: Logger;
  // undefined when the scheduler was never started (AIKONOS_SCHEDULER_ENABLED!=true).
  stopScheduler?: () => void;
  closeApp: () => Promise<void>;
  // undefined if the external surface never finished starting.
  closeExternal?: () => Promise<void>;
  approvals: { drain: (ok: boolean) => void };
  supervisor: { dispose: () => void };
  egressProxy: { stop: () => Promise<void> };
  auditConsumer: { stop: () => Promise<void> };
  clients: { north: { close: () => void }; south: { close: () => void } };
  exit: (code: number) => void;
  // Default 25s — comfortably under compose's 30s stop grace.
  forceExitMs?: number;
}

type ShutdownStep = [name: string, run: () => Promise<void> | void];

function shutdownSteps(deps: ShutdownDeps): ShutdownStep[] {
  return [
    ["scheduler", () => deps.stopScheduler?.()],
    ["http-app", () => deps.closeApp()],
    ["http-external", () => deps.closeExternal?.()],
    ["approvals", () => deps.approvals.drain(false)],
    ["supervisor", () => deps.supervisor.dispose()],
    ["egress-proxy", () => deps.egressProxy.stop()],
    ["audit-consumer", () => deps.auditConsumer.stop()],
    ["broker-clients", () => {
      deps.clients.north.close();
      deps.clients.south.close();
    }],
  ];
}

async function runShutdownSteps(deps: ShutdownDeps): Promise<void> {
  for (const [name, run] of shutdownSteps(deps)) {
    try {
      await run();
    } catch (err) {
      // One failed step must never block the rest — each is independently
      // best-effort during shutdown.
      deps.log.error({ err: String(err), step: name }, "shutdown step failed — continuing");
    }
  }
}

/** Builds the shutdown handler over injected deps. Returns a function callable per-signal; a second call while shutdown is in flight forces immediate exit(1). */
export function gracefulShutdown(deps: ShutdownDeps): (signal: string) => void {
  let shuttingDown = false;
  const forceExitMs = deps.forceExitMs ?? 25_000;

  return (signal: string) => {
    if (shuttingDown) {
      deps.log.warn({ signal }, "second shutdown signal received — forcing immediate exit");
      deps.exit(1);
      return;
    }
    shuttingDown = true;
    deps.log.info({ signal }, "shutting down");

    const forceTimer = setTimeout(() => {
      deps.log.error({ forceExitMs }, "graceful shutdown exceeded deadline — forcing exit");
      deps.exit(1);
    }, forceExitMs);
    forceTimer.unref();

    void runShutdownSteps(deps).then(() => {
      clearTimeout(forceTimer);
      deps.log.info("shutdown complete");
      deps.exit(0);
    });
  };
}

let installed = false;

/** Wires gracefulShutdown() to SIGTERM/SIGINT exactly once per process. */
export function installShutdownHandlers(deps: ShutdownDeps): void {
  if (installed) return;
  installed = true;
  const handler = gracefulShutdown(deps);
  process.on("SIGTERM", () => handler("SIGTERM"));
  process.on("SIGINT", () => handler("SIGINT"));
}
