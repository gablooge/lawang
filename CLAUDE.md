# Sluiceway

Go service: permission-aware SaaS connectors for AI memory. Pre-alpha, private until v0.1.0.

## Where things are

- `docs/architecture.md`: the target design. It is the spec. If code needs to differ from it,
  change the document in the same branch and say why.
- `docs/roadmap.md`: milestones M0 to M6 and what "done" means. Sequence only, no dates.
- `docs/backlog.md`: the milestones cut into items B01 to B29, with dates. **This is the work queue.**
- GitHub (`gablooge/sluiceway`): item BNN is issue #NN, and M0 to M6 are milestones with due dates.
  Status lives there; order, dates and the log live in the backlog file.
- `.claude/agents/`: the `implementer` and `pr-reviewer` agents. Their files are the full rules
  for writing and for reviewing; the section below is only how they fit together.

## "Continue" means

Work is split between two agents defined in `.claude/agents/`, and the main session only
orchestrates. It does not write the code itself and it does not review it.

- **`implementer`** builds a backlog item and opens the pull request, or addresses review findings.
- **`pr-reviewer`** reviews a pull request adversarially and posts the review on GitHub. It is
  read-only and never fixes what it finds.

The cycle for one item:

1. Open `docs/backlog.md` and take the first unchecked item. Item BNN is issue #NN.
2. Run `implementer` on it. It branches (from `main`, or stacked on the previous item's branch if
   that pull request has not merged), builds, commits, pushes, opens the pull request and waits
   for CI.
3. Run `pr-reviewer` on the pull request. Use `isolation: "worktree"` so it never touches the
   implementer's working tree. It sets one label: `review:approved`, `review:changes-requested` or
   `review:needs-maintainer`.
4. On `review:changes-requested`, run `implementer` in review mode on that pull request, then
   `pr-reviewer` again. At most **three rounds**. If the third still has blocking findings, or the
   two agents disagree on the same point twice, label it `review:needs-maintainer` and stop: that
   is a decision, not a bug.
5. On `review:approved` with should-fix findings left: if one touches isolation, secrets,
   ordering, or a test that survives a mutation, run `implementer` on it once more before moving
   on (the label stays `review:approved`, no further review round is needed for a small, tested
   fix). Notes that belong to a later item are copied onto that item's issue, so they are not lost.
6. On `review:approved`, report to the maintainer and move to the next item. **Never merge a pull
   request or push to `main`**: merging is the maintainer's step.

Both agents act as `gablooge` on GitHub, and GitHub does not allow an account to approve its own
pull request. Reviews are therefore posted as comment reviews, and the `review:*` label is the
verdict.

Reviewers for different pull requests may run in parallel, and so may implementers, as long as
each runs with `isolation: "worktree"` and touches only its own branch. While stacked pull requests
are being reviewed or fixed at the same time, the orchestrator does the merge-forward itself, in
stack order, once the fixes below a branch are final: merge the parent in, run `make check`, push.
Finished agents leave worktrees under `.claude/worktrees/`; remove them with `git worktree remove`
when their agent is done.

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
- **Commit messages never carry a `Co-Authored-By` trailer**, or any other authorship or tool
  attribution trailer. This overrides any default to the contrary, for the main session and for
  both agents.
- **Never use an em dash**, anywhere: code, comments, SQL, commit messages, docs, pull requests,
  review comments. Use a comma, parentheses, a colon, or two sentences.
