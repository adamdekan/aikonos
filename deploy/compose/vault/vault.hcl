# deploy/compose/vault/vault.hcl
#
# Durable-storage Vault config shared by every compose variant — the azure/onprem
# prod overlays AND local dev
#.
#
# No secrets here — only topology. Init/unseal differs by variant: local dev is
# auto-unsealed by the `vault-init` sidecar (AIKONOS_VAULT_AUTO_UNSEAL=true);
# azure/onprem unseal manually so keys never touch disk (docs/OPS-RUNBOOK.md
# "Vault operations — first boot").

# Path is /vault/file, not /vault/data: the hashicorp/vault image only auto-
# chowns /vault/file (+ /vault/config, /vault/logs) to the vault user in its
# entrypoint — a custom path left root-owned and Vault (non-root) can't write
# the keyring to it (verified: `mkdir /vault/data/core: permission denied`).
# The named volume is still called `vault-data` externally (backup/docs
# contract); only the in-container mount target follows the image's own
# convention.
storage "file" {
  path = "/vault/file"
}

# Same in-network reachability surface as dev-mode: plain HTTP on 0.0.0.0:8200
# (AIKONOS_VAULT_ADDR=http://vault:8200 in every .env.*.example is unchanged).
# The compose `backend` network is not internet-reachable; TLS termination for
# any Vault UI/API exposed further is out of scope for this checkpoint.
listener "tcp" {
  address     = "0.0.0.0:8200"
  tls_disable = 1
}

# cap_drop: ALL (hardening convention) rules out IPC_LOCK for mlock — disable
# it here instead of granting the capability back.
disable_mlock = true

# In-network name, not loopback: single-node file storage means this is only
# used for client redirects (e.g. a CLI following a redirect to the active
# node) — a redirect target of 127.0.0.1 is wrong for any client dialing the
# service by its compose DNS name (broker, seed script from another container).
api_addr = "http://vault:8200"
ui       = false
