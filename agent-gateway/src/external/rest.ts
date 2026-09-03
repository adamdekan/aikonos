// REST adapter for the external surface.
// POST /v1/agents/:id/invoke → SSE stream (always; no JSON response mode).
//
// Request body:
//   {
//     prompt: string,                                            // required, ≤16 KiB
//     history?: Array<{ role: "user" | "assistant", content: string }>,
//     // optional prior turns, ≤100 entries, ≤200 KiB total content bytes.
//     // Forwarded verbatim to seed the lazily-created thread session — the
//     // client owns conversation state and replays it per call.
//   }
//
// SSE event shapes (one `data: <json>\n\n` frame per event):
//   { type: "text_delta", delta: string }
//   { type: "tool_start", toolCallId: string, toolName: string }
//   { type: "tool_end", toolCallId: string, ok: boolean, result: string }
//   { type: "usage", inputTokens: number, outputTokens: number }
//   { type: "done" }
//   { type: "error", error: string }                              // trimmed, user-facing
//
// Prompt cap: 16 KiB. Exceeding it returns 413.
// History caps: not an array, a bad role/content shape, >100 turns, or >200 KiB
// of total content → 400. Body cap (256 KiB, server.ts) return Fastify's own 413.
// Auth: authenticateApiKey middleware (Bearer tk_…). 401 on miss/revoke.
// Agent must have gateway_enabled=true. 403 otherwise (deny-by-default).
// Agent must have approvalMode:auto. 409 otherwise (pre-session, no session built).
// URL :id must match the key-resolved agentId. 409 if they disagree.
import type { FastifyInstance } from "fastify";
import type { Config } from "../config.js";
import type { Logger } from "../log.js";
import type { BrokerClients } from "../broker/clients.js";
import type { AgentSpec } from "../pi/session.js";
import type { ChildSupervisor } from "../ipc/supervisor.js";
import { trimmedErrorMessage } from "../http-errors.js";
import { validateHistory } from "../history-validation.js";
import { authenticateApiKey } from "./auth.js";
import { runAgentInvocation } from "./core.js";

const PROMPT_MAX_BYTES = 16 * 1024; // 16 KiB

interface InvokeBody {
  prompt: string;
  history?: unknown;
}

interface InvokeParams {
  id: string;
}

export function registerExternalRoutes(
  app: FastifyInstance,
  cfg: Config,
  clients: BrokerClients,
  log: Logger,
  supervisor: ChildSupervisor,
): void {
  app.post<{ Params: InvokeParams; Body: InvokeBody }>(
    "/v1/agents/:id/invoke",
    async (req, reply) => {
      // 1. Auth: resolve the API key.
      const authHeader = req.headers.authorization;
      const auth = await authenticateApiKey(authHeader, cfg.defaultTenantId, clients.south, log);
      if (!auth.ok) {
        return reply.code(auth.status).send({ error: auth.message });
      }

      const { agentId, tenantId, principal, ownerGrant } = auth.principal;

      // 2. URL :id must agree with the key-resolved agentId. The resolved agent
      //    is authoritative; a mismatch means the caller supplied the wrong URL.
      if (req.params.id !== agentId) {
        return reply.code(409).send({
          error: `URL agent id '${req.params.id}' does not match key-resolved agent '${agentId}'`,
        });
      }

      // 3. Prompt size cap.
      const prompt = req.body?.prompt ?? "";
      if (Buffer.byteLength(prompt, "utf8") > PROMPT_MAX_BYTES) {
        return reply.code(413).send({ error: "prompt exceeds 16 KiB limit" });
      }

      // 3b. History shape + size caps.
      const historyValidation = validateHistory(req.body?.history);
      if (!historyValidation.ok) {
        return reply.code(400).send({ error: historyValidation.error });
      }
      const { history } = historyValidation;

      // 4. Load agent spec and check approvalMode.
      let agentSpec: AgentSpec;
      try {
        const resp = await clients.south.getAgentSpec({ agentId, tenantId });
        agentSpec = {
          model: resp.llmModel,
          approvalMode: resp.approvalMode,
          skills: resp.skills,
          preferredProvider: resp.preferredProvider,
          allowedProviders: resp.allowedProviders,
          soul: resp.soul ?? "",
          gatewayEnabled: resp.gatewayEnabled,
        };
      } catch (err) {
        log.warn({ err: String(err), agentId }, "getAgentSpec failed");
        return reply.code(502).send({ error: "agent spec unavailable" });
      }

      if (!agentSpec.gatewayEnabled) {
        return reply.code(403).send({ error: "external access not enabled for this agent" });
      }

      if (agentSpec.approvalMode !== "auto") {
        return reply.code(409).send({ error: "agent requires manual approval; not externally drivable" });
      }

      // 5. Stream SSE.
      reply.raw.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      });

      const ownerUserId = `svc-${agentId}`;
      try {
        const iter = runAgentInvocation(agentId, ownerUserId, tenantId, agentSpec.skills, prompt, {
          clients,
          cfg,
          log,
          supervisor,
        }, agentSpec, ownerGrant, history);
        for await (const ev of iter) {
          reply.raw.write(`data: ${JSON.stringify(ev)}\n\n`);
          if (ev.type === "done" || ev.type === "error") break;
        }
      } catch (err) {
        log.error({ err, agentId }, "external invocation failed");
        reply.raw.write(`data: ${JSON.stringify({ type: "error", error: trimmedErrorMessage(err) })}\n\n`);
      } finally {
        reply.raw.end();
      }
    },
  );
}
