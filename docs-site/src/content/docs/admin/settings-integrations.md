---
title: "Settings: integrations"
description: Connector Allowlist, Microsoft 365, Web Search, and Observability status.
sidebar:
  order: 11
---

This page covers four tabs on Settings (`/admin/settings`): Connector Allowlist, Microsoft 365,
Web Search, and Observability. See [Settings: governance](/admin/settings-governance/) and
[Settings: network & limits](/admin/settings-limits/) for the other Settings tabs.

## Connector Allowlist

A single "Restrict connectors to the allowlist" toggle, plus a checklist of the two fixed
connector providers: Google Drive and OneDrive. When restriction is on, only the checked
providers can have a new connection linked; an existing personal connection to an unchecked
provider is not retroactively broken, but no new one can be started. This allowlist has no
effect on MCP servers - those are governed separately, on [MCP servers](/admin/mcp/).

:::caution
Turning restriction on with zero providers checked means nobody in the organization can link a
new personal connection at all. The page warns you if you save it in that state.
:::

## Microsoft 365

Configures tenant-wide OneDrive access through your organization's own Microsoft 365 tenant,
instead of leaving OneDrive to each person's personal connection. See
[Connections](/concepts/connections/) for what this changes from a user's point of view, and
[Working folder](/guides/working-folder/) for how a tenant-wide OneDrive becomes someone's
working folder.

The form takes an Entra tenant ID, an Application (client) ID, and a client secret. The secret
is write-only: once saved, its field shows a placeholder indicating a secret is already stored,
rather than the secret itself. Leaving the field blank when you save preserves the stored secret,
so you don't have to re-enter it just to change the tenant or client ID. A help box lists the
exact Microsoft Graph permissions this integration needs and explains common AADSTS error codes
you might see while setting it up.
**Test connection** runs a real exchange against Microsoft and reports back the actual failure
reason if it doesn't work, rather than a generic error.

:::caution
Disconnect removes the tenant-wide configuration entirely and asks you to confirm first: doing
so stops OneDrive access for every user in the organization who was relying on this tenant-wide
connection, not just for one person.
:::

## Web Search

Configures the `web.search` tool's backing engine: a select for Brave, Exa, or Tavily, a Max
results field, and a write-only API key field (following the same placeholder pattern as the
Microsoft 365 secret). Leaving the API key field blank when you save preserves the key already
stored. Test connection probes the configured engine without changing anything.
Delete removes the configuration entirely and asks for confirmation; once deleted, `web.search`
is unavailable to every agent until it's configured again.

## Observability

A read-only status display: whether telemetry export is enabled or disabled, and the configured
endpoint. This page never writes anything - changing telemetry configuration is done by whoever
operates your Aikonos deployment, not from inside this admin page.
