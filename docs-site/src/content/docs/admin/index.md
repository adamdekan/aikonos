---
title: Administration overview
description: Who sees the Admin section, what each admin area covers, and how admin access is enforced.
sidebar:
  order: 0
---

The Admin section is visible only to tenant admins. It appears as its own collapsible section
in the sidebar, below your regular workspace navigation, and remembers whether you last left it
expanded or collapsed. If your account is not a tenant admin, you will not see it at all.

## The nine admin areas

- **[Access Control](/admin/access-control/)** - manage who has access to what: members, per-user
  and per-group tool and skill grants, agent ownership, provisioning rules, and raw relationship
  tuples.
- **[Tools](/admin/tools/)** - edit the tool vocabulary itself (effect class, description,
  enabled) and register custom tools backed by an MCP server.
- **[Skill bundles](/admin/skill-bundles/)** - author and manage skill bundles, including keyword
  auto-load triggers.
- **[MCP servers](/admin/mcp/)** - register MCP servers so their tools can be granted to agents.
- **[Agents](/admin/agents/)** - create and configure agents: identity, model and provider,
  skills, approval mode, personality, direct access, and API keys.
- **[LLM providers](/admin/providers/)** - configure the LLM providers Aikonos can use, their
  dialects, pricing, and defaults.
- **[Scheduled runs](/admin/runs/)** - an org-wide, read-only view of every user's scheduled
  runs.
- **[Policy & audit](/admin/policy-audit/)** - the live audit stream, historical audit search, a
  per-task decision trace, a policy simulator, and alert history.
- **Settings** - three pages covering [governance](/admin/settings-governance/),
  [network & limits](/admin/settings-limits/), and [integrations](/admin/settings-integrations/).

## Every admin page fails closed

If you reach an admin page or route without tenant-admin access, whether through the sidebar or
a direct link, the page shows "You are not a tenant admin." instead of an error or a partial
view. This check happens on the server every time, not only when the sidebar first decides
whether to show you the Admin section.

Because of that, whether an admin link appears in your sidebar is a convenience, not the actual
security boundary. Hiding a control from someone who lacks access is about not confusing them,
not about preventing the underlying action. The same principle applies to ordinary workspace
navigation, described in the [User Guide](/guides/).

## The "authorization service disabled" banner

Some admin pages, most visibly Access Control, can show a banner explaining that the
authorization service is disabled and the deployment is running in an allow-all mode. This is
not something you fix from inside the admin UI: it is a property of how your Aikonos deployment
was set up, not an admin setting. If you see it, contact whoever operates your Aikonos
deployment.
