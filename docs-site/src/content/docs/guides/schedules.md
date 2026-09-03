---
title: Schedules
description: Creating recurring or one-off scheduled runs, the guided recurrence selector, and workflow-bound schedules.
sidebar:
  order: 4
---

Schedules lets you set up an agent run that fires later, either once or on a recurring basis,
without you opening a chat session at the time it runs.

## When this page appears

Schedules only shows up in the sidebar if you have been granted the scheduler permission. See
[Skills & tools](/concepts/skills-and-tools/) for how permissions like this get granted.

## Creating a new schedule

The New schedule form has a prompt textarea (hidden for workflow-bound schedules, described
below), a Kind selector for Recurring (cron) or One-off, and either the recurrence selector or
a date-and-time picker depending on which kind you choose. Create adds the schedule to your
list.

## The recurrence selector

For a recurring schedule, a guided Frequency selector offers Every N minutes, Hourly, Daily,
Weekly, or Monthly, each with its own sub-controls: a minute interval, an hour interval and
minute, a time of day, a day of the month, or a row of weekday toggles. The weekday row
includes a "Weekdays" preset that selects Monday through Friday in one click.

An "Advanced" toggle switches to a raw cron expression instead, for anything the guided
controls cannot express. Whichever mode you are in, a plain-language description of the
resulting schedule is always shown underneath, so you can confirm what you are about to
create before you create it.

:::note
If an existing schedule's cron expression cannot be parsed back into the guided controls,
editing it opens directly in Advanced mode instead of silently misrepresenting the schedule.
:::

## One-off schedules

Choosing One-off replaces the recurrence selector with a single date-and-time picker for the
one moment the run should fire.

## Managing existing schedules

Each schedule row has an inline Edit (pencil) that opens the same form in place, with Save
and Cancel buttons. Pause and Resume toggle whether an active schedule keeps firing without
deleting it, and Delete removes it entirely.

## Workflow-bound schedules

A schedule can be bound to a workflow instead of a free-text prompt. A workflow-bound row
shows a "Workflow" badge and the workflow's display name in place of the prompt, or
"(deleted workflow)" if the workflow it pointed to no longer exists. Editing a workflow-bound
schedule only lets you change timing (its recurrence or run time); the workflow it runs and
the inputs it runs with are fixed at creation and cannot be edited afterward. See the
[Workflows guide](/guides/workflows/) for how a schedule gets bound to a workflow from chat in
the first place.

## Empty and error states

If you have no schedules, the page shows "No schedules yet." A generic error banner appears
if any operation fails.
