---
title: Chat
description: The composer's palettes and controls, replying and editing, personality, the skill timeline, and the approval modal.
sidebar:
  order: 1
---

Chat is where you talk to an agent. It is also where scheduled or workflow "improve" sessions
land, and where a delegated task shows up once you send it to your agent. This page covers
every control on the chat screen, most of them in the composer docked at the bottom.

## The composer

The composer's placeholder text rotates through a few hints while it is empty: the agent's
default greeting, `# mention a file for context`, `@ delegate to a teammate or group`, and
`/ run a skill`. Each hint points at one of the palettes described below.

## Running a skill with `/`

Typing `/` opens a palette of the skill bundles you have been granted, filtered as you keep
typing by matching the start of each bundle's name. Select one with Enter, Tab, or a click,
or move through the list with the arrow keys. You can also skip the palette entirely: sending
a message that starts with the exact `/<bundle-name>` activates that bundle even if the
palette is closed. See [Skills & tools](/concepts/skills-and-tools/) for what a bundle is and
how you get access to one.

## Delegating with `@`

Typing `@` opens a palette of teammates and groups you can delegate to. Selecting one inserts
`@DisplayName` into your message. If that mention is still in the text when you send it,
Aikonos routes the message as a delegation rather than an ordinary chat turn and shows a
confirmation modal before it actually sends, with Cancel and Confirm buttons. See
[Delegation & inbox](/concepts/delegation-and-inbox/) for what happens once the delegation
lands.

## Referencing a file with `#`

Typing `#` opens a palette of files in your workspace, so you can reference one by path
without typing the whole thing out.

## Attaching a file or image

The Attach button opens a file picker that accepts images and common document types. An image
you attach uploads to a `references` folder in your workspace (created automatically if it
does not exist yet), which is where the agent looks when analyzing an image you have shared.
Any other file type uploads to your workspace's root folder. Either way, a successful upload
inserts a `#<path>` mention at your cursor, referencing the file you attached.

## Sending and stopping

The button in the corner of the composer toggles between Send and Stop depending on whether a
run is in progress. While the agent is working, press Stop to cancel it.

## The working-folder control

If your organization has OneDrive set up and your workspace preferences have loaded
successfully, a small button appears in the composer reading "Working folder: Local" or
"Working folder: OneDrive · /\<path>". Opening it offers "Local workspace" or "OneDrive
folder...", the latter opening a folder picker. See the
[Working folder guide](/guides/working-folder/) for the full picker walkthrough.

## Replying to and editing prior turns

Every past turn in the conversation supports Reply, which quotes its last line into the
composer so you can respond to something specific, and Edit-and-resend on your own turns,
which lets you change what you asked and send it again.

## Agent personality

If your administrator has made an agent's personality editable, a Personality button appears
above the composer. It opens a modal with a textarea capped at 4096 bytes, with a live
counter showing how much room you have left. Save applies the change, Cancel discards it.

:::note
A conversation already in progress keeps the personality it started with. A personality
change only takes effect in a new conversation.
:::

## The skill timeline

When a message triggers a skill bundle by keyword instead of an explicit `/` command, chat
shows a skill timeline entry before the agent's reply. Each entry names a bundle and either
shows its description, if it loaded, or the reason it was suppressed instead, for example
because an administrator configured it not to auto-activate.

## Approvals

Some tool calls pause for your approval before they run. When one does, an approval modal
opens showing the tool's id and a pill reading `STEP-UP` or `HUMAN`, describing how serious
the request is. If more than one approval is waiting, a "1 of N" counter shows where you are
in the queue. Below that: the agent's name if the call is agent-bound, the reason it gave for
wanting to act, and a scrollable view of the exact arguments it plans to send.

For step-up requests, Approve stays disabled until you check "I have reviewed the arguments
above." Deny has focus by default, which makes it the button that fires if you press Enter
without selecting Approve first, and it is also what happens if you press Esc.

:::caution
Esc denies the request. This is deliberate: if you are unsure, the safe outcome is no,
not an accidental approval. Clicking outside the modal does nothing at all, since an
approval decision should never happen by accident.
:::

If your response to an approval fails to go through, the modal stays open with an inline
error instead of closing, so you can try again.

## When palettes are unavailable

If any of the data the palettes depend on fails to load, chat shows a persistent banner
reading "Mention and tool palettes unavailable" with a Retry button, instead of silently
leaving the `/`, `@`, and `#` palettes empty.
