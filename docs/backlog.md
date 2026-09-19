# Backlog: 30 days to v0.1.0

[roadmap.md](roadmap.md) says what order things land in and what "done" means. This file is the
working plan underneath it: each milestone cut into items small enough to finish in one sitting,
with target dates for the 30 days from **2026-09-18 to 2026-10-17**.

The dates live here and not in the roadmap on purpose. When they slip, edit this file; the roadmap
stays true.

## How to work this file

Every item is also a GitHub issue with the same number: **B03 is
[#3](https://github.com/gablooge/sluiceway/issues/3)**, B17 is #17, and so on. Each milestone is a
[GitHub milestone](https://github.com/gablooge/sluiceway/milestones) carrying the target date from
the table below. GitHub holds the status (open, closed, comments); this file holds the order, the
dates and the log.

1. Take the first unchecked item. Items are ordered so each one only depends on items above it.
2. Do it on a branch named after the item (`b03-store-rls`), with tests, until its **Done when**
   line is true and `make check` is green.
3. Tick the box, fill in the date in the log at the bottom, and commit the tick with the work.
   End the commit or pull request message with `Closes #NN` so the issue closes when it reaches
   `main`, and tick the checklist in the issue.
4. If an item turns out to be two days of work, split it here and open a new issue under the same
   milestone before continuing, rather than letting it sprawl.
5. When a date moves, change it in the table below and on the GitHub milestone together.

Items marked **(needs you)** cannot be finished by code alone: they need a real provider account,
a secret, or a decision. The code and the fake-provider tests still land; the real-tenant check is
ticked separately.

## Schedule at a glance

| Milestone | Items | Target window | Days |
|---|---|---|---|
| M0 Foundations | B01 to B04 | Sep 18 to Sep 21 | 4 |
| M1 ClickUp end to end | B05 to B12 | Sep 22 to Sep 29 | 8 |
| M2 Credentials and connecting | B13 to B14 | Sep 30 to Oct 1 | 2 |
| M3 The other four providers | B15 to B20 | Oct 2 to Oct 7 | 6 |
| M4 Reconciliation and access sync | B21 to B24 | Oct 8 to Oct 11 | 4 |
| M5 Hardening and operations | B25 to B27 | Oct 12 to Oct 14 | 3 |
| M6 v0.1.0, public | B28 to B29 | Oct 15 to Oct 16 | 2 |
| Buffer | | Oct 17 | 1 |

This is tight: the roadmap's own sizing adds up to more than 30 days of solo work, and one buffer
day is not much. If the plan slips, cut in this order, and move what was cut to "After v0.1" in the
roadmap:

1. HubSpot signed webhooks (it is reconcile-only by default anyway)
2. the `nango` vault (keep `local` and `azureapp`)
3. Teams (it shares the Graph client with Outlook, so it is the cheapest provider to add back)
4. access sync for providers other than Slack and ClickUp

Never cut: the isolation suite, the strict stub sink, the crash and re-send tests. Those are the
project's whole claim.

---

## M0 · Foundations

- [x] **B01 Skeleton, config, CI.** `cmd/sluiceway` with `serve`, `worker`, `migrate`, `version`
  (stubs where the role does not exist yet); `internal/config` from the environment with
  fail-closed defaults; `Makefile` with `check`; `.golangci.yml`; GitHub Actions running vet, lint
  and `go test -race`.
  **Done when:** `make check` is green locally and in CI; config tests prove an unset environment
  means production, and production refuses to start without a database URL.
- [x] **B02 `internal/ids`.** ULIDs; the blake3 recipes for `delivery_id` and record `id` with the
  `0x1F` separator; golden test vectors checked in as a file so another implementation can verify
  against them.
  **Done when:** golden tests pass; a test proves `("ab","c")` and `("a","bc")` differ; a test
  proves the same change under two tenants yields two ids.
- [x] **B03 Store, migrations, roles, RLS.** pgx pool and transaction helpers; goose migrations
  embedded; the three roles with `INHERIT FALSE`; `internal/tenancy` binding the transaction-local
  setting; integration tests on testcontainers Postgres. Settles open decisions 1 and 2 as ADRs.
  **Done when:** a query with no tenant bound returns zero rows; tenant A cannot read tenant B's
  row; migrations run and re-run as the non-superuser application role.
- [x] **B04 Outbox.** The table, the accept insert with `ON CONFLICT DO NOTHING`, the FIFO-head
  claim with `SKIP LOCKED`, row states, and the retry ladder as pure functions.
  **Done when:** two concurrent claimers never get the same row (run with `-race`, many
  iterations); version 2 of an entity is not claimable while version 1 is in flight; a repeated
  `delivery_id` inserts nothing.

## M1 · One provider end to end: ClickUp

- [x] **B05 Record format.** Go types for the envelope; the JSON Schema; ADRs for open decisions
  3 (scope id format) and 4 (field names), since this becomes a public contract.
  **Done when:** the schema validates the example in architecture section 6 and rejects a record
  with no `visibility.scope`, an unknown `kind`, or an `occurred_at` that is not RFC 3339.
- [ ] **B06 Provider interfaces and the ingress edge.** `internal/provider` with the interfaces
  from architecture section 7 and the registry; `/ingress/{provider}` with raw-body capture, a
  body-size cap and the handshake hook; a fake provider for tests.
  **Done when:** the handler hands the verifier byte-identical input (tested with a body whose
  JSON re-serialization differs); an oversize body is refused without being read into memory.
- [ ] **B07 The hub.** Delivery keys, candidate subscriptions under the resolver role,
  per-candidate constant-time verification, owner resolution, accept, sentinel parking.
  **Done when:** forged signature gets 401 and stores nothing; unknown workspace gets 200 and a
  parked row under the sentinel tenant; two tenants whose secrets both verify get a parked row,
  never a routed one; an identical re-send is a no-op.
- [ ] **B08 Pipeline stages.** Normalize hand-off, the automation-noise gate, the ledger,
  forward-only supersede, regex masking with the redaction map kept locally.
  **Done when:** an old version arriving after a newer one never gets a `supersedes` pointing at
  the newer; masked text contains no email, phone or IBAN from the fixture set; a ledgered id is
  skipped.
- [ ] **B09 Sinks.** `http`, the strict `stub`, `jsonl`; wire name as sink configuration; every
  record validated against the schema in tests.
  **Done when:** the stub rejects everything the schema rejects plus a repeated id with different
  content; the http sink classifies 401/403 as halt, 5xx and timeouts as retryable, 4xx per-record
  rejections as dead-letter.
- [ ] **B10 Worker drain.** Drain goroutine pool, each claimed row worked in a second transaction
  bound to its tenant (never a bind inside the worker role, architecture section 4), the two-transaction
  commit, degrade-to-minimal on hydration failure, dead letters and replay, graceful shutdown.
  **Done when:** a crash injected between prepare and deliver re-drains into "already delivered"
  with exactly one record at the stub; a failing sink walks the ladder and parks; replaying the
  dead letter delivers it.
- [ ] **B11 ClickUp provider.** Signature verification, `Parse`, the direct API client with a rate
  limiter that refuses requests above its capacity, hydration, the normalizer (a comment yields
  the comment and its re-hydrated parent task), golden files.
  **Done when:** signature-negative, parse and normalizer golden tests pass against recorded
  payloads with secrets scrubbed.
- [ ] **B12 M1 acceptance. (needs you)** An end-to-end test with a fake ClickUp API server, then
  the same run against a real workspace through a tunnel.
  **Done when:** every line of the roadmap's M1 "Done when" is demonstrated, the fake-server
  version in CI and the real-workspace version once by hand.

## M2 · Credentials and connecting

- [ ] **B13 Vaults.** The `Vault` interface; `local` (AES-GCM, refuses plaintext, refuses to start
  in production without a key); `nango` (self-hosted, HTTP only); `azureapp` (client credentials).
  Settles open decision 7 as an ADR.
  **Done when:** a test scans every table after a store and finds no token material; tampered
  ciphertext fails to decrypt; a missing key in production is a startup error.
- [ ] **B14 Operator API and `connect`.** `/v1` providers, connection lifecycle, capabilities,
  health; operator credential auth; `sluiceway connect <provider>` reading secrets from the
  environment only; the ClickUp registrar (update in place).
  **Done when:** a connection can be created, completed and deleted through `/v1`; deleting it
  calls the provider-side deregister (asserted against the fake server); re-registering does not
  duplicate the subscription.

## M3 · The other four providers

- [ ] **B15 Slack.** Events API, the URL challenge, the replay window, channels and DMs as scopes,
  mrkdwn links unwrapped before masking, hydration, normalizer goldens.
  **Done when:** signature-negative, stale-timestamp, challenge and normalizer tests pass; a DM's
  scope is the conversation, never a flag.
- [ ] **B16 Graph client and Outlook.** A shared Microsoft Graph client; per-mailbox subscriptions,
  the `validationToken` plain-text echo, `clientState` checks, mixed batches split per owner.
  **Done when:** a batch carrying two tenants' notifications produces two outbox rows with the
  right owners; a wrong `clientState` is refused.
- [ ] **B17 Teams.** Channel-message subscriptions, reply threading into `edges.reply_parent`,
  system events skipped.
  **Done when:** handshake, negative and normalizer golden tests pass.
- [ ] **B18 HubSpot.** Reconcile-only by default; signed webhooks (v3 signature) as an option.
  **Done when:** signature tests pass, and with webhooks off the provider registers no
  `WebhookSource` and the ingress route returns 404.
- [ ] **B19 Registrars and the renewal sweep.** Registrars for Slack, Outlook, Teams, HubSpot; the
  sweep framework (own ticker, advisory-lock election); renewal with recovery when the provider
  has deleted the subscription; an injectable clock.
  **Done when:** under a fast-forwarded clock an expiring subscription renews before it lapses; a
  404 on renew re-registers; two workers never run the same sweep at once.
- [ ] **B20 Subscription health and real tenants. (needs you)** `/v1/subscriptions/health`; one
  real event per provider landed at the sink.
  **Done when:** the health endpoint reports expiring and failed subscriptions; the real-tenant
  checklist below is ticked for each provider you have access to.

## M4 · Reconciliation and access sync

- [ ] **B21 Reconcile framework and ClickUp.** Cursors, chunked passes that commit per chunk and
  pace between chunks, replay through the accept path as a trusted synthesized delivery,
  `sluiceway reconcile <tenant> <provider>`.
  **Done when:** a test asserts no pause ever happens inside an open transaction; a gap left by a
  stopped webhook is filled and the overlap dedupes on record id.
- [ ] **B22 Slack and HubSpot reconcilers.**
  **Done when:** the drain keeps delivering live events while a large reconcile pass runs
  (asserted on delivery latency in an integration test).
- [ ] **B23 Member sources and identity.** `MemberSource` for all five providers; the email-join
  resolver, normalized and domain-restricted.
  **Done when:** an address outside the allowed domains resolves to nobody; a provider read error
  is an error, never an empty list.
- [ ] **B24 Membership diff and `AccessSink`.** The shadow record of what was sent, the diff,
  grants and revocations pushed to sinks that accept them.
  **Done when:** removing someone from a channel yields exactly one revocation; a provider outage
  yields none; a re-run with no change sends nothing.

## M5 · Hardening and operations

- [ ] **B25 Dead-letter re-resolution, retention, metrics.** Parked rows re-resolved on a sweep and
  deleted after retention; Prometheus metrics (outbox depth by stage, oldest undelivered age,
  errors by provider, sweep durations); `/readyz` beside `/healthz`.
  **Done when:** a row parked before its subscription existed delivers after the subscription is
  created; `/readyz` fails when Postgres is unreachable.
- [ ] **B26 Isolation suite.** Two tenants on one provider workspace, forged tenant attempts,
  cross-tenant claim checks, and a test that greps captured logs for token material.
  **Done when:** the suite is green in CI as its own job.
- [ ] **B27 Compose stack.** `docker compose up` with Postgres, serve, worker, the stub sink and
  the local vault; a multi-stage Dockerfile producing a static binary.
  **Done when:** the M1 acceptance test runs against the compose stack with only Docker installed.

## M6 · v0.1.0, public

- [ ] **B28 Docs.** A quickstart from a fresh clone, a setup guide per provider,
  `CONTRIBUTING.md`, the provider-authoring guide, `SECURITY.md`.
  **Done when:** the quickstart has been followed on a clean machine or container, start to finish.
- [ ] **B29 Release. (needs you)** Release workflow, image on GHCR, changelog, tag `v0.1.0`, flip
  the repository to public.
  **Done when:** `docker run ghcr.io/gablooge/sluiceway:v0.1.0 version` prints the tag.

---

## What needs you, and by when

| By | What | For |
|---|---|---|
| Sep 29 | A ClickUp workspace, an API token, and a tunnel (cloudflared or ngrok) | B12 |
| Oct 2 | A Slack app with a signing secret in a test workspace | B15, B20 |
| Oct 3 | An Azure app registration with Graph application permissions, and a test mailbox and team | B16, B17, B20 |
| Oct 5 | A HubSpot developer test portal | B18, B20 |
| Oct 15 | A decision to go public, and a final read of the README | B29 |

Real-tenant checklist (tick when one real event has reached the sink):

- [ ] ClickUp
- [ ] Slack
- [ ] Outlook
- [ ] Teams
- [ ] HubSpot

---

## Log

One line per finished item: date, item, anything worth remembering.

| Date | Item | Notes |
|---|---|---|
| 2026-09-19 | B01 | Code written 2026-09-18, first CI run green on PR #30 the next day. The build-info package is `internal/appversion`, because revive rejects package names that shadow the standard library (`version`, `buildinfo`). |
| 2026-09-18 | B02 | All 14 golden vectors cross-checked against the Python `blake3` package, so the recipes are reproducible outside Go. `RecordID` and `DeliveryID` return an error for an empty part or a part containing `0x1F`; architecture section 5 updated to say so. |
| 2026-09-19 | B03 | Roles and schema come from a one-time admin bootstrap script (`sluiceway migrate bootstrap`); migrations run as the application role, and `store.Open` refuses a superuser or BYPASSRLS login everywhere. Postgres 16 is the minimum. Found on the way: a transaction-local setting reads back as `''` on a pooled connection, so policies go through `current_tenant()`. Mutation-checked: dropping FORCE RLS fails six tests. ADRs 1 and 2 written; sqlc itself arrives with B04. |
| 2026-09-19 | B04 | Completes M0. A claim is a lease with a token, not a held lock. `delivery_id` is unique per tenant, not globally (design change, architecture section 5). The worker role sees only scheduling columns and can never read a payload. sqlc runs from its pinned Docker image (`make sqlc`, `make sqlc-check` in CI). Mutation-checked: the first version of the concurrent FIFO test missed a claim query with no head-of-key rule, so it was rewritten with adjacent versions and now catches it. Review round 1 (2026-09-19): `seq` is assigned at INSERT, not at COMMIT, so Accept and Replay now take a per-key `pg_advisory_xact_lock` before assigning one, and a replayed dead letter takes a fresh `seq` and goes to the BACK of its entity's queue (it could otherwise be leased alongside a newer version in flight). `prepared_at` lets a replay restore `prepared`. Round 2: the claim's snapshot-to-row-lock window is tested deterministically by pausing the claim in a test-only RESTRICTIVE select policy whose function waits on an advisory lock when it is shown a gate row, run under five planner settings. (The first attempt, a BEFORE UPDATE trigger, only paused under the default plan: do not use it.) The lock-before-INSERT order in `Accept` is pinned by stopping an INSERT after it has its `seq`. Round 3: the claim cost O(backlog) per poll (4.1 s at a million waiting rows), so the head of a key is now a stored marker, `is_head`, kept under the key's lock by every writer, the finishing transitions included, and guarded by a unique index and a CHECK ([ADR 10](adr/0010-outbox-head-marker.md)): 0.3 ms at the same size. Worth remembering: a condition that repeats what a CHECK already says (`state` next to `is_head`) made the planner read every waiting row at a batch of 100, so the claim has none, and a guard test bounds the claim's buffers and checks its plan. Also round 3: `Fail` and `MarkDead` take an `outbox.Cause` and no text (an HTTP client's error quotes the URL, API key included), `Claimed` is read-only, ordering keys are at most 512 bytes (`ErrBadOrderingKey`), a claim leases at most `MaxBatch` rows. After the review of the redesign: the head marker made the isolation level load-bearing (under a `default_transaction_isolation` of REPEATABLE READ a writer that waited for the key's lock decides on a snapshot from before the wait and strands a row, silently), so `store` begins every transaction READ COMMITTED by name. An ordering key with a NUL or invalid UTF-8 is `ErrBadOrderingKey` too, not the database's 22021. `Outbox.StrandedKeys` finds a key with work and no head, and ADR 10 has the repair; the sweep and the metric are B25. |
| 2026-09-19 | B05 | Format v1 lives in `internal/record`, and the contract is `record.v1.schema.json` there, embedded. ADRs 3 and 4. What differs from the proposal: a `format` version field, `meta.raw_ref` dropped, `delete` defined now because a closed `op` cannot gain a value later, `visibility` closed while unknown fields are allowed everywhere else, lowercase-only field names. **The record id recipe of B02 changed: the scope is hashed too**, so a record that moves to another scope is a new record even when the provider's version did not change (otherwise the ledger skips it and the sink keeps the old scope, silently); golden vectors regenerated and re-verified with Python `blake3`. The scope id uses the internal provider key, never the wire name, and percent-escapes the container id into one canonical spelling, because it is the join key between a record and its scope's membership. Worth remembering: `format: date-time` is only an annotation in many validators, so `occurred_at` has a pattern too and the tests compile the schema both ways; Python's `$` matches before a trailing newline, so every anchored pattern has a companion that does not lean on the anchor; Go's `encoding/json` matches field names case-insensitively and reads a missing `origin` as "trusted", so `Record` has a strict `UnmarshalJSON`. Left for later items: the kinds freeze at v0.1.0 (B18 is the last chance to add one), the strict stub must add the two checks no schema can make (self-supersede, a field name used twice) and compare content without `meta` (B09), normalizers must move `version` on every change including a move (B11), and the membership message and person identifier are B23 and B24 (issue #36). The schema `$id` needs the maintainer's confirmation before v0.1.0. |
