# Landscape: who else works on this problem

Working notes of the `bizdev` agent (Mode 1), for the maintainer. First written 2026-09-19 and
**rewritten on 2026-10-04**, because three things changed underneath the first pass: the project
was renamed to Lawang on 2026-09-20, the repository became public on the same day, and v0.1.0 was
cut from five providers to two on 2026-10-03. The first pass was never reviewed before those
changes landed, so this is a rewrite on top of `main` rather than a patch of the old text.

Nothing in this file is published by being committed. It is a working note in a public
repository: anyone can read it today, and the projects and people named in it can read it. It is
written on that assumption.

## How to read this file

Every claim about another project or community carries a link and the date the page was read.
Four markers separate what was confirmed from what was not:

- **FOUND**: a page says it, and the link is next to the claim. Not next to the paragraph, next
  to the claim.
- **INFER**: a reading of those sources by this review. It can be wrong.
- **SEARCH ONLY**: a search result summary reported it and no page was opened to confirm it.
  Treat it as a lead, not as a fact. This marker exists because the first pass used FOUND for
  such claims, which made the key untrustworthy.
- **NOT VERIFIED**: it was looked for and could not be confirmed from a primary source.

Dates are written as **read 2026-10-04, published 2026-06-11**, so that a read date is never
mistaken for a publication date. Where only one date is given, it is the read date.

**Quotations.** Every passage in quotation marks below was re-opened at its link on 2026-10-04 and
confirmed character for character against the page as it stood that day. The first pass carried
about twenty-five quotations whose exact wording it admitted it had not checked, four of them
under the names of real people. Each was either confirmed, turned into a paraphrase attributed to
the source rather than to a person's mouth, or removed. Section 9 lists what was removed and why.
A quotation that cannot be re-checked is not evidence, and a quotation under a named person's
name in a public file is their words or it is nothing.

## What Lawang can claim today

Not restated here, on purpose, because a restatement rots on its own. The
[README status paragraph](../README.md) is the single place that says what runs, and it was read
on **2026-10-04** for this rewrite. In short, from that paragraph: the foundations, the record
format, the accept path, the pipeline, the three sinks, the worker drain and the ClickUp provider
package exist; no provider is registered in the binary yet, so no `/ingress/{provider}` route
answers; and v0.1.0 is not released. Every comparison below is between what others ship and what
Lawang **plans**, except where a line says otherwise.

v0.1.0 ships **ClickUp and Slack**. Microsoft Teams, Outlook and HubSpot were cut from the
release on 2026-10-03 and are after v0.1 ([docs/backlog.md](../docs/backlog.md),
[docs/roadmap.md](../docs/roadmap.md)). Where a line below talks about "the five", it means the
five providers the architecture designs for, two of which are in the release.

---

## 1. The short answer

**Does a project already do what Lawang plans?** Re-asked on 2026-10-04, against a two-provider
v0.1.0: still no project was found that does it. The reason is narrower than the first pass said,
and one part of the old argument has to be given up.

What v0.1.0 plans is a combination: (a) a small self-hosted component that only ingests, (b)
webhooks first with reconciliation, (c) every record stamped with the source's own visibility,
with membership changes synced separately, (d) exactly-once delivery to a sink the adopter owns,
(e) for chat and tasks (Slack and ClickUp), (f) under Apache 2.0, with nothing held back for a
paid edition.

**Part (e) used to carry a lot of the argument, and it no longer can.** Until 2026-10-03 it read
"chat, mail, tasks and CRM" across five providers, and that breadth was what separated the
combination from Paragon (file storage, documents and CRM) and from the ACL-capable connectors
elsewhere, which cluster on files and wikis. Mail and CRM are now after v0.1. On provider count
Lawang is last in section 5's table: Onyx, Airweave and Glean each list connectors for all five,
PipesHub lists three, Lawang ships two. That is the accurate reading of the table, and it is
worth facing rather than hiding, so section 5 states it in the table itself.

What survives, re-checked provider by provider on 2026-10-04:

- **Slack.** Onyx syncs Slack permissions, in the Enterprise Edition
  (<https://docs.onyx.app/admins/connectors/overview>). PipesHub advertises permission-aware
  search across its connectors, under Apache 2.0 (<https://github.com/pipeshub-ai/pipeshub-ai>).
  So for Slack the open question is not whether anyone carries permissions. It is whether anyone
  carries them **out**. Both of those are complete products that enforce inside an index they
  own; neither hands an adopter a record with the source's visibility attached, to index wherever
  the adopter likes. For Slack, Lawang's claim is about shape, not about permissions, and it is
  weaker for that.
- **ClickUp.** Onyx, Glean and Airweave all list a ClickUp connector
  (<https://docs.onyx.app/admins/connectors/overview>, <https://docs.glean.com/connectors/>,
  <https://docs.airweave.ai/llms.txt>). None of them documents permission syncing for it: Onyx's
  permission-syncing list has twelve connectors and ClickUp is not one, Airweave's documentation
  index has no access control page at all, and Glean does not publish per-connector permission
  behaviour on the pages read. PipesHub has no ClickUp connector
  (<https://docs.pipeshub.com/connectors/overview>). No project was found that carries ClickUp's
  own visibility into a record it hands on.
- **(a), (c), (d) and (f) together.** Still no match. Everything found that carries permission
  data is either a complete product with its own index (Onyx, PipesHub, Airweave, Glean) or a
  hosted service (Paragon, Merge, Bedrock Managed Knowledge Base). The empty place is the
  component: something small enough for one team to read, that they run, that delivers into an
  index they already own.

**So the honest sentence is narrower than the first pass wrote.** Not "nobody does this", but:
no self-hosted, ingestion-only component was found that delivers permission-carrying records to a
sink the adopter owns, and for ClickUp specifically, no project at all was found that carries the
source's visibility. The niche is real and it is narrow, and on the strongest version of the
claim it is one provider wide. Anyone repeating it in public should say the narrow version.

The closest things found, and how they differ:

| Closest in | Project | How it differs from the plan |
|---|---|---|
| Function | Paragon Managed Sync (commercial) | Hosted product, not open source. Syncs into your RAG pipeline and keeps a permissions graph you query through an API. Its Managed Sync overview names File Storage, Documents and CRM, not chat or tasks. |
| Open source, permissions, same license | PipesHub | Apache 2.0, and advertises permission-aware search. It is a complete workplace AI platform with its own index and several data stores, not an ingestion component, and it has no ClickUp connector. Its connector documentation says syncing is periodic. |
| Open source, permissions, breadth | Onyx | A complete search and chat product, not an ingestion component. Permission syncing is an Enterprise Edition feature, and of the five providers it is documented for Slack, Teams and Outlook. |
| Open source, provider coverage | Airweave | MIT, connectors for all five. A retrieval layer with its own index. Its documentation index has no page on access control, and its Slack connector searches Slack at query time rather than ingesting. |

---

## 2. Projects and products, one by one

All pages in this section were read on **2026-10-04** unless a line says otherwise.

### Airbyte (data movement, ELT)

- **What it is.** FOUND: an ELT platform with a large connector catalog, publishing guides on RAG
  pipelines. <https://airbyte.com/agentic-data/rag-data-pipeline> (read 2026-09-19)
- **License and self-hosting.** FOUND: the connectors are "open sourced and available under the
  Elastic License 2.0 (ELv2) License" and the Airbyte Protocol is "open sourced and available
  under the MIT License". Airbyte Cloud, Enterprise and Agents "require a commercial license from
  Airbyte". The page's own summary of the ELv2 restriction: "Unless you want to host Airbyte
  yourself and sell it as an ELT/ETL tool, or sell a product that directly exposes Airbyte's UI
  or API, you should be good to go." <https://docs.airbyte.com/community/licenses>
  This settles a question the first pass left open.
- **Ingest.** FOUND: the Slack source supports Full Refresh Sync and Incremental Sync. The page
  does not say whether it polls or uses webhooks, and mentions no webhooks.
  <https://docs.airbyte.com/integrations/sources/slack>
- **Permissions.** FOUND: an Airbyte documentation issue opened 2025-08-18, still open on
  2026-10-04, lists the connectors that sync ACLs: SharePoint Enterprise (an enterprise
  connector, with `file_permissions` and `identities` streams), Google Drive (open source, the
  same two streams), and Salesforce (security objects synced as ordinary objects).
  <https://github.com/airbytehq/airbyte/issues/65073> The Slack source has a Channel Members
  stream, which is membership data, but nothing on that page ties it to messages as an access
  rule. FOUND, on the Slack source page: "Airbyte can only replicate messages from channels that
  the app has been added to."
- **Duplicates and edits.** FOUND: sync modes and cursors, resolved at the destination.
  Deduplication is a destination feature, not a per-event guarantee.
  <https://docs.airbyte.com/integrations/sources/slack>
- **Coverage of the five.** FOUND: Slack, Microsoft Teams, Outlook and HubSpot sources exist.
  <https://docs.airbyte.com/integrations/sources/microsoft-teams>
  <https://docs.airbyte.com/integrations/sources/outlook>
  <https://docs.airbyte.com/integrations/sources/hubspot> (read 2026-09-19)
  NOT VERIFIED: whether a ClickUp source exists.
- **Choose it instead when** you replicate many sources into a warehouse or lake on a schedule,
  minutes or hours of delay are fine, and permissions matter mainly for files.

### Nango (OAuth and integration infrastructure)

- **What it is.** FOUND: a code-first integration platform. Its repository describes it as
  "Connect your agents & product to 1,000 APIs." <https://github.com/NangoHQ/nango>
- **License and self-hosting.** FOUND: the free self-hosted edition includes "API Auth",
  "Proxy" and observability for those two. It does not include "Syncs", "Webhooks", "Triggers",
  "Tool calls", "MCP server", "RBAC" or "MFA".
  <https://nango.dev/docs/guides/platform/self-hosting> The repository's license is not one
  GitHub resolves to a standard identifier, and the first pass recorded it as the Elastic
  License. NOT VERIFIED today, because the `LICENSE` file itself was not opened.
- **Permissions.** FOUND: Nango's article sets out four designs for preserving user permissions
  (per-user authentication; organization-wide authentication with permission syncing;
  organization-wide authentication with your own internal permission model; delegated API
  access), and it states the risk plainly: "Failing to properly handle user permissions with your
  AI-enabled features/agents can create silent data leaks, introduce compliance risks, and allow
  the AI to bypass existing access controls."
  <https://nango.dev/blog/preserve-user-permissions-roles-api-integrations-ai-agents-rag/>
  (read 2026-10-04, published 2026-04-01) The first pass attributed a phrase to this page calling
  Nango "not a built-in permissions engine". That phrase is not on the page and has been removed.
  The article describes the designs and leaves the permission model to the integrating
  application; it makes no claim either way about Nango being a permissions engine.
- **Choose it instead when** you need OAuth and token handling for hundreds of APIs inside your
  own product. Lawang's README already says it can use Nango for this. INFER: the part Lawang
  needs (auth and proxy) is the part that is free to self-host, which makes the pairing
  practical.

### Unstructured (document parsing and ingest connectors)

- **What it is.** FOUND: a document parsing platform with source and destination connectors. The
  open source ingest library is Apache 2.0.
  <https://github.com/Unstructured-IO/unstructured-ingest> (read 2026-09-19)
- **Ingest.** FOUND (older documentation, read 2026-09-19): batch source connectors, including
  Slack, Outlook and SharePoint.
  <https://unstructured.readthedocs.io/en/latest/ingest/source_connectors.html>
- **Permissions.** FOUND: Unstructured's blog argues that connectors must carry metadata and that
  production RAG depends on it. (read 2026-09-19)
  <https://unstructured.io/blog/enterprise-rag-why-connectors-matter-in-production-systems>
  NOT VERIFIED: which open source connectors emit permission data today, and for which sources.
- **Choose it instead when** your hard problem is turning files (PDF, Office, HTML) into clean
  chunks. Lawang does not parse documents.

### LlamaIndex readers (LlamaHub) and LangChain document loaders

- **What they are.** FOUND: libraries of loaders that pull data into a framework's document type.
  The LlamaIndex Slack reader takes a token, channel ids and a date range, and returns documents.
  Its documentation mentions no membership, no permissions, no incremental updates and no
  deduplication. (read 2026-09-19)
  <https://github.com/run-llama/llama_index/tree/main/llama-index-integrations/readers/llama-index-readers-slack>
- **What users report.** FOUND: an issue from 2023-12-11 asks how to load large Slack channels
  after hitting a rate limit, and was closed as not planned. (read 2026-09-19)
  <https://github.com/run-llama/llama_index/issues/9426>
- **LangChain.** NOT VERIFIED: the current loader index page did not list loaders for the five
  providers when read on 2026-09-19. Older versions had several. Re-check before saying anything
  about it. <https://docs.langchain.com/oss/python/integrations/document_loaders>
- **License.** FOUND: both are MIT. The first pass asserted this from memory with no link, which
  is exactly what this file's rule 2 forbids, so the `LICENSE` files were opened for this
  rewrite. LlamaIndex: "The MIT License", copyright Jerry Liu,
  <https://github.com/run-llama/llama_index/blob/main/LICENSE>. LangChain: "MIT License",
  copyright LangChain, Inc., <https://github.com/langchain-ai/langchain/blob/master/LICENSE>.
- **Choose them instead when** you are building a prototype, a one-time load is enough, and
  everyone who can query may see everything that was loaded.

### Onyx (formerly Danswer)

- **What it is.** FOUND: its repository describes it as "Open Source AI Platform - AI Chat with
  advanced features that works with every LLM". It is an AI assistant and enterprise search
  product with its own index, chat and agents. <https://github.com/onyx-dot-app/onyx>
- **License.** FOUND: Community Edition under MIT, with a separate Enterprise Edition (GitHub
  does not resolve the repository to a single standard identifier). The connector overview says:
  "Permission-syncing connectors are an Enterprise Edition feature."
  <https://docs.onyx.app/admins/connectors/overview>
- **Permissions.** FOUND: the permission-syncing list on that page has twelve connectors:
  Confluence, Jira, Google Drive, Gmail, Slack, Salesforce, GitHub, Box, Canvas, SharePoint,
  Microsoft Teams and Outlook. **This moved since the first pass**, which found Slack and Outlook
  of the five; Microsoft Teams is on the list today. Of Lawang's two release providers, Slack has
  permission syncing and ClickUp does not.
- **Coverage of the five.** FOUND: Slack, Microsoft Teams, Outlook, ClickUp and HubSpot are all
  listed as connectors on the same page.
- **Ingest.** NOT VERIFIED: the documentation read does not say whether connectors poll or use
  webhooks. INFER from the tracker items below: indexing and permission sync are separate
  periodic jobs.
- **What their issue tracker shows about how hard this is.** These are not criticisms. They show
  that a careful, well-staffed team still meets these defects, which is the best evidence that
  the problem is real:
  - FOUND: issue #9664, opened 2026-03-26 and closed 2026-03-27. Slack permission sync accepted
    `start` and `end` parameters and never passed them on, so the sync would "paginate through
    **every message in every channel**" regardless of the connector's configured start date, and
    ran until a stall timeout killed it. <https://github.com/onyx-dot-app/onyx/issues/9664>
  - FOUND: pull request #14840, merged 2026-09-16. "A standard channel was indexed as public, so
    every Onyx user could search it. A standard channel is visible to its team, not the tenant."
    The fix reads every channel's readers from Graph's all-members call, once per channel. The
    same description notes that "A member row without an email is resolved by user id" and that a
    member from another tenant "is skipped instead of failing the channel".
    <https://github.com/onyx-dot-app/onyx/pull/14840>
  - FOUND: pull request #14848, merged 2026-09-27, "feat: sync OneDrive permissions". Among the
    changes its description lists: permission-only changes bypass the indexing deduplication
    gates and update ACLs per connector-credential pair. **A correction to the first pass:** it
    cited this pull request as open, and quoted two sentences about a permission change failing a
    content-hash check. Neither sentence is on the page on 2026-10-04 and the pull request is
    merged, so both quotations have been removed rather than re-attributed. What is on the page
    today supports the same underlying point, that a deduplication gate keyed on content can skip
    a permission-only change, so the design lesson stands on the current text.
    <https://github.com/onyx-dot-app/onyx/pull/14848>
- **Slack terms.** FOUND, Onyx's own words: "In June 2025, Slack introduced ToS and API changes
  that restricted customers from indexing their own data", Onyx "introduced the Slack Federated
  connector that uses the Search APIs as an alternative to the indexing connector", and "However,
  Slack has recently reversed these API restrictions, which allows the indexing connector to work
  again." <https://docs.onyx.app/admins/connectors/official/slack/slack_federated>
  NOT VERIFIED: the reversal, in Slack's own words. See section 3.
- **Choose it instead when** you want a complete product (search, chat, agents, admin screens)
  that you can self-host today. The Enterprise Edition is how Onyx funds a maintained product and
  support, and permission syncing is part of what it buys.

### Glean (the commercial reference)

- **What it is.** FOUND: a commercial enterprise search and assistant product. "Glean connectors
  fetch each source's permissions map, so search results only show a user what they're already
  allowed to see in the source application." Data goes to an isolated Glean tenant.
  <https://docs.glean.com/connectors/about>
- **Ingest.** FOUND: "Indexed connectors often use incremental crawls and sometimes webhooks or
  push APIs so the index approaches real time without requiring every query to hit the source."
  The same page describes identity resolution: "Glean aligns identities across systems (for
  example, recognizing that identifiers or display names in different apps refer to the same
  person when connector and directory data support that mapping)."
  <https://docs.glean.com/connectors/connectors-power-glean>
- **Coverage.** FOUND: the public connector directory lists 101 connectors, including Slack (and
  a separate "Slack Real Time Search"), Teams, Outlook, ClickUp and HubSpot.
  <https://docs.glean.com/connectors/> The first pass asserted this coverage from memory with no
  link, and that memory claim was carrying a cell in section 5's table, so the directory was
  opened for this rewrite. A HubSpot connector with incremental updates and webhooks is
  documented separately. <https://docs.glean.com/connectors/native/hubspot/home> (read
  2026-09-19) NOT VERIFIED: per-connector permission behaviour. The "about" page states the
  general rule; no page was found that says what each connector does.
- **Self-hosting and license.** FOUND: proprietary, a multi-tenant SaaS tenant per the "about"
  page. NOT VERIFIED: deployment options in a customer's own cloud, and pricing.
- **Choose it instead when** you are a larger company that wants to buy the whole result
  (connectors, permissions, ranking, assistant, support) rather than build a retrieval system.

### Merge (unified API)

- **What it is.** FOUND: a commercial unified API. "The Merge permissions.roles array indicates
  the permissions that a group or user has for a file or folder: Read, Write or Owner."
  <https://help.merge.dev/articles/10439047-file-storage-access-control-list-acls>
  Merge also publishes guidance on respecting ACLs when its File Storage data feeds a RAG
  pipeline.
  <https://help.merge.dev/articles/4066107080-how-do-i-respect-acls-when-using-merge-s-file-storage-data-in-a-rag-pipeline>
  (read 2026-09-19)
- **Ingest.** FOUND, on the ACL page: "Enabling webhooks allows permission changes to be
  reflected in real-time. This function is only supported by Google Drive and Box." And: "For
  Sharepoint, OneDrive and Dropbox, permissions will be updated based on your sync frequency."
  The first pass marked this FOUND from a search summary with no link; the page was opened for
  this rewrite.
- **Coverage.** INFER: ACL support is for the file storage and knowledge base categories. NOT
  VERIFIED per provider, including whether HubSpot and ClickUp are covered as data without ACLs.
- **Self-hosting.** Hosted product. NOT VERIFIED: any on-premises option.
- **Choose it instead when** you sell a SaaS product and need your customers to connect many
  file, HR or CRM systems through one API, with a vendor handling maintenance.

### Paragon (embedded integration platform, Managed Sync)

- **What it is.** FOUND: "Managed Sync provides pipelines to sync data from your users'
  integration sources to your app or RAG pipeline." It sends webhook updates for changes, and
  "Syncs also periodically perform a full refresh of their watched content to validate data
  accuracy and completeness." A Permissions API answers questions against a managed graph: "Use
  this API to check on the users that are allowed to read or write to a Synced Object."
  <https://docs.useparagon.com/managed-sync/overview>
- **Coverage.** FOUND: the overview names the File Storage, Documents and CRM categories, and
  names Google Drive as the one specific integration. It does not name Slack, Teams, Outlook,
  HubSpot or ClickUp. NOT VERIFIED: the full integration list.
- **Duplicates and edits.** FOUND: Paragon's article on freshness states the cost of duplicates
  directly, quoted in section 6. The first pass put the phrase "deduplicating conflicting
  versions" in section 5's table citing only "(vendor blog)" with no link. It was not confirmed
  at a page, so it has been removed from the table.
  <https://www.useparagon.com/blog/rag-freshness-incremental-sync-deduplication>
- **License and self-hosting.** Commercial. NOT VERIFIED: on-premises options for Managed Sync.
- **INFER:** this is the closest product to Lawang in function: ingestion for your own RAG
  pipeline, with permissions as a first-class part. The design differs. Paragon keeps the
  permission graph and you ask it at query time; Lawang plans to stamp a scope on each record and
  push membership to your sink, so enforcement needs no call back to Lawang.
- **Choose it instead when** you build a multi-tenant SaaS product, want a vendor to run both the
  ingestion and the permission graph, and file and CRM sources are what your customers need.

### Truto and Unified.to (unified APIs that normalize permissions)

- FOUND: Truto's guide recommends pre-filtering in the vector database, fetching several times
  more results than needed, post-filtering through an authorization service such as SpiceDB or
  OpenFGA, a live check against the source for stale entries, and audit logs. Its unified APIs
  return normalized permission arrays for Google Drive, Confluence, SharePoint, Notion and Box.
  <https://truto.one/blog/how-to-maintain-document-level-rbac-in-enterprise-rag-pipelines/>
  (read 2026-10-04, published 2026-05-06)
- FOUND: Unified.to publishes similar guidance. (read 2026-09-19)
  <https://unified.to/blog/permissions_security_and_compliance_in_rag_pipelines>
- **Choose them instead when** the sources are file and wiki systems and a hosted API is fine.

### Vectara (RAG platform)

- FOUND: access control is done with metadata filter attributes set at ingestion and applied at
  query time. (read 2026-09-19)
  <https://docs.vectara.com/docs/security/authorization/attribute-based-access-control>
- FOUND: data arrives through Vectara's indexing APIs or partner pipelines. The one "connector"
  in its agent documentation binds an agent to a Slack app, which is a chat surface, not
  ingestion. (read 2026-09-19) <https://docs.vectara.com/docs/agents/integrations>
- INFER: Vectara is a possible **destination** for Lawang records, not an alternative to it. The
  `visibility.scope` field maps directly onto a filter attribute.
- **Choose it instead when** you want retrieval and generation as a managed service and already
  have a way to get documents and their access attributes out of your sources.

### Microsoft 365 Copilot connectors (formerly Microsoft Graph connectors)

- FOUND (read 2026-10-04, page dated 2026-05-14 and updated 2026-09-25): two kinds. **Synced
  connectors** index external data into Microsoft Graph. "Each item includes content, metadata
  (like title and URL), and an access control list (ACL) that enforces permissions." On the
  continuous sync behaviour: "Connectors periodically check for changes. New, updated, or deleted
  content is reflected in the index." **Federated connectors** "Use a Model Context Protocol
  (MCP) model to fetch data in real time, without indexing content into Microsoft 365." Microsoft
  "offers over 100 prebuilt connectors", and custom connectors push items through the Graph
  connectors API. <https://learn.microsoft.com/en-us/microsoft-365/copilot/connectors/overview>
- The data lands in Microsoft Graph and serves Copilot and Microsoft Search. It is not a feed for
  your own index.
- INFER: the Graph connectors API, with its per-item ACL, is a possible Lawang sink later.
- **Choose it instead when** Copilot or Microsoft Search is where your people ask questions.

### Amazon Bedrock Managed Knowledge Base (ACL-aware retrieval)

- FOUND: permissions (allowed and denied users and groups) are ingested with the content, and
  retrieval filters on a user context you supply. For SharePoint, OneDrive, Google Drive and
  Confluence, a second real-time check asks the source whether the user still has access. S3 and
  Custom sources take ACLs from customer-provided metadata, with no real-time check. The
  connector support table has eight rows, and **none of Lawang's five providers is on it**.
  <https://docs.aws.amazon.com/bedrock/latest/userguide/kb-managed-acl.html>
- FOUND: the documentation is unusually plain about its limits, and it is the best model found
  for honest writing about an access feature. Its own words:
  - A heading: "ACL awareness is not authorization". Under it: "Bedrock Managed Knowledge Base
    does not authenticate end users".
  - "ACL-aware retrieval fails closed."
  - "Group memberships are as fresh as the last sync."
  - Email is the universal identifier, and: "If emails differ across systems, ACL matching fails
    silently and the user receives no results from that data source." A reassigned email address
    is listed under the customer's own responsibilities.
- INFER: the Custom source, which accepts customer-provided ACL metadata, is a possible sink.
- **Choose it instead when** you are on AWS, your sources are SharePoint, OneDrive, Google Drive
  or Confluence, and a managed service is what you want.

### Slack's own answer: Real-Time Search API and Data Access API

- FOUND: the Real-Time Search API "allows apps to access Slack data through a secure search
  interface". Results follow the asking person's own access: "Your app performs the search on
  behalf of the authenticated user, ensuring that only content the user has access to is
  returned." Data is not to be copied out: "You must not store or copy any of the data retrieved
  from this API." Private channel and direct message content needs both an admin installation
  with private scopes and the person's own consent in the Slack client.
  <https://docs.slack.dev/apis/web-api/real-time-search-api/>
  <https://slack.com/blog/news/mcp-real-time-search-api-now-available> (read 2026-09-19)
- FOUND: the Data Access API gives an app short-lived access to what the invoking user can see,
  for RAG queries. (read 2026-09-19) <https://api.slack.com/docs/apps/data-access-api>
- INFER: for Slack, the platform owner is steering AI products toward live, per-user retrieval
  and away from copying messages into an outside index. See section 4. Note that the storage
  prohibition quoted above is a term of that API, not of the Events API, and the two are
  different paths.

### Airweave

- FOUND: its repository describes it as "Open-source context retrieval layer for AI agents", MIT
  licensed. (The first pass quoted a longer version of this line ending "and RAG systems"; the
  line read on 2026-10-04 does not, so the shorter one is quoted.) It needs PostgreSQL, Vespa,
  Temporal and Redis. <https://github.com/airweave-ai/airweave>
- FOUND: its documentation index lists 50 connector pages, including ClickUp, HubSpot, Slack,
  Teams, and Outlook Mail and Outlook Calendar. <https://docs.airweave.ai/llms.txt>
- FOUND: the Slack connector is federated: "Instead of syncing all messages and files, this
  source searches Slack at query time using the search.all API endpoint." The OAuth flow requests
  the `search:read` user scope. <https://docs.airweave.ai/docs/connectors/slack.md>
- FOUND: the documentation index has no page on access control, ACLs, permissions or deletion
  handling. Its webhooks pages are about Airweave notifying you of sync events, not about
  receiving provider webhooks. INFER: isolation is by collection and by whose credentials made
  the connection. NOT VERIFIED: whether the code carries source ACLs for any connector. Its
  source was not read.
- **Choose it instead when** you want a ready retrieval layer for agents over many apps, and a
  separate collection per user or per tenant is enough access control. (That sentence is this
  review's reading of the documentation, not a quotation from it.)

### PipesHub

- FOUND: Apache 2.0. Its README calls it "The Open-Source Workplace AI Platform", and lists
  "Permission-Aware Search: Enforces source-level access controls so users only see what they're
  authorized to." It needs a graph database (Neo4j or ArangoDB), Qdrant, MongoDB and Redis, with
  Kafka in larger deployments. <https://github.com/pipeshub-ai/pipeshub-ai>
  (The first pass quoted a one-line description that is not on the README read on 2026-10-04, so
  the current line is quoted instead.)
- FOUND: its connector overview lists connectors for Slack, Microsoft Teams and Outlook, among
  about forty. ClickUp and HubSpot are not listed. On sync frequency the page says: "Periodic
  syncing - Data is synced on a schedule, not in real-time".
  <https://docs.pipeshub.com/connectors/overview>
- NOT VERIFIED: how any individual connector is scheduled or triggered, and whether any uses
  webhooks. The connector source was not read. (The first pass set this documentation line
  against the README's "real-time and scheduled indexing" and left the question open, which reads
  as an accusation written by somebody who did not check. Both pages are reported here, each with
  its link, and the open question sits in section 9 where it belongs.)
- **Choose it instead when** you want an open source, Apache 2.0, complete alternative to Glean,
  and you are ready to run its data stores.

### Related, not competing

- **Convoy** (Go, Elastic License 2.0): "The Cloud Native Webhooks Gateway". It ingests,
  persists, retries and delivers webhooks, with idempotency keys, and it knows nothing about
  providers, records or permissions. <https://github.com/frain-dev/convoy> Choose it when you
  need a general webhook gateway.
- **OpenFGA, SpiceDB, Oso, Cerbos**: authorization engines. They answer "may this person see this
  object" at query time. They are the enforcement half, and the guides from Truto, Paragon and
  Cerbos all place them after retrieval. <https://github.com/openfga/openfga>
  <https://www.cerbos.dev/blog/authorization-for-rag-applications-langchain-chromadb-cerbos>
  (read 2026-09-19) INFER: Lawang's scope and membership data could feed any of them.
- **Azure AI Search SharePoint indexer**: FOUND (read 2026-09-19): a preview API
  (2026-05-01-preview) ingests SharePoint permission metadata and honors it at query time.
  <https://learn.microsoft.com/en-us/azure/search/search-indexer-sharepoint-access-control-lists>

---

## 3. A platform fact that shapes everything: Slack's 2025 terms

- FOUND: on 2025-05-29 Slack announced that commercially distributed apps outside the Slack
  Marketplace get `conversations.history` and `conversations.replies` limited to "1 request per
  minute and will return a maximum of 15 objects per request". This applied immediately to apps
  created on or after 2025-05-29, and from 2025-06-30 to apps created before it. Slack's stated
  reason is to "prevent bulk data exfiltration by unvetted applications".
  <https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps/>
- FOUND, on the same page: "Internal customer-built applications are not impacted by these
  changes", and keep limits of "1,000 messages per request at 50+ requests per minute". A
  clarification followed on 2025-06-03.
  <https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/> (read 2026-09-19)
- FOUND: the terms were updated again on 2025-10-13 for the Real-Time Search API and the Data
  Access API, including language on temporary caching and storage. (read 2026-09-19)
  <https://docs.slack.dev/changelog/2025/10/13/api-terms-update/>
- NOT VERIFIED: Onyx's statement that Slack "has recently reversed these API restrictions". It
  was looked for in Slack's own changelog on 2026-09-19 and again on 2026-10-04 and not found
  there. Onyx says it; Slack's changelog, as read, does not. Both are reported, and neither is
  treated as settled.

INFER, and this needs the maintainer's own reading of Slack's terms rather than this review's:

1. A company that self-hosts Lawang for **its own** workspace, with a Slack app it created
   itself, looks like an internal customer-built app. Events API ingestion and history-based
   reconciliation should both work at normal limits.
2. A vendor that uses Lawang to ingest **its customers'** workspaces is commercially distributing
   a Slack app. Outside the Marketplace, Slack reconciliation would run at 15 objects per minute
   per workspace, which makes a backfill impractical. Inside the Marketplace, the vendor must
   pass Slack's review and follow its data terms.
3. Webhooks first (the Events API) is the path Slack itself recommends, which suits the design.
   The part at risk is the reconcile half of "webhooks first, reconciliation always".
4. Lawang's Slack documentation should say which of these two cases it supports, before anyone
   finds out the hard way. Slack is one of the two providers in v0.1.0, so this is a release
   question now rather than a later one.

---

## 4. The trend toward not copying data at all

FOUND: live, per-user retrieval is offered next to, or instead of, indexing by Slack (the
Real-Time Search API), Microsoft (federated Copilot connectors over MCP), Airweave (its Slack
connector) and Onyx (its federated Slack connector). Links are in section 2. Paragon's guide
describes tool calling at prompt time as "one of the safest ways to query data from integration
providers while respecting permissions", and lists its trade-offs, which include the provider's
API not being optimal for searching, and tool-calling performance.
<https://www.useparagon.com/learn/permissions-access-control-for-production-rag-apps/>

FOUND: the counter-argument, from Onyx's documentation, is that the indexed connector "performs
significantly better than the Slack Search APIs".
<https://docs.onyx.app/admins/connectors/official/slack/slack_federated>

INFER: "query the source live with the user's token" is a serious alternative to Lawang for some
teams, and the positioning must say so. Ingestion still wins when you need ranking across
sources, memory that outlives a query, offline processing (summaries, entity graphs), or a source
with no usable search API.

---

## 5. Comparison table

All cells read on **2026-10-04**. "?" means not verified. "The five" means Slack, Microsoft Teams,
Outlook, ClickUp and HubSpot, in that order (S, T, O, C, H), which is the set the architecture
designs for. v0.1.0 ships C and S.

The table has two columns about coverage on purpose. The first pass had one, headed by provider
count, and read against a two-provider release it says that two named open source projects cover
five providers to Lawang's two. **That is accurate, and it is kept.** Provider count is not the
axis this project competes on, so the other column states the axis it does compete on: whether
the permission data leaves the system with the record, into an index the adopter owns. Neither
column is hidden behind the other.

| Project | Kind | License | Self-host | Ingest | Permission data travels with the delivered record | Duplicates and edits | Connectors, of the five |
|---|---|---|---|---|---|---|---|
| **Lawang (v0.1.0 planned; the README says what runs)** | ingestion component | Apache 2.0 | yes | webhooks first, cursor reconcile (reconcile planned, M4) | **the design's whole point:** one scope per record, membership synced under the same scope id to a sink the adopter owns (planned, M4) | deterministic ids, forward-only supersede chain | **2 of 5** (C, S). T, O, H after v0.1 |
| Airbyte | ELT platform | connectors ELv2, protocol MIT | yes | sync modes; no webhooks documented for Slack | no, except 3 connectors with ACL streams (SharePoint Enterprise, Google Drive, Salesforce), none of them one of the five | sync modes at the destination | S T O H yes, C ? |
| Nango | auth and integration platform | not resolved by GitHub; ? | auth and proxy free; syncs and webhooks paid | polling syncs and webhooks | no, the integrating application decides | the integrating application decides | auth for most APIs, per provider ? |
| Unstructured ingest | parsing plus connectors | Apache 2.0 (library) | yes | batch | ? | re-run based | S O yes (2026-09-19), others ? |
| LlamaIndex / LangChain loaders | libraries | MIT | n/a | one-time pull | no (Slack reader) | no | S yes, others ? |
| Onyx | complete search and chat product | MIT (CE), Enterprise Edition separate | yes | periodic jobs (?) | no, enforced inside its own index. Permission sync is Enterprise Edition and covers S, T, O of the five | index upsert, separate permission sync | **5 of 5** as connectors |
| PipesHub | complete workplace AI platform | Apache 2.0 | yes | scheduled, per its connector docs | no, enforced inside its own index and graph database | ? | **3 of 5** (S, T, O). No C, no H |
| Airweave | retrieval layer | MIT | yes | scheduled sync; Slack is live search | not documented anywhere in its docs index | ? | **5 of 5** (Slack federated) |
| Glean | commercial product | proprietary | no (SaaS tenant) | incremental crawls, sometimes webhooks or push APIs | no, enforced inside Glean's own index | product internal | **5 of 5** in the connector directory |
| Merge | unified API | proprietary | no | polling; webhooks for Drive and Box ACLs only | yes for file storage models, as a Permissions sub-model you read through the API | ? | ? for all five |
| Paragon Managed Sync | embedded integration, managed ingestion | proprietary | ? | webhooks plus periodic full refresh | no, kept in a managed graph you query through the Permissions API | ? | none of the five named in the overview |
| Vectara | RAG platform | proprietary | ? | you push documents | metadata filters you set | you decide | n/a (a destination) |
| Copilot connectors | Microsoft 365 feature | proprietary | no | periodic crawl, or live (federated) | yes, a per-item ACL, into Microsoft Graph rather than your index | platform internal | 100+ prebuilt; the five not confirmed |
| Bedrock Managed Knowledge Base | AWS service | proprietary | no | crawl | yes, allow and deny lists, into the managed knowledge base | platform internal | **none of the five** |
| Slack Real-Time Search API | platform API | Slack terms | no | none (live) | native, per user, and copying the data out is forbidden | n/a | S only |

**What the table says, read honestly.** On provider count Lawang is last. On "does the permission
data leave with the record, into an index you own", the only other yes answers are hosted
products (Merge's file storage models, Copilot connectors into Microsoft Graph, Bedrock into its
own knowledge base), and all three carry it into a store the vendor defines rather than one the
adopter chose. The four open source projects enforce permissions inside their own index, which is
a different product, not a worse one: if you want the index as well, they give you more than
Lawang plans to.

---

## 6. The problem in people's own words

Phrases for the README and the docs. **Every quotation in this section was re-opened at its link
and confirmed character for character on 2026-10-04.** Where a sentence in the original runs on
past the quoted part, only the confirmed part is quoted. Where a source could not be confirmed,
it was removed, and section 9 lists which.

**Permission leaks**

- "The failure mode is simple: the model composes a fluent answer from a document the asking user
  was never allowed to see." Kevin Riedl, Wavect (read 2026-10-04, published 2026-06-11).
  <https://wavect.io/blog/rag-permissions-sharepoint-confluence-drive/>
- "A user queries 'Q4 revenue projections' and the system dutifully returns the most semantically
  similar chunks", which, the article continues, come from a confidential board deck the user was
  never supposed to see. Also: "The embedding model does not know about permissions." And: "This
  is the primary reason many enterprises stall on RAG deployments". Kirk Ryan (read 2026-10-04,
  published 2026-03-03).
  <https://kirkryan.co.uk/item-level-permissions-in-rag-why-your-vector-database-needs-access-control/>
- "Failing to properly handle user permissions with your AI-enabled features/agents can create
  silent data leaks, introduce compliance risks, and allow the AI to bypass existing access
  controls." Nango (read 2026-10-04, published 2026-04-01).
  <https://nango.dev/blog/preserve-user-permissions-roles-api-integrations-ai-agents-rag/>
- "access control bypass occurs when the RAG pipeline doesn't enforce the same permissions as the
  source system". Truto (read 2026-10-04, published 2026-05-06). Link in section 2.
- In Japanese, a Qiita article published 2026-09-17 (read 2026-09-19) makes the same point: a
  search service that fetches text with broad rights and hands it to a model has not inherited
  the source system's viewing limits, even where the person could not open the original document.
  This is a summary of the article, not a translation of any sentence in it.
  <https://qiita.com/kagi_to_packet/items/e5d13edbefc293de4e90>

**Stale permissions (the revocation window)**

- "From 9 AM Monday to 2 AM Tuesday, that user can still pull highly sensitive finance documents".
  The scenario around it: an employee leaves the finance team on Monday morning, and the nightly
  sync runs at 2 AM. Truto (read 2026-10-04, published 2026-05-06). Link in section 2.
- "permission changes in SharePoint are not automatically propagated to downstream systems", and
  a revoked user "may still see the document's content in the search index or RAG pipeline until
  the next ingestion run". Elena Vavilova, Microsoft ISE blog (read 2026-10-04, published
  2026-04-30). <https://devblogs.microsoft.com/ise/sharepoint-doc-level-access/>
- "you are now responsible for making sure your permissions data is always up-to-date". And:
  "Permissions are table-stakes." Paragon (read 2026-10-04).
  <https://www.useparagon.com/learn/permissions-access-control-for-production-rag-apps/>

**Why syncing permissions is hard**

- "Many integrations simply don't have permissions APIs", and many that do "don't expose one that
  offers a centralized view of both logic + data". Hazal Mestci, Oso (read 2026-10-04, published
  2025-07-01).
  <https://www.osohq.com/post/should-you-respect-3rd-party-permissions-or-sync-to-your-own-system-the-rag-chatbot-dilemma>
- "Attempting to flatten these complex, graph-like relationships into simple key-value metadata
  tags is incredibly difficult." Truto (read 2026-10-04, published 2026-05-06). Link in section 2.
- INFER: Lawang's providers have a simpler model than file systems. Visibility follows one
  container (channel, list, mailbox, portal), with no inheritance tree. That is why a single
  scope rule can be honest there. It would not be honest for SharePoint or Google Drive.

**Webhooks that go missing**

- "The subscription remains active and valid, but notifications stop being delivered for periods
  of approximately 1 to 1.5 hours, without any error or lifecycle notification being sent to my
  endpoint." A developer on Microsoft Q&A, about mail notifications (read 2026-10-04, posted
  2026-03-17).
  <https://learn.microsoft.com/en-us/answers/questions/5825832/bug-issue-microsoft-graph-webhook-subscriptions-in>
- Microsoft's own documentation, on what happens while delivery is paused: "Any notifications
  about resource changes that happen when the change notification delivery pauses and the time
  when the app successfully creates the subscription again are lost." And: "In such cases, the
  app should separately fetch those changes, for example using the delta query." The same page
  says that `missed` lifecycle notifications exist only for Outlook message, event and personal
  contact resources, that `subscriptionRemoved` adds the Teams `chatMessage` resource, and that a
  `lifecycleNotificationUrl` cannot be added later: "To add the **lifecycleNotificationUrl**
  property, you must delete the existing subscription and create a new subscription while
  specifying the property during subscription creation."
  <https://learn.microsoft.com/en-us/graph/change-notifications-lifecycle-events>
- Slack: "Your app should respond to the event request with an HTTP 2xx _within three seconds_",
  a failed request is retried "up to _3 times_ in a gradually increasing timetable", and "When
  your application enters any combination of these failure conditions for more than _95% of
  delivery attempts_ within 60 minutes, your application's event subscriptions will be
  temporarily disabled." The page does not say that events missed while disabled are sent again.
  <https://docs.slack.dev/apis/events-api/>

**Duplicates, edits, deletions, a stale index**

- "Duplicate chunks waste top-k slots, crowd out distinct evidence, inflate embedding and
  reranking costs, and make repeated claims look better supported than they are." Paragon (read
  2026-10-04). <https://www.useparagon.com/blog/rag-freshness-incremental-sync-deduplication>
- The same article lists what each team rebuilds: "auth, backfills, cursors, pagination, rate
  limits, retries, deletes, deduplication, reconciliation, normalization, and permissions".

**Words people use, and words they do not.** FOUND across the sources above: "permission-aware",
"ACL-aware", "document-level access control", "document-level permissions", "permission sync",
"ACL sync", "respect source permissions", "stale permissions", "data leak", "pre-filter" and
"post-filter", "incremental sync", "keep the index fresh". INFER: none of these sources says
"permission-stamped", uses "scope" in Lawang's sense, or calls this problem "AI memory". The
README should use the common words first and introduce its own terms second.

---

## 7. Where these people are

A map for later. **Nobody was contacted and nothing was joined or posted.** Rules change, so read
each community's current rules before any post.

**This section is not ready to act on.** Reddit's pages cannot be fetched by the tools this
review has, on 2026-09-19 and again on 2026-10-04, so none of the Reddit rows below carries a
verified rule, and this review will not characterise a community's rules from memory. The
`bizdev` rules require a draft to state each community's own self-promotion rules, so the Reddit
rows and the rows marked NOT VERIFIED cannot support a post until somebody opens the sidebar and
the wiki and fills them in. The first pass stated that "Reddit's general custom is roughly nine
ordinary contributions for each promotional one" with no source. No page was found that states
that ratio, so it has been removed rather than re-sourced.

### Global, English

| Place | What it is | Activity | Self-promotion rules |
|---|---|---|---|
| Hacker News, Show HN | General technology news, strong infrastructure audience | Very high | FOUND: "Show HN is for something you've made that other people can play with." Off topic are "blog posts, sign-up pages, newsletters, lists, and other reading material. Those can't be tried out, so can't be Show HNs." And: "If your work isn't ready for users to try out, please don't do a Show HN", "Please don't ask friends to upvote or comment." <https://news.ycombinator.com/showhn.html> So: not before M6. |
| Lobsters | Invite-only technical link site | Medium, high quality | FOUND: an invite is needed, and "As a rule of thumb, self-promo should be less than a quarter of one's stories and comments." New users also cannot "use tags for meta discussions or that are prone to off-topic stories", a list that includes `show` and `announce`. <https://lobste.rs/about> |
| r/Rag, r/LocalLLaMA, r/LangChain | RAG builders | High | NOT VERIFIED. The pages cannot be fetched here. Read each sidebar and wiki before posting. |
| r/dataengineering | Data engineers | High | NOT VERIFIED. The page cannot be fetched here. The first pass added "Known for strict limits on vendor posts" from memory; that has been removed, because it is a claim about a named community with no source. |
| r/golang | Go developers | High | NOT VERIFIED. The page cannot be fetched here. Read the rules on project posts and on AI-assisted code before posting: this repository is built with agents and should say so openly. |
| r/selfhosted | People who run their own services | High | NOT VERIFIED. The page cannot be fetched here. INFER, from the subreddit's purpose rather than from its rules: it wants something that installs today, so after M6. |
| MLOps Community (Slack) | Practitioners of ML in production | SEARCH ONLY: over 27,900 members, per <https://datatalks.club/blog/slack-communities.html> (read 2026-09-19), which is a third party rather than the community itself | NOT VERIFIED. The rules page exists but did not render: <https://mlops.notion.site/MLOps-Community-Rules-0c69be943d0f4efa9e7863414fefc250> |
| Relevance Slack and Haystack conference | Search relevance engineers (OpenSource Connections) | FOUND: the Slack had 650+ members in 2021, per <https://opensourceconnections.com/blog/2021/07/06/building-the-search-community-with-relevance-slack/> (read 2026-09-19); the conference is at <https://haystackconf.com/> | NOT VERIFIED. INFER: talks there are practitioner talks, not product pitches. |
| AI Engineer conferences | Large AI engineering events | FOUND (read 2026-09-19): the World's Fair ran 2026-06-29 to 2026-07-02 in San Francisco; the New York event is 2026-10-12 to 2026-10-14. <https://ai.engineer/> | Talks go through a call for speakers. |
| Project servers (Onyx Discord, LlamaIndex Discord, and similar) | Users of adjacent projects | High | INFER: good places to **listen** to the problem. Presenting your own project in another project's server is rarely welcome, and this review proposes no post there. |

### Go infrastructure

| Place | What it is | Notes |
|---|---|---|
| GopherCon Singapore | Closest major Go conference to Indonesia | FOUND (read 2026-09-19): the 2026 edition ran 2026-05-20 to 2026-05-22 and its call for proposals closed 2026-03-06. <https://2026.gophercon.sg/cfp/> INFER: the 2027 call probably opens around the start of 2027. Architecture section 10 is talk material. |
| GopherCon Europe (Berlin) and GopherCon (US) | Main Go conferences | <https://www.gophercon.eu/> <https://www.gophercon.com/> (read 2026-09-19) Calls for speakers through Sessionize. |
| Go wiki lists | User groups and conferences worldwide | <https://go.dev/wiki/GoUserGroups> (read 2026-09-19) |
| Golang Weekly and similar newsletters | Curated links | NOT VERIFIED: how to suggest a link. Editors choose; you may suggest once. |

### Indonesia and Southeast Asia

| Place | What it is | Notes |
|---|---|---|
| Golang Indonesia (Telegram) | Indonesian Go developers | FOUND (read 2026-09-19): <https://telegram.me/golangID>, has a code of conduct. NOT VERIFIED: size, and its rule on sharing projects. |
| GoJakarta meetup | Go meetup in Jakarta | FOUND (read 2026-09-19): the group exists at <https://www.meetup.com/gojakarta/>. SEARCH ONLY: that it moved to Luma in 2026-07. INFER: a talk is the natural format. |
| AI Tinkerers Jakarta | Builders of AI systems, demo-focused meetups | FOUND (read 2026-09-19): <https://jakarta.aitinkerers.org/>, part of a network across many cities. The format is a live demo, so after something runs. |
| Indonesia AI (Discord) and KOMUNITAS AICO (Discord) | Indonesian AI communities | SEARCH ONLY: about 5,400 and about 19,000 members. No page was opened that states either figure. NOT VERIFIED: their rules, and how much of the conversation is about engineering. |

### Other languages

| Place | What it is | Notes |
|---|---|---|
| GeekNews (Korea), Show GN | Korean technology news, modelled on Hacker News | FOUND (read 2026-09-19): Show GN is for things people can really run; no repeated submissions in a short time; do not ask people you know for upvotes or comments; early work is welcome. <https://hada.io/blog/geeknews-show/> |
| Qiita and Zenn (Japan) | Engineering article platforms | FOUND: RAG permission design is an active topic there, evidenced by the Qiita article cited in section 6 (published 2026-09-17). Articles are in Japanese. Propose one only if somebody can write and keep it current. |
| CSDN, Zhihu, Datawhale (Chinese) | Articles and study groups on RAG | SEARCH ONLY: that permission-aware knowledge bases ("RBAC-RAG") are discussed on these platforms. The one link here, <https://github.com/datawhalechina/all-in-rag> (read 2026-09-19), is a Chinese-language RAG study repository, which is not the same claim. NOT VERIFIED: their rules. |

INFER about all of these: the honest format everywhere is a technical article that teaches
something true (architecture section 10 has eleven candidates), with the project mentioned once.
That is Mode 4 work, and only when what it describes runs.

---

## 8. Trust signals that comparable projects show first

What a visitor sees before scrolling. The layouts were read 2026-09-19; the one quotation in the
table, OpenFGA's production statement, was re-confirmed on 2026-10-04 because it is a claim about
another project's production use, and the rest of this table is description rather than
quotation. Star counts
have been removed from this file: they are the fastest-moving numbers in it, nothing in
`positioning.md` rests on any of them, and re-reading six of them on every pass buys nothing.

| Project | First screen |
|---|---|
| OpenFGA (Apache 2.0) | Badges: release, Go reference, Go Report Card, coverage, CII Best Practices, OpenSSF Scorecard, SLSA 3, FOSSA, Artifact Hub, Docker pulls. A production statement, quoted exactly: "Used in production by Auth0 FGA since December 2021". Supported storage versions (PostgreSQL 14+, MySQL 8, SQLite in beta). Docker quickstart. <https://github.com/openfga/openfga> |
| River (MPL-2.0) | CI badge, Go reference, one sentence that says what it is, then working code, then the core idea about transactional enqueueing. <https://github.com/riverqueue/river> |
| Onyx | Demo animation and one install command. <https://github.com/onyx-dot-app/onyx> |
| PipesHub | Tagline and one install command. <https://github.com/pipeshub-ai/pipeshub-ai> |
| Nango | Badges (stars, license, downloads), then sections introducing what Nango is and how it works. <https://github.com/NangoHQ/nango> |
| Convoy | CI badges, container images, links to docs and community chat. <https://github.com/frain-dev/convoy> |
| Bedrock ACL documentation | Not a README, but the best model found for security honesty: a boxed statement of what the feature is **not**, a "Failure behavior" section, and a "Your responsibilities" list. <https://docs.aws.amazon.com/bedrock/latest/userguide/kb-managed-acl.html> |

INFER, the pattern for infrastructure that handles access: (1) one sentence of what it is, (2) an
honest status, (3) a command that ends in something visible, (4) supported versions, (5) a
security policy and a statement of what is and is not protected, (6) a CI badge that is green.
Lawang's README today has 1, 2 and a diagram. It cannot have 3 until M5 or M6. It can have 4, 5
and 6 sooner.

---

## 9. What this review could not verify

Written in the third person on purpose. This is a public file in a repository with one
maintainer's name on it, and a first-person list of things that are not known reads as the
maintainer saying them.

**Still open after the 2026-10-04 rewrite**

- Reddit community rules. The pages cannot be fetched by the tools this review has, on either
  read date. Section 7's Reddit rows are empty for that reason, and the section is marked not
  ready.
- The MLOps Community rules page. It did not render on either read date.
- Onyx's statement that Slack reversed its 2025 API restrictions. Slack's own changelog, as read
  on 2026-09-19 and 2026-10-04, does not show it.
- Whether a self-hosted Lawang is an "internal customer-built application" under Slack's terms.
  This needs the maintainer's own reading, and perhaps a lawyer's. Slack is in v0.1.0, so it is a
  release question.
- Whether a ClickUp source connector exists for Airbyte.
- Which Unstructured, LlamaIndex and LangChain connectors exist today for Teams, Outlook, ClickUp
  and HubSpot, and whether any of them emits permission data.
- How Onyx ingests (polling or webhooks) for each provider.
- Whether Airweave or PipesHub carry source ACLs in code for any connector. Only their
  documentation was read; no source was read.
- Glean's per-connector permission behaviour, Merge's and Paragon's coverage of the five
  providers, and the prices and self-hosted options of all three.
- Nango's license. The self-hosting page was read; the `LICENSE` file was not opened.
- PipesHub's per-connector schedule, and whether any of its connectors uses webhooks.
- The Indonesian Discord membership figures, the GoJakarta move to Luma, and the claim that
  RBAC-RAG is discussed on CSDN and Zhihu. All three are marked SEARCH ONLY: a search summary
  reported them and no page was opened.

**Quotations removed in the 2026-10-04 rewrite, and why**

The bar applied was: re-fetch the source and quote it exactly with its link and read date, or
stop presenting it as words somebody said. Five quotations did not clear it.

- "Vector databases do not inherit source-system permissions", cited to a Medium article. The
  first pass marked it unverified at the page. Removed, and not replaced, because section 6
  already makes the point with three confirmed sources.
- "The webhook makes you fast; the reconciliation makes you correct", cited to a dev.to article.
  Unverified at the page in the first pass. Removed. It is a good line, and it is not anybody's
  confirmed words.
- A RAG index "is a snapshot, and your sources keep changing after that snapshot is taken", cited
  to kapa.ai. Unverified at the page. Removed.
- Deleted documents that "still show up", cited to an Oracle developers blog. Unverified at the
  page. Removed.
- Two sentences about a content-hash check, attributed to Onyx pull request #14848. Neither is on
  that page on 2026-10-04, and the pull request is merged rather than open. Removed, and replaced
  in section 2 with what the page says today.

Three further quotations were replaced rather than removed, because the page had changed since
2026-09-19: Nango's "not a built-in permissions engine" (not on the page; the article's four
designs are described instead), Airweave's one-line description (shorter today), and PipesHub's
one-line description (different today).
