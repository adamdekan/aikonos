---
title: MCP servers
description: Registering and managing the MCP servers whose tools become available to agents.
sidebar:
  order: 4
---

MCP servers (`/admin/mcp`) is where you register the external MCP servers Aikonos can reach.
Registering a server doesn't grant anything by itself - it makes that server's tools *eligible*
to be added to the tool vocabulary on the [Tools](/admin/tools/) page and, from there, granted
to agents through [Access Control](/admin/access-control/). Three admin surfaces work together
here: this page registers the server itself, Tools classifies its individual tool ids, and
Access Control decides who and what can actually invoke them.

## Adding or editing a server

The form takes:

- **Name** - a label for the server.
- **URL** - where the server is reachable.
- **Transport** - `streamable_http` or `sse`.
- **Auth type** - `none` or `bearer`. Choosing `bearer` reveals a token field.

When editing an existing server, leaving the token field blank preserves the token already on
file - you don't have to re-enter a working credential just to change the name or URL.

## Managing existing servers

The table lists every registered server with per-row Edit and Delete.

## Transport and auth

Transport tells Aikonos how to talk to the server: `streamable_http` for a plain HTTP-based MCP
server, or `sse` for one that streams over server-sent events. Auth type controls what
credential, if any, gets sent with every call to that server: `none` sends nothing, and `bearer`
sends the token you configured with every request.

## Related pages

- [Tools](/admin/tools/) is where a registered server's individual tools get added to the tool
  vocabulary and classified.
- [Access Control](/admin/access-control/) is where an MCP-backed tool actually gets granted to
  users, groups, and agents once it exists in the vocabulary.
- [Skills & tools](/concepts/skills-and-tools/) covers what a tool grant means from a user's
  point of view.
