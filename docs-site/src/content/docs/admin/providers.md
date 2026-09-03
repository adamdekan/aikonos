---
title: LLM providers
description: Configuring LLM provider credentials, dialects, pricing, and tenant-wide defaults.
sidebar:
  order: 6
---

LLM providers (`/admin/providers`) is where you configure the LLM backends agents actually call:
credentials, API dialect, pricing, and which provider is the tenant default for chat and vision.

## Creating or editing a provider

The provider modal has:

- **ID** - immutable once the provider is created.
- **Name** and **Endpoint**.
- **API dialect** - `openai-completions`, `anthropic-messages`, or `azure-openai`. Choosing
  `azure-openai` reveals an API Version field, and each entry in the Models list is treated as an
  Azure deployment name rather than a model id.
- **Enabled** toggle.
- **Vision capable** toggle - only providers marked vision capable can be set as the tenant's
  default vision provider.
- **Fallback pricing** - per-1M-token input/output prices used only when a call reports no cost
  of its own. This is a backstop, not the primary pricing source.
- **API Key** - write-only; leaving it blank on an edit preserves the key already stored.
- **Models** - a repeatable list of model ids, each with an optional max output tokens, with
  add/remove controls per row. Cost is priced per provider by Fallback pricing above, not per
  model.

Leave **max output tokens** at 0 and the server picks its own default. Raise it when a workflow
reason step fails with a message about a truncated or empty response: reasoning models spend the
same output budget on their internal reasoning as on the answer, so a step that has to produce a
long structured result can exhaust the default before it writes anything. This field is the only
way to lift that ceiling without redeploying.

**Test connection**, in the modal footer, probes the provider with the values currently in the
form without saving anything - use it to check credentials before committing to Save.

## The providers table

Each row shows the provider's name (with an "unpriced - spend not tracked" warning badge when
both fallback prices are zero), endpoint, a summary of its models, and single-select toggles per
row for Enabled, Default, Default vision, and Fallback - only one provider can hold each of
these at a time. Default vision is disabled on any row that isn't vision capable. A key-present
indicator (checkmark or cross) shows whether a credential is actually stored, and Edit/Delete
round out the row.

:::caution
Deleting a provider that's currently the tenant Default, Default vision, or Fallback removes it
from that role along with everything else about it. Any spend cap that depends on this
provider's pricing (see [Settings - network & limits](/admin/settings-limits/)) stops tracking
spend for it once it's gone.
:::

## Pricing feeds spend caps

Spend caps can only track spend for a provider whose pricing is actually configured here - see
[Settings - network & limits](/admin/settings-limits/) for how caps use this pricing.
