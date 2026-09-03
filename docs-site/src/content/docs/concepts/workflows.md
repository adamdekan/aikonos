---
title: Workflows
description: Reusable, versioned templates agents build from work that succeeded, with owner approval and safe sharing.
sidebar:
  order: 3
---

A workflow is a saved, repeatable sequence of steps an agent can run again without you
re-explaining the task from scratch. After an agent completes a task well, you can ask it to
save that sequence as a workflow: which tools it called, in what order, and with which
arguments. The next time you need the same kind of work done, you run the workflow directly
instead of starting a new conversation from zero.

## Private by default, versioned as you refine it

A workflow you save belongs only to you at first. As you keep using and adjusting it, new
versions accumulate. Refining a workflow through chat proposes a new version rather than
overwriting the current one; you, as the owner, then approve or reject the proposal. You can
also pin a specific version so that running the workflow always uses that version, even if
newer proposals exist, until you clear the pin.

## Publishing to a group

Once a workflow has proven itself, you can publish it to one or more groups you can delegate
to, so teammates can run your version too. Publishing shares a copy of the current version,
not a live link to your private working copy. Every run of a shared workflow, by anyone, is
checked against that runner's own permissions at run time. A step your workflow includes does
not carry your authority with it: if a teammate running your workflow is not permitted to
call a step's tool, that step is denied for them the same way it would be denied if they
tried it directly in chat.

## Forking

If you want your own copy of someone else's shared workflow to adjust independently, you can
fork it. Forking creates a new, private workflow you own, seeded from the version you forked,
with no ongoing link back to the original.

## Reason steps

Most workflow steps call a tool. A workflow can also include a "reason" step: a bounded step
where the agent thinks through or synthesizes information between tool calls, without itself
calling any tool or carrying any authority. Reason steps are useful for combining or
interpreting the output of earlier steps before the workflow moves on to its next action.

## Ratings and scheduling

After a workflow run finishes, you can rate the run as good or bad, with an optional note,
which helps track whether a workflow is holding up over time. A workflow can also be set to
run on a schedule, so it fires automatically at a recurring time or on a one-off date without
anyone opening a chat session first.

## Related pages

- [Skills & tools](/concepts/skills-and-tools/) covers the tools a workflow's steps call.
- [Agents & tasks](/concepts/agents-and-tasks/) covers approvals, which still apply per step
  when a workflow runs.
- See the [Workflows guide](/guides/workflows/) for running, publishing, forking, and
  version-switching in detail, and the [Schedules guide](/guides/schedules/) for
  workflow-bound schedules.
- Administrators: see [Scheduled runs](/admin/runs/) for org-wide oversight of every
  scheduled run, including workflow-bound ones.
