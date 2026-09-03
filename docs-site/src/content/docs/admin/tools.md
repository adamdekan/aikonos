---
title: Tool vocabulary
description: Editing the tool/skill vocabulary and registering custom MCP-backed tools.
sidebar:
  order: 2
---

Tools (`/admin/skills`) manages the vocabulary of tool ids Aikonos knows about - their effect
class, description, and whether they're enabled at all. This is a different thing from deciding
*who* can use a tool: that authorization lives in [Access Control](/admin/access-control/), not
here.

## Effect class, description, and enabled

Each built-in tool has a row with an inline effect-class select, a description input, and an
Enabled toggle, each with its own Save. Effect class controls how cautiously a tool call is
treated (for example, whether it counts as a write or a destructive action); you can only
tighten a built-in tool's effect class, never loosen it below what it was designed for.

The Scope column is derived by the broker, not editable here - it describes what the tool
touches (for example a workspace file or an external connector) rather than who can use it.

## Registering a custom tool

The "Register skill" form lets you add a tool id backed by an MCP server, in the form
`mcp:<connection>:<tool>`, with its own effect-class select and description. This is how a tool
exposed by an [MCP server](/admin/mcp/) becomes something you can grant to agents through Access
Control.

## Deleting a tool

Delete asks for confirmation before removing a row. Deleting a built-in tool doesn't erase it
permanently - it reverts to its default effect class and description. Deleting a custom
MCP-backed tool id removes it from the vocabulary entirely.

## Where authorization actually happens

Editing a tool here changes how the tool is classified and described, not who can call it.
Granting a tool to a user or group happens on the [Access Control](/admin/access-control/) Tools
or Groups tab.

## Why effect class matters

Effect class is what your organization's policy checks and kill switches key off. The four
[Capabilities](/admin/settings-governance/) kill switches - network egress, external writes,
credential access, destructive actions - act on a tool's effect class, not on its individual
grants. Tightening a tool's effect class here can make it subject to a kill switch it wasn't
subject to before; loosening it below its built-in default is not possible, so a tool cannot
accidentally become less carefully checked than it was designed to be.

## Related pages

- [Skills & tools](/concepts/skills-and-tools/) explains what a tool is from a user's point of
  view and why access is deny-by-default.
- [Skill bundles](/admin/skill-bundles/) covers bundling tools together with instructions and
  keyword auto-load.
- [MCP servers](/admin/mcp/) covers registering the servers a custom tool id can be backed by.
