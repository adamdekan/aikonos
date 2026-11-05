---
title: Connections
description: Linking your Google Drive or OneDrive account so agents can read and write those drives on your behalf.
sidebar:
  order: 5
---

A connection links your Aikonos account to an outside service, currently Google Drive and
OneDrive, using your own credentials through that service's normal sign-in and consent flow.
Once connected, an agent acting on your behalf can read from and write to that drive, subject
to the same tool-level permission checks as any other action.

## Connection status

Each connection shows a status: connected, or reconnect needed. A connection needs
reconnecting when the underlying access has expired or been revoked on the provider's side,
for example after a long period of inactivity. When that happens, the fix is to reconnect
through the same flow you used the first time; nothing about your workspace or files changes
while the connection is stale, only the ability to reach that particular drive.

## Personal connections

Only services your organization has set up show up as connectable, on a page listing one
"Connect" button per available provider. Once a personal connection exists, you can revoke it
yourself at any time from that same page, which removes Aikonos's access to that drive
immediately; nothing else about your account or workspace is affected. If no provider has
been configured for your organization, the connect option does not appear at all.

## Organization-managed OneDrive

Your organization can also configure OneDrive centrally for everyone, instead of leaving it
to each person to connect individually. When that is set up, your OneDrive access is managed
by your organization rather than by a personal link you created: the Connections page shows a
"Managed by your organization" badge in place of a Connect or Revoke button, because there is
nothing for you to manually connect or disconnect. Your OneDrive still only becomes reachable
through your own sign-in; the organization-wide setup does not give anyone standing access to
your files.

## Related pages

- [Workspace & files](/concepts/workspace-and-files/) covers how a connected OneDrive
  account can become your working folder.
- See the [Connections guide](/guides/connections/) for the connect and revoke controls in
  detail.
- Administrators: see [Settings - integrations](/admin/settings-integrations/) for
  configuring the organization-wide Microsoft 365 connection and the connector allowlist.
