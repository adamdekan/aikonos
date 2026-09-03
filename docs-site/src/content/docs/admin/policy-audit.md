---
title: Policy & audit
description: The five Policy tabs - live Audit stream, Audit History, Decision Trace, Simulator, and Alerts.
sidebar:
  order: 8
---

Policy (`/admin/policy`) is a tab strip over five different ways to see what your organization's
policy engine has decided, is deciding right now, or would decide. See
[Governance & audit](/concepts/governance-and-audit/) for what a policy decision and the audit
trail mean conceptually.

## Audit

A live stream of decisions as they happen, over server-sent events.

- A connection-status pill shows live, connecting, or reconnecting.
- A row of decision-count chips totals allow, approval, deny, and overall counts for the current
  session.
- A tenant filter select reconnects the stream scoped to a single tenant.
- A free-text filter takes space-separated terms; prefixing a term with `-` excludes rows
  matching it.
- Pause stops new rows from appearing and buffers them behind a "▲ N new" jump pill; Resume (or
  clicking the pill) replays them into the table.
- Export JSON and Export CSV download whatever is currently loaded.
- Clicking, or pressing Enter or Space on, a row expands it to show the full event detail.
- A "Jump to latest" pill appears when you've scrolled away from the top of the table.

**Empty and error states:** if the stream endpoint isn't configured, or a status probe comes
back 501, Audit shows a "not configured" state with a Reconnect button. If reconnection attempts
are exhausted (a capped exponential backoff, five attempts), a disconnected banner appears, also
with Reconnect. A transient failure shows a reconnecting banner instead of dropping the view
entirely.

## Audit History

A server-side paginated search over the historical audit log, independent of whether you were
watching live when something happened.

- A filter form takes a start and end datetime, an actor, an event type, and a decision.
- Search runs the query; "Load more" fetches the next page using a cursor rather than reloading
  everything.
- **Verify integrity** runs a hash-chain and signature check over the log and shows the result as
  a card. Any break in the chain or a signature failure shows up as a clickable chip identifying
  the specific event, so you can jump straight to it instead of scanning the whole log.
- Export JSON and Export CSV download the current filtered result.
- Clicking a row opens a side inspector panel with its full detail.

## Decisions (Decision Trace)

Look up a single task by its task ID and click Explain to see the full per-step policy trace for
it: each step's tool id, effect class, outcome badge, a justification, and the outcome, rule id,
reason, and detail for every individual gate that step passed through. This is the tool for
answering "why did this specific task get approved, denied, or asked for step-up" after the
fact.

If no trace exists for the task ID you entered, the page shows "No decision trace found for this
task."

## Simulator

A synthetic dry run of the policy engine that touches no real data. The form takes a subject
user ID, a tool ID, an effect class, an optional host or FGA object, and a "Reads sensitive data"
toggle. Simulate renders the aggregate outcome plus the per-gate results that produced it, exactly
the shape you'd see in a real Decision Trace, without actually creating a task or invoking
anything.

Use this to test a policy question in advance - for example, whether a particular user would be
allowed to call a particular tool against a particular host - before it comes up for real.

## Alerts

A read-only table of alerting-rule events that have fired: time, rule name, a severity badge,
and detail. If the table is empty, the empty state explains that alerts only fire once
escalation, unusual-actor, off-hours-destructive, or deny-rate rules have actually been
configured - an empty Alerts tab does not mean nothing risky has happened, it can also mean no
alerting rules exist yet to catch it.

## Related pages

- [Governance & audit](/concepts/governance-and-audit/) explains what a policy decision is and
  why the audit trail can't be edited after the fact.
- [Settings - governance](/admin/settings-governance/) covers the org-wide kill switches whose
  effect you can see reflected in these decisions.
