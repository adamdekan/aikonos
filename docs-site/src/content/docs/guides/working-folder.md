---
title: Working folder (OneDrive)
description: Switching your working folder between the local workspace and a OneDrive folder.
sidebar:
  order: 8
---

The working-folder control on the composer decides where your agent reads and writes
documents, and what the Files explorer shows. See
[Workspace & files](/concepts/workspace-and-files/) for what the working folder governs.

## When it appears

The control only appears once your organization has OneDrive set up and your workspace
preferences have loaded successfully. If OneDrive is not available to you, the composer skips
the control entirely and your working folder is always the local workspace.

## Choosing Local workspace or OneDrive

Opening the control offers two choices: "Local workspace," Aikonos's own storage for you, or
"OneDrive folder...", which opens a folder picker over your OneDrive.

## The folder picker

The picker shows breadcrumbs starting from your OneDrive's root. Click a folder to move into
it. "Use this folder" sets your working folder to wherever you have navigated to; Cancel, the
close button, or Esc dismisses the picker without changing anything. If the picker fails to
load a folder's contents, a Retry button appears instead of leaving it stuck.

## What switching changes

Once you switch, your agent's document tools, and your composer's uploads and downloads, all
route to the folder you chose instead of the local workspace, until you switch back. The Files
explorer reflects the same choice: its header shows a backend indicator chip naming whichever
folder is active. See the [Files guide](/guides/files/) for that chip and the rest of the
explorer.

Switching is explicit. Aikonos never splits your files silently between local storage and
OneDrive at the same time, and if the OneDrive connection behind your working folder is
unavailable, the switch fails with an error rather than quietly falling back to local storage.

## The reconnect banner

If your OneDrive connection needs attention while it is set as your working folder, Files
shows a banner explaining that the connection needs to be refreshed and that it reconnects
automatically on your next sign-in. See [Connections](/concepts/connections/) for what a
reconnect involves.
