---
title: Delegation & inbox
description: Handing a task to a teammate or a group by mention, and how the recipient sees and acts on it.
sidebar:
  order: 6
---

Delegation lets you hand a task to someone else's agent instead of running it yourself. In
the chat composer, typing `@` opens a palette of teammates and groups you are able to
delegate to. Selecting a person or a group and sending the message routes it as a delegation
rather than an ordinary chat turn, and Aikonos asks you to confirm before it sends.

## What the recipient sees

A delegated task lands in the recipient's Inbox as a pending item, showing who sent it and
what was asked. From there, the recipient has three options: send it straight to their own
agent, which starts a conversation using the delegated request; open a new session with it
instead, which fills the composer without sending, so they can edit before running it; or
dismiss it without acting, which removes it from the inbox with no task started.

## Delegating to a group

Delegating to a group is a fan-out: the task is delivered to every member of that group's
inbox, not to a single designated person. Any member can act on it or dismiss it
independently of the others.

## Discovery is not authorization

The `@` palette only shows you who you appear able to delegate to, for the purpose of finding
the right person or group. It is not the permission check itself. When you actually send a
delegation, the server independently re-checks whether that delegation is allowed, regardless
of what the palette displayed. A name appearing in the palette is a convenience for finding
the right recipient, not a guarantee the send will succeed.

## Related pages

- [Agents & tasks](/concepts/agents-and-tasks/) covers what happens once a delegated task
  reaches an agent.
- See the [Chat guide](/guides/chat/) for the `@` mention palette, and the
  [Inbox guide](/guides/inbox/) for every inbox action.
- Administrators: delegation groups are managed through
  [Access Control](/admin/access-control/).
