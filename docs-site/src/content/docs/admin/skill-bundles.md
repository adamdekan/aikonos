---
title: Skill bundles
description: Uploading, editing, and granting skill bundles, including keyword auto-load configuration.
sidebar:
  order: 3
---

Skill bundles (`/admin/skill-bundles`) is where you author the reusable bundles users pick with
a `/` command in chat, or that auto-load themselves when a message matches their keywords. See
[Skills & tools](/concepts/skills-and-tools/) for what a bundle is from a user's point of view.

## Creating or editing a bundle

You can paste bundle text directly into a textarea, or upload a `.skill` or `.zip` file. Uploads
are validated: a file that isn't actually a valid archive is rejected with a message pointing you
back at the paste box instead of failing silently.

## Keywords and auto-load

When editing a bundle, a comma-separated Keywords input controls auto-load: if a user's chat
message contains one of these words, the bundle activates on its own, without them typing a `/`
command. Leaving Keywords empty means the bundle never auto-loads - it's only reachable through
the explicit `/` palette. See [Skills & tools](/concepts/skills-and-tools/) for what a user sees
in chat when a bundle auto-loads (or is suppressed instead).

## The bundle table

Each row shows the bundle's name, description, and a set of chips: the tools it allows, its
configured keywords, and flag badges for `no-model-invoke` (the bundle can't be triggered by
keyword matching or by the model itself, only by explicit `/` selection) and `context-fork`
(the bundle runs its guidance in a separate context). An inline "Grant to group" input and
button lets you assign the bundle to a group directly from the table, without leaving this page.

Edit opens the same paste/upload form pre-filled with the bundle's current content; Delete asks
for confirmation before removing the bundle.

## Related pages

- [Skills & tools](/concepts/skills-and-tools/) covers what a bundle does for a user and how the
  `/` palette and keyword auto-load look from their side.
- [Tools](/admin/tools/) covers the underlying tool vocabulary a bundle's allowed-tools list
  draws from.
- [Access Control](/admin/access-control/) is where a bundle gets granted to individual users, in
  addition to the inline "Grant to group" control on this page.
