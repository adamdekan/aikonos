// gRPC channel credentials for talking to the broker over SPIFFE mTLS.
//
// Host/dev mode (current): file-based mTLS using a minted agent-gateway SVID
// (cert/key) + the SPIRE trust bundle (ca). SPIRE SVIDs carry a URI SAN, but
// gRPC verifies the server by DNS hostname, so we override the target name to
// the broker's DNS SAN (AIKONOS_BROKER_SERVER_NAME) — exactly the planner-smoke
// pattern. In-cluster, this module is the seam to swap for the SPIFFE Workload
// API (deferred, the #1 plan risk).
import { readFileSync } from "node:fs";
import { ChannelCredentials, type ClientOptions } from "@grpc/grpc-js";
import type { Config } from "../config.js";

export function mtlsCredentials(cfg: Config): ChannelCredentials {
  const ca = readFileSync(cfg.tlsCa);
  const key = readFileSync(cfg.tlsKey);
  const cert = readFileSync(cfg.tlsCert);
  return ChannelCredentials.createSsl(ca, key, cert);
}

// Channel options pinning the server name to the broker's DNS SAN so gRPC's TLS
// hostname verification passes against the SPIRE-issued server cert.
export function channelOptions(cfg: Config): Partial<ClientOptions> {
  // 16 MiB to match the broker's raised north cap — workspace file
  // upload/download carry the file bytes in a unary message (10 MiB limit).
  const MAX_MSG = 16 * 1024 * 1024;
  return {
    "grpc.ssl_target_name_override": cfg.brokerServerName,
    "grpc.default_authority": cfg.brokerServerName,
    "grpc.max_receive_message_length": MAX_MSG,
    "grpc.max_send_message_length": MAX_MSG,
  } as Partial<ClientOptions>;
}
