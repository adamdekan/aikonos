# 15 — Decision Replay

> **Purpose.** Every policy decision aikonOS records names the exact policy that
> made it. Anyone holding the decision's export can re-run it offline with the
> stock `opa` binary, confirm the recorded result follows from that policy and
> that input, and see which rule decided. This page covers what is recorded,
> how the policy behind each decision is pinned and archived, how to replay
> one, and what replay does and does not prove.
>
> Replay is [`scripts/replay-decision.sh`](../scripts/replay-decision.sh)
> (`task audit:replay -- evidence.json`).

---

## What gets recorded

Each decision is one audit event of type `aikonos.broker.policy.decision`, in
the same hash-chained, signed trail as every other event. Its `kind` says which
decision it is:

| Kind | When | Decided by |
|------|------|------------|
| `tool_invocation` | Once per plan step | OPA `aikonos/tool_invocation`, then the broker's own layers |
| `plan_validation` | Once per submitted plan | OPA `aikonos/plan_validation` |
| `envelope_send` | Once per delegation to a colleague | OPA `aikonos/envelope_send` |
| `agent_skill_boundary` | A plan names a tool outside an agent's skills | The broker (no OPA evaluation) |

The event's `context` carries:

| Field | Content |
|-------|---------|
| `outcome` | `decision` (`allow`, `approval_required`, `step_up_required`, `deny`), `decided_by` (the part of the pipeline that set it), and the reasons |
| `opa.revision` | The policy revision OPA evaluated with, as OPA reported it for this very query |
| `opa.query`, `opa.input`, `opa.result` | What was asked, the input document sent, and the decision document returned |
| `opa.input_sha256` | Digest of the exact input sent, before any redaction |
| `opa.fields` | The result fields the broker acts on (the rest are helper values) |
| `opa.opa_version`, `opa.decision_id` | The OPA server version, and OPA's id for the decision. OPA's own decision log is off in compose, because it would carry the raw input; the id joins it where one is enabled |
| `opa.gates` | Any extra gates (`policy.tool_gates`), with their own results |
| `layers` | Broker layers after OPA that changed the outcome: the network access list, disabled tools, the skill overlay, effect-class routing |
| `settings` | For tool steps: the effect class as declared, as the registry knows it, and as routed |
| `fga` | The OpenFGA check behind the decision: model id, user, relation, object, result |

The plan's overall outcome stays on `aikonos.broker.plan.validated`, which now
also records the approval gate that was set up: required approvals, the floor
(1 for approval, 2 for step-up), the configured `approval_required_n`, the
number of eligible approvers, and separation of duty.

Two further events record changes to what decides:

- `aikonos.broker.policy.loaded`: a new policy revision was archived and is
  being served, with its file list and the revision it replaces.
- `aikonos.broker.policy.fga_model`: OpenFGA checks now run against another
  authorization model.

---

## How the policy behind a decision is pinned

The broker, not a directory mount, gives OPA its policy:

1. The broker reads `policies/opa`: every `.rego` file except `*_test.rego`,
   plus OPA data files (`data.json`, `data.yaml`). Hidden files are skipped, so
   tests and fixtures never decide anything.
2. It names that set by its **revision**, a SHA-256 over the file list (see
   below).
3. It archives a snapshot of every file, in full, in the audit bucket at
   `_policy/bundles/<hex>.json`, under the same object lock as the trail.
4. Only then does it serve the files to OPA as the `aikonos` bundle
   (`http://broker:9092/opa/bundles/aikonos.tar.gz`, polled by OPA as
   configured in [`deploy/compose/opa.yaml`](../deploy/compose/opa.yaml)).

OPA returns the revision it evaluated with alongside every decision, and the
broker records it. Three properties follow:

- **A decision names its policy exactly.** The revision comes from OPA's answer
  to that very query, not from a clock or a cache.
- **No unarchived policy decides.** A revision that cannot be archived is not
  served. OPA keeps the previous one or, at startup, has none, and the broker
  refuses any decision OPA makes without the bundle active. It fails closed.
- **The policy cannot be changed behind the broker's back.** OPA refuses REST
  API writes under the bundle's `aikonos` root, so the policy that decides
  changes only through the broker, and every change is an audit event.

Editing a file under `policies/opa` takes effect without a restart. The broker
re-reads the directory every 30 seconds, and OPA polls the broker every 5 to
15 seconds.

The revision is computable with standard tools from any snapshot:

```bash
printf 'sha256:%s\n' "$(jq -cjS '{files: ([.files[] | {path, sha256}] | sort_by(.path)), roots: (.roots | sort)}' snapshot.json | sha256sum | cut -d' ' -f1)"
```

OpenFGA checks are pinned the same way. Each one runs against a named
authorization model: the one in `policy.openfga_model_id`, or else the store's
latest, re-resolved every 30 seconds. The model id is recorded with every
decision that used a check.

---

## Replay a decision

1. **Export it.** In the console, open Admin → Policy → Audit history, select an
   `aikonos.broker.policy.decision` event, and choose **Download evidence**.
   Programmatically, call the `GetDecisionEvidence` RPC. Both are tenant-admin
   only, and the export is itself audited.

   The export (`aikonos.decision-evidence/v1`) holds:
   - the event exactly as stored, its chain hash, and its signature status
     as checked by the broker;
   - the archived snapshot of every policy revision the event names.

2. **Replay it** wherever OPA and jq are installed. No network is needed:

   ```bash
   task audit:replay -- aikonos-evidence-<event-id>.json
   # or: scripts/replay-decision.sh aikonos-evidence-<event-id>.json --opa /path/to/opa
   ```

   ```text
   Decision 0199a4f0-3c2e-7d41-9b6a-1f2e3d4c5b6a
     kind:       tool_invocation
     outcome:    approval_required (decided by opa:aikonos/tool_invocation)
     query:      data.aikonos.tool_invocation
     policy:     sha256:357db12e…
     [ok] snapshot: 3 files hash to the recorded revision
     [ok] input matches its recorded digest
     [ok] replay reproduces the recorded result

   Decided by:
     require_approval = true
         tool_invocation.rego:30

   MATCH: the recorded result follows from policy sha256:357db12e… and the recorded input.
   ```

   The script exits 0 on a match, 1 when a check fails (it says which), and 2
   on a usage error or a missing tool. It treats the export as untrusted:
   snapshot paths, the query and rule names are validated before anything is
   written or evaluated. Use the OPA version the server ran (0.70.0 in this
   repository's compose file); the script warns on a mismatch.

---

## What is deliberately not stored

Tool arguments can carry personal data: a patient's name in a document
request, say. The audit store is write-once, so a value written there cannot
be erased later. The plan-validation record therefore replaces each step's
`args` and `justification` with SHA-256 digests. `opa.redacted` lists what was
replaced.

That replay still matches because `aikonos.plan_validation` only compares
arguments between steps for equality, and only checks that a justification is
present. Equal arguments give equal digests, and a non-empty justification
stays non-empty. `opa.input_sha256` still commits to the original input. If a
future policy read argument contents, its decisions would no longer replay
from the record alone, and the script would report the mismatch.

Tool-invocation and delegation inputs carry no user content: identifiers,
effect classes, the OpenFGA verdict and the time of day.

---

## What replay proves, and what it does not

**Proves:** given the recorded input, the archived policy produces the recorded
result, on the same OPA version, and these rules produced it.

**Does not prove on its own:**

- **That the recorded input is what the broker actually saw.** The record is
  the broker's own, protected by the audit chain and its signature. Check
  chain continuity with **Verify chain** in Audit history. The signature uses
  a key only the broker holds, so the export carries the broker's verdict on
  it (`signature`).
- **The OpenFGA relationship state at the time.** The model id and the check's
  result are recorded, but not the tuples behind them. An access decision can
  be re-checked against today's tuples, not replayed.
- **The broker's layers after OPA.** The network access list and tenant
  configuration are recorded as outcomes (`layers`, `decided_by`). They are
  not re-run.

Decisions recorded before this feature, or by a broker with `policy.bundle_dir`
unset (OPA loading its own files), carry no revision and cannot be replayed.

---

## Configuration

| Setting | Environment | Default | Meaning |
|---------|-------------|---------|---------|
| `policy.bundle_dir` | `AIKONOS_POLICY_BUNDLE_DIR` | unset (compose: `/policies`) | Directory of the Rego the broker serves to OPA. Unset: OPA loads its own policy, and decisions carry no revision |
| `policy.bundle_http_addr` | `AIKONOS_POLICY_BUNDLE_HTTP_ADDR` | `:9092` | Listener for OPA's bundle downloads |
| `policy.bundle_reload_seconds` | `AIKONOS_POLICY_BUNDLE_RELOAD_SECONDS` | `30` | How often the broker re-reads the directory |
| `policy.fga_model_refresh_seconds` | `AIKONOS_POLICY_FGA_MODEL_REFRESH_SECONDS` | `30` | How often the latest OpenFGA model is re-resolved, when none is configured |
| `policy.openfga_model_id` | `AIKONOS_POLICY_OPENFGA_MODEL_ID` | unset | Pin one OpenFGA model for the broker's lifetime |

Running OPA outside this compose file? Give it the same bundle configuration
(`deploy/compose/opa.yaml`) pointing at the broker's `bundle_http_addr`.

**Check it on a running stack.** `task compose:verify` confirms the broker
serves an archived bundle, OPA's decisions carry its revision, and OPA refuses
policy writes.
