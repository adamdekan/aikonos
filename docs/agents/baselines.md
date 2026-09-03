# Agent behavioral baselines

Operator reference for "what's normal for this agent" — spot anomalies (skill-set drift,
rate-limit saturation, latency/volume spikes) by comparing live metrics against each agent's
established baseline. Agent-specific numbers are **runtime config, not fixed constants** — this
doc gives the template and the exact PromQL/CLI queries an operator runs to derive them per
agent, not invented thresholds.

## What defines an agent's baseline

| Dimension | Source of truth | How to read it |
|---|---|---|
| Expected tool set | `agent.Skills` (agent-bound tasks) or per-user FGA `can_invoke skill:<tool>` grants (personal tasks) | Access Control UI, or `gateway /admin/skills` → broker `ListSkills` |
| RPM / TPM ceiling | `rate_limit_policies` table (`broker/internal/db/rate_limit_policies.go`) | Rate Limits admin panel, or `SELECT * FROM rate_limit_policies WHERE tenant_id = '<t>'` |
| Typical data volume | No stored baseline — derive from `aikonos.broker.tool.invoke.duration` histogram + tool-specific payload size over a representative window | PromQL below |

### Tool set

Agent-bound tasks are gated by `agent.Skills` — a flat list of tool ids, checked directly
against plan steps in `checkAgentSkills` (`broker/internal/broker/service_plan.go:498`); any
step outside that list is rejected before OPA runs. Skill bundles under `skills/` (e.g.
`web.fetch`, `doc.write`, `email.draft`) define the tool manifests an agent can be assigned;
`capabilitySkills` (`broker/internal/broker/skills_admin.go:31` — currently `scheduler`,
`workflows`, `vision`) are FGA-gated authz skills layered on top, not tool-bundle skills.

Personal (non-agent-bound) sessions have no fixed "expected set" — the tool set is whatever
skills the user's group grants via OpenFGA (`can_invoke`, `skill:<tool>`). Baseline = today's
grant list; a call to a tool outside it is a policy denial, not a metrics anomaly (`fga_decision`
in the SubmitPlan audit trail is the record to check).

**Query the current tool set for one agent:**
```sql
-- broker/internal/db (read via psql or an admin RPC, not raw app queries)
SELECT skills FROM agents WHERE id = '<agent-id>';
```
Or via UI: Access Control → agent detail, or `GET /admin/agents/:id` (agent-gateway proxy).

### RPM / TPM ceiling

Row shape (`rate_limit_policies`, migration 021): `tenant_id`, `agent_id` (nullable — nil applies
tenant-wide), `provider` (nullable — nil applies to all LLM providers, RPM-only ignores
provider-scoped rows), `rpm_limit` (int32, nullable), `tpm_limit` (int32, nullable). Encoding at
enforcement time (`broker/internal/ratelimit/limiter.go`): `nil` = unconstrained, `0` = deny-all,
`>0` = enforced ceiling. RPM is checked per-agent and per-tenant (global) window; TPM is checked
per-agent, per-provider, and per-tenant window — all fixed 1-minute windows.

**Query the current policy set for one tenant:**
```sql
SELECT agent_id, provider, rpm_limit, tpm_limit FROM rate_limit_policies WHERE tenant_id = '<t>';
```
Or via UI: admin Rate Limits panel.

### Typical data volume

No first-class "data volume" metric exists. Approximate it from tool-invocation counts and
duration (`aikonos.broker.tool.invoke.duration` — see PromQL below) plus tool-specific evidence
(e.g. `web.fetch` response size is not currently instrumented; use the audit trail's per-call
`content_length` field for `web.fetch` specifically, or workspace file sizes under
`Root/<tenant>/<user>/` for `doc.write`/file-producing tools).

## PromQL — F23 lifecycle metrics

Instruments defined in `broker/internal/metrics/task.go`, registered on OTel meter
`github.com/adamdekan/aikonos/broker`. Names below are the Prometheus series names the OTel
Prometheus exporter produces by applying its standard dot→underscore rename plus the
`_total` (monotonic counter) / `_seconds_{bucket,sum,count}` (histogram, unit `s`) suffixing to
each instrument. Confirm against your running stack before relying on them:
`curl -s localhost:9095/api/v1/label/__name__/values | grep aikonos_broker`. As with every
broker series, Prometheus overwrites the scrape-time `job` label; filter on `exported_job`
(the `job` value the collector itself reports), same gotcha as the CP0.2 rate-limit alert.

**Task transitions** (`aikonos.broker.task.transitions`, `Int64Counter`, attrs `from`/`to`/`tenant`)
→ `aikonos_broker_task_transitions_total`:
```promql
# transition rate for one agent's tenant, by from/to pair
sum by (from, to) (rate(aikonos_broker_task_transitions_total{exported_job="aikonos-broker", tenant="<tenant-id>"}[5m]))

# baseline: establish over a representative window, then alert on deviation
sum by (from, to) (increase(aikonos_broker_task_transitions_total{exported_job="aikonos-broker", tenant="<tenant-id>"}[1h]))
```
Anomaly signal: an agent whose baseline is mostly `EXECUTING→COMPLETED` suddenly showing a spike
in `EXECUTING→FAILED` or `EXECUTING→CANCELLED`.

**Plan outcomes** (`aikonos.broker.plan.outcomes`, `Int64Counter`, attr `outcome` —
`DENIED`/`NEEDS_STEP_UP`/`NEEDS_HUMAN`/`APPROVED`) → `aikonos_broker_plan_outcomes_total`:
```promql
sum by (outcome) (rate(aikonos_broker_plan_outcomes_total{exported_job="aikonos-broker"}[5m]))
```
Anomaly signal: a rising `DENIED` share for a given agent/tenant relative to its established
`APPROVED`-dominant baseline — indicates skill-boundary drift or a compromised plan source. This
counter has no `tenant`/`agent` attribute today; per-tenant isolation requires cross-referencing
the audit trail (`plan.outcome` events carry tenant/agent).

**Tool invocation duration** (`aikonos.broker.tool.invoke.duration`, `Float64Histogram`, unit `s`,
attrs `tenant`/`tool_id`) → `aikonos_broker_tool_invoke_duration_seconds_{bucket,sum,count}`:
```promql
# p95 latency per tool, for one tenant
histogram_quantile(0.95, sum by (le, tool_id) (rate(aikonos_broker_tool_invoke_duration_seconds_bucket{exported_job="aikonos-broker", tenant="<tenant-id>"}[5m])))

# call volume per tool (proxy for data volume when payload size isn't instrumented)
sum by (tool_id) (increase(aikonos_broker_tool_invoke_duration_seconds_count{exported_job="aikonos-broker", tenant="<tenant-id>"}[1h]))
```
Anomaly signal: p95 latency or call count for a given `tool_id` deviating sharply from the
agent's established baseline — either a runaway loop (call count) or a struggling downstream
(latency).

## PromQL — rate-limit hits

`rate_limit_hits_total` (`Int64Counter`, emitted from `broker/internal/ratelimit/limiter.go`'s
`recordHit`, registered in `broker/cmd/broker/main.go:380` — name already includes `_total` at the
OTel level, so no collector suffixing/renaming applies here; independently re-verified per
CP0.2). Attrs: `tenant`, `agent`, `limit_type` (`"rpm"` or `"tpm"`), `result`
(`"allowed"`/`"denied"`).

```promql
# denial rate for one agent — sustained non-zero means it's running against its ceiling
sum by (limit_type) (rate(rate_limit_hits_total{exported_job="aikonos-broker", tenant="<tenant-id>", agent="<agent-id>", result="denied"}[5m]))

# baseline: an agent that never denies has headroom; one denying occasionally near legitimate
# peak load is expected — the anomaly is a step-change in denial rate, not any nonzero value
increase(rate_limit_hits_total{exported_job="aikonos-broker", tenant="<tenant-id>", agent="<agent-id>", result="denied"}[1h])
```
The existing Grafana alert (`deploy/compose/obs/grafana/provisioning/alerting/rate-limit-alert.yaml`)
and dashboard panel (`deploy/compose/obs/grafana/provisioning/dashboards/broker-actions.json`)
use this exact series.

## Automated baseline learning

Everything above is this repo's **hand-authored reference** — an operator derives it once and
updates it manually. The broker also maintains a **learned envelope** per agent, computed from
observed `InvokeTool` activity, no operator action required. Two sources, two jobs:

| Source | What it holds | Who keeps it current |
|---|---|---|
| This doc | Intent — what an agent's tool set/RPM/TPM ceiling *should* be, per its skill grants and rate-limit policy | Operator, on skill/policy change |
| The learner (`broker/internal/baseline/`) | Reality — what an agent *actually* does, re-derived on a ticker | Itself, automatically |

Neither supersedes the other. The learner cannot see intent (a newly-granted tool is legitimate
but unknown until it learns); this doc cannot see drift (a compromised agent calling its
existing tools far more than usual reads as "in policy" here). Use both.

**Pipeline.** Every successful agent-bound `InvokeTool` call is observed in-memory, flushed to
`agent_behavior_windows` (migration 032) on a window ticker (`AIKONOS_BASELINE_WINDOW_SECONDS`,
default 60s), and periodically rolled up by a learn ticker
(`AIKONOS_BASELINE_LEARN_INTERVAL_SECONDS`, default 3600s) into `agent_baselines`: `tool_set`
(union of tool ids seen), `rpm_p95` and `cost_p95` (95th percentile per-window totals over the
last N windows). Personal (non-agent-bound) sessions are out of scope — no stable agent key to
learn against.

**Maturity gate.** No drift is emitted until a baseline has `sample_windows ≥
AIKONOS_BASELINE_MIN_SAMPLE_WINDOWS` (default 30) — a freshly-created agent has no envelope to
drift from, so the learning phase is silent by design.

**Drift kinds**, checked per just-closed window once mature:

- **`unknown_tool`** — the window called a `tool_id` not in the learned `tool_set`. Always
  emitted (high signal); a legitimately newly-granted tool surfaces once and the next learn
  cycle absorbs it.
- **`rate`** — window invocations exceed `rpm_p95 × AIKONOS_BASELINE_DRIFT_MULTIPLIER` (default
  2.0), not bare p95. By construction ~5% of normal windows exceed p95, so gating on the
  multiplier (not p95 itself) keeps the signal to "materially above normal."

**Monitoring, not enforcement.** A drift finding never blocks, delays, or fails `InvokeTool` —
it emits one `aikonos.agent.baseline_drift` audit event (context: `kind`, `observed`, `ceiling`,
`tool`) and increments the `aikonos.broker.agent.baseline_drift` OTel counter. Enforcement
remains with the rate limiter (RPM/TPM ceilings above) and the four authz layers; this signal is
purely for an operator (or the Grafana alert
`deploy/compose/obs/grafana/provisioning/alerting/baseline-drift-alert.yaml`) to notice and
investigate.

**PromQL — drift counter** (`aikonos.broker.agent.baseline_drift`, `Int64Counter`) →
`aikonos_broker_agent_baseline_drift_total`:
```promql
# drift events per agent, by kind, over the last 5 minutes
sum by (tenant, agent, kind) (increase(aikonos_broker_agent_baseline_drift_total{exported_job="aikonos-broker"}[5m]))
```
Same `exported_job` gotcha as every other broker series in this doc (Prometheus overwrites the
scrape-time `job` label).

**Tuning knobs** (broker viper + `compose.yaml`, all `AIKONOS_BASELINE_*`):

| Var | Default | Meaning |
|---|---|---|
| `AIKONOS_BASELINE_ENABLED` | `true` | Master switch; `false` disables the recorder + both tickers, `InvokeTool` unaffected |
| `AIKONOS_BASELINE_WINDOW_SECONDS` | `60` | Flush interval — how often in-memory observations land in `agent_behavior_windows` |
| `AIKONOS_BASELINE_LEARN_INTERVAL_SECONDS` | `3600` | How often the envelope (`agent_baselines`) is recomputed |
| `AIKONOS_BASELINE_MIN_SAMPLE_WINDOWS` | `30` | Maturity gate — windows required before drift can fire |
| `AIKONOS_BASELINE_DRIFT_MULTIPLIER` | `2.0` | Rate-drift threshold multiplier over `rpm_p95` |
| `AIKONOS_BASELINE_RETENTION_WINDOWS` | `10080` | Raw-window retention before pruning (1-minute windows → 7 days) |

## Establishing a baseline for a new agent

1. Read its `agent.Skills` and the tenant's `rate_limit_policies` row (queries above) — this is
   the *declared* ceiling, not yet an observed baseline.
2. Run the agent under expected load for a representative window (a work day, a batch cycle —
   whatever matches its actual usage pattern).
3. Snapshot the four query families above (`increase(...[<window>])` variants) for that window.
   Record the numbers here or in the tenant's runbook — this repo does not template per-agent
   baseline files; there is no fixed set of agent types to enumerate statically.
4. Re-run periodically (e.g. after a soul/skill change, F28) since a baseline drifts with the
   agent's actual job, not just with attacker behavior.
