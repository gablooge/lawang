# Landscape: who else works on this problem

First pass, written 2026-09-19 by the `bizdev` agent (Mode 1). Every source below was read on
**2026-09-19** unless a line says otherwise. Other projects change quickly. Re-check any line here
before relying on it, and always before saying it in public.

How to read this file:

- **FOUND** means a source says it, and the link is next to it.
- **INFER** means it is my reading of the sources. It can be wrong.
- **NOT VERIFIED** means I looked and could not confirm it from a primary source.

One caution about method. Pages were read through a tool that fetches a page and summarizes it.
Quotations below were extracted by that tool. They are very likely right, but the maintainer should
open the link and check the exact words before quoting anyone in public.

What Sluiceway itself can claim today is in the README status paragraph: milestone M0 only
(configuration, id recipes, Postgres with row-level security, the outbox). Nothing ingests a
webhook yet. No provider works yet. v0.1.0 is planned, not released. Every comparison below is
between what others ship and what Sluiceway **plans**.

---

## 1. The short answer

**Does a project already do exactly what Sluiceway plans?** As far as this review found: no.

What Sluiceway plans is a combination: (a) a small self-hosted component that only ingests, (b)
webhooks first with reconciliation, (c) every record stamped with the source's own visibility,
with membership changes synced separately, (d) exactly-once delivery to a sink the adopter owns,
(e) for chat, mail, tasks and CRM (Slack, Microsoft Teams, Outlook, ClickUp, HubSpot), (f) under
Apache 2.0 with nothing held back for a paid edition.

The closest things found, and how they differ:

| Closest in | Project | How it differs from the plan |
|---|---|---|
| Function | Paragon Managed Sync (commercial) | Hosted product, not open source. Syncs into your RAG pipeline and keeps a permissions graph you query through an API. Its documentation names file storage, documents and CRM, not chat or mail. |
| Open source, permissions | Onyx | A complete search and chat product, not an ingestion component. Permission sync is an Enterprise Edition feature, not part of the MIT edition. |
| Open source, Apache 2.0 | PipesHub | A complete workplace AI platform with its own index and several data stores. Its connector documentation says syncing is periodic. No ClickUp or HubSpot connector was listed. |
| Open source, same five providers | Airweave | Has connectors for all five. It is a retrieval layer with its own index. No documentation of source ACLs travelling with records was found. Its Slack connector searches Slack live and does not ingest. |

INFER: the space around Sluiceway is crowded with complete products and with hosted unified APIs.
The empty place is the small, self-hosted, ingestion-only component for teams that already own
their index. That is a real niche, and it is also a narrow one. See `positioning.md`.

---

## 2. Projects and products, one by one

### Airbyte (data movement, ELT)

- **What it is.** FOUND: an ELT platform with a large connector catalog. It has moved toward AI
  use cases and publishes guides on RAG pipelines.
  <https://airbyte.com/agentic-data/rag-data-pipeline>
- **License and self-hosting.** FOUND: the platform is under Elastic License 2.0 and can be
  self-hosted. The license forbids offering it to others as a managed service. The Airbyte
  protocol is MIT. <https://docs.airbyte.com/community/licenses>
  NOT VERIFIED: which connectors are MIT and which are ELv2 today. Sources disagree.
- **Ingest.** FOUND: the Slack source polls the Slack Web API with full refresh and incremental
  sync modes. No webhooks are mentioned. <https://docs.airbyte.com/integrations/sources/slack>
- **Permissions.** FOUND: an Airbyte issue opened 2025-08-18 (still open) lists the connectors
  that sync ACLs: SharePoint Enterprise (enterprise connector, `file_permissions` and
  `identities` streams), Google Drive (open source, permissions and identities streams), and
  Salesforce (security objects synced as ordinary objects).
  <https://github.com/airbytehq/airbyte/issues/65073> The Slack source has a Channel Members
  stream, which is membership data, but nothing ties it to messages as an access rule.
- **Duplicates and edits.** FOUND: handled by sync modes and cursors at the destination.
  Deduplication is a destination feature, not a per-event guarantee.
- **Coverage of the five.** FOUND: Slack, Microsoft Teams, Outlook and HubSpot sources exist.
  <https://docs.airbyte.com/integrations/sources/microsoft-teams>
  <https://docs.airbyte.com/integrations/sources/outlook>
  <https://docs.airbyte.com/integrations/sources/hubspot> NOT VERIFIED: ClickUp.
- **Choose it instead when** you replicate many sources into a warehouse or lake on a schedule,
  when minutes or hours of delay are fine, and when permissions matter mainly for files.

### Nango (OAuth and integration infrastructure)

- **What it is.** FOUND: a code-first integration platform: managed OAuth for 1,000+ APIs, token
  refresh, a proxy, syncs, webhooks, tool calls. <https://github.com/NangoHQ/nango>
- **License and self-hosting.** FOUND: Elastic License. The free self-hosted edition includes API
  auth and the proxy. It does not include syncs, webhooks, triggers, tool calls, the MCP server,
  RBAC or MFA. Those need Enterprise self-hosting or the cloud.
  <https://nango.dev/docs/guides/platform/self-hosting>
- **Permissions.** FOUND: Nango's own article (2026-04-01) describes three designs (per-user
  auth, org-wide auth with permission syncing, org-wide auth with your own rules) and says the
  developer implements the permission logic. Nango is "not a built-in permissions engine" in the
  tool's summary of that article.
  <https://nango.dev/blog/preserve-user-permissions-roles-api-integrations-ai-agents-rag/>
- **Choose it instead when** you need OAuth and token handling for hundreds of APIs inside your
  own product. Sluiceway's README already says it can use Nango for this. INFER: the part
  Sluiceway needs (auth and proxy) is the part that is free to self-host, which makes the pairing
  practical.

### Unstructured (document parsing and ingest connectors)

- **What it is.** FOUND: a document parsing platform with source and destination connectors. The
  open source ingest library is Apache 2.0. <https://github.com/Unstructured-IO/unstructured-ingest>
- **Ingest.** FOUND (older documentation): batch source connectors, including Slack, Outlook and
  SharePoint. <https://unstructured.readthedocs.io/en/latest/ingest/source_connectors.html>
- **Permissions.** FOUND: Unstructured's blog argues that connectors must carry metadata and that
  production RAG depends on it.
  <https://unstructured.io/blog/enterprise-rag-why-connectors-matter-in-production-systems>
  NOT VERIFIED: which open source connectors emit permission data today, and for which sources.
- **Choose it instead when** your hard problem is turning files (PDF, Office, HTML) into clean
  chunks. Sluiceway does not parse documents.

### LlamaIndex readers (LlamaHub) and LangChain document loaders

- **What they are.** FOUND: libraries of loaders that pull data into a framework's document type.
  The LlamaIndex Slack reader takes a token, channel ids and a date range, and returns documents.
  Its documentation mentions no membership, no permissions, no incremental updates and no
  deduplication.
  <https://github.com/run-llama/llama_index/tree/main/llama-index-integrations/readers/llama-index-readers-slack>
- **What users report.** FOUND: an issue from 2023-12-11 asks how to load large Slack channels
  after hitting "Rate limit error reached, sleeping for: 10 seconds". It was closed as not planned.
  <https://github.com/run-llama/llama_index/issues/9426>
- **LangChain.** NOT VERIFIED: the current loader index page did not list loaders for the five
  providers when read. Older versions had several. Re-check before saying anything about it.
  <https://docs.langchain.com/oss/python/integrations/document_loaders>
- **License.** Both frameworks are MIT (widely known; not re-read today).
- **Choose them instead when** you are building a prototype, a one-time load is enough, and
  everyone who can query may see everything that was loaded.

### Onyx (formerly Danswer)

- **What it is.** FOUND: an open source AI assistant and enterprise search product with 50+
  connectors, its own index, chat and agents. About 32.2k GitHub stars.
  <https://github.com/onyx-dot-app/onyx>
- **License.** FOUND: Community Edition under MIT, Enterprise Edition for larger organizations.
  "Permission-syncing connectors are an Enterprise Edition feature."
  <https://docs.onyx.app/admins/connectors/overview>
- **Permissions.** FOUND: permission sync is listed for Confluence, Jira, Google Drive, Gmail,
  Slack, Salesforce, GitHub, Box, Canvas, SharePoint and Outlook (same page). Teams channel
  access also exists in code, see the pull request below.
- **Coverage of the five.** FOUND: Slack, Microsoft Teams, Outlook, ClickUp and HubSpot are all
  listed as connectors. Of these, permission sync is documented for Slack and Outlook only.
- **Ingest.** NOT VERIFIED: the documentation read does not say whether connectors poll or use
  webhooks. INFER from the issues below: indexing and permission sync are separate periodic jobs.
- **What their issue tracker shows about how hard this is.** These are not criticisms. They show
  that a careful, well-staffed team still meets these defects, which is the best evidence that
  the problem is real:
  - FOUND: issue #9664 (opened 2026-03-26, closed): Slack permission sync ignored the connector's
    start date and paged "through **every message in every channel**", running for hours until a
    stall timeout. <https://github.com/onyx-dot-app/onyx/issues/9664>
  - FOUND: pull request #14840 (merged 2026-09-16): a Teams standard channel "was indexed as
    public", so every Onyx user could search it, while in Teams it is visible only to its team.
    The fix reads members from Graph's all-members call once per channel.
    <https://github.com/onyx-dot-app/onyx/pull/14840>
  - FOUND: pull request #14848 (open on 2026-09-19): "When permissions change but content and
    `doc_updated_at` remain unchanged", a later copy of the document "fails the content-hash
    check, which excludes permissions, and never reaches the ACL upsert", so a removed user's
    grant stays until a separate permission sync repairs it.
    <https://github.com/onyx-dot-app/onyx/pull/14848>
- **Slack terms.** FOUND: Onyx's documentation says "In June 2025, Slack introduced ToS and API
  changes that restricted customers from indexing their own data", that Onyx added a federated
  Slack connector in response, and that "Slack has recently reversed these API restrictions".
  <https://docs.onyx.app/admins/connectors/official/slack/slack_federated>
  NOT VERIFIED: the reversal, in Slack's own words. See section 3.
- **Choose it instead when** you want a complete product (search, chat, agents, admin screens)
  that you can self-host today, and you accept that permission sync needs the Enterprise Edition.

### Glean (the commercial reference)

- **What it is.** FOUND: a commercial enterprise search and assistant product. Connectors "fetch
  each source's permissions map, so search results only show a user what they're already allowed
  to see in the source application." Data goes to an isolated Glean tenant.
  <https://docs.glean.com/connectors/about>
- **Ingest.** FOUND: "Indexed connectors often use incremental crawls and sometimes webhooks or
  push APIs so the index approaches real time." Glean also aligns identities across systems.
  <https://docs.glean.com/connectors/connectors-power-glean>
- **Coverage.** FOUND: a HubSpot connector with incremental updates and webhooks is documented.
  <https://docs.glean.com/connectors/native/hubspot/home> NOT VERIFIED today: the other four,
  although Glean is widely known to cover Slack, Teams and Outlook.
- **Self-hosting and license.** FOUND: proprietary, multi-tenant SaaS per the page above.
  NOT VERIFIED: current options for deployment in the customer's cloud, and pricing.
- **Choose it instead when** you are a larger company that wants to buy the whole result
  (connectors, permissions, ranking, assistant, support) and not build a retrieval system.

### Merge (unified API)

- **What it is.** FOUND: a commercial unified API. File Storage models carry a Permissions
  sub-model with roles (read, write, owner) for users and groups. Merge documents how to build
  "ACL-aware embeddings" with it.
  <https://help.merge.dev/articles/10439047-file-storage-access-control-list-acls>
  <https://help.merge.dev/articles/4066107080-how-do-i-respect-acls-when-using-merge-s-file-storage-data-in-a-rag-pipeline>
- **Ingest.** FOUND (search summary of Merge's help center): ACLs update in near real time where
  the third-party API supports webhooks, which is Google Drive and Box. For SharePoint, OneDrive
  and Dropbox, permissions update at the sync frequency, which depends on the plan.
- **Coverage.** INFER: ACL support is for file storage and knowledge base categories. HubSpot
  (CRM) and ClickUp (ticketing) are covered as data, without ACLs. NOT VERIFIED per provider.
- **Self-hosting.** Hosted product. NOT VERIFIED: any on-premises option.
- **Choose it instead when** you sell a SaaS product and need your customers to connect many
  file and HR or CRM systems through one API, with a vendor handling maintenance.

### Paragon (embedded integration platform, Managed Sync)

- **What it is.** FOUND: Managed Sync "provides pipelines to sync data from your users'
  integration sources to your app or RAG pipeline". It sends webhook updates for changes and
  "periodically perform[s] a full refresh". A Permissions API answers three questions against a
  managed graph: batch-check, list-objects, list-users.
  <https://docs.useparagon.com/managed-sync/overview>
  <https://www.useparagon.com/learn/permissions-access-control-for-production-rag-apps/>
- **Coverage.** FOUND: the overview names File Storage, Documents and CRM categories. It does not
  name Slack, Teams, Outlook, HubSpot or ClickUp. NOT VERIFIED: the full list.
- **License and self-hosting.** Commercial. NOT VERIFIED: on-premises options for Managed Sync.
- **INFER:** this is the closest product to Sluiceway in function: ingestion for your own RAG
  pipeline, with permissions as a first-class part. The design differs: Paragon keeps the
  permission graph and you ask it at query time. Sluiceway plans to stamp a scope on each record
  and push membership to your sink, so enforcement needs no call to Sluiceway.
- **Choose it instead when** you build a multi-tenant SaaS product, want a vendor to run
  ingestion and the permission graph, and file and CRM sources are what your customers need.

### Truto and Unified.to (unified APIs that normalize permissions)

- FOUND: Truto's guide (2026-05-06) recommends pre-filtering in the vector database, fetching
  3 to 5 times more results than needed, post-filtering through an authorization service such as
  SpiceDB or OpenFGA, a live check against the source for stale entries, and audit logs. Truto's
  unified APIs return normalized permission arrays for Google Drive, Confluence, SharePoint,
  Notion and Box.
  <https://truto.one/blog/how-to-maintain-document-level-rbac-in-enterprise-rag-pipelines/>
- FOUND: Unified.to publishes similar guidance.
  <https://unified.to/blog/permissions_security_and_compliance_in_rag_pipelines>
- **Choose them instead when** the sources are file and wiki systems and a hosted API is fine.

### Vectara (RAG platform)

- FOUND: access control is done with metadata filter attributes set at ingestion and applied at
  query time. <https://docs.vectara.com/docs/security/authorization/attribute-based-access-control>
- FOUND: data arrives through Vectara's indexing APIs or partner pipelines. The one "connector"
  in its agent documentation binds an agent to a Slack app. It is a chat surface, not ingestion.
  <https://docs.vectara.com/docs/agents/integrations>
- INFER: Vectara is a possible **destination** for Sluiceway records, not an alternative to it.
  The `visibility.scope` field maps directly to a filter attribute.
- **Choose it instead when** you want retrieval and generation as a managed service and already
  have a way to get documents and their access attributes out of your sources.

### Microsoft 365 Copilot connectors (formerly Microsoft Graph connectors)

- FOUND (page dated 2026-05-14): two kinds. **Synced connectors** index external data into
  Microsoft Graph. "Each item includes content, metadata (like title and URL), and an access
  control list (ACL) that enforces permissions." Connectors "periodically check for changes".
  **Federated connectors** "use a Model Context Protocol (MCP) model to fetch data in real time,
  without indexing content into Microsoft 365". More than 100 prebuilt connectors. Custom
  connectors push items through the Graph connectors API.
  <https://learn.microsoft.com/en-us/microsoft-365/copilot/connectors/overview>
- The data lands in Microsoft Graph and serves Copilot and Microsoft Search. It is not a feed for
  your own index.
- INFER: the Graph connectors API, with a per-item ACL, is a possible Sluiceway sink later.
- **Choose it instead when** Copilot or Microsoft Search is where your people ask questions.

### Amazon Bedrock Managed Knowledge Base (ACL-aware retrieval)

- FOUND: permissions (allowed and denied users and groups) are ingested with content, and
  retrieval filters on a user context. For SharePoint, OneDrive, Google Drive and Confluence a
  second, real-time check asks the source whether the user still has access. S3 and Custom
  sources take ACLs from customer-provided metadata. None of Sluiceway's five providers is on
  the list. <https://docs.aws.amazon.com/bedrock/latest/userguide/kb-managed-acl.html>
- FOUND: the documentation is unusually plain about limits, and it is a model for honest writing:
  - "ACL awareness is not authorization". The service "does not authenticate end users", and the
    application is responsible for passing a verified identity.
  - "ACL-aware retrieval fails closed."
  - "Group memberships are as fresh as the last sync."
  - Email is the universal identifier. If emails differ across systems, "ACL matching fails
    silently". A reassigned email address is the customer's problem to detect.
- INFER: the Custom source, which accepts customer-provided ACL metadata, is a possible sink.
- **Choose it instead when** you are on AWS, your sources are SharePoint, OneDrive, Google Drive
  or Confluence, and a managed service is what you want.

### Slack's own answer: Real-Time Search API and Data Access API

- FOUND: Slack now offers a Real-Time Search API. Data stays in Slack, and results follow each
  user's permissions. Private channel content needs consent from an admin and from the end user.
  <https://docs.slack.dev/apis/web-api/real-time-search-api/>
  <https://slack.com/blog/news/mcp-real-time-search-api-now-available>
- FOUND: the Data Access API gives an app short-lived access to what the invoking user can see,
  for RAG queries. <https://api.slack.com/docs/apps/data-access-api>
- INFER: for Slack, the platform owner is steering AI products toward live, per-user retrieval
  and away from copying messages into an outside index. See section 3.

### Airweave

- FOUND: "Open-source context retrieval layer for AI agents and RAG systems". MIT. About 6.6k
  stars. Python and TypeScript. Needs PostgreSQL, Vespa, Temporal and Redis. 50+ connectors,
  including Slack, Teams, Outlook Mail, ClickUp and HubSpot.
  <https://github.com/airweave-ai/airweave> <https://docs.airweave.ai/llms.txt>
- FOUND: the Slack connector is federated: "Instead of syncing all messages and files, this
  source searches Slack at query time using the search.all API endpoint", with the user's own
  `search:read` scope. <https://docs.airweave.ai/docs/connectors/slack.md>
- FOUND: the documentation index has no page on access control, ACLs or deletion handling. Its
  webhooks pages are about Airweave notifying you of sync events, not about receiving provider
  webhooks. INFER: isolation is by collection and by whose credentials made the connection.
  NOT VERIFIED: whether the code carries source ACLs for some connectors.
- **Choose it instead when** you want a ready retrieval layer for agents over many apps, and
  "each user or tenant has their own collection" is enough access control.

### PipesHub

- FOUND: "an open-source platform for securely connecting enterprise knowledge to AI", Apache
  2.0, about 3.8k stars, "Permission-Aware Search: Enforces source-level access controls". Needs
  a graph database (Neo4j or ArangoDB), Qdrant, MongoDB and Redis, with Kafka in larger
  deployments. <https://github.com/pipeshub-ai/pipeshub-ai>
- FOUND: connectors include Slack, Microsoft Teams and Outlook. ClickUp and HubSpot were not
  listed. The connector overview says "Periodic syncing - Data is synced on a schedule, not in
  real-time" and describes connectors as importing "data into PipesHub for indexing".
  <https://docs.pipeshub.com/connectors/overview> The README says "real-time and scheduled
  indexing". NOT VERIFIED: which connectors use webhooks.
- **Choose it instead when** you want an open source, Apache 2.0, complete alternative to Glean
  and you are ready to run its data stores.

### Related, not competing

- **Convoy** (Go, Elastic License 2.0, about 2.9k stars): a webhooks gateway that ingests,
  persists, retries and delivers webhooks, with idempotency keys. It knows nothing about
  providers, records or permissions. <https://github.com/frain-dev/convoy> Choose it when you
  need a general webhook gateway.
- **OpenFGA, SpiceDB, Oso, Cerbos**: authorization engines. They answer "may this person see this
  object" at query time. They are the enforcement half, and guides from Truto, Paragon and Cerbos
  place them after retrieval. <https://github.com/openfga/openfga>
  <https://www.cerbos.dev/blog/authorization-for-rag-applications-langchain-chromadb-cerbos>
  INFER: Sluiceway's scope and membership data could feed any of them.
- **Azure AI Search SharePoint indexer**: FOUND: a preview API (2026-05-01-preview) ingests
  SharePoint permission metadata and honors it at query time.
  <https://learn.microsoft.com/en-us/azure/search/search-indexer-sharepoint-access-control-lists>

---

## 3. A platform fact that shapes everything: Slack's 2025 terms

- FOUND: on 2025-05-29 Slack announced that commercially distributed apps outside the Slack
  Marketplace get `conversations.history` and `conversations.replies` limited to **1 request per
  minute and 15 objects per request**. This applied at once to new apps and new installations.
  New API terms took effect for older apps on 2025-06-30. Slack's stated reason includes
  preventing "bulk data exfiltration".
  <https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps/>
- FOUND: "Internal customer-built applications" keep the old limits (50+ requests per minute,
  up to 1,000 objects). The same note points developers to the Events API for "focused,
  contextual information". A clarification followed on 2025-06-03.
  <https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/>
- FOUND: the terms were updated again on 2025-10-13 for the Real-Time Search API and the Data
  Access API, including language on temporary caching and storage.
  <https://docs.slack.dev/changelog/2025/10/13/api-terms-update/>
- NOT VERIFIED: Onyx's statement that Slack "recently reversed these API restrictions". I did not
  find this in Slack's own changelog.

INFER, and this needs the maintainer's own reading of Slack's terms, not mine:

1. A company that self-hosts Sluiceway for **its own** workspace, with a Slack app it created
   itself, looks like an internal customer-built app. Events API ingestion and history-based
   reconciliation should both work at normal limits.
2. A vendor that uses Sluiceway to ingest **its customers'** workspaces is commercially
   distributing a Slack app. Outside the Marketplace, Slack reconciliation would run at 15
   messages per minute per workspace, which makes backfill impractical. Inside the Marketplace,
   the vendor must pass Slack's review and follow its data terms.
3. Webhooks first (the Events API) is the path Slack itself recommends, which suits the design.
   The part at risk is the reconcile half of "webhooks first, reconciliation always".
4. Sluiceway's documentation for Slack should say which of these two cases it supports, before
   anyone finds out the hard way.

---

## 4. The trend toward not copying data at all

FOUND: three separate vendors now offer live, per-user retrieval next to, or in place of,
indexing: Slack (Real-Time Search API), Microsoft (federated Copilot connectors over MCP), and
Airweave and Onyx for their Slack connectors (links above). Paragon lists "RAG queries with tool
calling" at prompt time as the safest of four permission designs, "but performance-limited".

FOUND: the counter-argument, from Onyx's documentation: the indexed connector "performs
significantly better than the Slack Search APIs at finding relevant results".

INFER: "just query the source live with the user's token" is a serious alternative to Sluiceway
for some teams, and the positioning must say so. Ingestion still wins when you need ranking
across sources, memory that outlives a query, offline processing (summaries, entity graphs), or a
source with no usable search API.

---

## 5. Comparison table

All cells read 2026-09-19. "?" means not verified. "Five" means Slack, Microsoft Teams, Outlook,
ClickUp, HubSpot, in that order (S, T, O, C, H).

| Project | Kind | License | Self-host | Ingest | Source permissions carried | Duplicates and edits | Covers (S T O C H) |
|---|---|---|---|---|---|---|---|
| **Sluiceway (planned v0.1; only M0 runs)** | ingestion component | Apache 2.0 | yes | webhooks first, cursor reconcile (planned) | scope on every record plus membership sync (planned) | deterministic ids, supersede chain (id recipes and outbox run today) | planned: all five |
| Airbyte | ELT platform | ELv2 (protocol MIT) | yes | polling | 3 connectors (SharePoint Enterprise, Google Drive, Salesforce) | sync modes at the destination | S T O H yes, C ? |
| Nango | auth and integration platform | Elastic | auth and proxy free; syncs and webhooks paid | polling syncs and webhooks | developer builds it | developer builds it | auth for most APIs, per provider ? |
| Unstructured ingest | parsing plus connectors | Apache 2.0 (library) | yes | batch | ? | re-run based | S O yes, others ? |
| LlamaIndex / LangChain loaders | libraries | MIT | n/a | one-time pull | no (Slack reader) | no | S yes, others ? |
| Onyx | complete search and chat product | MIT (CE), EE separate | yes | periodic jobs (?) | yes, EE only; documented for S and O of the five | index upsert, separate permission sync | all five as connectors |
| PipesHub | complete workplace AI platform | Apache 2.0 | yes | scheduled (docs), real-time (README) | yes, graph database | ? | S T O yes, C H no |
| Airweave | retrieval layer | MIT | yes | scheduled sync; Slack is live search | not documented | ? | all five (Slack federated) |
| Glean | commercial product | proprietary | no (SaaS tenant) | crawl, some webhooks, push API | yes, core feature | product internal | H yes, others ? (widely known yes) |
| Merge | unified API | proprietary | no | polling, webhooks for Drive and Box ACLs | file storage models | ? | H and C as data (?), no ACLs |
| Paragon Managed Sync | embedded integration, managed ingestion | proprietary | ? | webhooks plus periodic full refresh | managed graph plus Permissions API | "deduplicating conflicting versions" (vendor blog) | not named in overview |
| Vectara | RAG platform | proprietary | ? | you push documents | metadata filters you set | you decide | n/a (a destination) |
| Copilot connectors | Microsoft 365 feature | proprietary | no | periodic crawl, or live (federated) | per-item ACL | platform internal | 100+ prebuilt, five ? |
| Bedrock Managed Knowledge Base | AWS service | proprietary | no | crawl | allow and deny lists, real-time re-check for 4 sources | platform internal | none of the five |
| Slack Real-Time Search API | platform API | Slack terms | no | none (live) | native, per user | n/a | S only |

---

## 6. The problem in people's own words

Phrases collected for the README and the docs. Each was read on 2026-09-19. Check the exact
wording at the link before quoting in public.

**Permission leaks**

- "The failure mode is simple: the model composes a fluent answer from a document the asking user
  was never allowed to see." Kevin Riedl, Wavect, 2026-06-11.
  <https://wavect.io/blog/rag-permissions-sharepoint-confluence-drive/>
- "A user queries 'Q4 revenue projections' and the system dutifully returns the most semantically
  similar chunks" from "a confidential board deck that the user was never supposed to see."
  "The embedding model does not know about permissions." "This is the primary reason many
  enterprises stall on RAG deployments". Kirk Ryan, 2026-03-03.
  <https://kirkryan.co.uk/item-level-permissions-in-rag-why-your-vector-database-needs-access-control/>
- "Failing to properly handle user permissions with your AI-enabled features/agents can create
  silent data leaks". Nango blog, 2026-04-01 (link in section 2).
- "Vector databases do not inherit source-system permissions." Medium article found in search.
  NOT VERIFIED at the page itself.
  <https://medium.com/@riadmouja47/why-rag-security-is-broken-and-how-to-fix-it-8697845a1a3d>
- "Access control bypass occurs when the RAG pipeline doesn't enforce the same permissions as the
  source system." Truto, 2026-05-06 (link in section 2).
- In Japanese, from a Qiita article dated 2026-09-17: a search service that fetches text with
  broad rights and hands it to the model has not inherited the source system's viewing limits,
  even when the user could not open the original document.
  <https://qiita.com/kagi_to_packet/items/e5d13edbefc293de4e90>

**Stale permissions (the revocation window)**

- "From 9 AM Monday to 2 AM Tuesday, that user can still pull highly sensitive finance documents
  from your RAG pipeline." Truto (same link).
- "permission changes in SharePoint are not automatically propagated" and a revoked user "may
  still see the document's content in the search index or RAG pipeline until the next ingestion
  run." Elena Vavilova, Microsoft ISE blog, 2026-04-30.
  <https://devblogs.microsoft.com/ise/sharepoint-doc-level-access/>
- "You are now responsible for making sure your permissions data is always up-to-date." Paragon.
  <https://www.useparagon.com/learn/permissions-access-control-for-production-rag-apps/>
- "Permissions are table-stakes." Paragon (same page).

**Why syncing permissions is hard**

- "Many integrations simply don't have permissions APIs" and many that do "don't expose one that
  offers a centralized view of both logic + data." Hazal Mestci, Oso, 2025-07-01.
  <https://www.osohq.com/post/should-you-respect-3rd-party-permissions-or-sync-to-your-own-system-the-rag-chatbot-dilemma>
- "Attempting to flatten these complex, graph-like relationships into simple key-value metadata
  tags is incredibly difficult." Truto (same link as above).
- INFER: Sluiceway's five providers have a simpler model than file systems: visibility follows
  one container (channel, list, mailbox, portal), with no inheritance tree. That is why a single
  scope rule can be honest there. It would not be honest for SharePoint or Google Drive.

**Webhooks that go missing**

- "The subscription remains active and valid, but notifications stop being delivered for periods
  of approximately 1 to 1.5 hours, without any error or lifecycle notification being sent to my
  endpoint." A developer on Microsoft Q&A, 2026-03-17, about mail notifications.
  <https://learn.microsoft.com/en-us/answers/questions/5825832/bug-issue-microsoft-graph-webhook-subscriptions-in>
- Microsoft's own documentation: notifications during a pause "are lost", and "the app should
  separately fetch those changes, for example using the delta query". Missed-notification events
  exist only for Outlook resources, and only if the subscription was created with a
  `lifecycleNotificationUrl`.
  <https://learn.microsoft.com/en-us/graph/change-notifications-lifecycle-events>
- Slack: respond "within three seconds", retries "up to 3 times", and event subscriptions are
  "temporarily disabled" when more than 95% of deliveries fail within 60 minutes. The page does
  not say that events missed while disabled are sent again.
  <https://docs.slack.dev/apis/events-api/>
- "The webhook makes you fast; the reconciliation makes you correct." From a general webhook
  article found in search. NOT VERIFIED at the page itself.
  <https://dev.to/weston_carnes_d580b505e0c/webhook-reliability-delivering-and-receiving-events-without-losing-them-16j5>

**Duplicates, edits, deletions, a stale index**

- "Duplicate chunks waste top-k slots, crowd out distinct evidence, inflate embedding and
  reranking costs, and make repeated claims look better supported than they are." Paragon blog.
  <https://www.useparagon.com/blog/rag-freshness-incremental-sync-deduplication>
- The same article lists what each team rebuilds: "auth, backfills, cursors, pagination, rate
  limits, retries, deletes, deduplication, reconciliation, normalization, and permissions".
- A RAG index "is a snapshot, and your sources keep changing after that snapshot is taken."
  kapa.ai (search summary; NOT VERIFIED at the page).
  <https://www.kapa.ai/library/how-to-keep-a-rag-knowledge-base-in-sync-with-changing-docs>
- Deleted documents that "still show up" because the delete never reached chunks and embeddings.
  Oracle developers blog (search summary; NOT VERIFIED at the page).
  <https://blogs.oracle.com/developers/how-to-detect-rag-index-drift-deleted-docs-stale-chunks-and-duplicate-embeddings>

**Words people use, and words they do not.** FOUND across the sources: "permission-aware",
"ACL-aware", "document-level access control", "document-level permissions", "permission sync",
"ACL sync", "respect source permissions", "stale permissions", "data leak", "pre-filter" and
"post-filter", "incremental sync", "keep the index fresh". INFER: nobody in these sources says
"permission-stamped", "scope" in Sluiceway's sense, or "AI memory" for this problem. The README
should use the common words first and introduce its own terms second.

---

## 7. Where these people are

A map for later. **Nobody was contacted and nothing was joined or posted.** Rules change, so read
each community's current rules before any post. Reddit pages could not be fetched by my tools, so
every Reddit rule below is NOT VERIFIED.

### Global, English

| Place | What it is | Activity | Self-promotion rules |
|---|---|---|---|
| Hacker News, Show HN | General technology news, strong infrastructure audience | Very high | FOUND: Show HN is for "something you've made that other people can play with". If it is not ready to try, "please don't do a Show HN". "Please don't ask friends to upvote or comment." <https://news.ycombinator.com/showhn.html> So: not before M6. |
| Lobsters | Invite-only technical link site | Medium, high quality | FOUND: invite needed. "self-promo should be less than a quarter of one's stories and comments." New users cannot use some tags, `show` among them. <https://lobste.rs/about> |
| r/Rag, r/LocalLLaMA, r/LangChain | RAG builders | High | NOT VERIFIED. Reddit's general custom is roughly nine ordinary contributions for each promotional one, and moderators judge patterns. Read the sidebar first. |
| r/dataengineering | Data engineers | High | NOT VERIFIED. Known for strict limits on vendor posts. |
| r/golang | Go developers | High | NOT VERIFIED. Read the rules on project posts and on AI-assisted code before posting: this repository is built with agents and should say so openly. |
| r/selfhosted | People who run their own services | High | NOT VERIFIED. INFER: wants something that installs today, so after M6. |
| MLOps Community (Slack) | Practitioners of ML in production | FOUND: over 27,900 members per <https://datatalks.club/blog/slack-communities.html> | Rules page exists but could not be read: <https://mlops.notion.site/MLOps-Community-Rules-0c69be943d0f4efa9e7863414fefc250>. Search summary says bare links to your own posts are unwelcome: bring the content and context. |
| Relevance Slack and Haystack conference | Search relevance engineers (OpenSource Connections) | FOUND: Slack had 650+ members in 2021 <https://opensourceconnections.com/blog/2021/07/06/building-the-search-community-with-relevance-slack/>; conference at <https://haystackconf.com/> | NOT VERIFIED. Talks are practitioner talks, not product pitches. |
| AI Engineer conferences | Large AI engineering events | FOUND: World's Fair was 2026-06-29 to 2026-07-02 in San Francisco; New York event 2026-10-12 to 2026-10-14 <https://ai.engineer/> | Talks through a call for speakers. |
| Project servers (Onyx Discord, LlamaIndex Discord, and similar) | Users of adjacent projects | High | INFER: good places to **listen** to the problem. Presenting your own project in another project's server is rarely welcome. |

### Go infrastructure

| Place | What it is | Notes |
|---|---|---|
| GopherCon Singapore | Closest major Go conference to Indonesia | FOUND: 2026 edition was 2026-05-20 to 2026-05-22; its call for proposals closed 2026-03-06. <https://2026.gophercon.sg/cfp/> INFER: the 2027 call likely opens around the start of 2027. Architecture section 10 is talk material. |
| GopherCon Europe (Berlin) and GopherCon (US) | Main Go conferences | <https://www.gophercon.eu/> <https://www.gophercon.com/> Calls for speakers through Sessionize. |
| Go wiki lists | User groups and conferences worldwide | <https://go.dev/wiki/GoUserGroups> |
| Golang Weekly and similar newsletters | Curated links | NOT VERIFIED: how to suggest a link. Editors choose; you may suggest once. |

### Indonesia and Southeast Asia

| Place | What it is | Notes |
|---|---|---|
| Golang Indonesia (Telegram) | Indonesian Go developers | FOUND: <https://telegram.me/golangID>, has a code of conduct. NOT VERIFIED: size and its rule on sharing projects. |
| GoJakarta meetup | Go meetup in Jakarta | FOUND: <https://www.meetup.com/gojakarta/>, moving to Luma in 2026-07 per the search summary. A talk is the natural format. |
| AI Tinkerers Jakarta | Builders of AI systems, demo-focused meetups | FOUND: <https://jakarta.aitinkerers.org/>, part of a network in 231 cities. Format is a live demo, so after something runs. |
| Indonesia AI (Discord) and KOMUNITAS AICO (Discord) | Indonesian AI communities | FOUND in search: about 5,400 and about 19,000 members. NOT VERIFIED: rules, and how much of the talk is about engineering. |

### Other languages

| Place | What it is | Notes |
|---|---|---|
| GeekNews (Korea), Show GN | Korean technology news, modelled on Hacker News | FOUND: Show GN is for things people can really run; no repeated submissions in a short time; do not ask people you know for upvotes or comments; early work is welcome. <https://hada.io/blog/geeknews-show/> |
| Qiita and Zenn (Japan) | Engineering article platforms | FOUND: RAG permission design is an active topic (the Qiita article in section 6, dated 2026-09-17). Articles are in Japanese. Propose one only if someone can write and maintain it. |
| CSDN, Zhihu, Datawhale (Chinese) | Articles and study groups on RAG | FOUND in search: permission-aware knowledge bases ("RBAC-RAG") are discussed. <https://github.com/datawhalechina/all-in-rag> NOT VERIFIED: rules. |

INFER about all of these: the honest format everywhere is a technical article that teaches
something true (architecture section 10 has twelve candidates), with the project mentioned once.
That is Mode 4 work, and only when what it describes runs.

---

## 8. Trust signals that comparable projects show first

What a visitor sees before scrolling, read 2026-09-19.

| Project | First screen |
|---|---|
| OpenFGA (Apache 2.0, about 5.8k stars) | Badges: release, Go reference, Go Report Card, coverage, CII Best Practices, OpenSSF Scorecard, SLSA 3, FOSSA. A production statement ("Used in production by Auth0 FGA since December 2021"). Supported storage versions (PostgreSQL 14+, MySQL 8). Docker quickstart. <https://github.com/openfga/openfga> |
| River (MPL-2.0, about 5.7k stars) | CI badge, Go reference, one sentence that says what it is, then working code, then the core idea about transactional enqueueing. <https://github.com/riverqueue/river> |
| Onyx | Demo animation and one install command. <https://github.com/onyx-dot-app/onyx> |
| PipesHub | Tagline and one install command. <https://github.com/pipeshub-ai/pipeshub-ai> |
| Nango | Badges (stars, license, downloads), "What is Nango?", "How it works". <https://github.com/NangoHQ/nango> |
| Convoy | CI badges, container images, links to docs and community chat. <https://github.com/frain-dev/convoy> |
| Bedrock ACL documentation | Not a README, but the best model found for security honesty: a boxed statement of what the feature is **not**, a "Failure behavior" section, and a "Your responsibilities" list. <https://docs.aws.amazon.com/bedrock/latest/userguide/kb-managed-acl.html> |

INFER, the pattern for infrastructure that handles access: (1) one sentence of what it is, (2) an
honest status, (3) a command that ends in something visible, (4) supported versions, (5) a
security policy and a statement of what is and is not protected, (6) a CI badge that is green.
Sluiceway's README today has 1, 2 and a diagram. It cannot have 3 until M5 or M6. It can have 4,
5 and 6 sooner.

---

## 9. What I could not verify

- Reddit community rules (the pages cannot be fetched by my tools).
- The MLOps Community rules page (it did not render).
- Onyx's statement that Slack reversed its 2025 API restrictions. Slack's own changelog did not
  show it in what I read.
- Whether my reading of "internal customer-built application" in Slack's terms fits a
  self-hosted Sluiceway. This needs the maintainer, and perhaps a lawyer.
- Which Airbyte connectors are MIT and which are ELv2 today, and whether a ClickUp source exists.
- Which Unstructured, LlamaIndex and LangChain connectors exist today for Teams, Outlook,
  ClickUp and HubSpot, and whether any emits permission data.
- How Onyx ingests (polling or webhooks) for each of the five providers.
- Whether Airweave or PipesHub carry source ACLs for Slack, Teams or Outlook in code, beyond
  what their documentation says. I did not read their source code.
- Glean's, Merge's and Paragon's exact coverage of the five providers, their prices, and any
  self-hosted options.
- The publication dates the tool reported for two Paragon pages (2026-09-18). They may be "last
  updated" dates.
- Exact wording of every quotation (see the caution at the top).
- Several items marked "search summary" in section 6, which I did not open directly.
