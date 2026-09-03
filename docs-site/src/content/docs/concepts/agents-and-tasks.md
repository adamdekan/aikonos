---
title: Agents & tasks
description: What a Aikonos agent is, how it handles a task, and how approvals work.
sidebar:
  order: 1
---

An agent is the assistant you talk to in Aikonos. Your organization sets up each agent with a
name, a model that powers its responses, and the skills and tools it is allowed to use. An
agent can also have a personality: instructions that shape its tone and focus, which you can
edit yourself if your administrator has made the agent's personality editable. Some agents
also have a preferred model provider, used instead of your organization's default when one is
set.

You are assigned to one or more agents, either directly or through a group you belong to.
Each assigned agent shows up as its own entry in the sidebar, so a research agent and a
scheduling agent, for example, stay in separate conversations.

## What happens when you send a message

When you send a message, the agent does not just generate text. It works out a plan: what it
needs to do and which tools it needs to call to do it. As it acts, it streams its reply back
to you turn by turn, and you can see the tools it invokes along the way.

## Approvals

Not every action an agent wants to take runs immediately. Low-risk actions, like reading a
file you already have open, usually proceed right away once the permission check passes.
Riskier actions, such as sending something externally or making a destructive change, pause
and show you an approval prompt before they run.

The approval prompt shows which tool the agent wants to call, a `STEP-UP` or `HUMAN` pill
describing how serious the request is, and the reason the agent gave for wanting to do it.
You can expand the arguments the agent plans to send. For the more sensitive requests, the
Approve button stays disabled until you confirm you have reviewed those arguments.

Deny is the button that has focus by default, and pressing Esc also denies the request. That
is deliberate: if you are not sure, the safe path is to say no rather than approve by
accident. Approving lets the specific action through; denying stops it without ending your
conversation.

## Unattended mode

Some agents can run in an unattended, or auto-approve, mode, where actions that would
normally need your sign-off proceed on their own. This is a per-agent setting your
administrator configures, and it trades convenience for exposure: an agent acting unattended
cannot pause and ask you when it is unsure. Your organization can also switch unattended mode
off everywhere at once, which overrides any individual agent's setting.

## Related pages

- [Skills & tools](/concepts/skills-and-tools/) covers what an agent is allowed to call in
  the first place.
- [Governance & audit](/concepts/governance-and-audit/) covers how every action, approved or
  denied, gets checked and recorded.
- See the [Chat guide](/guides/chat/) for the composer, the approval modal, and every other
  chat control in detail.
- Administrators: see [Agents](/admin/agents/) for how agents are configured, and
  [Settings - governance](/admin/settings-governance/) for the org-wide unattended-mode
  switch.
