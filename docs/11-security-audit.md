# 11 — Security & Functionality Audit (Phase 9 connectors + admin console)

> Audit date: 2026-06-04. Scope: the connector (Google Drive / OneDrive OAuth +
> Vault) and admin-console (OpenFGA role view/assign/revoke) work merged in PRs
> #7 and #8, plus the surrounding broker/gateway/web/deploy surface they touch.
> Method: multi-agent review across 8 dimensions (authz, secrets/OAuth, SSRF,
> gateway/web, deploy, input/DB, admin functionality, connector functionality)
> with adversarial verification of every finding + a build-health pass.
> Result: **51 findings confirmed** (1 critical, 6 high, 6 medium, 26 low, 12
> info), 2 refuted. Build/vet/test/gofmt/typecheck were green throughout.

## Headline

The critical + 5 of the 6 highs + 2 mediums were **one root cause**: the broker
resolved the acting principal from the client-supplied `user_id` field rather
than the verified OIDC subject. A holder of *any* valid bearer token could set
`user_id=admin@example.com` and have the tenant-admin gate evaluated against the
admin — full privilege escalation (grant self admin, edit any group, reassign
agents), and on the connector side, store/read OAuth tokens under another user's
Vault path. **Fixed** by binding the principal to the authenticated identity in
`callerIdentity` (broker/internal/broker/connectors.go), shared by the connector
and admin RPCs: when a verified OIDC identity is present, a `user_id` that
disagrees with the token is rejected; the request field is the source only in
OIDC-disabled dev mode. Regression tests cover the spoof + wildcard paths; the
in-cluster check is added to `scripts/verify-admin.sh` (step 6b).

## Fixed in this pass

| Sev | Area | Fix |
|---|---|---|
| Critical | Admin authz bypass | `callerIdentity` binds the acting user to the verified OIDC subject; rejects forged `user_id`. |
| High | Connector cross-user token write | Same `callerIdentity` fix — Vault path keyed by the authenticated principal. |
| High | Gateway reachable cluster-wide | Added `agent-gateway-ingress` NetworkPolicy: ingress :8080 only from `frontend` + `admin` pods. |
| Medium | OAuth state→user binding was unenforceable | The state↔user check is now meaningful because `user` is the verified subject. |
| Medium | Token refresh write-back errors silently dropped | Threaded a logger into `Registry`; `FreshAccessToken` warns on write-back failure (rotation-aware). |
| Low | FGA wildcard / `#`-suffix injection | `validateManagedObject` rejects `*` ids and objects carrying a relation suffix; adds 256-char field bound. |
| Low | Vault path charset | `secrets.ConnectorPath` validates each segment against a strict charset; single source for the data path + the workspace reference (de-dups `vaultPath`). |
| Low | web.fetch SSRF gap | `isDisallowedIP` now also blocks CGNAT/RFC 6598 (100.64.0.0/10), incl. v4-mapped. |
| Low | OAuth callback postMessage | Pinned `targetOrigin` to the app origin; the receiver validates `e.origin`. |
| Low | Broker NetworkPolicy missing Vault | Added port 8200 to the `aikonos-system` egress (connectors were blocked under enforcement). |
| Low | Admin/observability open ingress | Both NetworkPolicies now deny in-cluster ingress (port-forward only); admin gains resource limits + liveness probe + `.dockerignore` secrets. |
| Low | `ReadTuples` silent truncation | Warns when the page cap (20×100) is hit with more to read. |
| Low | SPA stale state on mutation 403 | `onAssign`/`onRevoke` reload on 403 so the forbidden panel never sits on stale data. |
| Low/Info | Cleanup | Removed dead `ListObjectsForUser`/`listObjects`; renamed `connectorIdentity`→`callerIdentity`; verbless `status.Errorf`→`status.Error`; refreshed `engine.go` header; fixed `provider.go` godoc; swept expired OAuth state on `Take`; warn on connect-without-refresh-token; added a gofmt gate to CI. |

## Refuted (not real)

- "Admin proxy is an open proxy to the whole gateway API" — the proxy is
  host-pinned to `GATEWAY_URL`; it forwards paths but cannot reach another host.
- "RevokeConnector passes the wrong token to Google revoke" — passing the
  refresh token is correct; Google revokes the whole grant, and revoke is
  best-effort anyway.

## Accepted residual risk / documented follow-ups (not fixed here)

These are larger, architectural, or future-multi-tenant items. They are mitigated
for the current single-node dev posture by network isolation + the identity-
binding fix above, and should be tracked before any shared/production cluster.

1. **No real admin/end-user login (dev-grade).** The gateway mints per-user OIDC
   tokens via Keycloak ROPC with a *shared* demo password, and the acting user is
   taken from the `x-aikonos-user` header. So whoever can reach the gateway/admin
   port can still obtain a genuine `admin@example.com` token. The broker fix stops
   a *non-admin token holder* from escalating, but the gateway still forges admin
   tokens on request. **Real fix:** browser auth-code+PKCE login in the
   frontend/admin SPA; the gateway forwards the user's own bearer and stops
   minting tokens. Until then the admin/observability consoles are internal tools
   behind network isolation (now enforced by the new NetworkPolicies).
2. **Multi-tenant isolation for group/skill/agent tuples.** `group:`/`skill:`/
   `agent:` objects carry no tenant component, so `validateManagedObject` can only
   pin `tenant:` objects to the home tenant. Single-tenant today (`Deps.TenantID`);
   multi-tenant needs tenant-namespaced object ids or a `tenant:` membership check
   on read + write.
3. ~~**Type-prefix OpenFGA Read unverified in-cluster.**~~ **Confirmed + fixed
   (2026-06-04).** OpenFGA v1.5.3 rejects a bare type-prefix Read (object id and
   user both empty → 400), so the per-section `group:`/`skill:`/`agent:` reads all
   failed and the console showed only tenant roles. `assembleAssignments` now does
   a single all-tuples Read + buckets by object type in-process (`roles.go`), and
   `fgaClient.read` omits `tuple_key` on an empty filter (`fga.go`). Verified live:
   all four sections populate, no warnings. `verify-admin.sh` also fixed (it used
   plaintext grpcurl against the mTLS+OIDC north port and a `pipefail`-masked grep,
   so it had never actually run) — now 3/3 deny checks pass incl. user_id spoofing.
4. **Connector state store is in-memory** ⇒ single-replica only (DB-backed
   migration 004 is the HA follow-up; the Deployment is pinned to `replicas: 1`).
5. **OneDrive has no per-token revoke**; Google revoke is best-effort. Surface in
   the UI that OneDrive access persists until expiry.
6. **Hardcoded dev credentials** in committed manifests (Keycloak/DB passwords) —
   source from Vault/External Secrets before any shared cluster; the seeded
   `admin@example.com` must be dev-only.

## Verification

`go build/vet/test ./broker/...` (incl. `-race`), `gofmt -l broker/`, gateway
`tsc --noEmit`, `admin/web` + `frontend` builds, and the admin proxy smoke all
pass. New regression tests: `TestAssignRole_RejectsUserIdSpoofing`,
`TestListAssignments_RejectsUserIdSpoofing`,
`TestConnectorIdentity_RejectsUserIdMismatch`, `TestAssignRole_RejectsWildcardSubject`.
