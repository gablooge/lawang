# Contributing to Lawang

> **This file is a draft.** It lives in `growth/drafts/` and is not in force. It takes effect only
> if the maintainer moves it to the repository root. Passages marked
> `[maintainer: confirm or change]` are claims only they can settle. Delete this block and those
> markers on the way.

Thank you for looking at this. This page says what you need, how to run the checks, and what kind
of review your pull request will get. Please read the review section: it is not the usual one.

## Where the project is today

Lawang is pre-alpha. There is no release, no container image and no quickstart.

This page does not say what is built, on purpose. A status written down twice goes out of date in
one of the two places, and the one a contributor reads first is the one that misleads them. Two
places are kept current, and they are the answer:

- The status paragraph at the top of [README.md](README.md): what runs today, in prose.
- [docs/backlog.md](docs/backlog.md): the work queue, item by item, with what is done and what is
  next. Item BNN is issue #NN on GitHub, and the issue carries the real state.

Read both before you write anything. The project moves quickly enough that a fortnight-old
impression of it is usually wrong.

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

You do not need a provider account to contribute: not ClickUp or Slack, which v0.1.0 connects, and
not Microsoft or HubSpot, which come after it. You do not need any AI tool either, even though the
maintainer uses one (see "The review you will get").

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

**It is not one review. Expect up to three rounds.** The cycle is written down in `CLAUDE.md`
under "The cycle for one item". The reviewer posts its findings. You answer every one of them,
with a fix or with a reason why it is wrong, and push. The reviewer reads your new commits and
reviews again. Three rounds is the limit: if blocking findings survive the third, or the same
point is disputed twice, the pull request is labelled `review:needs-maintainer` and waits for a
person to decide. Rounds can also happen after an approval, when a later fix gets a delta review
of just the new commits. Pull request #47 is a worked example, with three full rounds and three
delta reviews. This is normal here and it is not a judgement on your change, but it does mean the
exchange is longer than one review, and all of it is on the record in public.

**How long it takes.** This depends on whether your change is on the backlog queue, and a change
from outside the project is not. Measured from this repository's own pull requests on 2026-10-04:

| Kind of pull request | First review | Opened to merged |
|---|---|---|
| A backlog item, in queue order. Ten so far | 16 to 103 minutes, every one of them | 4 hours 34 minutes to 12 days. The two slowest are #48 and #49, and the reason is below |
| Everything else. Seven so far | 16 minutes to 15 days. Three were read within eight hours (#33, #44, #51), three waited two weeks (#35, #45, #46), and #34 was merged with no review posted at all | #33, #34, #44 and #51 merged inside a day. #46 merged 14 days 8 hours after it was opened, on the day its first review arrived. #35 and #45 are still open |

So the honest recent range for a pull request from outside the queue is **days to weeks**, and the
longest wait on record is the one a contributor would be in. One person maintains this in their
spare time. There is no service level agreement, no promise and no target: the table is a
measurement of the past, not a commitment about the future, and it may get better or worse.

**What the second row really measures**, because the label is wider than the thing. It is not
"not a backlog item". Four pull requests outside the queue were handled as fast as anything in it.
The three that waited two weeks (#35 waited 15 days, #45 and #46 waited 14 days each) are
documentation and growth branches, opened within two days of each other, and they were parked
while the queue ran. #46 then merged an hour after its first review finally arrived, which is the
shape of the whole row: the wait is until somebody sits down, and after that it is quick. So the
split is between what the maintainer was working on at the time and what was parked. Your pull
request will in practice be the parked kind, which is why days to weeks is the number to plan for,
but the mechanism is attention and not a rule about labels.

**And the eleven days in the first row are not a stacking delay.** #48 was opened on
2026-09-21T01:23:45Z, first reviewed 41 minutes later, approved at 03:04:26Z, and approved again
after a close-out delta review at 04:34:58Z. It then sat approved, green and unblocked until it
was merged on 2026-10-02T13:47:29Z, 11 days and 9 hours later. Nothing was below it to wait for:
#47 had already merged 54 minutes before #48 was opened. #49 is the one that did wait on the
branch below it, because its branch carried #48's commits and could not go in first, and it merged
18 hours after #48 did. The branch was not the only thing holding #49, though, and on its own it
was not enough. #49 was labelled `review:needs-maintainer` at 2026-09-21T08:20:31Z, and nothing at
all happened on it until 2026-10-02T14:30:50Z, 11 days and 6 hours later, 43 minutes after #48
merged. It was not approved until 2026-10-03T01:49:07Z. Had #48 merged on day one, #49 still could
not have merged, because it was waiting for a person to decide and not for a branch. The two
causes ended within the same hour, which is what makes either one look sufficient by itself. So
the longest waits in the queue row are the merge step and the wait for a decision, and both are
the same one person as everything else here.

Four things make it so, and all four are mechanisms rather than moods. The queue comes first,
because the backlog is what gets the project to a release. Every round above is a person deciding
to sit down and run the cycle. Merging is a person too, even after the review has said yes. And a
pull request labelled `review:needs-maintainer` waits for that same person, however long that
takes: of the four mechanisms this is the one you can set off yourself, by disagreeing twice or by
raising something the agents may not settle, and the one measured case of it cost eleven days. A
quiet fortnight is capacity, not disregard, and a polite comment on a quiet pull request is
welcome.

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
longer. `[maintainer: confirm or change]` No fork pull request has arrived yet, so this describes
an intention rather than a habit. The reasoning is on issue #29.

## Who decides

One person maintains Lawang: [@gablooge](https://github.com/gablooge). They decide what is built,
in what order, and what is merged. There is no committee, no vote and no second maintainer today,
and the project says so rather than implying otherwise.

What that means in practice:

- The order of work is [docs/backlog.md](docs/backlog.md). A change that jumps the queue waits for
  the queue, even when it is good. That single mechanism explains the waiting times in "How long
  it takes" above, and it is the thing to plan around: a change that lines up with the current
  backlog item is looked at quickly, and one that does not may sit.
- There is no guarantee on any answer. A quiet week, or a quiet fortnight, is capacity and not
  disregard.
- A "no" is a real possible answer, and you should get a reason with it.

## Licence

Lawang is [Apache 2.0](LICENSE). By opening a pull request you agree that your contribution is
licensed under it. There is no contributor licence agreement to sign.

## Code of conduct

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Conduct reports go to
conduct@samsulhadi.com. If the report is about the maintainer, the Code of Conduct has a
subsection that says what to do, because there is no independent person inside this project to
send it to.
