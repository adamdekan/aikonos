---
title: Skills & tools
description: What tools and skill bundles are, and how access to them is granted.
sidebar:
  order: 2
---

A tool is a single action an agent can take: reading a file, creating a document, fetching a
web page, searching a connected drive. Every tool an agent might call has to be granted
before it can be used. Access is deny-by-default: unless your account or a group you belong
to has been granted a tool, no agent acting on your behalf can call it, regardless of what
the model itself is capable of.

Most tool grants happen at the group level. Your administrator adds you to a group such as
"security-team" or "content-writers," and that group's tool grants become yours. If you see
"You do not have access to tool ..." in chat, the fix is not something you can do yourself:
ask your administrator to add you to a group that grants it, or to grant it to you directly.

## Skill bundles

A skill bundle packages instructions and a set of allowed tools around a specific job, for
example drafting a particular kind of report or working with a particular data source. You
select a bundle in chat by typing `/` followed by its name, which activates it for that
conversation. Bundles are assigned to you through groups, the same way individual tools are.

## Keyword auto-load

Some skill bundles are set up to load automatically when your message contains certain
keywords, without you having to type a `/` command. When that happens, chat shows a skill
timeline entry before the agent's reply: a short line naming the bundle that loaded, with a
description. If a matching bundle was suppressed instead of loaded, for example because an
administrator configured it not to auto-activate, the entry explains why. Either way, you see
exactly what got added to the conversation and why.

## Related pages

- [Agents & tasks](/concepts/agents-and-tasks/) covers what an agent does with the tools it
  is granted.
- [Governance & audit](/concepts/governance-and-audit/) covers how tool calls are checked at
  the moment they happen, not just at grant time.
- See the [Chat guide](/guides/chat/) for the `/` skill palette and the skill timeline in the
  composer.
- Administrators: see [Access Control](/admin/access-control/) for granting tools and skills
  to users and groups, [Tools](/admin/tools/) for the tool vocabulary itself, and
  [Skill bundles](/admin/skill-bundles/) for authoring and keyword configuration.
