# Candidate `good first issue` items

Read on 2026-09-20, against `main` at commit `905a23d` (the merge of the rename), in a clean
worktree.

These are drafts. **Nothing here has been opened as an issue.** The maintainer decides which ones
become issues, in their own words.

A first issue here has to be: small enough for one sitting, self-contained, verifiable by anyone,
and free of any need for a provider account, a credential or the maintainer's machine. Each item
below was found by reading the code, not by guessing what a project like this usually needs.

A note before the list. There are few of these, and that is the finding underneath the list: the
code that exists is already covered thoroughly, so the easy surface a newcomer normally starts on
(a missing test, an unhandled error, a stale comment) has mostly been used up. See "What this
means" at the end.

---

## 1. The README's link to the extension points lands in the wrong place

**Files:** `README.md` (line 73), plus a new test.

`README.md` links to `docs/architecture.md#extension-points`. The heading in that file is
`## 7. Extension points`, whose anchor is `#7-extension-points`. There is no HTML anchor named
`extension-points`. A reader who clicks the link, which is the one link in the README that tells a
person how to add a provider, lands at the top of a long document instead.

Fixing the link is one character. The useful part is stopping the next one: a check that every
relative link in every Markdown file points at a file that exists, and that every `#anchor` matches
a heading in the target file. This is the check that would have caught the rename from Sluiceway to
Lawang if it had moved a file.

**Smallest version that helps:** a test-only Go package (for example `internal/doclinks`, holding
only `links_test.go`) that walks the Markdown files of the repository, and for each relative link
checks the file exists and, for a `.md` target, that the anchor matches a heading. Anchors follow
GitHub's rule: lower case, punctuation other than hyphens removed, spaces turned into hyphens.
Skip `http`, `https` and `mailto` links: this check must not reach the network, because CI has no
network guarantee and a link checker that calls the internet is flaky.

**Done when:** the test fails on the README link as it is today, passes after the link is corrected
to `#7-extension-points`, runs inside `make check` with no new dependency, and reports the file, the
link and the reason when it fails.

**Size:** S.

---

## 2. Nothing holds the usage text to the commands the binary accepts

**Files:** `cmd/lawang/main_test.go`.

`lawang help` prints a list of commands. `run` in `cmd/lawang/main.go` dispatches on a `switch`.
Nothing ties the two together. `TestUsageDocumentsTheConfigurationAndTheExitCodes` checks the
variables, the exit codes and the word `help`, but not the command list. A command added to the
`switch` and forgotten in the usage text would ship undocumented, and a command removed from the
`switch` would stay in the usage text and mislead.

**Smallest version that helps:** a test that reads the `Commands:` block of the usage text, takes
the first word of each line, and for each one calls `run` with that word and an empty environment.
The exit code must not be 2 and the output must not contain `unknown command`. Two of the commands
(`serve`, `worker`, `migrate`) refuse to start without configuration, which is exit 1: that is the
expected result, and it is different from "I do not know this word".

**Done when:** the test passes today; it fails when a line is removed from the usage text; it fails
when a case is removed from the `switch`. Say in the pull request which of those two you tried.

**Size:** S.

---

## 3. `go test -short` should skip the tests that need Docker

**Files:** `internal/testdb/testdb.go`, `Makefile`.

The integration tests start a real Postgres with testcontainers. They already skip, with a clear
message, when Docker is not running. They do not skip when Docker is running but the person just
wants a fast loop, and a container start costs seconds on every run.

**Smallest version that helps:** in `internal/testdb`, `NewRaw` and `NewRawCluster` skip the test
when `testing.Short()` is true, with a message saying why. Add a `Makefile` target
(`make test-short`, running `go test -short ./...`) and one line in `CONTRIBUTING.md`.

**Watch out for:** `make check` and CI must keep running the full suite. `-short` is for a
developer's own loop, never for the check that says a change is ready.

**Done when:** `go test -short ./...` starts no container and every skipped test says why;
`make test` and CI are unchanged; the skip is in one place, so a new integration test gets the
behaviour for free.

**Size:** S.

---

## 4. The retry ladder's comment says "about a day", and it is about nineteen hours

**Files:** `internal/outbox/ladder.go`, `internal/outbox/ladder_test.go`.

`DefaultLadder` is 5 seconds, 30 seconds, 2 minutes, 10 minutes, 1 hour, 6 hours, 12 hours. The
comment says it "gives a failing sink about a day to come back before a row is parked". The waits
add up to 19 hours, 12 minutes and 35 seconds, so the last attempt happens about 19 hours after the
first, not about 24. An operator who reads the comment and builds an alert around a day is working
from the wrong number.

`TestDefaultLadderOnlyClimbs` proves the steps increase. Nothing proves the total.

**Smallest version that helps:** correct the comment to the real figure, and add a test that sums
`DefaultLadder` and fails if the total leaves the stated window. The test's job is to make the
comment and the code fall out of step loudly, so the assertion should name the documented figure.

**Done when:** the comment states a number a reader can check; a test fails if a step is added,
removed or changed without the comment being updated.

**Size:** S.

---

## 5. The "no em dash" rule is enforced only on the maintainer's machine

**Files:** `Makefile`, `.github/workflows/ci.yml`.

This repository has a standing rule: never an em dash (the character U+2014), anywhere, including
code, comments, SQL and documents. Today the rule is enforced by the review agent, which runs on
the maintainer's machine. An outside contributor has no way to check their change before they
submit it, and will find out only from a review finding. There are zero em dashes in the
repository right now (checked 2026-09-20), so the check would be green from the first day.

**Smallest version that helps:** a `Makefile` target that lists the repository's tracked text files
and fails if any contains U+2014, printing the file and line. Wire it into `make check`, so a
contributor sees it locally, and CI gets it for free.

**Watch out for:** binary files (the check should skip anything that is not text), and the message,
which should say what to use instead (a comma, parentheses, a colon, or two sentences) so the fix
is obvious.

**Done when:** `make check` fails on a branch where an em dash was added to any tracked text file,
names the file and the line, and is green on `main`.

**Size:** S.

---

## 6. There is no template for a decision record

**Files:** `docs/adr/0000-template.md` (new), `docs/adr/README.md`.

`docs/adr/README.md` says "one short file per settled decision" and that a record "states the
decision, why, and what it costs". Three records exist (0001, 0002, 0010) and they share a shape,
but the shape is written down nowhere. Someone who wants to contribute a decision has to reverse
engineer it from the three files.

**Smallest version that helps:** read the three records, take the sections they have in common, and
write `0000-template.md` with those headings and one sentence under each saying what belongs there.
Add a row to the table in `docs/adr/README.md` marking it as a template, not a decision, so nobody
mistakes 0000 for a real record.

**Done when:** a person who has read only the template can write a record that looks like the three
that exist; the index says plainly that 0000 is a template.

**Size:** S.

---

## What this means

Six candidates is not many for a repository of this size, and padding the list would waste a
newcomer's time worse than a short list does. The reason is worth knowing: every backlog item so
far went through an adversarial review that asks for error paths, boundary cases, dead code and
stale comments before it merges. That is good for the code and it removes exactly the small,
unglamorous work a newcomer usually starts on.

Two consequences for the maintainer to weigh:

1. **The supply of first issues will come from new surface, not from old.** Each provider package
   (ClickUp, Slack, Teams, Outlook, HubSpot) brings normalizer edge cases, golden files and setup
   documentation, and those are naturally small and self-contained. The provider-authoring guide of
   B28 is what turns them into work a stranger can pick up. Until then, expect few.
2. **Some of what a newcomer could do needs a decision first, not a pull request.** The notes on
   issues #28 and #29 hold several small, well-described pieces of work (pin the GitHub Actions by
   commit SHA, add a build smoke step, add `govulncheck` to `make check`, pin the golangci-lint
   version, refuse a non-UTF8 database in the preflight). They are deliberately not repeated here,
   because they belong to those items and to the maintainer's sequencing. Any of them would make a
   good first issue the moment the maintainer decides it is wanted now.
