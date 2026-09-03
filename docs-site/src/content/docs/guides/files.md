---
title: Files
description: Navigating, filtering, uploading, renaming, and deleting files in your workspace explorer.
sidebar:
  order: 2
---

The Files screen is a directory-aware explorer over your workspace. It shows breadcrumbs for
the folder you are in, lists folders before files, and shows a backend indicator chip in the
header naming where your files actually live: "Local workspace" or "OneDrive ·
/\<path>". See [Workspace & files](/concepts/workspace-and-files/) for what a workspace is and
how the OneDrive backend gets chosen.

## Navigating folders

Click a folder row to move into it, or click a breadcrumb to jump back up. Each navigation
loads only that folder's contents, so opening a deep folder does not require loading your
entire workspace first.

## Filtering the current folder

A filter box above the list narrows what you see to names matching what you type, scoped to
the folder you are currently in. It clears automatically whenever you navigate to a different
folder.

## Creating a folder

The New folder button opens an inline name field. Press Enter to create the folder, or Esc to
cancel without creating anything.

## Uploading files

The Upload button opens a file picker, or you can drag files directly onto the list, or onto a
specific folder row to upload straight into that folder without opening it first.

:::caution
Uploads are capped at 10 MiB per file. A file over that limit is rejected with an error
naming the file, rather than failing silently or uploading a truncated copy.
:::

## Renaming, downloading, and deleting

Each row has a pencil icon for renaming: it opens an inline input, and Enter, clicking away,
or Esc all behave as you would expect (Enter and blur confirm the new name, Esc cancels).
Download fetches the file. Delete opens a confirmation modal with Cancel and Delete buttons;
a folder can only be deleted while it is empty.

## OneDrive and reconnecting

If your working folder is OneDrive and the connection needs attention, Files shows a banner:
"OneDrive connection needs to be refreshed. It reconnects automatically on your next sign-in."
Nothing about your files changes while this banner is showing; it only means the explorer
cannot reach OneDrive until the connection refreshes. See the
[Working folder guide](/guides/working-folder/) for switching backends, and
[Connections](/concepts/connections/) for what a reconnect actually involves.

## Empty and loading states

While a folder's contents are loading, Files shows "Loading…" An empty folder shows "This
folder is empty." Any failed operation shows an error banner, and Files refetches the current
folder in the background to make sure what you see matches what is actually there.
