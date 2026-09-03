// Holds the north + south broker gRPC clients. The SPIFFE SVID certs are static
// file mounts (dev-CA locally, company cert on-prem) with no runtime rotation,
// so the clients are built once at construction. Consumers (GovernanceBridge,
// REST endpoints) read `.north` / `.south` per call.
import type { Config } from "../config";
import { NorthClient } from "./north";
import { SouthClient } from "./south";

/** Holds the north + south broker gRPC clients. */
export class BrokerClients {
  north: NorthClient;
  south: SouthClient;

  constructor(cfg: Config) {
    this.north = new NorthClient(cfg);
    this.south = new SouthClient(cfg);
  }
}
