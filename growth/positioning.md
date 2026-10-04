# Positioning

Working note of the `bizdev` agent (Mode 1), for the maintainer. First written 2026-09-19,
**rewritten 2026-10-04** against today's `main`. The evidence, with a link and a read date on
every claim about another project, is in [landscape.md](landscape.md).

**This page is a proposal, and it is public.** The repository has been public since 2026-09-20,
so anyone can read this file, including every project and person it names. What it is not is
published in the sense `growth/README.md` rule 1 means: nothing here is published by being
committed, nothing here is announced, and nothing here speaks in the maintainer's voice. The
maintainer says in public what the maintainer decides to say.

**What runs today** is not restated on this page, on purpose. It is in the
[README status paragraph](../README.md), which was read on **2026-10-04** for this rewrite, and
pointing at it is the same discipline rule 2 imposes on claims about other projects: one place
to maintain, and a date next to it. The three claims below carry their own markers, re-derived
from [docs/backlog.md](../docs/backlog.md) on 2026-10-04.

**v0.1.0 ships two providers, ClickUp and Slack.** Microsoft Teams, Outlook and HubSpot were cut
from the release on 2026-10-03 to hold the date, not for any reason of design, and they are the
first entry under [After v0.1](../docs/roadmap.md#after-v01). This page says "two" wherever it
used to say "five", and that change is load-bearing: see "What only this project does" below.

## Who it is for

A small backend or platform team (about 2 to 10 engineers) that is building an internal
assistant, an enterprise search, or an agent memory over their company's own Slack and ClickUp
(and, after v0.1, Microsoft Teams, Outlook and HubSpot). They already own the retrieval side: an
index (pgvector, OpenSearch, Qdrant or similar) and their own query code. They self-host because
the content is sensitive. They do not want to adopt a complete product in order to get
connectors.

The cut to two providers narrows who can use v0.1.0 on day one. It does not change the pain. The
three moments below are the same with two sources as with five, because one leaked channel is
enough.

**The moment they feel the pain** is one of three:

1. The security review asks: "can the assistant show a private channel to someone who is not in
   it?", and nobody can prove the answer.
2. Someone leaves a channel or a list, and the assistant still answers from it the next day.
3. The webhook endpoint was down for an hour, and now there are missing messages, or a backfill
   has produced duplicates.

## Who it is not for, and what to use instead

| If you | Use instead |
|---|---|
| want a complete search and chat product today | Onyx, PipesHub, or Glean |
| need Teams, Outlook or HubSpot now | Onyx, PipesHub, Airweave or Glean. Those three providers are after v0.1 here, with no date |
| replicate data to a warehouse for analytics | Airbyte |
| need file and wiki permissions (SharePoint, Google Drive, Confluence) | Bedrock Managed Knowledge Base, the Azure AI Search SharePoint indexer, Onyx Enterprise Edition, Merge, Paragon |
| sell a SaaS product and need your customers to connect hundreds of apps | Nango, Paragon, Merge |
| ask your questions in Microsoft 365 Copilot | Copilot connectors |
| do not want to copy data at all | live, per-user retrieval: Slack's Real-Time Search API, federated Copilot connectors, MCP servers with user tokens |
| need something in production this quarter | any of the above. Lawang is pre-alpha and v0.1.0 is not released |
| are a vendor ingesting your customers' Slack workspaces | read Slack's 2025 API terms first (landscape section 3). This case may need a Slack Marketplace app |

## The problem, in their words

Each of these was re-opened at its source and confirmed word for word on 2026-10-04. Sources,
authors and dates are in landscape section 6.

- "The failure mode is simple: the model composes a fluent answer from a document the asking user
  was never allowed to see." (Kevin Riedl, Wavect)
- "The embedding model does not know about permissions." (Kirk Ryan)
- A revoked user "may still see the document's content in the search index or RAG pipeline until
  the next ingestion run." (Elena Vavilova, Microsoft ISE)
- "The subscription remains active and valid, but notifications stop being delivered for periods
  of approximately 1 to 1.5 hours" (a developer on Microsoft Q&A)
- "Duplicate chunks waste top-k slots, crowd out distinct evidence, inflate embedding and
  reranking costs" (Paragon)

The common search words are "permission-aware", "ACL-aware", "document-level access control",
"permission sync" and "stale permissions". Lawang's own words ("permission-stamped", "scope")
should come second.

## What only this project does, or plans to do

"Only" means: as far as the review of 2026-10-04 found, against the projects in landscape
section 5. Re-check before saying it in public, and say the narrow version.

**Read landscape section 1 first.** The cut to two providers took the breadth out of the
"nobody does this" argument, and what is left is narrower than the first pass claimed: no
self-hosted, ingestion-only component was found that delivers permission-carrying records to a
sink the adopter owns, and for ClickUp no project at all was found that carries the source's own
visibility. For Slack, Onyx (Enterprise Edition) and PipesHub (Apache 2.0) both carry
permissions, inside their own index. The difference there is shape, not permissions.

1. **Permissions as part of the record, in an ingestion-only component, fully under Apache 2.0.**
   Every record carries one scope, and membership changes are synced to your sink as their own
   flow, so enforcement in your retrieval layer is one rule and needs no call back to Lawang.
   Everything else found that carries permission data is either a complete product with its own
   index (Onyx, PipesHub, Airweave, Glean) or a hosted service (Paragon, Merge, Bedrock).
   Status, as of 2026-10-04: the record format, including `visibility.scope`, is **settled and
   frozen and runs today** ([ADR 4](../docs/adr/0004-record-format-v1.md), the schema embedded in
   `internal/record` and enforced in CI, B05 merged). Access sync, which is the membership half,
   is **planned for v0.1** (M4, B23 and B24, neither started).
2. **Exactly once from webhook to sink, with reconciliation through the same path.** One
   deterministic id per change, so a provider re-send, a worker crash and a backfill overlap are
   all no-ops. Loaders and ELT tools poll; complete products upsert into their own index.
   Status, as of 2026-10-04: the id recipes, the outbox with its accept dedupe and FIFO-per-entity
   claim, the ingress edge and hub, the pipeline with the ledger and the forward-only supersede
   chain, the three sinks, and the worker drain with its two-transaction commit all **run today**
   with tests (B02, B04, B06 to B10, merged). **Nothing runs end to end against a provider yet:**
   no provider is registered in the binary, so every `/ingress/{provider}` path is a 404, and
   `lawang worker` is not wired up. Reconciliation is **planned for v0.1** (M4, B21 and B22).
3. **Small to run, isolated by construction.** One binary and one Postgres, with row-level
   security forced on every table and a start-up check that refuses unsafe database roles.
   Comparable open source platforms need several data stores, because they also do retrieval
   (landscape section 2 lists what Airweave and PipesHub need).
   Status, as of 2026-10-04: the store, the three roles, row-level security, the preflight check,
   the tenancy binding and the outbox **run today** (B03 and B04, merged), and the drain works
   each row under its own tenant (B10, merged). The isolation test suite and the compose stack
   are **planned for v0.1** (M5, B26 and B27).

Things that would strengthen the claim but are **after v0.1**: deletions (`op: "delete"`),
untrusted-origin marking, a pgvector sink, and the Teams, Outlook and HubSpot providers.

## The strongest honest objection, and the answer

**"It is pre-alpha, with one maintainer, two providers in the release and no release yet. Onyx
and PipesHub exist, run today, cover more sources, and have teams behind them. And the platforms
are moving toward live retrieval rather than copying: Slack restricted bulk history access for
commercial apps in 2025."**

The answer, without decoration:

- All of that is true, and the provider count got worse on 2026-10-03, not better. Do not plan a
  production system on Lawang before v0.1.0, and judge it then by whether the quickstart works,
  not by this page.
- The design is not new. It is a rewrite of a Python predecessor, and the roadmap's
  [parity table](../docs/roadmap.md) says item by item what carries over, what changed and what
  was deliberately dropped. Architecture section 10 lists eleven principles, each taken from a
  real defect or near miss in that predecessor. Two of them match defects a mature project fixed
  in public this year: a Teams standard channel indexed as public, and a deduplication gate keyed
  on content that can skip a permission-only change (landscape section 2, Onyx). The problem is
  real and the design addresses it on purpose.
- It is small on purpose. A team can read the whole of it, which matters for a component that
  decides who may see what.
- It is Apache 2.0, and no part of it is held back for a paid edition. That is a fact about
  Lawang, and it is cheap to say when there is no product to maintain and nobody to support. Onyx
  funds a maintained product, documentation and support through its Enterprise Edition, and a
  team that needs those should weigh them against a license.
- On live retrieval: it is the right choice for some teams, and the table above says so.
  Ingestion is still needed for ranking across sources, for memory and offline processing, and
  for sources with no good search API. For Slack, a company ingesting its own workspace with its
  own app appears to keep normal limits, and that reading needs the maintainer's own check
  (landscape section 3).
- One maintainer is a real risk. A `GOVERNANCE` note that says so, with typical answer times, is
  more convincing than silence (Mode 3).

## Three suggestions for the README's first screen

Suggestions only. The README was not edited.

1. **Open with the problem in the reader's words, then the product.** Today the first sentence
   describes connectors. Put one or two sentences before it, for example: "An assistant that
   reads your company's Slack and task tracker must never answer from a channel the asking person
   cannot see. Lawang ingests those tools and delivers each change once, with the source's own
   visibility attached." Use "permission-aware" and "document-level access control" early, since
   those are the words people search for. Evidence: landscape section 6.
2. **Add a short "What Lawang guarantees, and what you must do" block.** Model it on the Bedrock
   documentation, which is the best example found of a vendor stating a limit plainly: a boxed
   "this is not authorization" line, a failure behaviour statement, and a list of the adopter's
   responsibilities. For Lawang: it stamps the scope and syncs membership; you authenticate users
   and filter at retrieval; between a revocation in the source and the next access sync there is
   a window, and here is how long. Stating the window is what separates this project from the
   "stale permissions" complaints collected in landscape section 6. Evidence: landscape
   sections 2 (Bedrock) and 6.
3. **Show the trust signals that comparable infrastructure shows first.** A CI badge and a
   license badge, supported versions (Postgres 16 or newer, the Go version), a link to a security
   policy with a private reporting address, and, in place of the quickstart that cannot exist
   before M5, a short "what runs today" list by milestone that is updated as milestones close.
   When the compose stack lands, replace that list with one command that ends in a visible
   record. Evidence: landscape section 8 (OpenFGA, River, Onyx, PipesHub), and the Show HN rule
   that the thing must be usable.

## What Mode 2 should look at first

Candidate gaps noticed during this run. Each needs the usefulness review before it becomes a
`growth` issue, and none of them is a decision.

1. **Slack under the 2025 terms, and it is more urgent than it was.** Slack is one of the two
   providers in v0.1.0, so this is a release question rather than a later one. The docs should
   say which deployments are supported (a company's own app for its own workspace) and what
   reconciliation does if the app is rate limited to 1 request per minute and 15 objects.
   Evidence: landscape section 3.
2. **"How do I enforce this in MY retrieval layer?"** Every vendor guide answers it (pre-filter,
   fetch more, post-filter, optional live check). Lawang has the format but no worked example for
   a real index (pgvector, Qdrant, OpenSearch) or for an authorization engine (OpenFGA, SpiceDB).
   The pgvector sink is after v0.1, so the example would have to use the `http` or `jsonl` sink.
   Evidence: landscape section 2 (Truto, Paragon, Bedrock) and "Related, not competing".
3. **Deletions and the revocation window.** Deleted content that still shows up, and stale
   permissions, are the two most repeated complaints in landscape section 6. Deletes are after
   v0.1, and the access sync interval is not yet stated anywhere an adopter would look.
   Evidence: landscape section 6.
4. **Identity by email.** The planned resolver joins on email (B23, M4, still in v0.1.0). Bedrock
   documents how this fails: "If emails differ across systems, ACL matching fails silently and
   the user receives no results from that data source", and a reassigned address is the
   customer's problem to detect. Onyx had to resolve Teams members by user id when no email was
   present, and to skip members from another tenant. Evidence: landscape section 2 (Bedrock,
   Onyx pull request #14840).

**A candidate that closed itself, recorded so that Mode 2 does not re-propose it.** The first
pass proposed "no reconciler is planned for Outlook or Teams in v0.1", on the ground that M4
named ClickUp, Slack and HubSpot. The 2026-10-03 cut closed it: M4 today reads "reconcilers for
ClickUp and Slack", which is exactly the two providers v0.1.0 ships, so no provider in the
release is without a reconciler. The Microsoft evidence behind it is still good, and it is still
in landscape section 6 under "Webhooks that go missing". It belongs to B16 and B17 under the
`Post v0.1.0` milestone now, not to v0.1, and it is worth re-reading when either is picked up:
Graph loses notifications during a pause, `missed` lifecycle notifications exist only for Outlook
resources, and a `lifecycleNotificationUrl` cannot be added to a subscription after it is
created, which is why [#37](https://github.com/gablooge/lawang/issues/37) has to be settled
before the first Graph subscription exists.
