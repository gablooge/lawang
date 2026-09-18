# Sluiceway

Go service: permission-aware SaaS connectors for AI memory. Pre-alpha, private until v0.1.0.

## Where things are

- `docs/architecture.md`: the target design. It is the spec. If code needs to differ from it,
  change the document in the same branch and say why.
- `docs/roadmap.md`: milestones M0 to M6 and what "done" means. Sequence only, no dates.
- `docs/backlog.md`: the milestones cut into items B01 to B29, with dates. **This is the work queue.**
- GitHub (`gablooge/sluiceway`): item BNN is issue #NN, and M0 to M6 are milestones with due dates.
  Status lives there; order, dates and the log live in the backlog file.

## "Continue" means

1. Open `docs/backlog.md` and take the first unchecked item.
2. Branch `bNN-short-name` from `main`.
3. Build it with tests until the item's **Done when** line is true and `make check` is green.
4. Tick the box and add a line to the log at the bottom of the backlog.
5. Comment on the item's issue with what was done and anything that differs from the design
   (`gh issue comment NN`). Do not close the issue by hand: the commit message ends with
   `Closes #NN`, so it closes when the work reaches `main`.
6. Stop at the item boundary and report. Do not commit or push unless asked.

## Rules for the code

- `make check` (tidy, vet, golangci-lint, `go test -race`) must pass before an item is done.
- Dependencies stay close to the list in architecture section 12. Standard library first.
- Everything lives under `internal/` until v0.1.
- Never log or return token material, secrets, or a database URL. Errors name the variable, not
  its value.
- Fail closed: a missing tenant, secret, key or identity is a refusal, never a default.
- A test double must reject whatever the real system rejects.
- Integration tests use testcontainers Postgres and connect as the non-superuser `sluiceway`
  role, not as the superuser.
- No em dashes in prose, comments or commit messages.
