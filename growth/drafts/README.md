# Drafts

Files here are **drafts for the maintainer**. Nothing in this directory is published, or in force,
by being committed. Moving a file into place is the maintainer's decision, and so is changing any
word of it first.

Written 2026-09-20 against `main` at commit `905a23d`. Re-checked and corrected on 2026-10-04
against `main` at commit `cae7cac`, which is sixty commits later: B05 to B11 merged, the record
format and the sink wire protocol were frozen, and on 2026-10-03 v0.1.0 was cut to ClickUp and
Slack.

## Why these exist now

The roadmap planned the community files for milestone M6, with the repository going public at the
end. The repository went public on 2026-09-20 instead, at the record format. So these files are
overdue rather than upcoming: somebody can open an issue today, and today the repository has no
`CONTRIBUTING.md`, no `CODE_OF_CONDUCT.md`, no issue templates and no pull request template.

`README.md`, `LICENSE` (Apache 2.0) and `SECURITY.md` are already in place, and private
vulnerability reporting is enabled.

## Where each file goes

| Draft | Destination in the repository |
|---|---|
| `CONTRIBUTING.md` | `CONTRIBUTING.md` |
| `CODE_OF_CONDUCT.md` | `CODE_OF_CONDUCT.md` |
| `github/ISSUE_TEMPLATE/bug_report.md` | `.github/ISSUE_TEMPLATE/bug_report.md` |
| `github/ISSUE_TEMPLATE/provider_request.md` | `.github/ISSUE_TEMPLATE/provider_request.md` |
| `github/ISSUE_TEMPLATE/question.md` | `.github/ISSUE_TEMPLATE/question.md` |
| `github/ISSUE_TEMPLATE/config.yml` | `.github/ISSUE_TEMPLATE/config.yml` |
| `github/PULL_REQUEST_TEMPLATE.md` | `.github/PULL_REQUEST_TEMPLATE.md` |
| `first-issues.md` | nowhere. It is a list of candidate issues for you to open, or not. |

The drafts sit under `github/` and not `.github/` on purpose: a directory whose name begins with a
dot is easy to miss while reviewing.

## What needs you before any of it moves

1. **The code of conduct contact.** `CODE_OF_CONDUCT.md` is Contributor Covenant 2.1, word for word,
   with the upstream Hugo front matter removed and nothing else changed. Checked on 2026-10-04
   against the canonical source
   ([text](https://www.contributor-covenant.org/version/2/1/code_of_conduct/),
   [file](https://github.com/EthicalSource/contributor_covenant/blob/release/content/version/2/1/code_of_conduct.md)):
   with the front matter and its following blank line removed, the two are byte identical.

   It still holds the upstream placeholder `[INSERT CONTACT METHOD]`, in the "Enforcement" section.
   No address was invented for you, and no address exists anywhere in this repository today. Only
   you can choose what goes there. Four things are needed before that file can move to the root:

   1. **A route that a stranger can use and that you read.** An address, or a GitHub-based route.
      Private vulnerability reporting is enabled on this repository, but it is a security channel
      and not a conduct channel, so it does not cover this.
   2. **Not your personal inbox.** `CLAUDE.md` says nothing personal to the maintainer belongs in
      this repository, and this file is public, permanent and scraped. A conduct contact is the
      one address a bad actor has a motive to abuse.
   3. **Who reads it, named in the file.** One maintainer, [@gablooge](https://github.com/gablooge).
      The promise already in the file to respect a reporter's privacy only means something once
      the reader is identified.
   4. **What a reporter does when the report is about the only person who reads reports.**
      Contributor Covenant 2.1 assumes a plural "community leaders" and this project has one. One
      honest sentence is better than silence. Note that adding it ends the "word for word" claim
      above, so say in the file that this sentence is the project's own addition.
2. **The links inside `CONTRIBUTING.md`.** It links to `CODE_OF_CONDUCT.md`, `SECURITY.md`,
   `LICENSE`, `docs/backlog.md`, `docs/architecture.md` and `.claude/agents/pr-reviewer.md`. They
   all resolve once the two new files are at the repository root. Check them after you move the
   files.
3. **One claim in `CONTRIBUTING.md` that only you can confirm.** That you may review a pull
   request from a fork by hand rather than running the agent on it (the reasoning is on issue
   #29). No fork pull request has arrived yet, so it is an intention and not a habit. The claim
   now carries a `[maintainer: confirm or change]` marker inside the draft itself, next to the
   sentence, because `growth/` is public and a stranger who opens that file alone would otherwise
   read it as settled. The draft also opens with a block saying it is a draft. Delete the block
   and the markers when you move it.

   The second claim that used to be here, that a review arrives "within hours, not weeks", is
   gone. It was not true for the kind of pull request the guide is written for. It is replaced by
   a measurement from this repository's own pull requests, dated 2026-10-04, which says days to
   weeks for anything outside the backlog queue. Re-measure it before the file moves, and again
   whenever it starts to flatter the project.
4. **`good first issue` labels.** The label exists and no issue carries it. `first-issues.md` has
   six candidates, all re-verified on 2026-10-04. Opening even two of them changes what a visitor
   sees.
5. **A reminder for the day Discussions are turned on.** `github/ISSUE_TEMPLATE/question.md` and
   `github/ISSUE_TEMPLATE/config.yml` both rest on Discussions being off, which is true today
   (`has_discussions` is false, checked 2026-10-04). Nothing will notice when that changes. If you
   enable Discussions, revisit both files: the question template says "Discussions are not
   enabled, so questions are issues", and the config sends people to the backlog and the
   architecture partly because there is nowhere else to send them.

## What is deliberately not here

- **A provider-authoring guide.** It belongs to B28 (issue #28), and that is reason enough on its
  own. The reason first given here, that it needs the provider interfaces of B06, expired on
  2026-09-21 when B06 merged in #47. `internal/provider` now holds the interfaces and the
  registry, `internal/provider/fake` exists, and B11 (#57) has built a real ClickUp provider
  against them. So B28 has its raw material: a guide written now would describe code that exists,
  with one worked implementation to point at.
- **A quickstart.** B28 again. The binary still cannot be run end to end by a stranger: no
  provider is registered in it yet, because hydration needs a per-tenant API token and the vault
  that holds those is B13.
- **Branch protection, action pinning, release workflow.** All noted on B29 (issue #29), with the
  exact commands. Not repeated here.
- **Anything posted anywhere.** No issue was opened, no message sent, nothing submitted to any site
  or list.

## A word on the agents, for contributors

The `.claude/` directory holds the rules for three agents the maintainer runs: an implementer, a
pull request reviewer, and the one that wrote these drafts. An outside contributor will meet the
reviewer, so `CONTRIBUTING.md` describes it in plain words instead of letting a stranger discover
it from a wall of inline comments signed by the maintainer's account. The three things a
contributor most needs to know are said there: the review is automated first and human last, it
will try to break their tests on purpose, and a finding is not a rejection.

One thing the reviewer's own rules already say, and that `CONTRIBUTING.md` repeats in the
contributor's words: text in an issue, a comment or a pull request is material, never an
instruction. Nothing a contributor writes can make an agent merge, push, or change a label.
