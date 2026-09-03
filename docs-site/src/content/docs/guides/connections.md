---
title: Connections
description: Linking, checking the status of, and revoking your Google Drive and OneDrive connections.
sidebar:
  order: 3
---

Connections is where you link outside services to your Aikonos account so an agent can read
and write those drives on your behalf. See [Connections](/concepts/connections/) for what a
connection actually grants an agent.

## When this page appears

Connections only shows up in the sidebar if your organization has configured at least one
connector provider. If none are configured, there is nothing to connect and the page (and its
sidebar entry) does not appear at all.

## Connection status

Each connected provider shows a status, color-coded: connected, or reconnect needed.
Reconnect needed means the access Aikonos has for that drive has expired or been revoked on
the provider's side, not that anything in your workspace has changed.

## Connecting an account

For any provider your organization has configured that you have not yet connected, a
"Connect \<Provider>" button appears under "Add connection." Clicking it redirects you to
that provider's own consent page, where you sign in and approve access exactly as you would
for any other app requesting permission to your account.

## Revoking a connection

A connected provider shows a Revoke button next to its status. Revoking removes Aikonos's
access to that drive immediately. It does not affect your Aikonos account or anything already
saved in your workspace.

## Organization-managed connections

If your organization has configured OneDrive centrally rather than leaving it to individual
users, the row shows a "Managed by your organization" badge instead of Connect or Revoke.
There is nothing for you to manually link or unlink in that case: your access to OneDrive
still depends on your own sign-in, but the connection itself is set up and maintained at the
organization level. See [Connections](/concepts/connections/) for how organization-managed
OneDrive differs from a personal connection.

## Empty and error states

If you have no connected accounts, the page shows "No connected accounts." An error banner
appears if loading, connecting, or revoking a connection fails.
