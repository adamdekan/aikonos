---
title: Workspace & files
description: Your private file storage in Aikonos, where chat sessions and attachments live, and how the working folder can route to OneDrive.
sidebar:
  order: 4
---

Every Aikonos user has a private workspace: a personal area for folders, files you upload, and
files your agent creates on your behalf. Nobody else's workspace is visible from yours, and
an agent working on your behalf only reaches into your workspace, not anyone else's.

## What lives in your workspace

Your chat sessions are saved as files under a `.agent/Sessions` folder in your workspace,
which is how your conversation history persists between visits. Images you attach in the
composer are uploaded to a `references` folder, which is where the agent looks when it
analyzes an image you have shared. Everything else you upload, and everything your agent
produces as output, lands in your workspace's regular folders, which you can browse, rename,
move, and delete through the files explorer.

There is a 10 MiB limit on a single upload from the browser. A file larger than that is
rejected with a message naming the file, rather than failing silently.

## Local workspace or OneDrive

By default, your working folder is the local Aikonos workspace described above. If your
organization has enabled it, you can instead point your working folder at a folder in your
own OneDrive. Once set, your working folder governs where the files explorer looks, where
composer uploads and downloads go, and where your agent's document tools read and write,
until you switch it back.

Switching backends is explicit: nothing silently splits your files across local storage and
OneDrive at once. If the workspace preference itself, or the OneDrive connection behind it,
is unavailable, the switch fails with an error rather than quietly falling back to local
storage.

## Related pages

- [Connections](/concepts/connections/) covers linking the OneDrive account this working
  folder depends on.
- See the [Files guide](/guides/files/) for the folder explorer's controls, and the
  [Working folder guide](/guides/working-folder/) for picking a OneDrive folder.
- Administrators: see [Settings - integrations](/admin/settings-integrations/) for the
  org-wide Microsoft 365 connection that makes OneDrive working folders possible.
