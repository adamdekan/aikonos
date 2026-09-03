# 10 — Enabling Enforcement

> **Purpose.** The platform's auth layers — real OIDC validation, OpenFGA ReBAC,
> Biscuit capability tokens, and NATS event routing — are **config-gated**. Out of
> the box the compose `core` profile wires OIDC + NATS on, but the broker starts in
> the **OpenFGA dev allow-all stub** until you seed the store. This runbook turns
> full enforcement **on** and validates it end-to-end with
> [`scripts/compose-verify.sh`](../scripts/compose-verify.sh) (`task compose:verify`).
>
> Do this *before* any user-facing testing: you don't want a user to be the first
> thing that ever exercises auth against the running broker.

---

## What "enforcement on" means

| Feature | Off | On |
|---|---|---|
| OIDC | `AIKONOS_OIDC_ISSUER=""` → interceptor passes through | issuer set (`.env`) → bearer token required + validated against JWKS |
| ReBAC | `AIKONOS_POLICY_OPENFGA_STORE_ID=""` → `CheckFGA` allow-all stub | store id set → real OpenFGA Check, fails closed |
| Capability | always on — broker always builds a minter | set a persistent root key (else read-or-created in Vault, ephemeral on Vault restart) |
| NATS | `AIKONOS_NATS_URL=""` → bus disabled, `StreamTaskEvents` Unimplemented | url set → events published, streaming works |

The compose `.env.local.example` ships OIDC (`http://keycloak:8080/realms/aikonos`) and NATS
(`nats://nats:4222`) **on**. The one knob you must flip yourself is OpenFGA: the store
id is empty until `scripts/compose-seed-openfga.sh` writes it back to `.env`.

Capability enforcement is **always active**: `InvokeTool` requires a valid Biscuit and
`SendEnvelope` mints/attenuates one. The only knob is whether the root key persists.

---

## Prerequisites

- Proto stubs generated on the host (`(cd agent-gateway && npm ci)` then `task proto:gen`).
- The `core` stack up (`task compose:up`).
- For the seed script: the [`fga` CLI](https://github.com/openfga/cli)
  (`go install github.com/openfga/cli/cmd/fga@latest`) + `jq`.

Keycloak and OpenFGA are part of the `core` profile — no separate deploy step. The
realm import (`deploy/compose/keycloak-realm.json`) gives the access token issuer
`http://keycloak:8080/realms/aikonos`, an `aud=aikonos-broker` claim, and a `tenant_id`
claim — exactly what the broker's validator checks. The OpenFGA schema is migrated by
the `openfga-migrate` one-shot; the Postgres datastore (`postgres-data` volume) makes
the store durable across restarts.

---

## Step 1 — Seed the OpenFGA store, model, and tuples

```bash
task compose:seed
```

This wraps five steps:
1. `scripts/compose-dev-ca.sh` — mint the dev-CA + leaf certs (idempotent).
2. `scripts/compose-vault-seed.sh` — provision the broker's Vault AppRole
   (enables approle, writes least-privilege policy, injects role_id/secret_id
   into `.env`). Re-run after any Vault restart (dev Vault is in-memory).
3. `scripts/compose-seed-openfga.sh` — find-or-create the store, write
   `policies/fga/model.fga` + (optionally) dev-seed tuples, and set
   `AIKONOS_POLICY_OPENFGA_STORE_ID` in `.env`. Dev-seed tuples (alice/bob)
   are written only when `AIKONOS_SEED_DEMO_TUPLES=1`; the script passes this
   flag when invoked via `task compose:seed` (local dev default). On-prem /
   production leave it unset — the store and model are seeded without demo accounts.
4. `scripts/compose-seed-skill-bundles.sh` — upload bundled first-party skills.
5. `docker compose up -d broker` — recreate the broker so it reads the new store id
   and switches from allow-all stub to live ReBAC.

The dev-seed tuples (when present) make `alice`/`bob` tenant members and peers,
put both in a `memory-users` group granting `skill:memory.read` +
`skill:memory.write` (the agent-memory tools), put both in a `skill-authors`
group granting `skill:personal-skills` (author/import/transfer own skills), and the broker
**writes** `user:<owner> owner|approver task:<id>` tuples on task creation, so
`can_approve` resolves for freshly-created tasks and `SendEnvelope`'s ReBAC Check
passes with OpenFGA on. The store survives container restarts (Postgres datastore),
so this is a one-time step per fresh `postgres-data` volume.

---

## Step 2 — Confirm the broker is enforcing

```bash
docker compose logs broker | grep -Ei "OIDC validation enabled|Event bus connected|openfga_mode"
```

You should see `OIDC validation enabled`, `Event bus connected`, and
`openfga_mode: live (store …)`. If you instead see the allow-all stub, re-run
`task compose:seed`.

---

## Step 3 — Run the smoke test

```bash
task compose:verify
```

`scripts/compose-verify.sh` drives the published localhost ports (no `kubectl
port-forward`): it checks the webui + gateway are reachable, the broker's north/south
gRPC ports are open, the broker is in live OpenFGA mode, and exercises the governed
flow. It prints `✓`/`✗` per check and exits non-zero on any failure.

| Check | Asserts |
|---|---|
| webui reachable | `GET http://localhost:4200/` → 200 |
| gateway healthz | `GET http://localhost:8080/healthz` → 200 |
| broker ports | north (`9090`) + south (`9091`) mTLS ports listening |
| OpenFGA live | broker log shows `openfga_mode … live` (not the stub) |
| OIDC | unauthenticated `CreateTask` → `Unauthenticated`; valid bearer → task created |
| approval | `SubmitPlan` with a `write_external` step → `NEEDS_HUMAN` |
| ReBAC | `ApproveTask` by an un-related user → `PermissionDenied`; by the owner → `APPROVED` |
| capability | `SendEnvelope` of an un-held scope → `PermissionDenied`; held scope → token minted |
| NATS | `StreamTaskEvents` delivers a `STATUS_CHANGED` event after `EmitStatus` |

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `invalid bearer token` in broker logs | Token `iss` ≠ `AIKONOS_OIDC_ISSUER`. Confirm `KC_HOSTNAME=keycloak` so the issuer URL matches the token's `iss` exactly (set in `compose.yaml`). |
| `claims invalid: aud` | The `aud=aikonos-broker` mapper didn't apply — re-check `deploy/compose/keycloak-realm.json`; the audience must equal `AIKONOS_OIDC_AUDIENCE`. |
| ReBAC always denies | Store/model/tuples not seeded, or store id stale in `.env` — re-run `task compose:seed`. |
| ReBAC always allows (stub) | `AIKONOS_POLICY_OPENFGA_STORE_ID` empty — `task compose:seed` writes it and recreates the broker. |
| `StreamTaskEvents` returns `Unimplemented` | `AIKONOS_NATS_URL` unset in `.env` — the bus is disabled. |
| password grant returns `invalid_grant: "Account is not fully set up"` | Keycloak 24's *Verify Profile* required action — the realm import sets `firstName`/`lastName` + `requiredActions: []` to avoid it. |
| broker mTLS handshake fails | dev-CA certs missing or service renamed — re-run `bash scripts/compose-dev-ca.sh`; the cert DNS-SAN must equal the compose service name (`broker`/`agent-gateway`). |

---

See also: `docs/02-policy-model.md`
(Rego/FGA reference), `docs/OPS-RUNBOOK.md` (daily ops), `deploy/compose/README.md`
(full compose operator guide).
