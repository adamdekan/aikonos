---
title: Agents
description: Creating and configuring agents - identity, provider, capabilities, behavior, access, and API keys.
sidebar:
  order: 5
---

Agents (`/admin/agents`) covers the full lifecycle of an agent: its identity, which LLM provider
it prefers, what it's allowed to do, how it behaves, who can reach it, and its API keys for
external invocation. See [Agents & tasks](/concepts/agents-and-tasks/) for what an agent is from
a user's point of view.

## Creating or editing an agent

"New Agent" opens a modal organized into sections.

**Identity** - Name (required) and Model. The Model list offers only models your configured
providers actually carry, grouped under the provider that serves each one. Choosing a model also
pins its provider, so there is no separate provider field to keep in sync. Leaving Model blank
means the agent inherits both the tenant's default provider and its model.

Picking the provider along with the model is what makes the choice take effect: a model is only
used when the provider serving the agent carries it, and a provider on its own has no effect at
all. If a model an agent already uses is later removed from every provider, the modal keeps the
stored value and flags it as no longer offered, so opening the modal never silently changes an
agent's configuration.

**Capabilities** - Skills and MCP servers, each as a searchable checklist with Select All and
Select None. These are the tools and MCP-backed capabilities this agent can use; a capability
not checked here is unreachable by this agent no matter what any user's own grants allow.

**Behavior**:

- **Approval mode** - `needs_approval` or `auto`. `auto` lets this agent's tool calls proceed
  without a human approval step. This is still subject to the org-wide automation kill switch on
  [Settings - governance](/admin/settings-governance/): if that switch is off, no agent can run
  in `auto` mode regardless of its own setting.
- **Allow invocation via external surface** - a toggle exposing this agent to the external
  invoke API, separate from ordinary chat access.
- **Personality** - a free-text instructions field, capped at 4096 bytes with a live counter.

**Access** (edit-only) - an "Assign to" input taking a `user:email` or `group:name`, plus a
button to add it. The assignment list below shows everyone and every group with direct access,
each with a per-row Revoke.

**API keys** (edit-only) - minting a key shows the raw key exactly once, in a banner with a Copy
button; it cannot be retrieved again after you close it, so copy it before dismissing the
banner. The list below shows existing keys by prefix and label only, each with Revoke. The mint
form takes an optional label to help you tell keys apart later.

:::caution
Revoking an agent's API key or a user's/group's access is immediate and cannot be undone from
this page - anything already authenticated with a revoked key stops working right away.
:::

## The agents table

Each row shows the agent's name, model, an approval-mode badge (with an additional "ext" badge
if external invocation is enabled), skill and MCP counts, how many users/groups it's assigned
to, and Edit/Delete actions.
