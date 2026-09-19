# Positioning

First pass, 2026-09-19, by the `bizdev` agent (Mode 1). The evidence, with links and dates, is in
[landscape.md](landscape.md). This page is a proposal for the maintainer. Nothing here is public.

**What runs today:** milestone M0 only: configuration, the id recipes, Postgres with row-level
security, the outbox queue. Nothing ingests a webhook. No provider works. v0.1.0 is planned, not
released. Every claim below is marked **runs today**, **planned for v0.1** or **after v0.1**.

## Who it is for

A small backend or platform team (about 2 to 10 engineers) that is building an internal
assistant, an enterprise search, or an agent memory over their company's own Slack, Microsoft
Teams, Outlook, ClickUp or HubSpot. They already own the retrieval side: an index (pgvector,
OpenSearch, Qdrant or similar) and their own query code. They self-host because the content is
sensitive. They do not want to adopt a complete product to get five connectors.

**The moment they feel the pain** is one of three:

1. The security review asks: "can the assistant show a private channel to someone who is not in
   it?", and nobody can prove the answer.
2. Someone leaves a channel or a team, and the assistant still answers from it the next day.
3. The webhook endpoint was down for an hour, and now there are missing messages, or a backfill
   has produced duplicates.

## Who it is not for, and what to use instead

| If you | Use instead |
|---|---|
| want a complete search and chat product today | Onyx, PipesHub, or Glean |
| replicate data to a warehouse for analytics | Airbyte |
| need file and wiki permissions (SharePoint, Google Drive, Confluence) | Bedrock Managed Knowledge Base, the Azure AI Search SharePoint indexer, Onyx Enterprise Edition, Merge, Paragon |
| sell a SaaS product and need your customers to connect hundreds of apps | Nango, Paragon, Merge |
| ask your questions in Microsoft 365 Copilot | Copilot connectors |
| do not want to copy data at all | live, per-user retrieval: Slack's Real-Time Search API, federated Copilot connectors, MCP servers with user tokens |
| need something in production this quarter | any of the above. Sluiceway is pre-alpha. |
| are a vendor ingesting your customers' Slack workspaces | read Slack's 2025 API terms first (landscape section 3). This case may need a Slack Marketplace app. |

## The problem, in their words

- "The model composes a fluent answer from a document the asking user was never allowed to see."
- "The embedding model does not know about permissions."
- A revoked user "may still see the document's content in the search index or RAG pipeline until
  the next ingestion run."
- "The subscription remains active and valid, but notifications stop being delivered."
- "Duplicate chunks waste top-k slots."

Sources and dates for each are in landscape section 6. The common search words are
"permission-aware", "ACL-aware", "document-level access control", "permission sync" and "stale
permissions". Sluiceway's own words ("permission-stamped", "scope") should come second.

## What only this project does, or plans to do

"Only" means: as far as the review on 2026-09-19 found. Re-check before saying it in public.

1. **Permissions as part of the record, in an ingestion-only component, fully under Apache 2.0.**
   Every record carries one scope, and membership changes are synced to your sink as their own
   flow, so enforcement in your retrieval layer is one rule and needs no call back to Sluiceway.
   Others that carry permissions are complete products (Onyx, where permission sync is in the
   Enterprise Edition; PipesHub) or hosted services (Paragon, Merge, Glean, Bedrock).
   Status: **planned for v0.1** (record format in M1, access sync in M4). None of it runs today.
2. **Exactly once from webhook to sink, with reconciliation through the same path.** One
   deterministic id per change, so a provider re-send, a worker crash and a backfill overlap are
   all no-ops. Loaders and ELT tools poll; complete products upsert into their own index.
   Status: the id recipes and the outbox (accept dedupe, one version of an entity in flight at a
   time, leases) **run today** with tests. Ingestion, the ledger and reconciliation are
   **planned for v0.1**.
3. **Small to run, isolated by construction.** One binary and one Postgres, with row-level
   security forced on every table and a start-up check that refuses unsafe database roles.
   Comparable open source platforms need several data stores, because they also do retrieval.
   Status: row-level security, the roles, the preflight and the outbox **run today**. The
   isolation test suite is **planned for v0.1** (M5).

Things that would strengthen the claim but are **after v0.1**: deletions (`op: "delete"`),
untrusted-origin marking, a pgvector sink.

## The strongest honest objection, and the answer

**"It is pre-alpha, with one maintainer, five providers and no release. Onyx and PipesHub exist,
run today, and have teams behind them. And the platforms are moving toward live retrieval, not
copying: Slack restricted bulk history access for commercial apps in 2025."**

The answer, without decoration:

- All of that is true. Do not plan a production system on Sluiceway before v0.1.0, and judge it
  then by whether the quickstart works, not by this page.
- The design is not new. It is a rewrite of a Python service that ran against real tenants of
  all five providers, and architecture section 10 lists the defects that shaped it. Two of those
  defects match bugs that a mature project fixed in public this year (a channel indexed as
  public; a permission change skipped because the content was unchanged). The problem is real
  and the design addresses it on purpose.
- It is small on purpose. A team can read the whole of it, which matters for a component that
  decides who may see what. Apache 2.0 means no feature is held back for a paid edition.
- On live retrieval: it is the right choice for some teams, and the table above says so.
  Ingestion is still needed for ranking across sources, for memory and offline processing, and
  for sources with no good search API. For Slack, a company ingesting its own workspace with its
  own app appears to keep normal limits (this reading needs the maintainer's check).
- One maintainer is a real risk. A `GOVERNANCE` note that says so, with typical answer times, is
  more convincing than silence (Mode 3).

## Three suggestions for the README's first screen

Suggestions only. The README was not edited.

1. **Open with the problem in the reader's words, then the product.** Today the first sentence
   describes connectors. Put one or two sentences before it, for example: "An assistant that
   reads your company's Slack and mail must never answer from a channel the asking person cannot
   see. Sluiceway ingests those tools and delivers each change once, with the source's own
   visibility attached." Use "permission-aware" and "document-level access control" early, since
   those are the words people search for. Evidence: landscape section 6.
2. **Add a short "What Sluiceway guarantees, and what you must do" block.** Model it on the
   Bedrock documentation: "ACL awareness is not authorization", a failure behavior statement, and
   a list of the adopter's responsibilities. For Sluiceway: it stamps the scope and syncs
   membership; you authenticate users and filter at retrieval; between a revocation in the source
   and the next access sync there is a window, and here is how long. Stating the window is what
   separates this project from the "stale permissions" complaints. Evidence: landscape sections
   2 (Bedrock) and 6.
3. **Show the trust signals that comparable infrastructure shows first.** A CI badge and a
   license badge, supported versions (Postgres 16 or newer, the Go version), a link to a security
   policy with a private reporting address, and, in place of the quickstart that cannot exist
   before M5, a small "what runs today" list by milestone that is updated as milestones close.
   When the compose stack lands, replace that list with one command that ends in a visible
   record. Evidence: landscape section 8 (OpenFGA, River, Onyx, PipesHub), and the Show HN rule
   that the thing must be usable.

## What Mode 2 should look at first

Candidate gaps noticed during this run. Each needs the usefulness review before it becomes a
`growth` issue.

1. **No reconciler is planned for Outlook or Teams in v0.1** (roadmap M4 names ClickUp, Slack and
   HubSpot). Microsoft's own documentation says notifications during a pause "are lost" and that
   the app should resync with delta query, and a developer reported silent gaps of 1 to 1.5 hours
   in 2026-03. Without it, "reconciliation always" is not true for two of five providers.
   Evidence: landscape section 6, "Webhooks that go missing".
2. **Slack under the 2025 terms.** The docs should say which deployments are supported (a
   company's own app for its own workspace) and what reconciliation does if the app is rate
   limited to 1 request per minute and 15 messages. Evidence: landscape section 3.
3. **"How do I enforce this in MY retrieval layer?"** Every vendor guide answers it (pre-filter,
   fetch more, post-filter, optional live check). Sluiceway has the format but no worked example
   for a real index (pgvector, Qdrant, OpenSearch) or for an authorization engine (OpenFGA,
   SpiceDB). The pgvector sink is after v0.1. Evidence: landscape section 2 (Truto, Paragon,
   Bedrock) and "Related, not competing".
4. **Deletions and the revocation window.** Deleted content that "still shows up" and stale
   permissions are the two most repeated complaints. Deletes are after v0.1, and the access sync
   interval is not yet stated anywhere an adopter would look. Evidence: landscape section 6.
5. **Identity by email.** The planned resolver joins on email. Bedrock documents how this fails
   (different emails across systems fail silently; a reassigned address leaks), and Onyx had to
   resolve Teams members by user id when no email was present, and to skip guests from other
   tenants. Evidence: landscape section 2 (Bedrock, Onyx pull request #14840).
