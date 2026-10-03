# Drafts

Files here are **drafts for the maintainer**. Nothing in this directory is published, or in force,
by being committed. Moving a file into place is the maintainer's decision, and so is changing any
word of it first.

Written 2026-09-20, against `main` at commit `905a23d`.

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
   with the project's Hugo front matter removed and nothing else changed. It still holds the
   upstream placeholder `[INSERT CONTACT METHOD]`, in the "Enforcement" section. No address was
   invented for you, and no address exists anywhere in this repository today. Only you can choose
   what goes there. Whatever you pick becomes public and permanent, so a dedicated address or a
   GitHub-based route is usually kinder to your personal inbox than your main one.
2. **The links inside `CONTRIBUTING.md`.** It links to `CODE_OF_CONDUCT.md`, `SECURITY.md`,
   `LICENSE`, `docs/backlog.md`, `docs/architecture.md` and `.claude/agents/pr-reviewer.md`. They
   all resolve once the two new files are at the repository root. Check them after you move the
   files.
3. **Two claims in `CONTRIBUTING.md` that only you can confirm.** That a review usually arrives
   "within hours, not weeks", and that you may review a pull request from a fork by hand rather
   than running the agent on it (the reasoning is on issue #29). Both are written as habits, not
   promises. Change them if they are not true.
4. **`good first issue` labels.** The label exists and no issue carries it. `first-issues.md` has
   six candidates. Opening even two of them changes what a visitor sees.

## What is deliberately not here

- **A provider-authoring guide.** It belongs to B28 (issue #28), it needs the provider interfaces
  that land with B06, and writing it now would describe code that does not exist.
- **A quickstart.** There is nothing to start yet. B28 again.
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
