// SCIM 2.0 passthrough on the external surface (AG-16). Every request under
// /scim/v2 goes to the broker's south ScimCall with sha256(bearer token); the
// broker resolves the token within the configured tenant, applies the
// request and answers with the SCIM status and body. The raw token never
// leaves this module.
import { createHash } from "node:crypto";
import type { FastifyInstance } from "fastify";
import type { Config } from "../config.js";
import type { BrokerClients } from "../broker/clients.js";
import type { Logger } from "../log.js";

const SCIM_TYPE = "application/scim+json";

function scimError(status: number, detail: string): string {
  return JSON.stringify({
    schemas: ["urn:ietf:params:scim:api:messages:2.0:Error"],
    status: String(status),
    detail,
  });
}

export function registerScimRoutes(app: FastifyInstance, cfg: Config, clients: BrokerClients, log: Logger): void {
  void app.register(
    async (scope) => {
      // SCIM clients send application/scim+json; keep every body raw for the broker.
      scope.removeAllContentTypeParsers();
      scope.addContentTypeParser("*", { parseAs: "buffer" }, (_req, body, done) => done(null, body));

      scope.all<{ Params: { "*": string } }>("/*", async (req, reply) => {
        const m = /^Bearer (\S+)$/.exec(req.headers.authorization ?? "");
        if (!m) {
          return reply.code(401).type(SCIM_TYPE).send(scimError(401, "bearer token required"));
        }
        const q = req.url.indexOf("?");
        try {
          const resp = await clients.south.scimCall({
            tenantId: cfg.defaultTenantId,
            tokenHashHex: createHash("sha256").update(m[1], "utf8").digest("hex"),
            method: req.method,
            path: "/" + (req.params["*"] ?? ""),
            query: q >= 0 ? req.url.slice(q + 1) : "",
            body: Buffer.isBuffer(req.body) ? req.body : Buffer.alloc(0),
          });
          reply.code(resp.status);
          if (resp.body.length === 0) return reply.send();
          return reply.type(SCIM_TYPE).send(Buffer.from(resp.body));
        } catch (err) {
          log.warn({ err: String(err) }, "scim: broker call failed");
          return reply.code(503).type(SCIM_TYPE).send(scimError(503, "provisioning unavailable"));
        }
      });
    },
    { prefix: "/scim/v2" },
  );
}
