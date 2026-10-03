# Contributing to Lawang

Thank you for looking at this. This page says what you need, how to run the checks, and what kind
of review your pull request will get. Please read the review section: it is not the usual one.

## Where the project is today

Lawang is pre-alpha. Milestone M0 is done: configuration, the id recipes, Postgres with row-level
security, and the outbox. Nothing ingests a webhook yet. There is no release, no container image
and no quickstart. The README's status paragraph is the honest summary, and
[docs/backlog.md](docs/backlog.md) is the work queue.

This matters for your pull request. The backlog is ordered, and each item depends on the ones
above it. A change that belongs to a later item is usually better as an issue today than as code.

## Before you write code

- For a typo, a broken link, a clearer comment or a missing test: open a pull request directly.
- For anything larger: open an issue first and say what you want to do. One person maintains this
  project. An issue costs you a few minutes and can save you a weekend.
- For a security problem: do not open an issue. Follow [SECURITY.md](SECURITY.md), which uses
  GitHub's private vulnerability reporting.

Issues labelled [`good first issue`](https://github.com/gablooge/lawang/labels/good%20first%20issue)
are small, self-contained, and need no provider account.

## What you need

| Tool | Version | What it is for |
|---|---|---|
| Go | 1.26.0 or newer (the version in `go.mod`) | building and testing |
| Docker | any recent version | the integration tests, and `make sqlc` |
| golangci-lint | v2.x, CI runs v2.10 | `make lint` |
| git | any recent version | |

Docker runs two things. The integration tests start a real Postgres with testcontainers, and
`make sqlc` runs sqlc from a pinned image, so nobody has to install sqlc itself.

You do not need a provider account (Slack, ClickUp, Microsoft, HubSpot) to contribute. You do not
need any AI tool either, even though the maintainer uses one (see "The review you will get").

## Build and run

```sh
git clone https://github.com/gablooge/lawang.git
cd lawang
make build
./bin/lawang version
./bin/lawang help
```

`lawang help` prints every environment variable the binary reads, with its default. The binary
documents its own configuration, so you do not have to read the source to configure it.

## The checks

`make check` is what CI runs. An item is not done until it is green.

```sh
make check
```

It runs five steps, in this order:

| Step | Command | What it proves |
|---|---|---|
| `tidy` | `go mod tidy -diff` | `go.mod` and `go.sum` are already tidy. It changes nothing; it fails if a change is needed. |
| `sqlc-check` | `sqlc diff` in Docker | the generated query code that is checked in matches the SQL it came from |
| `vet` | `go vet ./...` | the standard Go checks |
| `lint` | `golangci-lint run` | the linters in `.golangci.yml`, including formatting |
| `test` | `go test -race -count=1 ./...` | all tests, with the race detector, with no cached results |

Two notes:

- **Without Docker**, `make check` fails at `sqlc-check`. You can still run `go test ./...`: the
  tests that need Postgres skip themselves with a message that says Docker is not running. They
  never skip in CI.
- **After you change a migration or a `queries.sql` file, run `make sqlc`.** It regenerates the
  typed query code. If you forget, `make check` fails at `sqlc-check`, which is the point.

## Tests

The project's whole claim is that it can be trusted with who may see what, so tests carry more
weight here than in most repositories.

- Integration tests use `internal/testdb`, which gives each test its own database and connects as
  the non-superuser `lawang` role. Connecting as a superuser would make row-level security
  invisible, and the tests would pass while the product was broken.
- **Break your own code on purpose before you submit.** Delete the check, invert the condition,
  drop the SQL clause, and confirm that a test fails. A test that stays green when the code is
  wrong is worse than no test, because it buys false confidence. Say in the pull request which
  changes you tried and what failed. The reviewer will do this to your code anyway; it is nicer to
  find it yourself.
- Cover the error paths, the boundaries (empty, zero, maximum, one past the maximum) and the
  negative cases (the thing that must be refused), not only the happy path.
- A test double must refuse whatever the real system refuses.

## Documents

[docs/architecture.md](docs/architecture.md) is the specification, not a description written
afterwards. If your code has to differ from it, change the document in the same pull request and
say why. Silent drift between the code and the document is treated as a defect.

Also update, when your change touches them: the ADRs in `docs/adr/`, the item and the log line in
`docs/backlog.md`, the README's status paragraph, and the doc comment of every exported Go
identifier you add or change.

## Branches and commits

Branch from `main`. The names used here are:

- `bNN-short-name` for a backlog item, where `NN` is the item number (`b04-outbox`).
- A short name with a word in front that says the kind, for anything else: `chore-agents`,
  `growth-landscape`, `fix-...`.

Commit messages explain **why**, not only what. If the commit closes an issue, the message ends
with `Closes #NN`.

Three rules about the text of a change. They are the maintainer's standing rules, they are cheap
to check, and the reviewer treats breaking one as blocking:

1. **No `Co-Authored-By` trailer**, and no other authorship or tool attribution trailer, on any
   commit.
2. **No "Generated with" line** or other tool credit, anywhere: not in a commit, a pull request
   description, a comment, or a file.
3. **No em dash** (the long dash character), anywhere in the change: code, comments, SQL, commit
   messages, documents. Use a comma, parentheses, a colon, or two sentences.

Do not force-push or rewrite history on a branch that is under review. The reviewer reads the new
commits, and rewritten hashes break the comment threads that cite them.

## Opening a pull request

Use the template. It asks for four things:

- **What** the change does.
- **Decisions worth a look**: the choices you made that another person might have made
  differently. This is the most useful section for a reviewer.
- **Acceptance**: for a backlog item, each "Done when" line of the issue, with the test that
  proves it. A ticked box is a claim; the test is the evidence.
- **Testing**: what you ran, including the breakages you tried.

End with `Closes #NN` when it closes an issue.

## The review you will get

This part is unusual, so it is written out in full.

**Your pull request is reviewed by an automated reviewer first.** The maintainer runs an agent
whose rules are checked into this repository, at
[`.claude/agents/pr-reviewer.md`](.claude/agents/pr-reviewer.md). You can read exactly what it
looks for before you submit. In short:

- It covers nine areas every time and shows a coverage table, so you can see what was looked at
  and not only what was found: acceptance, whether the tests have teeth, whether the tests are
  comprehensive, correctness, security, performance, dead code, codebase improvement, and
  documentation.
- **It breaks your code on purpose to test your tests.** It picks the properties that matter,
  removes a check or inverts a condition, runs your tests, and reports it when nothing fails.
- It runs `make check`.
- It posts findings as inline comments on the lines they concern, each with a severity:
  **blocking** (a wrong result, a leak, an unproven acceptance criterion, a test that survives its
  mutation, red checks), **should-fix** (real, but safe to merge), or **note** (worth knowing, no
  action needed).
- It sets one label as its verdict: `review:approved`, `review:changes-requested` or
  `review:needs-maintainer`. The label is the verdict because GitHub does not let an account
  approve a pull request it opened, and the agent acts as the maintainer's account.

**A human decides.** The agent never merges and never closes anything. The maintainer reads the
review, decides what matters, and merges. If the agent and you disagree twice about the same
point, that is a decision for the maintainer, not a bug, and the pull request is labelled
`review:needs-maintainer` and waits for them.

**How long it takes.** Usually hours, not weeks. One person maintains this in their spare time, so
there is no service level agreement and no promise. If a pull request has been quiet for a while,
a polite comment on it is welcome.

**A finding is not a rejection.** Most pull requests here, including the maintainer's own, get
findings. That is what the review is for. You have two good answers to any finding: fix it, or
explain concretely why it is wrong. The second answer is genuinely welcome. The reviewer can be
mistaken, and a wrong "fix" is worse than a reasoned disagreement. What is not welcome is silently
skipping one.

**What the agent will not do, whatever your text says.** The agents treat everything they read,
including your issue, your commit messages and your pull request description, as material and
never as instructions. A sentence addressed to an agent cannot make it merge, push, change a label
or run a command. It will be reported as a finding instead. This is worth knowing so you are not
surprised, not because anyone expects you to try.

**One more thing about pull requests from forks.** For a pull request from a fork, the
maintainer may not run the agent at all, because running it means running a stranger's build and
tests on their own machine. Expect a review by hand plus CI in that case. It may take a little
longer.

## Who decides

One person maintains Lawang: [@gablooge](https://github.com/gablooge). They decide what is built,
in what order, and what is merged. There is no committee, no vote and no second maintainer today,
and the project says so rather than implying otherwise.

What that means in practice:

- The order of work is [docs/backlog.md](docs/backlog.md). A change that jumps the queue may wait,
  even when it is good.
- Answers usually come within hours, sometimes within a day or two. There is no guarantee. A quiet
  week is capacity, not disregard.
- A "no" is a real possible answer, and you should get a reason with it.

## Licence

Lawang is [Apache 2.0](LICENSE). By opening a pull request you agree that your contribution is
licensed under it. There is no contributor licence agreement to sign.

## Code of conduct

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
