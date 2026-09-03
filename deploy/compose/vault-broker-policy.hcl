# Least-privilege Vault policy for the Aikonos broker (AppRole "aikonos-broker").
#
# The broker authenticates with an AppRole bound to THIS policy — never the root
# token. It grants access to exactly the KV-v2 path families the broker uses
# and nothing else: no sys/, no other secrets, no token/auth management.
# (KV-v2 splits a logical path into data/<p> for values and metadata/<p> for
# version metadata + recursive delete.)
#
# Provisioned by scripts/compose-vault-seed.sh. Keep this the single source of
# truth for the broker's Vault grants.

# Broker-owned signing keys — each a single fixed-path shared key (no per-tenant
# suffix, so a wildcard would be broader than the broker ever needs), read-or-
# created on first boot (cas=0 create, then read). No delete: keys must never be
# removable at runtime. No metadata/broker/* grant: nothing under this family is
# ever version-deleted (unlike workspaces/mcp/providers/m365/websearch below).
# Exact paths per broker/internal/secrets/vault.go's *KeyLogical constants.
path "secret/data/broker/capability" {           # Biscuit capability root key
  capabilities = ["create", "read", "update"]
}
path "secret/data/broker/gateway-grant" {        # owner-grant HMAC signing key
  capabilities = ["create", "read", "update"]
}
path "secret/data/broker/audit-signing-key" {    # WORM audit chain signing key
  capabilities = ["create", "read", "update"]
}
path "secret/data/broker/workspace-session-key" { # workspace session signing key
  capabilities = ["create", "read", "update"]
}

# Per-user connector OAuth tokens (Google Drive / OneDrive): read JIT, write on
# refresh-rotation, delete on revoke.
path "secret/data/workspaces/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/workspaces/*" {
  capabilities = ["read", "delete", "list"]
}

# Per-connection MCP server bearer tokens: write on add, read JIT, delete on remove.
path "secret/data/mcp/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/mcp/*" {
  capabilities = ["read", "delete", "list"]
}

# Per-tenant LLM provider API keys (F4): write on admin upsert + seed, read JIT
# at session build, delete on provider removal.
path "secret/data/providers/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/providers/*" {
  capabilities = ["read", "delete", "list"]
}

# Per-tenant M365 (Entra OBO) app client secret:
# write on admin upsert, read JIT for the OBO exchange + has_secret probe,
# delete on connection removal.
path "secret/data/m365/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/m365/*" {
  capabilities = ["read", "delete", "list"]
}

# Per-tenant web.search engine API key: write on
# admin upsert, read JIT at web.search invocation + has_key probe, delete on
# config removal.
path "secret/data/websearch/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/websearch/*" {
  capabilities = ["read", "delete", "list"]
}
