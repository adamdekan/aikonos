---
title: Inbox
description: Acting on delegated tasks sent to you or a group you belong to.
sidebar:
  order: 6
---

Inbox is where delegated tasks land: work a teammate handed to you, or to a group you belong
to, instead of doing it themselves. See [Delegation & inbox](/concepts/delegation-and-inbox/)
for what a delegation is and how it gets sent.

## The unread badge

The Inbox entry in the sidebar shows a badge with your current count of pending envelopes, so
you can tell at a glance whether anything is waiting without opening the page.

## Acting on an envelope

Each item in your inbox is an envelope: a request someone delegated to you, showing who sent
it and what they asked. Three actions are available on every envelope:

- **Send to agent** seeds your chat prompt with the delegated request and submits it right
  away, then dismisses the envelope in the background. This is the one-click path when you
  are ready to act on the request as written.
- **New session** seeds your composer with the same request, but does not send it and does
  not dismiss the envelope. Use this when you want to edit the request before running it.
- **OK** dismisses the envelope without acting on it at all. Nothing is sent to an agent.

Dismissing an envelope, whether through Send to agent or OK, removes it from your list right
away. If the dismissal fails on the server, Inbox puts the envelope back and shows an error so
you are not left thinking it was handled when it was not.

If a task was delegated to a group you belong to rather than to you directly, it shows up in
every member's inbox the same way. Any member acting on it or dismissing it does not affect
what other members see; see [Delegation & inbox](/concepts/delegation-and-inbox/) for more on
group delegation.

## Loading and empty states

Inbox shows a loading indicator while it fetches your pending envelopes. Once loaded, an empty
inbox shows "No pending delegations."
