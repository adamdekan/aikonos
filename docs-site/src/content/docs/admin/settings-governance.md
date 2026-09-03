---
title: "Settings: governance"
description: General config, Org Instructions, Automation Controls, Workflow Governance, and the Capabilities kill switches.
sidebar:
  order: 9
---

Settings (`/admin/settings`) is a tab strip covering every org-wide configuration surface. This
page documents the five governance-focused tabs: General, Org Instructions, Automation Controls,
Workflow Governance, and Capabilities. See [Settings: network & limits](/admin/settings-limits/)
and [Settings: integrations](/admin/settings-integrations/) for the rest of the tabs.

## General

A raw table of broker configuration keys: each row shows the key, its kind, its default value, a
doc string explaining what it does, and an editable value input with its own per-row Save and
inline success/error feedback. This is the lowest-level configuration surface in the admin area -
prefer the dedicated tabs below, and the other admin pages, for anything they cover, and use
General only for settings that don't have a dedicated control yet.

## Org Instructions

A single preamble, up to 4000 characters with a live counter, that gets prepended to every
agent's session across the whole organization. It shapes how agents behave, for example a
standing instruction about tone or a compliance reminder, but it never grants any tool or skill
by itself - it's instructions, not authorization. Save and Revert sit next to an "Unsaved
changes" indicator, and the page shows who last changed it and when.

## Automation Controls

A single toggle: "Allow unattended (auto-approve) mode." This is an org-wide kill switch that
sits above every individual agent's own approval-mode setting (see [Agents](/admin/agents/)): if
this is off, no agent can run with `auto` approval mode, regardless of what that agent is
configured to do. Turning it on means a tool call from an `auto`-mode agent can proceed with no
human in the loop at all, which is also what makes an agent more exposed to prompt injection -
there's no approval step to catch a malicious instruction hidden in content the agent read. The
toggle is optimistic: it flips immediately and rolls back automatically if saving it fails. The
tab shows who last changed the setting.

## Workflow Governance

A single toggle: "Allow workflow sharing." Turning this off refuses any new attempt to publish a
workflow to a group, org-wide, regardless of whether the individual publishing it has the
workflows permission. It does not touch workflows already shared before you turned it off - those
existing shares remain visible to their groups. See
[Workflows](/concepts/workflows/) for what publishing a workflow does. The tab shows who last
changed the setting.

## Capabilities

Four kill switches, one per effect class: Network egress, External writes, Credential access,
and Destructive actions. Each is framed the same way: it can only remove access that would
otherwise be allowed, never grant anything beyond what a user's or agent's own permissions
already permit. A toggled-off row shows a Disabled badge, and the tab shows who last changed
each one.

:::caution
Turning off a capability here takes effect immediately, for everyone, including agents and
workflows already in the middle of something. Any tool call that falls into a disabled effect
class starts failing with a denial the moment you flip the switch, even for someone who was
individually granted that tool. Use these switches for genuine incidents or organization-wide
policy changes, not as a routine per-user control - that's what [Access Control](/admin/access-control/)
is for.
:::
