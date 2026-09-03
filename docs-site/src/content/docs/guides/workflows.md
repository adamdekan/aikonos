---
title: Workflows
description: Running, improving, publishing, versioning, forking, and deleting workflows.
sidebar:
  order: 5
---

Workflows lists the reusable, saved sequences an agent has built from work it already did
well. See [Workflows](/concepts/workflows/) for what a workflow is, how versions and
publishing work, and what forking gives you.

## When this page appears

Workflows only shows up in the sidebar if you have been granted the workflows permission.

## Own and shared workflows

The page lists your own workflows separately from ones shared with you through a group.
A shared card offers Run, Versions, and Fork. A private card you own offers Run, Improve,
Publish, Versions, Fork, and Delete.

## Finding a workflow

A name filter narrows the list as you type. Below the list, a "Load more" button fetches the
next page of workflows rather than loading everything at once.

## Access and requirements

A card whose required skill, or whose bound agent, you do not have access to is shown greyed
out with Run disabled, labeled "needs: \<missing requirements>" or "no access to this
workflow's agent." This is a convenience so you know why you cannot run it; the server checks
your access again regardless of what the card shows.

## Running a workflow

Run opens a modal that first previews the workflow: its steps and what it requires. Below
that, a form asks for any inputs the workflow needs, typed to match what each input expects
(a checkbox, a dropdown, a number, or free text), with required fields marked `*`. Run stays
disabled until every required input is filled in.

Once you start the run, the modal streams live progress step by step. When it finishes, you
see the overall status (completed or failed, including which step it halted at if it did not
finish), a breakdown of what each step did, and a Good or Bad rating with an optional note.
Submit Rating records your feedback, or Skip moves on without rating it.

The finished run also lands as its own chat session, with the workflow's name, step count,
and final result rendered visibly, so you can review it later the same way you would review
any other conversation.

## Improving a workflow

Improve, available only to the owner, starts a chat session that asks the agent to propose a
new version of the workflow based on what you tell it to change. The agent proposes the new
version; you approve or reject it from the Versions modal described below. See
[Workflows](/concepts/workflows/) for why refining a workflow proposes a new version instead
of overwriting the current one.

## Publishing a workflow

Publish opens a dialog listing the groups you can delegate to, each with a toggle. Checking a
group and confirming shares the current version of the workflow with everyone in it.

## Versions

Versions opens a modal listing every version of the workflow with its approval state: an
"active" badge on whichever version currently runs when the workflow is run, Pin on any other
approved version to make it the one that runs instead, and, for the owner, Approve or Reject
buttons on any version still awaiting a decision. "Clear pin" at the bottom returns the
workflow to always running its latest approved version.

## Forking a workflow

Fork opens a modal with a single "New name" field, pre-filled as "Fork of \<name>." Confirming
creates a new, private workflow you own, seeded from the version you forked, with no ongoing
link back to the original.

## Deleting a workflow

Delete opens a confirmation modal. If the workflow is shared, the confirmation calls this out
specifically: deleting it removes it for everyone in the groups it is shared with, not only
for you.

## Empty and unavailable states

An empty list shows "No workflows yet. Ask the agent to run a task and save it as a
workflow." A filter with no matches shows "No workflows match \"\<query>\"." If the shared
half of the list cannot load, a persistent banner reads "Shared workflows are temporarily
unavailable (authorization service unreachable)," with your own workflows still shown
normally alongside it.
