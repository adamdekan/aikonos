# aikonos-docs-mcp

Streamable-HTTP MCP server that serves the Aikonos repository's Markdown documentation so an
internal agent can answer questions about Aikonos's functionality, configuration, and deployment.
Read-only. Mesh-internal only — not exposed to the public internet.

---

## Tools

| Tool | Input | Description |
|------|-------|-------------|
| `search_docs` | `query: string`, `limit?: 1–20` (default 8) | Full-text search across the corpus. Returns ranked results (score desc, path asc) with path, score, and a snippet of matching lines. |
| `list_docs` | _(none)_ | Lists all Markdown files in the corpus as `path — title` lines, sorted by path. |
| `read_doc` | `path: string` | Returns the full content of a corpus file by relative path (e.g. `docs/00-aikonos-architecture.md`). Rejects path escapes and non-`.md` files with a safe error. |

---

## Corpus mount set

The service reads a read-only bind-mount at `/corpus`:

| Host path | Mount |
|-----------|-------|
| `./docs/` | `/corpus/docs/` |
| `./README.md` | `/corpus/README.md` |
| `./ROADMAP.md` | `/corpus/ROADMAP.md` |
| `./SECURITY.md` | `/corpus/SECURITY.md` |

No persistent index or cache — every tool call reads the corpus live from disk.

---

## Start the service

```bash
docker compose --profile docs-mcp up -d --build
```

The service starts on port `8060`. Within the `aikonos_mesh` Docker network, the MCP endpoint is:

```
http://aikonos-docs-mcp:8060/mcp
```

Transport: `streamable_http`. Auth: none.

---

## Attach to an agent

Register the server in the Aikonos webui (Admin → MCP connections):

| Field | Value |
|-------|-------|
| Name | `aikonos-docs` (or any label) |
| URL | `http://aikonos-docs-mcp:8060/mcp` |
| Transport | `streamable_http` |
| Auth | none |

The broker dials the server directly over the mesh network. No changes to broker, gateway, proto,
or FGA are required — the MCP attach path is already wire-compatible.

> **On-prem:** the broker needs `AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true` to dial the RFC1918 mesh
> subnet. See `deploy/onprem/README.md` → "Attaching a self-hosted MCP server" for full setup.
