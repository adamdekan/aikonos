---
title: Scheduled runs oversight
description: An org-wide, read-only view of every user's scheduled runs.
sidebar:
  order: 7
---

Scheduled runs (`/admin/runs`) gives you visibility into every scheduled run across the whole
tenant, not just your own. See [Schedules](/guides/schedules/) for what a schedule is and how a
user creates one. This is useful for confirming what's actually set to fire across your
organization, or for tracking down whose schedule is behind unexpected agent activity, without
needing to open each person's own account to check.

## Filtering

An owner-email filter input plus a Filter button narrows the table down to one person's
schedules.

## What the table shows

Each row shows the owner, the schedule (a cron expression, or "once" for a one-off), the
prompt - or, for a workflow-bound schedule, a "Workflow" badge and the workflow's display name
in place of a prompt - the next-fire time, a state badge, and the last run's status plus how
many times it has run.

## This page is oversight only

You cannot create, edit, pause, or delete a schedule from here. Managing an individual schedule,
including pausing it or changing its recurrence, happens on the owner's own
[Schedules](/guides/schedules/) page; this view exists so you can see what's scheduled across
the organization without needing access to each person's account.

## Workflow-bound rows

A schedule bound to a workflow instead of a free-text prompt shows the same "Workflow" badge and
display name here that it does on the owner's own Schedules page. See
[Workflows](/concepts/workflows/) for what a workflow-bound schedule runs and why its prompt
field is replaced entirely.

## Empty and error states

If nothing is scheduled anywhere in the tenant, the table shows "No scheduled runs." A generic
error banner appears if the list fails to load.
