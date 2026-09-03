# Contributing

Issues and pull requests are welcome.

## Before you open a pull request

Sign off your commits with `git commit -s`. That records your agreement with
the [Developer Certificate of Origin](https://developercertificate.org/), which
is how contributions are accepted here. There is no CLA.

Run what CI runs:

```bash
task broker:test     # Go unit tests
task policy:test     # OPA policy tests
task test:all        # gateway, webui and docs-mcp suites
task compose:verify  # smoke-test a running stack
```

CI runs the same suites plus a compose config drift check, a migrations check,
and an OpenSSF Scorecard scan. A pull request that fails locally will fail
there too.

## Two conventions that are enforced

These are not style preferences. Breaking either one breaks a security
property, so a reviewer will ask you to change it.

**Database access goes through `broker/internal/db/` and nowhere else, and
every query begins with `withTenant()`.** That call sets the Postgres session
variable row-level security reads. A query that skips it runs without tenant
isolation.

**Task state changes go through `db.TaskRepo.Transition()`.** The state machine
is the only authority on which transition is legal. Writing a task's status
directly bypasses that check.

## Where to start

`ROADMAP.md` lists open work and the architectural invariants any change has to
preserve. Reading the invariants first will save you a rejected pull request:
they are the reason the codebase is shaped the way it is.

For how the authorization model fits together, read
[`docs/02-policy-model.md`](docs/02-policy-model.md).

## Security issues do not belong here

Do not open an issue for a suspected vulnerability. See
[SECURITY.md](SECURITY.md) for the private reporting channel.

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
