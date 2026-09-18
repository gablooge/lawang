# Roadmap

This is **sequence, not schedule**: the order milestones land in and what "done" means for each.
Dates are deliberately absent, because a design document that carries dates starts lying the week
they slip. The design itself is in [architecture.md](architecture.md). The dated, item-by-item
working plan is kept apart in [backlog.md](backlog.md), where slipping is cheap.

The repository stays private until **M6**, so the first thing anyone sees is something that runs.

---

## Milestones

Sizes are relative effort: **S** is a few days, **M** about a week, **L** more than a week.

### M0 · Foundations (S)

The skeleton everything else hangs on.

- `cmd/sluiceway` with the `serve`, `worker`, `migrate` and `version` subcommands
- environment config with fail-closed defaults (production unless development is explicit)
- `internal/ids`: ULIDs and the three blake3 key recipes, with golden test vectors
- Postgres via pgx, goose migrations embedded in the binary, the three roles
- `internal/tenancy`: RLS binding with a transaction-local setting
- the outbox table and its FIFO-head claim with `SKIP LOCKED`
- CI: `go vet`, `golangci-lint`, unit tests with `-race`, integration tests against Postgres

**Done when:** CI is green; an integration test proves a query with no tenant bound returns zero
rows; migrations run and are re-runnable as the **non-superuser** application role, not only as
superuser; two concurrent claimers never receive the same row, and a second version of an entity is
not claimable while the first is in flight.

### M1 · One provider end to end: ClickUp (M)

The whole pipeline, proven on the simplest provider.

- the `/ingress/{provider}` edge: raw-body capture, a body-size cap, handshake hook
- the hub: delivery keys, candidate subscriptions under the resolver role, per-candidate signature
  verification, owner resolution, accept with `delivery_id` dedupe, sentinel parking
- the worker drain: hydrate, normalize, gate, ledger, forward-only supersede, mask, deliver, and the
  two-transaction commit
- retry ladder, dead-letter state, and replay
- sinks: `http`, a **strict** `stub` that rejects anything the real format forbids, and `jsonl`
- the record format as a JSON Schema, enforced in CI against real normalizer output
- ClickUp: webhook verification, the registrar (update in place), direct-API hydration, the
  normalizer (a comment yields the comment and its re-hydrated parent task)

**Done when:** a comment on a real ClickUp task produces exactly two records at the sink; re-sending
the same webhook produces no new records; a crash between prepare and deliver re-drains into
"already delivered" rather than a duplicate; a forged signature gets 401 and stores nothing; a
webhook for an unknown workspace is parked, never routed.

### M2 · Credentials and connecting (S)

- the `Vault` interface; `local` (AES-GCM, key from the environment, refuses to store plaintext);
  `nango` (self-hosted, HTTP only); `azureapp` (client credentials for Microsoft Graph)
- `sluiceway connect <provider>`, reading secrets from the environment, never from arguments
- `/v1` operator API: providers, the connection lifecycle, capabilities, health

**Done when:** a connection can be created, completed and deleted through `/v1`; deleting it
deregisters the provider-side webhook; no table contains token material (checked by a test that
scans the database after a full connect); the local vault refuses to start in production without a
key.

### M3 · The other four providers (L)

- **Slack:** Events API, the URL challenge (signature-checked when a signing secret is set), the
  replay window, channels and DMs as scopes, mrkdwn links unwrapped before masking
- **Outlook:** a Graph subscription per mailbox, the `validationToken` echo as plain text,
  `clientState` checks, mixed-subscription batches split per owner, direct Graph hydration
- **Teams:** channel-message subscriptions, hourly expiry, reply threading, system events skipped
- **HubSpot:** reconcile-only by default, signed webhooks as an option
- the renewal sweep, including recovery when a provider has deleted the subscription
- `/v1/subscriptions/health`

**Done when:** each provider lands a real event at the sink from a real tenant, and each has
signature-negative, handshake, and normalizer golden-file tests; an expiring subscription renews
before it lapses under a fast-forwarded clock.

### M4 · Reconciliation and access sync (M)

- cursors, chunked reconcile passes with pacing between chunks and never inside a transaction
- reconcilers for ClickUp, Slack and HubSpot
- member sources for all five providers; the identity resolver (email join, domain-restricted)
- membership diff against a local record of what was sent, pushed to sinks that accept it
- provider read failures abort the pass instead of reading as "no members"

**Done when:** a gap created by stopping the webhook is filled by the next reconcile pass, with the
overlap dedupe proven by record id; the drain keeps delivering live events while a large
reconciliation runs; revoking someone from a channel produces exactly one revocation, and a
provider outage produces none.

### M5 · Hardening and operations (M)

- dead-letter re-resolution for parked rows, and retention for rows that never resolve
- Prometheus metrics: outbox depth by stage, oldest undelivered age, delivery and hydration errors
  by provider, sweep durations; `/readyz` next to `/healthz`
- the isolation suite: two tenants on the same provider, forged tenant attempts, cross-tenant claim
  checks, and a grep test that no log line contains token material
- `docker compose up` for local development with the stub sink and the local vault, needing no
  third-party accounts

**Done when:** the isolation suite is green in CI and the compose stack runs the M1 acceptance test
locally with nothing but Docker installed.

### M6 · v0.1.0, public (S)

- a quickstart that works from a fresh clone
- setup guides per provider
- a container image published to GHCR, a tagged release, a changelog
- `CONTRIBUTING.md` and a provider-authoring guide
- the repository flips to public

**Done when:** someone who has never seen the project can follow the quickstart to a record at the
stub sink.

---

## Parity with the Python predecessor

What carries over, what changes, and what is intentionally left behind.

| Capability | Milestone | Status in the rewrite |
|---|---|---|
| Five providers with webhook ingest | M1, M3 | same |
| Constant-time signature checks over raw bytes; handshakes | M1, M3 | same |
| Tenant from the owned row; ambiguous owner refused | M1 | same |
| Outbox accept with delivery dedupe; 202 before any provider I/O | M1 | same |
| FIFO per entity, retry ladder, dead letters as a row state, replay | M0, M1 | same |
| Degrade to a minimal record when hydration fails | M1 | same |
| Ledger idempotency; forward-only supersede chain | M1 | same |
| PII masking with the redaction map kept locally, never sent | M1 | same |
| Strict stub sink; record format enforced in CI | M1 | same, and the stub is strict from day one |
| Wire name separate from the internal key | M1 | same, now sink configuration |
| Local, Nango and Azure-app vaults; connect CLI | M2 | same |
| Registrars and renewal with deleted-subscription recovery | M3 | same |
| Reconciliation for ClickUp, Slack and HubSpot | M4 | **changed:** chunked commits, pacing outside transactions |
| Membership sync with a shadow diff and email identity join | M4 | **generalized** into access sync for any sink that accepts membership |
| DMs made private through a flag | M1, M3 | **changed:** one uniform scope-membership rule, no private flag |
| Worker as a single sequential loop | M0 | **changed:** independent goroutines; sweeps elected by advisory lock |
| Hydration through MCP server containers | later | **changed:** direct API clients by default; MCP becomes an optional hydrator |
| Idle container reaping | none | **dropped:** no per-tenant containers in the default deployment |
| Dead-letter re-resolution and retention | M5 | same |
| Operator API and health surfaces | M2, M5 | same, plus Prometheus metrics |

---

## After v0.1

Roughly in priority order.

- **Deletions.** Providers that report deletes, and reconciliation that notices absences, emit
  `op: "delete"` records. The format already reserves the field.
- **Untrusted-origin marking** populated per provider (inbound mail, external guests). The format
  already carries `origin.untrusted`.
- **An MCP-backed hydrator** and a tool-calling facade for acting on providers.
- **More providers:** Gmail, Google Drive, Notion, Asana, Jira, GitHub.
- **More sinks:** pgvector, webhook fan-out, S3.
- **Kubernetes:** a Helm chart and a guide to running workers as a deployment.
- **OAuth consent flows** through Nango's connect UI for providers that need user consent.

---

## Open decisions

Each becomes a short decision record under `docs/adr/` when it is settled.

| # | Question | Leaning | Why it matters |
|---|---|---|---|
| 1 | Typed queries with `sqlc`, or hand-written pgx | `sqlc` | Most bugs in the predecessor's data layer were query-shape mistakes a generator catches at build time |
| 2 | Migration tool | `goose`, embedded | Plain SQL files, runs inside the binary, no separate install |
| 3 | Scope id format | `{source}:{container_kind}:{container_id}` | Readable and deterministic; the tenant travels separately |
| 4 | Final field names in the record format | the proposal in architecture section 6 | This is a public contract once v0.1 ships, so it should settle before M1 finishes |
| 5 | Default hydration path | direct API clients | Three of five providers needed them anyway |
| 6 | One binary with modes, or two binaries | one binary | Simpler releases; roles are just subcommands |
| 7 | Vault encryption key source | environment master key for v0.1, a KMS interface later | Keeps the local setup dependency-free |
| 8 | Configuration | environment only for v0.1 | Twelve-factor, container-friendly; a file format can come later |
| 9 | License | Apache 2.0 | Patent grant, standard for Go infrastructure projects |
