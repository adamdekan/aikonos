---
title: "Settings: network & limits"
description: Network Access allowlisting, per-agent/provider Rate Limits, and monthly Spend Caps.
sidebar:
  order: 10
---

This page covers three tabs on Settings (`/admin/settings`): Network Access, Rate Limits, and
Spend Caps. See [Settings: governance](/admin/settings-governance/) for the other governance
tabs.

## Network Access

A rule table controlling which hosts an agent's `web.fetch` calls (and similar egress) may
reach. Each rule has a scope (Tenant, Group, or User, with an optional scope value), an action
(ALLOW, DENY, or ASK), a host pattern (an exact host, a `*.suffix` wildcard, or a bare `*` for
everything), and an optional note. Add rule creates a new one; each row has its own Delete.

By default, with no rules at all, network access is allow-all - nothing is blocked. To build an
actual allowlist, add a broad `TENANT * DENY` catch-all rule, then add specific `ALLOW` entries
for the hosts you want reachable; the more specific ALLOW entries take precedence over the
catch-all DENY.

## Rate Limits

A table of per-(agent, provider) request and token limits: requests per minute (RPM) and tokens
per minute (TPM). Leaving agent or provider blank makes the policy a wildcard, matching whatever
isn't covered by a more specific policy. Leaving the limit itself blank means unlimited; setting
it to `0` means deny-all for that agent/provider combination. The table renders these as "∞" and
"deny-all" respectively so they're not mistaken for typos. Add Policy creates a new row; each row
has its own Delete.

## Spend Caps

Monthly LLM spend caps, scoped at three levels:

- **Org** - a single cap for the whole tenant, shown with the current period's spend against it
  as a utilization bar and percentage. Set org cap / Clear manage it.
- **Per-user** - pick or type a user (a datalist suggests existing members), enter a cap amount,
  and Set cap. A table below lists every user with a cap, their current spend, their cap, and
  percentage used, each with a Delete to remove that user's cap.
- **Per-agent** - the same pattern, scoped to individual agents instead of users.

:::caution
Spend caps only work for a provider whose per-1M-token pricing is actually configured on
[LLM providers](/admin/providers/). A cap set against an unpriced provider cannot track spend
for it, so calls through that provider keep running regardless of the cap you set.
:::
