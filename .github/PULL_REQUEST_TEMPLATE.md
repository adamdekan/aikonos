## What this changes

<!-- What behaviour differs after this, and why. Not a file list. -->

## How it was verified

<!-- The commands you ran and what they reported. -->

- [ ] `task broker:test`
- [ ] `task policy:test`
- [ ] `task test:all`
- [ ] `task compose:verify` against a running stack

## Checklist

- [ ] Commits are signed off (`git commit -s`)
- [ ] Database access goes through `broker/internal/db/` and every query starts with `withTenant()`
- [ ] Task state changes go through `db.TaskRepo.Transition()`
- [ ] No architectural invariant in `ROADMAP.md` is weakened, or the change explains why the weakening is correct
- [ ] Documentation updated where behaviour changed
