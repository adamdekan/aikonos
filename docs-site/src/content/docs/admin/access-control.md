---
title: Access Control
description: The seven Access Control tabs - Members, Users, Groups, Tools, Agents, Provisioning, and Advanced.
sidebar:
  order: 1
---

Access Control (`/admin/roles`) is where you manage who can use Aikonos and what they can do
once they're in. It is a tab strip with seven tabs, each covering a different slice of the same
underlying grants.

:::note
If OpenFGA (the authorization service) is disabled, Access Control shows a banner explaining the
deployment is running in an allow-all dev mode. See [Administration overview](/admin/) for what
that means - it is a deployment concern, not something to fix here.
:::

## Members

A read-only roster of everyone who has ever signed in: name and email, a role badge (admin or
user), an origin badge (provisioned automatically, or added manually), and when they were last
seen. A filter input narrows the roster by name, email, or subject substring, and an Export CSV
button downloads the current list.

## Users

A master-detail view. The list on the left is filtered by search; selecting someone opens a
detail pane with:

- **Groups** - each group shown as a chip with an × to remove it, a dropdown to add an existing
  group, and a "+ New group" inline form (with name-pattern validation) to create one on the
  spot.
- **Tenant role** - a chip plus Set member / Set admin buttons. These buttons are disabled if the
  user has no tenant-level relationship tuple at all yet.
- **Effective Tools** - a table of every tool the user can actually invoke, each row showing
  whether the grant is direct or comes via a named group. Direct grants get a Revoke button;
  grants that only exist because of a group can only be changed on that group.
- **Assigned Skills** - the skill bundles available to the user, again with provenance (direct
  vs. via group).
- **Owned/Assigned Agents** - the agents this person owns and the agents they've been given
  direct access to.

## Groups

Also a master-detail view, with a "+ New group" button and a search box on the list side. The
detail pane for a selected group has:

- **Delegation group** toggle - turns on whether members of this group may delegate chat tasks
  to each other. See [Delegation & inbox](/concepts/delegation-and-inbox/) for what delegation
  means day to day.
- **Members** - chip removal for existing members, plus a bulk-add control: a checkbox list with
  an "Add selected (N)" button when there's more than one candidate to add, or a plain
  single-select dropdown when there's only one.
- **Managers** - chip removal plus a dropdown to add a manager.
- **Tools Granted** - chip removal, plus a bulk-add checklist showing each tool's scope, or a
  dropdown when there's only one candidate.
- **Skills Granted** - chip removal plus a dropdown to add a skill bundle.
- **Agents usable** - read-only; shows which agents this group's members can use, but agent
  access itself is managed from the Agents tab or the Agents admin page.

## Tools

A master list of every tool and skill id known to the system - the tool registry plus anything
still referenced by a live grant, so a stale or unregistered grant shows up flagged rather than
silently vanishing. Each row shows the tool's scope. Selecting one shows which groups can invoke
it (chip removal, dropdown to grant) and a read-only "Users with access" table with the same
direct/via-group provenance used elsewhere.

## Agents

A master list of agents. Selecting one lets you manage:

- **Owner** - a chip plus a reassign dropdown. Reassigning an agent revokes the current owner's
  ownership tuple, then assigns the new one, as a sequence of two steps.
- **Usable by** - a combined dropdown for adding either a user or a group, with chip removal for
  existing entries.
- **MCP connectors permitted** - chip removal and a dropdown to add an MCP server this agent may
  reach.

## Provisioning

Provisioning rules automatically assign new users to groups the first time they sign in. A
banner at the top explains the two kinds of rule: a wildcard rule (matches any email at sign-in
time) and an exact-email rule. The add-rule form takes a matcher and a comma-separated list of
groups; after adding a rule, a notice tells you how many *existing* users it retroactively
applied to.

:::caution
A wildcard rule only ever applies at the moment a user is first seen. It does nothing for users
who already signed in before the rule existed - use an exact-email rule, or grant access
directly from the Users or Groups tab, for someone already provisioned. Deleting a provisioning
rule never revokes roles it already granted; removing it only stops it from applying to new
sign-ins going forward.
:::

Each rule has a per-row Delete, confirmed with a native browser confirm dialog.

## Advanced

Advanced is a raw relationship-tuple editor: CRUD over the underlying authorization tuples,
organized by section. Per-section "Assign" opens a modal with a Relation select, an Object field
(input or select depending on the relation), and a Subject input with autocomplete. Existing
tuples are listed with a per-row Revoke. Where a UUID would otherwise be shown for an MCP
connector or an agent, Advanced resolves it to a human-readable name when it can.

Treat Advanced as the escape hatch, not the everyday tool. It has none of the validation or
guardrails the other six tabs build in - prefer Members, Users, Groups, Tools, Agents, or
Provisioning for anything they can express, and reach for Advanced only when they can't.
