---
title: Governance & audit
description: How Aikonos checks every action against your organization's rules, and how those decisions are recorded.
sidebar:
  order: 7
---

Every tool call an agent makes, whether it is reading a file, sending a message, or reaching
out to the web, is checked against your organization's rules at the moment it happens, not
just when you first signed in. This check happens on the server, not in your browser. Hiding
a menu item or a sidebar entry because you lack access to it is a convenience for you, not the
actual enforcement: even if a control were somehow visible, the underlying action would still
be checked and denied on the server if you are not permitted to take it.

## Approvals as part of governance

Some checks pass immediately; others pause and ask for your approval, or an administrator's,
before the action proceeds. This is the same approval flow described in
[Agents & tasks](/concepts/agents-and-tasks/): it exists so that riskier actions get a human
decision at the moment they matter, not only a policy set in advance.

## Organization-wide kill switches

Beyond individual grants, your organization can turn categories of capability off entirely,
for everyone, regardless of what any individual user or group has been granted. Examples
include disabling categories of action outright, such as network access, external writes, or
destructive changes, turning off unattended auto-approve mode everywhere, and turning off
workflow sharing so no workflow can be published to a group even if an individual workflow
owner tries. These switches only remove access; they never grant anything beyond what
individual permissions already allow.

## Limits that can decline a request

Your organization can also set rate limits and monthly spend caps, per person, per agent, or
organization-wide. When one of these limits is reached, further requests are declined until
the limit resets, independent of whether the request would otherwise have been permitted.

## The audit trail

Every decision, whether an action was allowed, sent for approval, or denied, is written to an
audit trail that cannot be edited or deleted after the fact. Administrators can review this
trail to see exactly what happened, when, and why a particular action was allowed or blocked.
This is what makes it possible to answer, after the fact, exactly what an agent did on your
behalf and under what authority.

## Related pages

- [Agents & tasks](/concepts/agents-and-tasks/) covers the approval prompts you see day to
  day.
- [Skills & tools](/concepts/skills-and-tools/) covers how individual tool grants relate to
  these org-wide switches.
- Administrators: see [Policy & audit](/admin/policy-audit/) for the live audit stream,
  history, and decision trace, [Settings - governance](/admin/settings-governance/) for the
  org-wide kill switches, and [Settings - network & limits](/admin/settings-limits/) for rate
  limits and spend caps.
