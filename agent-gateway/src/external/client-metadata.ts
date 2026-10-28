// AG-09: the aikonOS OAuth client metadata document (CIMD). An MCP
// authorization server that supports client_id_metadata_document fetches this
// URL — which is itself the client_id — to learn our redirect URI, so no
// per-server registration is needed. Point the broker's
// connectors.mcp_client_metadata_url at <externalBaseUrl>/oauth/client-metadata.json.
// Not mounted unless both the public base URL and the connector redirect URI
// are configured.
import type { FastifyInstance } from "fastify";
import type { Config } from "../config.js";

export const CLIENT_METADATA_PATH = "/oauth/client-metadata.json";

export function registerClientMetadataRoute(
  app: FastifyInstance,
  cfg: Pick<Config, "externalBaseUrl" | "connectorsRedirectUri">,
): void {
  if (!cfg.externalBaseUrl || !cfg.connectorsRedirectUri) return;
  const doc = {
    client_id: `${cfg.externalBaseUrl}${CLIENT_METADATA_PATH}`,
    client_name: "aikonOS",
    redirect_uris: [cfg.connectorsRedirectUri],
    grant_types: ["authorization_code", "refresh_token"],
    response_types: ["code"],
    token_endpoint_auth_method: "none",
  };
  app.get(CLIENT_METADATA_PATH, async (_req, reply) => {
    reply.header("cache-control", "public, max-age=3600").send(doc);
  });
}
