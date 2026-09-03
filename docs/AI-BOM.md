# AI Bill of Materials

Generated: 2026-07-03T22:04:21Z — git 67a8043

Committed artifact; regenerate at release with `scripts/generate-ai-bom.sh`. Contains no key material — provider
rows carry a `Has Key` boolean only, never key bytes/values.

## LLM providers

_Providers are per-deployment Postgres (`llm_providers`) runtime state and are_
_deliberately NOT pinned in this committed artifact — dev/test fixture rows would_
_otherwise leak into a canonical BOM._

_Regenerate against a live deployment with `scripts/generate-ai-bom.sh --live-db`_
_(requires a reachable compose stack's `postgres` service) before treating this_
_section as current for that environment._

## Skill bundles

| Skill | Version | Effect Class | SBOM Ref |
|---|---|---|---|
| doc.write | 0.1.0 | write_local | — |
| email.draft | 0.1.0 | write_external | — |
| web.fetch | 0.1.0 | read_only | — |

## Versions

| Component | Version |
|---|---|
| Pi harness (agent-gateway) | 0.1.0 |
| Broker | 67a8043 |
